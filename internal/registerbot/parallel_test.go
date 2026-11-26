package registerbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
)

func fixedRouteTestGateway(t *testing.T) (*Gateway, ChannelSettings, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	node := "node-a"
	pins := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"version":"fixture"}`))
		case r.URL.Path == "/proxies":
			_, _ = fmt.Fprintf(w, `{"proxies":{"group":{"type":"Selector","now":%q,"all":["node-a","node-b"]}}}`, node)
		case r.Method == http.MethodPut:
			var request struct {
				Name string `json:"name"`
			}
			if errDecode := json.NewDecoder(r.Body).Decode(&request); errDecode != nil {
				t.Error(errDecode)
			}
			node = request.Name
			pins.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	set := DefaultSettings()
	set.Gateway.ControllerURL = server.URL
	set.Gateway.ProxyURL = server.URL
	set.Gateway.VerifyExitIP = false
	gateway := NewGateway(func() Settings { return set })
	if status := gateway.Probe(context.Background(), true); status.Mode != GatewayModeController {
		t.Fatal("fixture controller unavailable")
	}
	return gateway, ChannelSettings{ID: "one", Group: "group", Nodes: []string{"node-a"}}, pins
}

func TestGatewayFixedRouteAllowsParallelAccounts(t *testing.T) {
	gateway, channel, pins := fixedRouteTestGateway(t)
	first, errFirst := gateway.Acquire(context.Background(), channel, "first")
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, errSecond := gateway.Acquire(ctx, channel, "second")
	if errSecond != nil {
		t.Fatalf("same fixed route was serialized: %v", errSecond)
	}
	if first.Node != second.Node || pins.Load() != 1 {
		t.Fatal("parallel account repinned an active route")
	}
	gateway.Release(channel.Group, "first")
	conflict := channel
	conflict.Nodes = []string{"node-b"}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, errConflict := gateway.Acquire(cancelled, conflict, "conflict"); !errors.Is(errConflict, context.Canceled) {
		t.Fatalf("conflicting route entered live lease: %v", errConflict)
	}
	if pins.Load() != 1 {
		t.Fatal("route changed while the second account remained active")
	}
	gateway.Release(channel.Group, "second")
	third, errThird := gateway.Acquire(context.Background(), conflict, "third")
	if errThird != nil || third.Node != "node-b" {
		t.Fatalf("released route did not advance: %+v %v", third, errThird)
	}
	gateway.Release(channel.Group, "third")
	t.Log("two accounts share one confirmed fixed route; incompatible routes wait until both release")
}

func TestGatewayDegradedForwardingAllowsParallelAccounts(t *testing.T) {
	set := DefaultSettings()
	g := NewGateway(func() Settings { return set })
	channel := DefaultFreeAIChannel()
	if _, errFirst := g.Acquire(context.Background(), channel, "one"); errFirst != nil {
		t.Fatal(errFirst)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, errSecond := g.Acquire(ctx, channel, "two"); errSecond != nil {
		t.Fatalf("non-mutating fallback serialized: %v", errSecond)
	}
	g.Release(channel.Group, "one")
	g.Release(channel.Group, "two")
}

type parallelFixtureDriver struct {
	entered      chan string
	release      chan struct{}
	active, peak atomic.Int32
}

func (d *parallelFixtureDriver) Kind() string                { return ChannelKindFreeAIApiKey }
func (d *parallelFixtureDriver) Probe(context.Context) error { return nil }
func (d *parallelFixtureDriver) Run(ctx context.Context, task *Task) (Harvest, error) {
	n := d.active.Add(1)
	defer d.active.Add(-1)
	for old := d.peak.Load(); n > old; old = d.peak.Load() {
		if d.peak.CompareAndSwap(old, n) {
			break
		}
	}
	select {
	case d.entered <- task.Account.Email:
	case <-ctx.Done():
		return Harvest{}, ctx.Err()
	}
	select {
	case <-d.release:
	case <-ctx.Done():
		return Harvest{}, ctx.Err()
	}
	return Harvest{APIKey: "sk-" + strings.Repeat(task.Account.Email[:1], 43)}, nil
}

func TestEngineBatchReachesConfiguredConcurrencyThroughGateway(t *testing.T) {
	gateway, channel, _ := fixedRouteTestGateway(t)
	settings := testSettings()
	settings.Channels[0].ManualMode = false
	settings.Channels[0].Concurrency = 2
	settings.Channels[0].RotateNode = false
	settings.Channels[0].Group, settings.Channels[0].Nodes = channel.Group, channel.Nodes
	e := newTestEngine(t, settings, func(context.Context, ChannelSettings, Harvest) error { return nil })
	e.gateway = gateway
	driver := &parallelFixtureDriver{entered: make(chan string, 3), release: make(chan struct{})}
	defer close(driver.release)
	e.drivers[ChannelKindFreeAIApiKey] = driver
	e.runAccount = func(ctx context.Context, task *Task, harvest Harvest) (Harvest, error) {
		// Browser DOM behavior is covered by the separate installed-browser test.
		// Here the real scheduler and gateway must allow both drivers to enter.
		task.session = &Session{}
		return e.execute(ctx, task, harvest)
	}
	run, errStart := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "a@example.invalid|fixture\nb@example.invalid|fixture\nc@example.invalid|fixture"})
	if errStart != nil {
		t.Fatal(errStart)
	}
	waitEntered := func() {
		t.Helper()
		select {
		case <-driver.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("batch did not reach a second active driver through the gateway")
		}
	}
	waitEntered()
	waitEntered()
	if driver.peak.Load() != 2 {
		t.Fatal("configured concurrency was not reached")
	}
	driver.release <- struct{}{}
	driver.release <- struct{}{}
	waitEntered()
	driver.release <- struct{}{}
	done := awaitRun(t, e, run.ID, func(run RunView) bool { return run.Status == "done" })
	if done.Counters.Done != 3 || driver.peak.Load() != 2 {
		t.Fatalf("batch done=%d peak=%d", done.Counters.Done, driver.peak.Load())
	}
	t.Log("3 input accounts completed; 2 simultaneous drivers through the real gateway; concurrency ceiling respected")
}
