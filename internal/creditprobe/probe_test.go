package creditprobe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// fakeGate answers like a provider that admits requests only when the caller
// can afford the worst-case cost of the request.
type fakeGate struct {
	// balances maps an API key to the credit it reports.
	balances map[string]float64
	// outputPerM is the probe model's output price used for the reservation.
	outputPerM float64
	// mu guards requests and keys.
	mu sync.Mutex
	// requests records every observed max_tokens ceiling.
	requests []int
	// keys records every key that was probed.
	keys []string
}

func (g *fakeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, errRead := io.ReadAll(r.Body)
	if errRead != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	maxTokens := int(ojson.Get(string(body), "max_tokens").Int())
	g.mu.Lock()
	g.requests = append(g.requests, maxTokens)
	g.keys = append(g.keys, key)
	g.mu.Unlock()
	balance, known := g.balances[key]
	if !known {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
		return
	}
	reservation := float64(maxTokens) * g.outputPerM / 1_000_000
	if balance >= reservation {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		return
	}
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = w.Write([]byte(`{"error":{"code":402,"type":"insufficient_quota","message":"Insufficient balance: $` +
		strconv.FormatFloat(balance, 'f', 6, 64) + ` available, $` + strconv.FormatFloat(reservation, 'f', 6, 64) + ` required"}}`))
}

func newGate(t *testing.T, balances map[string]float64) (*fakeGate, *httptest.Server) {
	t.Helper()
	gate := &fakeGate{balances: balances, outputPerM: 15}
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	return gate, server
}

func testProbe(model string, maxTokens int) *config.OpenAICompatibilityCreditProbe {
	probe := &config.OpenAICompatibilityCreditProbe{
		Enabled:         true,
		Model:           model,
		MaxTokens:       maxTokens,
		TimeoutSeconds:  5,
		Concurrency:     4,
		InitialCredit:   5,
		IntervalMinutes: 30,
		Tiers: []config.OpenAICompatibilityCreditTier{
			{Model: "premium-model", OutputPerM: 15},
			{Model: "cheap-model", OutputPerM: 2},
		},
	}
	return probe
}

func TestParseBalanceHandlesNegativeAndPositive(t *testing.T) {
	cases := []struct {
		body string
		want float64
	}{
		{`{"error":{"message":"Insufficient balance: $2.085277 available, $2.155797 required"}}`, 2.085277},
		{`{"error":{"message":"Insufficient balance: $-0.000120 available, $12.000003 required"}}`, -0.00012},
		{`{"error":{"message":"Insufficient balance: $0.000000 available, $1.920003 required"}}`, 0},
	}
	for _, item := range cases {
		got, ok := parseBalance(item.body)
		if !ok {
			t.Fatalf("parseBalance(%s) reported no match", item.body)
		}
		if diff := got - item.want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("parseBalance(%s) = %v, want %v", item.body, got, item.want)
		}
	}
	if _, ok := parseBalance(`{"error":{"message":"rate limited"}}`); ok {
		t.Fatalf("parseBalance matched a body without a balance figure")
	}
	required, ok := parseRequired(`{"error":{"message":"Insufficient balance: $2.085277 available, $2.155797 required"}}`)
	if !ok || required != 2.155797 {
		t.Fatalf("parseRequired = %v/%v, want 2.155797", required, ok)
	}
}

func TestTierReservationAndClassification(t *testing.T) {
	probe := testProbe("premium-model", 800000)
	if got := probe.Tiers[0].Reservation(); got != 1.92 {
		t.Fatalf("premium reservation = %v, want 1.92 (128000 x $15/1M)", got)
	}
	if got := probe.Tiers[1].Reservation(); got != 0.256 {
		t.Fatalf("cheap reservation = %v, want 0.256 (128000 x $2/1M)", got)
	}
	if got := reservationFor(probe, 800000); got != 12 {
		t.Fatalf("probe reservation at 800000 = %v, want 12", got)
	}
	cases := []struct {
		balance float64
		want    int
	}{
		{4.999976, 0},
		{1.92, 0},
		{1.919999, 1},
		{0.30, 1},
		{0.255, -1},
		{-0.00012, -1},
	}
	for _, item := range cases {
		if got := classifyTier(probe, item.balance); got != item.want {
			t.Fatalf("classifyTier(%v) = %d, want %d", item.balance, got, item.want)
		}
	}
}

