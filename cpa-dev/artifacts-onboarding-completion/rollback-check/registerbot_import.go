package management

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registerbot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

const registerBotProviderPath = "/v8/management/config/api-keys/openai-compatibility"

type registerBotProvider struct {
	Name    string                  `json:"name"`
	BaseURL string                  `json:"base-url"`
	Prefix  string                  `json:"prefix"`
	Models  []registerBotProbeModel `json:"models"`
	Keys    []struct {
		Key   string `json:"api-key"`
		Proxy string `json:"proxy-url"`
	} `json:"keys"`
}

type registerBotProbeModel struct {
	Name                   string `json:"name"`
	UseMaxCompletionTokens bool   `json:"use-max-completion-tokens"`
}

func registerBotVerificationModel(source []byte, channel registerbot.ChannelSettings) (registerBotProbeModel, error) {
	var groups []registerBotProvider
	if errDecode := json.Unmarshal(source, &groups); errDecode != nil {
		return registerBotProbeModel{}, errors.New("registerbot: verification model configuration invalid")
	}
	for _, group := range groups {
		if group.Name != channel.Provider {
			continue
		}
		for _, model := range group.Models {
			model.Name = strings.TrimSpace(model.Name)
			if model.Name != "" {
				return model, nil
			}
		}
	}
	return registerBotProbeModel{}, errors.New("registerbot: verification requires a configured provider model")
}

// This adapter uses the exact v8 GET/PUT contract used by providerKeyImport.ts.
// It dispatches privately to ConfigV8, preserving its validation, ETag/If-Match,
// file CAS, auth-index stripping and runtime reload without a loopback secret.
type registerBotConfigResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *registerBotConfigResponse) Header() http.Header { return r.header }
func (r *registerBotConfigResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *registerBotConfigResponse) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

