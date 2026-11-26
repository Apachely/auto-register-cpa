package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registerbot"
)

const importTestKey = "sk-fixture12345678901234567890"

func TestRegisterBotProxyPoolManagementContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	handler := NewHandler(&config.Config{}, configPath, nil)
	t.Cleanup(func() {
		if errClose := handler.CloseRegisterBot(); errClose != nil {
			t.Error(errClose)
		}
	})

	request := httptest.NewRequest(http.MethodPut, "/v8/management/registerbot/proxy-pool", bytes.NewBufferString(`{"mode":"enforce","max_usable_accounts_per_ip":3,"failure_cooldown_seconds":45}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	handler.PutRegisterBotProxyPool(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var saved registerbot.ProxyPoolSnapshot
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &saved); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !saved.Enabled || saved.MaxUsableAccountsPerIP != 3 || saved.FailureCooldownSeconds != 45 {
		t.Fatalf("saved proxy pool mismatch: %+v", saved)
	}

	invalid := httptest.NewRequest(http.MethodPut, "/v8/management/registerbot/proxy-pool", bytes.NewBufferString(`{"mode":"enforce","max_usable_accounts_per_ip":4,"failure_cooldown_seconds":45}`))
	invalid.Header.Set("Content-Type", "application/json")
	invalidRecorder := httptest.NewRecorder()
	invalidContext, _ := gin.CreateTestContext(invalidRecorder)
	invalidContext.Request = invalid
	handler.PutRegisterBotProxyPool(invalidContext)
	if invalidRecorder.Code != http.StatusConflict || !strings.Contains(invalidRecorder.Body.String(), "proxy_pool_update_failed") {
		t.Fatalf("invalid policy accepted: status=%d body=%s", invalidRecorder.Code, invalidRecorder.Body.String())
	}
}

func TestRegisterBotImportPlanPreservesAndDeduplicates(t *testing.T) {
	source := []byte(`[{"name":"FreeAi","base-url":"https://api.freeaiapikey.com/v1","prefix":"shared","opaque":{"integer":9007199254740993},"keys":[{"api-key":"existing","proxy-url":"http://127.0.0.1:12450","priority":17}]},{"name":"Other","base-url":"https://example.invalid","keys":[]}]`)
	channel := registerbot.DefaultFreeAIChannel()
	next, endpoint, present, errPlan := registerBotImportPlan(source, channel, importTestKey)
	if errPlan != nil || present || endpoint != "https://api.freeaiapikey.com/v1/chat/completions" {
		t.Fatalf("unexpected plan endpoint=%s present=%v error=%v", endpoint, present, errPlan)
	}
	if !strings.Contains(string(next), "9007199254740993") || !strings.Contains(string(next), `"priority":17`) || !strings.Contains(string(next), `"prefix":"shared"`) {
		t.Fatal("unrelated provider fields were changed")
	}
	var groups []registerBotProvider
	if errDecode := json.Unmarshal(next, &groups); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(groups) != 2 || len(groups[0].Keys) != 2 || groups[0].Keys[1].Proxy != channel.ProxyURL {
		t.Fatal("wrong destination or egress")
	}
	_, _, present, errPlan = registerBotImportPlan(next, channel, importTestKey)
	if errPlan != nil || !present {
		t.Fatal("repeat import was not idempotent")
	}
}

func TestRegisterBotImportRejectsDestinationDrift(t *testing.T) {
	channel := registerbot.DefaultFreeAIChannel()
	for _, source := range []string{
		`[]`,
		`[{"name":"FreeAi","base-url":"https://unrelated.invalid/v1"}]`,
		`[{"name":"FreeAi"},{"name":"FreeAi"}]`,
		`[{"name":"FreeAi","base-url":"https://api.freeaiapikey.com/v1","keys":[{"api-key":"` + importTestKey + `","proxy-url":"direct"}]}]`,
	} {
		if _, _, _, errPlan := registerBotImportPlan([]byte(source), channel, importTestKey); errPlan == nil {
			t.Fatal("unsafe destination accepted")
		}
	}
}

func TestRegisterBotConfigBridgeUsesETagCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver: {port: 8317}\napi-keys:\n  openai-compatibility:\n    - name: FreeAi\n      base-url: https://api.freeaiapikey.com/v1\n      keys: []\n"
	if errWrite := os.WriteFile(path, []byte(raw), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	read, errRead := h.registerBotConfigRequest(context.Background(), http.MethodGet, nil, "")
	if errRead != nil || read.status != http.StatusOK {
		t.Fatalf("read failed %v", errRead)
	}
	etag := read.header.Get("ETag")
	body, _, _, errPlan := registerBotImportPlan(read.body.Bytes(), registerbot.DefaultFreeAIChannel(), importTestKey)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	written, errWrite := h.registerBotConfigRequest(context.Background(), http.MethodPut, body, etag)
	if errWrite != nil || written.status != http.StatusOK {
		t.Fatalf("write status=%d error=%v", written.status, errWrite)
	}
	stale, errStale := h.registerBotConfigRequest(context.Background(), http.MethodPut, read.body.Bytes(), etag)
	if errStale != nil || stale.status != http.StatusPreconditionFailed {
		t.Fatal("stale provider replacement was not rejected")
	}
	confirm, errConfirm := h.registerBotConfigRequest(context.Background(), http.MethodGet, nil, "")
	if errConfirm != nil {
		t.Fatal(errConfirm)
	}
	_, _, present, errPlan := registerBotImportPlan(confirm.body.Bytes(), registerbot.DefaultFreeAIChannel(), importTestKey)
	if errPlan != nil || !present {
		t.Fatal("confirmed imported key missing")
	}
}

func TestRegisterBotVerificationRejectsInvalidAndRedirect(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Error("verification must perform inference, not read the public catalog")
				}
				if r.Header.Get("Authorization") != "Bearer "+importTestKey {
					t.Error("missing verification credential")
				}
				if code == http.StatusFound {
					w.Header().Set("Location", "https://example.invalid/secret")
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hi"}}]}`))
			}))
			defer server.Close()
			errVerify := verifyRegisterBotKey(context.Background(), server.URL, "direct", importTestKey, registerBotProbeModel{Name: "fixture-model"})
			if (errVerify == nil) != (code == http.StatusOK) {
				t.Fatalf("unexpected verify result: %v", errVerify)
			}
		})
	}
}