func TestMeasureBucketsCredentialsByTier(t *testing.T) {
	balances := map[string]float64{
		"sk-rich":     4.999976,
		"sk-mid":      1.508296,
		"sk-poor":     0.100000,
		"sk-negative": -0.00012,
		"sk-unknown":  0,
	}
	delete(balances, "sk-unknown")
	gate, server := newGate(t, balances)
	entry := &config.OpenAICompatibility{
		Name:    "gate",
		BaseURL: server.URL,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{
			{APIKey: "sk-rich"},
			{APIKey: "sk-mid"},
			{APIKey: "sk-poor"},
			{APIKey: "sk-negative"},
			{APIKey: "sk-rejected"},
		},
		CreditProbe: testProbe("premium-model", 800000),
	}
	entry.CreditProbe.Tiers[0].MaxTokens = 128000
	entry.CreditProbe.Tiers[1].MaxTokens = 128000

	manager := NewManager()
	summary := manager.Refresh(context.Background(), entry, 0)
	if summary == nil {
		t.Fatal("measure returned no summary")
	}
	if summary.Total != 5 || summary.Measured != 4 {
		t.Fatalf("total/measured = %d/%d, want 5/4", summary.Total, summary.Measured)
	}
	// sk-rich still affords the premium tier, sk-mid only the fallback tier,
	// while sk-poor ($0.10) and sk-negative fall below every reservation.
	if summary.Tiers[0].Count != 1 || summary.Tiers[1].Count != 1 {
		t.Fatalf("tier buckets = %d/%d, want 1/1 (%+v)", summary.Tiers[0].Count, summary.Tiers[1].Count, summary.Tiers)
	}
	if summary.Tiers[0].Available != 4.999976 {
		t.Fatalf("premium available = %v, want 4.999976", summary.Tiers[0].Available)
	}
	if diff := summary.Tiers[1].Available - 1.508296; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("fallback available = %v, want 1.508296", summary.Tiers[1].Available)
	}
	if diff := summary.Remaining - 6.608272; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("remaining = %v, want 6.608272", summary.Remaining)
	}
	// Spend is derived against the assumed $5 initial credit, but only over
	// credentials still holding a positive balance: rich 0.000024, mid
	// 3.491704, poor 4.9. sk-negative is excluded because a non-positive
	// balance cannot be told apart from a never-credited credential, and the
	// rejected key contributes nothing because no balance was read.
	if diff := summary.Used - 8.391728; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("used = %v, want 8.391728", summary.Used)
	}
	if summary.Credited != 3 || summary.Unusable != 2 {
		t.Fatalf("credited/unusable = %d/%d, want 3/2", summary.Credited, summary.Unusable)
	}
	for _, cred := range summary.Credentials {
		if strings.Contains(cred.Masked, "sk-rich") && !strings.HasSuffix(cred.Masked, "rich") {
			t.Fatalf("masking lost its tail: %q", cred.Masked)
		}
		if len(cred.Masked) > 16 {
			t.Fatalf("mask too long, may leak the credential: %q", cred.Masked)
		}
	}
	var rejected *Credential
	for i := range summary.Credentials {
		if summary.Credentials[i].Unknown {
			rejected = &summary.Credentials[i]
		}
	}
	if rejected == nil || rejected.Reason != "invalid_key" {
		t.Fatalf("rejected credential not reported as invalid_key: %+v", rejected)
	}
	// A single refused round must be enough for every balance below the
	// $12 reservation, so the sweep makes exactly one request per credential.
	if len(gate.requests) != 5 {
		t.Fatalf("expected one probe per credential, got %d", len(gate.requests))
	}
}

func TestMeasureEscalatesUntilRefused(t *testing.T) {
	gate, server := newGate(t, map[string]float64{"sk-wealthy": 60})
	entry := &config.OpenAICompatibility{
		Name:          "gate",
		BaseURL:       server.URL,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "sk-wealthy"}},
		CreditProbe:   testProbe("premium-model", 800000),
	}
	manager := NewManager()
	summary := manager.Refresh(context.Background(), entry, 0)
	if summary == nil || summary.Measured != 1 {
		t.Fatalf("expected one measured credential, got %+v", summary)
	}
	cred := summary.Credentials[0]
	if !cred.Exact || cred.Balance != 60 {
		t.Fatalf("escalation did not reach a refused probe: %+v", cred)
	}
	if len(gate.requests) < 2 {
		t.Fatalf("expected escalation across rounds, got requests %v", gate.requests)
	}
	for i := 1; i < len(gate.requests); i++ {
		if gate.requests[i] <= gate.requests[i-1] {
			t.Fatalf("ceilings did not grow: %v", gate.requests)
		}
	}
}

