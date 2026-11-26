package registerbot

import (
	"context"
	"sync"
	"testing"
)

type fakeProxyPoolGateway struct {
	mu       sync.Mutex
	nodes    []string
	exits    map[string]string
	mode     string
	releases int
}

func (f *fakeProxyPoolGateway) Probe(context.Context, bool) Status {
	mode := f.mode
	if mode == "" {
		mode = GatewayModeController
	}
	return Status{Mode: mode}
}

func (f *fakeProxyPoolGateway) NodePool(ChannelSettings) []string {
	return append([]string(nil), f.nodes...)
}

func (f *fakeProxyPoolGateway) Acquire(_ context.Context, channel ChannelSettings, _ string) (PinResult, error) {
	node := ""
	if len(channel.Nodes) > 0 {
		node = channel.Nodes[0]
	}
	f.mu.Lock()
	exitIP := f.exits[node]
	f.mu.Unlock()
	return PinResult{Group: channel.Group, Node: node, ExitIP: exitIP, Mode: GatewayModeController, Verified: true}, nil
}

func (f *fakeProxyPoolGateway) Release(string, string) {
	f.mu.Lock()
	f.releases++
	f.mu.Unlock()
}

func enforcedProxyPoolSettings() ProxyPoolSettings {
	return ProxyPoolSettings{Mode: ProxyPoolModeEnforce, MaxUsableAccountsPerIP: 3, FailureCooldownSeconds: 60}
}

func newTestProxyPool(t *testing.T, gateway proxyPoolGateway) *ProxyPool {
	t.Helper()
	pool, errNew := newProxyPool(t.TempDir(), enforcedProxyPoolSettings(), gateway)
	if errNew != nil {
		t.Fatal(errNew)
	}
	return pool
}

func TestProxyPoolRotatesMeasuredExitAndSkipsAliasNodes(t *testing.T) {
	gateway := &fakeProxyPoolGateway{
		nodes: []string{"node-a", "node-b", "node-c"},
		exits: map[string]string{"node-a": "203.0.113.1", "node-b": "203.0.113.1", "node-c": "203.0.113.2"},
	}
	pool := newTestProxyPool(t, gateway)
	channel := ChannelSettings{ID: "test", Group: "group", Enabled: true}
	first, errFirst := pool.Acquire(context.Background(), channel, "row-1")
	if errFirst != nil || first.Node != "node-a" || first.ExitIP != "203.0.113.1" {
		t.Fatalf("first=%+v error=%v", first, errFirst)
	}
	if _, errComplete := pool.Complete("row-1", Harvest{APIKey: testAPIKey, ExitIP: first.ExitIP}); errComplete != nil {
		t.Fatal(errComplete)
	}
	pool.ReleaseRoute(channel.Group, "row-1")
	second, errSecond := pool.Acquire(context.Background(), channel, "row-2")
	if errSecond != nil || second.Node != "node-c" || second.ExitIP != "203.0.113.2" {
		t.Fatalf("second=%+v error=%v", second, errSecond)
	}
	snapshot := pool.Snapshot()
	if snapshot.LastExitIP != "203.0.113.2" || snapshot.Nodes[1].Failures == 0 {
		t.Fatalf("alias node was not rejected: %+v", snapshot)
	}
}

func TestProxyPoolCountsOnlyCreditedVerifiedKeysAndPersistsIdempotently(t *testing.T) {
	dir := t.TempDir()
	gateway := &fakeProxyPoolGateway{nodes: []string{"node-a"}, exits: map[string]string{"node-a": "203.0.113.10"}}
	pool, errNew := newProxyPool(dir, enforcedProxyPoolSettings(), gateway)
	if errNew != nil {
		t.Fatal(errNew)
	}
	pool.doc.Reservations["credited"] = proxyPoolReservation{Holder: "credited", Group: "group", Node: "node-a", ExitIP: "203.0.113.10"}
	pool.doc.Reservations["no-credit"] = proxyPoolReservation{Holder: "no-credit", Group: "group", Node: "node-a", ExitIP: "203.0.113.10"}
	if usable, errComplete := pool.Complete("credited", Harvest{APIKey: testAPIKey, ExitIP: "203.0.113.10", CreditConfirmed: true}); errComplete != nil || !usable {
		t.Fatalf("credited completion usable=%v error=%v", usable, errComplete)
	}
	if usable, errComplete := pool.Complete("no-credit", Harvest{APIKey: testAPIKey, ExitIP: "203.0.113.10"}); errComplete != nil || usable {
		t.Fatalf("uncredited completion usable=%v error=%v", usable, errComplete)
	}
	if usable, errComplete := pool.Complete("credited", Harvest{APIKey: testAPIKey, ExitIP: "203.0.113.10", CreditConfirmed: true}); errComplete != nil || !usable {
		t.Fatalf("idempotent completion usable=%v error=%v", usable, errComplete)
	}
	reloaded, errReload := newProxyPool(dir, enforcedProxyPoolSettings(), gateway)
	if errReload != nil {
		t.Fatal(errReload)
	}
	snapshot := reloaded.Snapshot()
	if snapshot.UsableAccounts != 1 || len(snapshot.IPs) != 1 || snapshot.IPs[0].UnusableAccounts != 1 {
		t.Fatalf("unexpected durable quota ledger: %+v", snapshot)
	}
}