func TestRegisterBotVerificationRejectsPublicCatalogAndErrorBodies(t *testing.T) {
	for _, body := range []string{`{"data":[{"id":"fixture-model"}]}`, `{"choices":[]}`, `{"error":{"message":"Invalid API key"}}`, `{"choices":[{"message":{"role":"user"}}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		errVerify := verifyRegisterBotKey(context.Background(), server.URL, "direct", importTestKey, registerBotProbeModel{Name: "fixture-model"})
		server.Close()
		if errVerify == nil {
			t.Fatal("a public catalog or error response was accepted as authentication")
		}
	}
}

func TestRegisterBotVerificationMatchesPanelAutomaticModelAndTokenField(t *testing.T) {
	model, errModel := registerBotVerificationModel([]byte(`[{"name":"FreeAi","models":[{"name":" "},{"name":"fixture-opus","use-max-completion-tokens":true},{"name":"second"}]}]`), registerbot.DefaultFreeAIChannel())
	if errModel != nil || model.Name != "fixture-opus" || !model.UseMaxCompletionTokens {
		t.Fatal("wrong automatic model")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if errDecode := json.NewDecoder(r.Body).Decode(&payload); errDecode != nil {
			t.Error(errDecode)
		}
		if payload["model"] != "fixture-opus" || payload["max_completion_tokens"] != float64(5) || payload["max_tokens"] != nil || payload["stream"] != false {
			t.Errorf("unexpected bounded inference payload: %v", payload)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hi"}}]}`))
	}))
	defer server.Close()
	if errVerify := verifyRegisterBotKey(context.Background(), server.URL, "direct", importTestKey, model); errVerify != nil {
		t.Fatal(errVerify)
	}
	if _, errMissing := registerBotVerificationModel([]byte(`[{"name":"FreeAi","models":[]}]`), registerbot.DefaultFreeAIChannel()); errMissing == nil {
		t.Fatal("missing verification model accepted")
	}
}