func TestSnapshotServesCacheAndNeverBlocks(t *testing.T) {
	gate, server := newGate(t, map[string]float64{"sk-a": 3})
	entry := &config.OpenAICompatibility{
		Name:          "gate",
		BaseURL:       server.URL,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "sk-a"}},
		CreditProbe:   testProbe("premium-model", 800000),
	}
	manager := NewManager()
	first := manager.Snapshot(entry, 0)
	if first == nil || !first.Refreshing {
		t.Fatalf("cold snapshot should schedule a refresh: %+v", first)
	}
	if len(first.Tiers) != 2 {
		t.Fatalf("cold snapshot must still describe the configured tiers: %+v", first.Tiers)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		done := len(gate.requests) > 0
		gate.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := manager.Snapshot(entry, 0)
	if second == nil {
		t.Fatal("warm snapshot returned nothing")
	}
	if second.Total != 1 {
		t.Fatalf("warm snapshot lost its totals: %+v", second)
	}
}

func TestProbeBudgetStaysBounded(t *testing.T) {
	probe := testProbe("premium-model", 800000)
	entry := &config.OpenAICompatibility{Name: "big", BaseURL: "https://example.invalid/v1", CreditProbe: probe}
	for i := 0; i < 200; i++ {
		entry.APIKeyEntries = append(entry.APIKeyEntries, config.OpenAICompatibilityAPIKey{APIKey: "sk-x" + strconv.Itoa(i)})
	}
	budget := probeBudget(entry)
	if budget <= 0 || budget > 30*time.Minute {
		t.Fatalf("unexpected budget %v for 200 keys", budget)
	}
}

func TestNormalizeCreditProbeDropsUnusableBlocks(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
		{Name: "off", BaseURL: "https://a.invalid/v1", CreditProbe: &config.OpenAICompatibilityCreditProbe{Enabled: false}},
		{Name: "nomodel", BaseURL: "https://b.invalid/v1", CreditProbe: &config.OpenAICompatibilityCreditProbe{
			Enabled: true,
			Tiers:   []config.OpenAICompatibilityCreditTier{{Model: "m", OutputPerM: 1}},
		}},
		{Name: "ok", BaseURL: "https://c.invalid/v1", ModelPoolOrdering: " PRIORITY ", CreditProbe: &config.OpenAICompatibilityCreditProbe{
			Enabled: true,
			Model:   "m",
			Tiers: []config.OpenAICompatibilityCreditTier{
				{Model: "m", OutputPerM: 15},
				{Model: "", OutputPerM: 3},
			},
		}},
	}}
	cfg.SanitizeOpenAICompatibility()
	if len(cfg.OpenAICompatibility) != 3 {
		t.Fatalf("sanitization dropped providers: %d", len(cfg.OpenAICompatibility))
	}
	byName := map[string]config.OpenAICompatibility{}
	for _, entry := range cfg.OpenAICompatibility {
		byName[entry.Name] = entry
	}
	if byName["off"].CreditProbe != nil {
		t.Fatalf("disabled probe must normalize to nil")
	}
	if byName["nomodel"].CreditProbe != nil {
		t.Fatalf("probe without a model must normalize to nil")
	}
	if byName["ok"].CreditProbe == nil {
		t.Fatalf("valid probe was dropped")
	}
	if len(byName["ok"].CreditProbe.Tiers) != 1 {
		t.Fatalf("tier without a model must be dropped: %+v", byName["ok"].CreditProbe.Tiers)
	}
	if byName["ok"].CreditProbe.MaxTokens != config.DefaultCreditProbeTierMaxTokens {
		t.Fatalf("max-tokens default not applied: %+v", byName["ok"].CreditProbe)
	}
	if byName["ok"].ModelPoolOrdering != config.ModelPoolOrderingPriority {
		t.Fatalf("ordering not normalized: %q", byName["ok"].ModelPoolOrdering)
	}
	okEntry := byName["ok"]
	if !okEntry.IsPriorityModelPoolOrdering() {
		t.Fatalf("priority flag not detected after normalization")
	}
}
