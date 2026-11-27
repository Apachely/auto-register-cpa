package registerbot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGatewayProbeNodesIsReadOnlyAndSplitsResults(t *testing.T) {
	var mu sync.Mutex
	puts := 0
	delays := 0
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"version":"test"}`))
		case r.URL.Path == "/proxies":
			_, _ = w.Write([]byte(`{"proxies":{"group":{"type":"Selector","now":"node-a","all":["node-a","node-b","node-c"]}}}`))
		case r.Method == http.MethodPut:
			puts++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/delay"):
			delays++
			if r.URL.Path == "/proxies/node-b/delay" {
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = w.Write([]byte(`{"message":"Timeout"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"delay":%d}`, 400+delays)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer controller.Close()
	set := DefaultSettings()
	set.Gateway.ControllerURL = controller.URL
	set.Gateway.ProxyURL = controller.URL
	set.Gateway.VerifyExitIP = false
	set.Gateway.NodeProbeConcurrency = 3
	set.Gateway.NodeProbeTimeoutSeconds = 4
	gateway := NewGateway(func() Settings { return set })
	if status := gateway.Probe(context.Background(), true); status.Mode != GatewayModeController {
		t.Fatalf("probe mode=%s", status.Mode)
	}
	result := gateway.ProbeNodes(context.Background(), "group", []string{"node-a", "node-b", "node-c"})
	if result.Total != 3 || len(result.Reachable) != 2 || len(result.Unreachable) != 1 || result.Unreachable[0] != "node-b" {
		t.Fatalf("result=%+v", result)
	}
	mu.Lock()
	observedPuts, observedDelays := puts, delays
	mu.Unlock()
	// A sweep must never move the selector an account is holding.
	if observedPuts != 0 {
		t.Fatalf("sweep changed selector state: %d writes", observedPuts)
	}
	if observedDelays != 3 {
		t.Fatalf("delay requests=%d", observedDelays)
	}
}

func TestGatewayProbeNodesWithoutController(t *testing.T) {
	set := DefaultSettings()
	set.Gateway.ControllerURL = "http://127.0.0.1:1"
	set.Gateway.AutoDiscover = false
	gateway := NewGateway(func() Settings { return set })
	result := gateway.ProbeNodes(context.Background(), "group", []string{"node-a"})
	if len(result.Reachable) != 0 || len(result.Unreachable) != 0 || result.Total != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestProxyPoolApplyProbeResultsPromotesAndCools(t *testing.T) {
	gateway := &fakeProxyPoolGateway{nodes: []string{"node-a", "node-b"}, exits: map[string]string{"node-a": "203.0.113.1"}}
	pool := newTestProxyPool(t, gateway)
	now := time.Unix(1_800_000_000, 0)
	if errMark := pool.markFailureAt("group", "node-a", "exit_unverified", now); errMark != nil {
		t.Fatal(errMark)
	}
	record := pool.doc.Nodes[proxyNodeKey("group", "node-a")]
	if record.CooldownUntil == 0 || record.Failures != 1 {
		t.Fatalf("precondition: %+v", record)
	}
	result := NodeProbeResult{
		Group:       "group",
		Total:       2,
		Reachable:   map[string]int{"node-a": 210},
		Unreachable: []string{"node-b"},
	}
	promoted, cooled, errApply := pool.ApplyProbeResults(result, now.Add(time.Second))
	if errApply != nil {
		t.Fatal(errApply)
	}
	if promoted != 1 || cooled != 1 {
		t.Fatalf("promoted=%d cooled=%d", promoted, cooled)
	}
	recovered := pool.doc.Nodes[proxyNodeKey("group", "node-a")]
	if recovered.CooldownUntil != 0 || recovered.Failures != 0 || recovered.LastReason != "" {
		t.Fatalf("reachable node stayed penalised: %+v", recovered)
	}
	if recovered.ExitIP != "" {
		t.Fatalf("probe must not invent an exit address: %+v", recovered)
	}
	penalised := pool.doc.Nodes[proxyNodeKey("group", "node-b")]
	if penalised.LastReason != "node_unreachable" || penalised.Failures != 1 {
		t.Fatalf("unreachable node: %+v", penalised)
	}
	want := now.Add(time.Second).Add(60 * time.Second).UnixMilli()
	if penalised.CooldownUntil != want {
		t.Fatalf("cooldown=%d want=%d", penalised.CooldownUntil, want)
	}
	if pool.doc.LastProbeAt != now.Add(time.Second).UnixMilli() || pool.doc.Reachable != 1 {
		t.Fatalf("probe bookkeeping: %+v", pool.doc)
	}
	snapshot := pool.Snapshot()
	if snapshot.ProbeReachableNodes != 1 || snapshot.LastProbeAt == 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestPreferMeasuredOrdersReadyThenUnknownThenCooling(t *testing.T) {
	nowMS := time.Unix(1_800_000_600, 0).UnixMilli()
	records := map[string]proxyPoolNode{
		proxyNodeKey("group", "measured"): {Group: "group", Node: "measured", ExitIP: "203.0.113.9"},
		proxyNodeKey("group", "cooling"):  {Group: "group", Node: "cooling", ExitIP: "203.0.113.8", CooldownUntil: nowMS + 30_000},
		proxyNodeKey("group", "unknown"):  {Group: "group", Node: "unknown"},
	}
	ordered := preferMeasured("group", []string{"cooling", "unknown", "measured", "untracked"}, records, nowMS)
	want := []string{"measured", "unknown", "untracked", "cooling"}
	if len(ordered) != len(want) {
		t.Fatalf("ordered=%v", ordered)
	}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("ordered=%v want=%v", ordered, want)
		}
	}
}

func TestProxyPoolAcquireSkipsCoolingNodeFromSweep(t *testing.T) {
	gateway := &fakeProxyPoolGateway{
		nodes: []string{"node-a", "node-b"},
		exits: map[string]string{"node-a": "203.0.113.1", "node-b": "203.0.113.2"},
	}
	pool := newTestProxyPool(t, gateway)
	now := time.Now()
	// The sweep proves node-a is down; node-b is untested but reachable.
	if _, _, errApply := pool.ApplyProbeResults(NodeProbeResult{
		Group: "group", Total: 2, Reachable: map[string]int{"node-b": 180}, Unreachable: []string{"node-a"},
	}, now); errApply != nil {
		t.Fatal(errApply)
	}
	channel := ChannelSettings{ID: "one", Group: "group", RotateNode: true}
	pin, errAcquire := pool.Acquire(context.Background(), channel, "holder")
	if errAcquire != nil {
		t.Fatalf("acquire: %v", errAcquire)
	}
	if pin.Node != "node-b" {
		t.Fatalf("cooled node was preferred: %s", pin.Node)
	}
}