func TestRegisterBotVerificationReasoningAtTokenLimit(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"reasoning_content", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning_content":"fixture output"}}],"usage":{"completion_tokens":5}}`, true},
		{"reasoning_alias", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning":"fixture output"}}],"usage":{"completion_tokens":5}}`, true},
		{"no_output", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":""}}],"usage":{"completion_tokens":5}}`, false},
		{"whitespace_only", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","reasoning":"  "}}],"usage":{"completion_tokens":5}}`, false},
		{"no_completion_tokens", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","reasoning":"fixture output"}}]}`, false},
		{"not_token_limited", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","reasoning":"fixture output"}}],"usage":{"completion_tokens":5}}`, false},
		{"error_with_reasoning", `{"error":{"message":"denied"},"choices":[{"finish_reason":"length","message":{"role":"assistant","reasoning":"fixture output"}}],"usage":{"completion_tokens":5}}`, false},
		{"wrong_role", `{"choices":[{"finish_reason":"length","message":{"role":"user","reasoning":"fixture output"}}],"usage":{"completion_tokens":5}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+importTestKey {
					t.Error("missing authentication")
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			err := verifyRegisterBotKey(context.Background(), server.URL, "direct", importTestKey, registerBotProbeModel{Name: "fixture-model"})
			if (err == nil) != test.valid {
				t.Fatalf("accepted=%v want=%v error=%v", err == nil, test.valid, err)
			}
		})
	}
}

func TestRegisterBotConcurrentVerifiedKeysMergeWithoutLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver: {port: 8317}\napi-keys:\n  openai-compatibility:\n    - name: FreeAi\n      base-url: https://api.freeaiapikey.com/v1\n      models: [{name: fixture-model}]\n      keys: []\n"
	if errWrite := os.WriteFile(path, []byte(raw), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	channel := registerbot.DefaultFreeAIChannel()
	start := make(chan struct{})
	errors := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			// Two workers per key also exercise idempotent concurrent deduplication.
			key := "sk-" + strings.Repeat(string(rune('A'+index%4)), 43)
			errors <- h.commitRegisterBotKey(context.Background(), channel, key, "https://api.freeaiapikey.com/v1/chat/completions", registerBotProbeModel{Name: "fixture-model"})
		}(i)
	}
	close(start)
	wg.Wait()
	close(errors)
	for errCommit := range errors {
		if errCommit != nil {
			t.Fatal(errCommit)
		}
	}
	response, errRead := h.registerBotConfigRequest(context.Background(), http.MethodGet, nil, "")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var groups []registerBotProvider
	if errDecode := json.Unmarshal(response.body.Bytes(), &groups); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(groups) != 1 || len(groups[0].Keys) != 4 {
		t.Fatal("concurrent verified key writes lost or duplicated a key")
	}
	for _, key := range groups[0].Keys {
		if key.Proxy != channel.ProxyURL {
			t.Fatal("proxy was not preserved")
		}
	}
	before, errBefore := os.ReadFile(path)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	errDrift := h.commitRegisterBotKey(context.Background(), channel, importTestKey, "https://api.freeaiapikey.com/v1/chat/completions", registerBotProbeModel{Name: "changed-model"})
	after, errAfter := os.ReadFile(path)
	if errDrift == nil || errAfter != nil || string(before) != string(after) {
		t.Fatal("changed verification contract was written")
	}
	t.Log("8 concurrent commits -> 4 unique keys; no lost writes; proxy and verification contract preserved")
}
