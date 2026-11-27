package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// newOpenAICompatOrderedPoolManager builds a manager whose single provider
// entry publishes one alias over two upstream models with the given ordering.
func newOpenAICompatOrderedPoolManager(t *testing.T, ordering string, executor *openAICompatPoolExecutor) *Manager {
	t.Helper()
	cfg := &internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{{
			Name:    "pool",
			BaseURL: "https://pool.invalid/v1",
			Models: []internalconfig.OpenAICompatibilityModel{
				{Name: "premium-model", Alias: "tier-alias", ForceMapping: true},
				{Name: "cheap-model", Alias: "tier-alias", ForceMapping: true},
			},
			ModelPoolOrdering: ordering,
		}},
	}
	// Sanitizing here also proves the new ordering field survives the
	// normalization path that every real config load runs.
	cfg.SanitizeOpenAICompatibility()
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	if executor == nil {
		executor = &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
	}
	m.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "ordered-pool-auth-" + t.Name(),
		Provider: openAICompatPoolProviderKey,
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":      "test-key",
			"compat_name":  "pool",
			"provider_key": openAICompatPoolProviderKey,
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, openAICompatPoolProviderKey, []*registry.ModelInfo{{ID: "tier-alias"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	return m
}

// TestOpenAICompatPoolOrderingDefaultsToRoundRobin guards the compatibility
// default: an unset ordering must keep rotating so existing providers are
// unaffected by the new field.
func TestOpenAICompatPoolOrderingDefaultsToRoundRobin(t *testing.T) {
	cfg := &internalconfig.OpenAICompatibility{}
	if cfg.IsPriorityModelPoolOrdering() {
		t.Fatalf("empty ordering reported priority; round-robin must stay the default")
	}
	entry := &internalconfig.OpenAICompatibility{ModelPoolOrdering: "  Priority  "}
	if !entry.IsPriorityModelPoolOrdering() {
		t.Fatalf("priority ordering not recognized after normalization")
	}
	unknown := &internalconfig.OpenAICompatibility{ModelPoolOrdering: "whatever"}
	if unknown.IsPriorityModelPoolOrdering() {
		t.Fatalf("unknown ordering must fall back to round-robin")
	}
}

// TestOrderModelPoolKeepsConfigOrderUnderPriority asserts the pool is handed to
// the execution loop in configuration order instead of rotated.
func TestOrderModelPoolKeepsConfigOrderUnderPriority(t *testing.T) {
	executor := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
	m := newOpenAICompatOrderedPoolManager(t, internalconfig.ModelPoolOrderingPriority, executor)
	auth, ok := m.GetByID("ordered-pool-auth-" + t.Name())
	if !ok || auth == nil {
		t.Fatalf("get auth: not found")
	}
	pool := []string{"premium-model", "cheap-model"}
	for i := 0; i < 4; i++ {
		got := m.orderModelPool(true, auth, "tier-alias", pool)
		if !reflect.DeepEqual(got, pool) {
			t.Fatalf("priority pool changed order on round %d: %v", i, got)
		}
	}
	rotated := m.orderModelPool(false, auth, "tier-alias", pool)
	rotatedAgain := m.orderModelPool(false, auth, "tier-alias", pool)
	if reflect.DeepEqual(rotated, rotatedAgain) {
		t.Fatalf("round-robin pool did not rotate: %v vs %v", rotated, rotatedAgain)
	}
	if single := m.orderModelPool(true, auth, "tier-alias", []string{"only"}); len(single) != 1 || single[0] != "only" {
		t.Fatalf("single-entry pool must pass through unchanged, got %v", single)
	}
}

// TestManagerExecute_OpenAICompatPriorityPoolDrainsPremiumFirst is the core
// requirement: while the premium tier is admissible, the cheap tier must stay
// untouched, no matter how many requests arrive.
func TestManagerExecute_OpenAICompatPriorityPoolDrainsPremiumFirst(t *testing.T) {
	executor := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
	m := newOpenAICompatOrderedPoolManager(t, internalconfig.ModelPoolOrderingPriority, executor)
	for i := 0; i < 5; i++ {
		if _, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey},
			cliproxyexecutor.Request{Model: "tier-alias"}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	for _, model := range executor.ExecuteModels() {
		if model != "premium-model" {
			t.Fatalf("priority pool leaked to %q; premium must be drained first", model)
		}
	}
}

// TestManagerExecute_OpenAICompatPriorityPoolFallsBackOnPaymentRequired proves
// the tier switch is invisible: one request, premium refused with 402, cheap
// served, and the response header naming the model that actually served it.
func TestManagerExecute_OpenAICompatPriorityPoolFallsBackOnPaymentRequired(t *testing.T) {
	balanceErr := &Error{
		HTTPStatus: http.StatusPaymentRequired,
		Code:       "insufficient_quota",
		Message:    "Insufficient balance: $2.085277 available, $2.155797 required",
	}
	executor := &openAICompatPoolExecutor{
		id:            openAICompatPoolProviderKey,
		executeErrors: map[string]error{"premium-model": balanceErr},
		executePayloads: map[string][]byte{
			"cheap-model": []byte(`{"id":"resp-1","model":"cheap-model","choices":[{"message":{"content":"ok"}}]}`),
		},
	}
	m := newOpenAICompatOrderedPoolManager(t, internalconfig.ModelPoolOrderingPriority, executor)
	resp, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey},
		cliproxyexecutor.Request{Model: "tier-alias"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	models := executor.ExecuteModels()
	if len(models) != 2 || models[0] != "premium-model" || models[1] != "cheap-model" {
		t.Fatalf("expected premium then cheap within one request, got %v", models)
	}
	if got := resp.Headers.Get(ServedModelHeader); got != "cheap-model" {
		t.Fatalf("%s = %q, want cheap-model", ServedModelHeader, got)
	}
}

// TestManagerExecute_OpenAICompatPriorityPoolSkipsCooledPremiumOnLaterRequests
// shows the steady state: after the premium tier recorded its own model-level
// cooldown, later requests must go straight to the cheap tier without paying
// for another refused premium round trip.
func TestManagerExecute_OpenAICompatPriorityPoolSkipsCooledPremiumOnLaterRequests(t *testing.T) {
	balanceErr := &Error{
		HTTPStatus: http.StatusPaymentRequired,
		Code:       "insufficient_quota",
		Message:    "Insufficient balance: $0.10 available, $1.92 required",
	}
	executor := &openAICompatPoolExecutor{
		id:            openAICompatPoolProviderKey,
		executeErrors: map[string]error{"premium-model": balanceErr},
		executePayloads: map[string][]byte{
			"cheap-model": []byte(`{"id":"resp-2","model":"cheap-model","choices":[{"message":{"content":"ok"}}]}`),
		},
	}
	m := newOpenAICompatOrderedPoolManager(t, internalconfig.ModelPoolOrderingPriority, executor)
	for i := 0; i < 2; i++ {
		if _, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey},
			cliproxyexecutor.Request{Model: "tier-alias"}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	executor.mu.Lock()
	executor.executeModels = nil
	executor.mu.Unlock()
	if _, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey},
		cliproxyexecutor.Request{Model: "tier-alias"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("third execute: %v", err)
	}
	models := executor.ExecuteModels()
	if len(models) != 1 || models[0] != "cheap-model" {
		t.Fatalf("cooled premium must be skipped, got %v", models)
	}
}