func (h *Handler) registerBotConfigRequest(ctx context.Context, method string, body []byte, etag string) (*registerBotConfigResponse, error) {
	req, errRequest := http.NewRequestWithContext(ctx, method, registerBotProviderPath, bytes.NewReader(body))
	if errRequest != nil {
		return nil, errors.New("registerbot: configuration request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("If-Match", etag)
	}
	response := &registerBotConfigResponse{header: make(http.Header)}
	router := gin.New()
	router.Handle(method, "/v8/management/config/*path", func(c *gin.Context) {
		c.Set(ConfigV8ContextKey, true)
		h.ConfigV8(c)
	})
	router.ServeHTTP(response, req)
	return response, nil
}

func registerBotImportPlan(source []byte, channel registerbot.ChannelSettings, key string) ([]byte, string, bool, error) {
	var groups []registerBotProvider
	if errDecode := json.Unmarshal(source, &groups); errDecode != nil {
		return nil, "", false, errors.New("registerbot: provider configuration unavailable")
	}
	index := -1
	for i, group := range groups {
		if group.Name == channel.Provider {
			if index >= 0 {
				return nil, "", false, errors.New("registerbot: ambiguous provider destination")
			}
			index = i
		}
	}
	if index < 0 {
		return nil, "", false, errors.New("registerbot: provider destination missing")
	}
	group := groups[index]
	base := strings.TrimRight(strings.TrimSpace(group.BaseURL), "/")
	if base != "https://api.freeaiapikey.com/v1" {
		return nil, "", false, errors.New("registerbot: provider destination changed")
	}
	if channel.ProxyURL == "" {
		return nil, "", false, errors.New("registerbot: provider proxy missing")
	}
	for _, existing := range group.Keys {
		if existing.Key == key {
			if existing.Proxy != channel.ProxyURL {
				return nil, "", false, errors.New("registerbot: provider proxy mismatch")
			}
			return nil, base + "/chat/completions", true, nil
		}
	}
	doc := ojson.NewDoc(source)
	path := fmt.Sprintf("%d.keys", index)
	if group.Keys == nil {
		if errSet := doc.Set(path, []any{}); errSet != nil {
			return nil, "", false, errors.New("registerbot: invalid provider key collection")
		}
	}
	if errSet := doc.Set(path+".-1", map[string]string{"api-key": key, "proxy-url": channel.ProxyURL}); errSet != nil {
		return nil, "", false, errors.New("registerbot: provider key append failed")
	}
	return doc.Bytes(), base + "/chat/completions", false, nil
}

func validRegisterBotKey(key string) bool {
	if !strings.HasPrefix(key, "sk-") || len(key) < 19 || len(key) > 512 {
		return false
	}
	for _, r := range key[3:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func verifyRegisterBotKey(ctx context.Context, endpoint, proxy, key string, model registerBotProbeModel) error {
	if strings.TrimSpace(model.Name) == "" {
		return errors.New("registerbot: verification model missing")
	}
	transport, _, errTransport := proxyutil.BuildHTTPTransport(proxy)
	if errTransport != nil {
		return errors.New("registerbot: key verification proxy invalid")
	}
	if transport == nil {
		transport = proxyutil.NewDirectTransport()
	}
	defer transport.CloseIdleConnections()
	// Only establishment is bounded; response headers and reads have no timeout.
	if proxy == "direct" || strings.HasPrefix(proxy, "http://") || strings.HasPrefix(proxy, "https://") {
		transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	transport.ResponseHeaderTimeout = 0
	transport.TLSHandshakeTimeout = 0
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	// The model catalog is public. Only an authenticated inference response proves
	// that the copied key works. Match the panel's automatic per-key test request.
	payload := struct {
		Model               string              `json:"model"`
		Messages            []map[string]string `json:"messages"`
		Stream              bool                `json:"stream"`
		MaxTokens           int                 `json:"max_tokens,omitempty"`
		MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	}{Model: model.Name, Messages: []map[string]string{{"role": "user", "content": "Hi"}}}
	if model.UseMaxCompletionTokens {
		payload.MaxCompletionTokens = 5
	} else {
		payload.MaxTokens = 5
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return errors.New("registerbot: verification request encoding failed")
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return errors.New("registerbot: invalid verification endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	response, errResponse := client.Do(req)
	if errResponse != nil {
		return errors.New("registerbot: key verification connection failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("registerbot: key verification HTTP %d", response.StatusCode)
	}
	data, errRead := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if errRead != nil || len(data) > 4*1024*1024 {
		return errors.New("registerbot: key verification response invalid")
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if errDecode := json.Unmarshal(data, &completion); errDecode != nil || completion.Error != nil || len(completion.Choices) == 0 || completion.Choices[0].Message.Role != "assistant" || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		return errors.New("registerbot: key verification returned no assistant completion")
	}
	return nil
}

func (h *Handler) importRegisterBotKey(ctx context.Context, channel registerbot.ChannelSettings, harvest registerbot.Harvest) (resultErr error) {
	defer func() {
		if resultErr == nil {
			return
		}
		if ctx.Err() != nil {
			resultErr = ctx.Err()
			return
		}
		reason := "write_failed"
		switch {
		case strings.Contains(resultErr.Error(), "verification"), strings.Contains(resultErr.Error(), "invalid harvested"):
			reason = "verification_failed"
		case strings.Contains(resultErr.Error(), "destination"), strings.Contains(resultErr.Error(), "proxy mismatch"):
			reason = "provider_missing"
		case strings.Contains(resultErr.Error(), "conflict"), strings.Contains(resultErr.Error(), "ETag"):
			reason = "config_conflict"
		}
		resultErr = &registerbot.ImportError{Reason: reason}
	}()
	if !validRegisterBotKey(harvest.APIKey) {
		return errors.New("registerbot: invalid harvested key")
	}
	snapshot, errRead := h.registerBotConfigRequest(ctx, http.MethodGet, nil, "")
	if errRead != nil || snapshot.status != http.StatusOK {
		return errors.New("registerbot: provider configuration unavailable")
	}
	etag := snapshot.header.Get("ETag")
	if len(etag) != 66 || etag[0] != '"' || etag[65] != '"' {
		return errors.New("registerbot: provider configuration ETag missing")
	}
	body, endpoint, present, errPlan := registerBotImportPlan(snapshot.body.Bytes(), channel, harvest.APIKey)
	if errPlan != nil {
		return errPlan
	}
	model, errModel := registerBotVerificationModel(snapshot.body.Bytes(), channel)
	if errModel != nil {
		return errModel
	}
	// Browser fallback is not evidence that the saved provider can use the key.
	// Verify through the exact proxy that the provider entry will persist.
	if errVerify := verifyRegisterBotKey(ctx, endpoint, channel.ProxyURL, harvest.APIKey, model); errVerify != nil {
		return errVerify
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	if !present {
		response, errWrite := h.registerBotConfigRequest(ctx, http.MethodPut, body, etag)
		if errWrite != nil {
			return errors.New("registerbot: provider write unconfirmed")
		}
		if response.status == http.StatusConflict || response.status == http.StatusPreconditionFailed {
			return errors.New("registerbot: provider configuration conflict; resume import")
		}
		if response.status != http.StatusOK {
			return errors.New("registerbot: provider write rejected")
		}
	}
	// Read back even an already-present key: configuration may have changed
	// while inference was in flight. Never report success for a removed key.
	confirmation, errConfirm := h.registerBotConfigRequest(ctx, http.MethodGet, nil, "")
	if errConfirm != nil || confirmation.status != http.StatusOK {
		return errors.New("registerbot: provider readback failed")
	}
	_, confirmedEndpoint, confirmed, errPlan := registerBotImportPlan(confirmation.body.Bytes(), channel, harvest.APIKey)
	if errPlan != nil || !confirmed || confirmedEndpoint != endpoint {
		return errors.New("registerbot: provider write unconfirmed")
	}
	confirmedModel, errModel := registerBotVerificationModel(confirmation.body.Bytes(), channel)
	if errModel != nil || confirmedModel != model {
		return errors.New("registerbot: provider configuration conflict after key verification")
	}
	return nil
}