func TestProxyPoolRejectsExhaustedIPBeforeReservation(t *testing.T) {
	gateway := &fakeProxyPoolGateway{
		nodes: []string{"full", "ready"},
		exits: map[string]string{"full": "203.0.113.20", "ready": "203.0.113.21"},
	}
	pool := newTestProxyPool(t, gateway)
	pool.doc.IPs["203.0.113.20"] = proxyPoolIP{ExitIP: "203.0.113.20", UsableAccounts: 3}
	channel := ChannelSettings{ID: "test", Group: "group", Enabled: true}
	pin, errAcquire := pool.Acquire(context.Background(), channel, "row")
	if errAcquire != nil || pin.Node != "ready" || pin.ExitIP != "203.0.113.21" {
		t.Fatalf("pin=%+v error=%v", pin, errAcquire)
	}
	if reservation := pool.doc.Reservations["row"]; reservation.ExitIP != "203.0.113.21" {
		t.Fatalf("exhausted exit reserved: %+v", reservation)
	}
}

func TestProxyPoolReservationsConsumePendingIPCapacity(t *testing.T) {
	gateway := &fakeProxyPoolGateway{
		nodes: []string{"reserved-alias", "ready"},
		exits: map[string]string{"reserved-alias": "203.0.113.22", "ready": "203.0.113.23"},
	}
	pool := newTestProxyPool(t, gateway)
	pool.doc.IPs["203.0.113.22"] = proxyPoolIP{ExitIP: "203.0.113.22", UsableAccounts: 2}
	pool.doc.Reservations["waiting-row"] = proxyPoolReservation{
		Holder: "waiting-row", Group: "group", Node: "original", ExitIP: "203.0.113.22",
	}
	channel := ChannelSettings{ID: "test", Group: "group", Enabled: true}
	pin, errAcquire := pool.Acquire(context.Background(), channel, "new-row")
	if errAcquire != nil || pin.Node != "ready" || pin.ExitIP != "203.0.113.23" {
		t.Fatalf("pending capacity was ignored: pin=%+v error=%v", pin, errAcquire)
	}
}

func TestProxyPoolResumeKeepsReservedNode(t *testing.T) {
	gateway := &fakeProxyPoolGateway{
		nodes: []string{"node-a", "node-b"},
		exits: map[string]string{"node-a": "203.0.113.31", "node-b": "203.0.113.32"},
	}
	pool := newTestProxyPool(t, gateway)
	pool.doc.Cursor = 1
	pool.doc.Reservations["row"] = proxyPoolReservation{
		Holder: "row", Group: "group", Node: "node-a", ExitIP: "203.0.113.31",
	}
	channel := ChannelSettings{ID: "test", Group: "group", Enabled: true}
	pin, errAcquire := pool.Acquire(context.Background(), channel, "row")
	if errAcquire != nil || pin.Node != "node-a" {
		t.Fatalf("resume changed route: pin=%+v error=%v", pin, errAcquire)
	}
}

func TestProxyPoolCompletionRollsBackWhenLedgerWriteFails(t *testing.T) {
	gateway := &fakeProxyPoolGateway{nodes: []string{"node-a"}, exits: map[string]string{"node-a": "203.0.113.33"}}
	pool := newTestProxyPool(t, gateway)
	pool.doc.Reservations["row"] = proxyPoolReservation{
		Holder: "row", Group: "group", Node: "node-a", ExitIP: "203.0.113.33",
	}
	pool.path = t.TempDir()
	usable, errComplete := pool.Complete("row", Harvest{
		APIKey: testAPIKey, ExitIP: "203.0.113.33", CreditConfirmed: true,
	})
	if errComplete == nil || usable {
		t.Fatalf("ledger write failure was ignored: usable=%v error=%v", usable, errComplete)
	}
	if _, exists := pool.doc.Reservations["row"]; !exists {
		t.Fatal("reservation was lost after failed ledger write")
	}
	if record := pool.doc.IPs["203.0.113.33"]; record.UsableAccounts != 0 {
		t.Fatalf("quota changed after failed ledger write: %+v", record)
	}
}

func TestProxyPoolRefreshRequiresControllerAndDiscoversChannelNodes(t *testing.T) {
	gateway := &fakeProxyPoolGateway{nodes: []string{"node-a", "node-b"}, exits: map[string]string{}}
	pool := newTestProxyPool(t, gateway)
	channel := ChannelSettings{ID: "test", Group: "group", Enabled: true}
	snapshot, errRefresh := pool.Refresh(context.Background(), []ChannelSettings{channel})
	if errRefresh != nil || snapshot.TotalNodes != 2 {
		t.Fatalf("snapshot=%+v error=%v", snapshot, errRefresh)
	}
	gateway.mode = GatewayModeProxyOnly
	if _, errRefresh = pool.Refresh(context.Background(), []ChannelSettings{channel}); proxyPoolReason(errRefresh) != "proxy_pool_unavailable" {
		t.Fatalf("unexpected degraded refresh error: %v", errRefresh)
	}
}