// TestManagerExecuteStream_OpenAICompatPriorityPoolFallsBackBeforeFirstByte
// covers the streaming twin, which is the path Claude Code actually uses.
func TestManagerExecuteStream_OpenAICompatPriorityPoolFallsBackBeforeFirstByte(t *testing.T) {
	balanceErr := &Error{
		HTTPStatus: http.StatusPaymentRequired,
		Code:       "insufficient_quota",
		Message:    "Insufficient balance: $0.10 available, $1.92 required",
	}
	executor := &openAICompatPoolExecutor{
		id:                openAICompatPoolProviderKey,
		streamFirstErrors: map[string]error{"premium-model": balanceErr},
		streamPayloads: map[string][]cliproxyexecutor.StreamChunk{
			"cheap-model": {{Payload: []byte(`{"id":"resp-3","model":"cheap-model"}`)}},
		},
	}
	m := newOpenAICompatOrderedPoolManager(t, internalconfig.ModelPoolOrderingPriority, executor)
	result, err := m.ExecuteStream(context.Background(), []string{openAICompatPoolProviderKey},
		cliproxyexecutor.Request{Model: "tier-alias"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	models := executor.StreamModels()
	if len(models) != 2 || models[0] != "premium-model" || models[1] != "cheap-model" {
		t.Fatalf("expected premium then cheap in one stream, got %v", models)
	}
	if got := result.Headers.Get(ServedModelHeader); got != "cheap-model" {
		t.Fatalf("%s = %q, want cheap-model", ServedModelHeader, got)
	}
}

// TestOrderModelPoolIgnoresNonPoolProviders keeps the new branch inert for any
// provider that does not declare a pool.
func TestOrderModelPoolIgnoresNonPoolProviders(t *testing.T) {
	executor := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
	m := newOpenAICompatOrderedPoolManager(t, internalconfig.ModelPoolOrderingPriority, executor)
	auth, ok := m.GetByID("ordered-pool-auth-" + t.Name())
	if !ok || auth == nil {
		t.Fatalf("get auth: not found")
	}
	if got := m.orderModelPool(false, auth, "unrelated-model", []string{"a", "b"}); len(got) != 2 {
		t.Fatalf("rotation must still return the full pool, got %v", got)
	}
	other := &Auth{ID: "other", Provider: "gemini"}
	if openAICompatEntryForPool(&internalconfig.Config{}, other, "x") != nil {
		t.Fatalf("non openai-compatibility credentials must resolve no pool entry")
	}
}
