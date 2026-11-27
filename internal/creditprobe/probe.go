// Package creditprobe measures the remaining upstream credit of every
// credential of an OpenAI-compatible provider without spending anything.
//
// Providers that gate requests on a worst-case reservation answer an
// unaffordable request with a rejection body such as
//
//	Insufficient balance: $2.085277 available, $2.155797 required
//
// before any generation happens, so the rejection itself reports the exact
// remaining balance. The probe sends one deliberately tiny prompt with a
// completion ceiling far above any plausible balance; when a credential is
// rich enough to afford it, the ceiling escalates and the probe repeats until
// the reservation is refused. A refused request is never billed, which makes
// the measurement free.
package creditprobe

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// Credential is one probed API key of a provider.
type Credential struct {
	// Masked identifies the credential for display without leaking it.
	Masked string `json:"masked"`
	// Balance is the measured remaining credit.
	Balance float64 `json:"balance"`
	// Reservation is the ceiling that produced the answer, i.e. a proven lower
	// bound for Balance when the provider never refused the probe.
	Reservation float64 `json:"reservation"`
	// Tier is the index of the first credit tier this credential can still
	// afford; -1 means no tier is affordable.
	Tier int `json:"tier"`
	// TierModel is the model name of Tier.
	TierModel string `json:"tier_model,omitempty"`
	// Exact reports that the provider refused the probe and echoed the balance.
	Exact bool `json:"exact"`
	// Unknown reports that no balance could be read (auth error, network
	// failure, or a provider answering the probe without a balance figure).
	Unknown bool `json:"unknown,omitempty"`
	// Reason explains an unknown or partial measurement.
	Reason string `json:"reason,omitempty"`
}

// TierStat is one credit tier with the bucketed totals of its credentials.
type TierStat struct {
	// Model is the tier model name.
	Model string `json:"model"`
	// Reservation is the worst-case cost the tier needs per request.
	Reservation float64 `json:"reservation"`
	// Count is how many credentials fall into this tier.
	Count int `json:"count"`
	// Available is the summed balance of the tier's credentials.
	Available float64 `json:"available"`
}

// Summary is the aggregated credit state of one provider entry.
type Summary struct {
	// Provider is the configured provider name.
	Provider string `json:"provider,omitempty"`
	// BaseURL is the probed endpoint.
	BaseURL string `json:"base_url,omitempty"`
	// CheckedAt is the moment the measurement finished. A zero value means no
	// measurement has completed yet.
	CheckedAt time.Time `json:"checked_at"`
	// Total counts the credentials covered by this summary.
	Total int `json:"total"`
	// Measured counts credentials with a readable balance.
	Measured int `json:"measured"`
	// Credited counts readable credentials still holding a positive balance.
	// Spend is derived only over these.
	Credited int `json:"credited"`
	// Unusable counts credentials whose balance no longer affords any tier.
	Unusable int `json:"unusable"`
	// Remaining is the sum of all positive readable balances.
	Remaining float64 `json:"remaining"`
	// Used is the sum of max(0, initial credit - balance).
	Used float64 `json:"used"`
	// InitialCredit is the per-credential credit assumed when deriving Used.
	InitialCredit float64 `json:"initial_credit"`
	// Tiers mirrors the configured tiers with their reservations and totals.
	Tiers []TierStat `json:"tiers,omitempty"`
	// Credentials carries the per-credential readings.
	Credentials []Credential `json:"credentials,omitempty"`
	// Refreshing reports a measurement started but not finished for this key.
	Refreshing bool `json:"refreshing,omitempty"`
}

// maxEscalations bounds how many times an affordable probe raises its ceiling.
const maxEscalations = 3

// escalationFactor grows the probe ceiling between rounds.
const escalationFactor = 10

// bodyLimit caps how much of an upstream body is retained for parsing.
const bodyLimit = 8 << 10

var (
	balancePattern  = regexp.MustCompile(`\$(-?[0-9]+(?:\.[0-9]+)?)\s+available`)
	requiredPattern = regexp.MustCompile(`available,\s*\$(-?[0-9]+(?:\.[0-9]+)?)\s+required`)
)

// Manager caches one Summary per provider entry and serializes refreshes.
type Manager struct {
	mu            sync.Mutex
	summaries     map[string]*Summary
	inFlight      map[string]bool
	proxied       map[string]*http.Client
	direct        *http.Client
	directTimeout time.Duration
}

// NewManager builds an empty manager.
func NewManager() *Manager {
	return &Manager{
		summaries: make(map[string]*Summary),
		inFlight:  make(map[string]bool),
		proxied:   make(map[string]*http.Client),
	}
}

// providerKey identifies a provider entry for caching. The base URL alone is
// not enough because several entries may share an endpoint with other keys.
func providerKey(entry *config.OpenAICompatibility, index int) string {
	if entry == nil {
		return "invalid"
	}
	if name := strings.TrimSpace(entry.Name); name != "" {
		return "name:" + name
	}
	return "index:" + strconv.Itoa(index)
}

// Snapshot returns the cached summary for a provider entry and starts a
// background refresh when nothing is cached or the cache is older than the
// configured interval. It never blocks a reader on network work.
func (m *Manager) Snapshot(entry *config.OpenAICompatibility, index int) *Summary {
	if m == nil || entry == nil || entry.CreditProbe == nil || !entry.CreditProbe.Enabled {
		return nil
	}
	key := providerKey(entry, index)
	m.mu.Lock()
	cached := m.summaries[key]
	interval := entry.CreditProbe.CreditProbeInterval()
	stale := cached == nil || time.Since(cached.CheckedAt) > interval
	refreshing := m.inFlight[key] || stale
	out := cloneSummary(cached)
	m.mu.Unlock()
	if out == nil {
		out = &Summary{
			Provider:      strings.TrimSpace(entry.Name),
			BaseURL:       strings.TrimSpace(entry.BaseURL),
			Total:         len(entry.APIKeyEntries),
			InitialCredit: entry.CreditProbe.InitialCredit,
		}
		for _, tier := range entry.CreditProbe.Tiers {
			out.Tiers = append(out.Tiers, TierStat{Model: strings.TrimSpace(tier.Model), Reservation: tier.Reservation()})
		}
	}
	out.Refreshing = refreshing
	if stale {
		m.startRefresh(key, entry)
	}
	return out
}

// Refresh performs a synchronous measurement for the explicit management call.
func (m *Manager) Refresh(ctx context.Context, entry *config.OpenAICompatibility, index int) *Summary {
	if m == nil || entry == nil || entry.CreditProbe == nil || !entry.CreditProbe.Enabled {
		return nil
	}
	summary := m.measure(ctx, entry)
	if summary == nil {
		return nil
	}
	m.mu.Lock()
	m.summaries[providerKey(entry, index)] = summary
	m.mu.Unlock()
	return cloneSummary(summary)
}

// startRefresh launches one background measurement per provider at a time.
func (m *Manager) startRefresh(key string, entry *config.OpenAICompatibility) {
	m.mu.Lock()
	if m.inFlight[key] {
		m.mu.Unlock()
		return
	}
	m.inFlight[key] = true
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeBudget(entry))
		defer cancel()
		summary := m.measure(ctx, entry)
		m.mu.Lock()
		if summary != nil {
			m.summaries[key] = summary
		}
		delete(m.inFlight, key)
		m.mu.Unlock()
	}()
}

// creditProbeBudgetCap bounds one sweep regardless of key count, so a very
// large credential pool can never accumulate unbounded background work.
const creditProbeBudgetCap = 10 * time.Minute

// probeBudget bounds the whole measurement so a stuck provider cannot pile up
// goroutines indefinitely.
func probeBudget(entry *config.OpenAICompatibility) time.Duration {
	probe := entry.CreditProbe
	timeout := config.DefaultCreditProbeTimeout
	limit := config.DefaultCreditProbeConcurrency
	rounds := maxEscalations + 1
	if probe != nil {
		timeout = probe.CreditProbeTimeout()
		limit = probe.CreditProbeLimit()
	}
	keys := len(entry.APIKeyEntries)
	waves := (keys + limit - 1) / limit
	if waves < 1 {
		waves = 1
	}
	budget := time.Duration(waves*rounds) * timeout * 2
	if budget > creditProbeBudgetCap {
		return creditProbeBudgetCap
	}
	return budget
}

// measure probes every credential of the entry concurrently.
func (m *Manager) measure(ctx context.Context, entry *config.OpenAICompatibility) *Summary {
	if entry == nil || entry.CreditProbe == nil {
		return nil
	}
	probe := entry.CreditProbe
	base := strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
	if base == "" {
		return nil
	}
	summary := &Summary{
		Provider:      strings.TrimSpace(entry.Name),
		BaseURL:       base,
		Total:         len(entry.APIKeyEntries),
		InitialCredit: probe.InitialCredit,
		CheckedAt:     time.Now(),
	}
	for _, tier := range probe.Tiers {
		summary.Tiers = append(summary.Tiers, TierStat{Model: strings.TrimSpace(tier.Model), Reservation: tier.Reservation()})
	}
	credentials := make([]Credential, len(entry.APIKeyEntries))
	for i := range credentials {
		credentials[i] = Credential{Masked: maskKey(entry.APIKeyEntries[i].APIKey), Tier: -1, Unknown: true, Reason: "not_probed"}
	}
	sem := make(chan struct{}, probe.CreditProbeLimit())
	var wg sync.WaitGroup
	for i := range entry.APIKeyEntries {
		apiKey := strings.TrimSpace(entry.APIKeyEntries[i].APIKey)
		if apiKey == "" {
			continue
		}
		proxy := strings.TrimSpace(entry.APIKeyEntries[i].ProxyURL)
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, key, proxyURL string) {
			defer wg.Done()
			defer func() { <-sem }()
			credentials[idx] = m.probeOne(ctx, base, key, proxyURL, probe)
		}(i, apiKey, proxy)
	}
	wg.Wait()

	for _, cred := range credentials {
		if cred.Unknown {
			continue
		}
		summary.Measured++
		if cred.Balance > 0 {
			summary.Remaining += cred.Balance
			summary.Credited++
			// Spend is derived against the assumed initial credit and only for
			// credentials that still hold a positive balance. A credential
			// reporting zero or slightly negative credit is either fully
			// drained or was never granted the credit at all, and the probe
			// cannot tell those apart, so it is excluded rather than counted
			// as a full initial-credit spend.
			if probe.InitialCredit > 0 {
				if used := probe.InitialCredit - cred.Balance; used > 0 {
					summary.Used += used
				}
			}
		}
		if cred.Tier < 0 {
			summary.Unusable++
		}
		if cred.Tier >= 0 && cred.Tier < len(summary.Tiers) {
			summary.Tiers[cred.Tier].Count++
			if cred.Balance > 0 {
				summary.Tiers[cred.Tier].Available += cred.Balance
			}
		}
	}
	summary.Credentials = credentials
	summary.Remaining = round6(summary.Remaining)
	summary.Used = round6(summary.Used)
	for i := range summary.Tiers {
		summary.Tiers[i].Available = round6(summary.Tiers[i].Available)
	}
	return summary
}

// probeOne measures a single credential, escalating the ceiling while the
// provider keeps accepting the request.
func (m *Manager) probeOne(ctx context.Context, base, apiKey, proxyURL string, probe *config.OpenAICompatibilityCreditProbe) Credential {
	cred := Credential{Masked: maskKey(apiKey), Tier: -1, Unknown: true, Reason: "no_response"}
	maxTokens := probe.MaxTokens
	if maxTokens <= 0 {
		maxTokens = config.DefaultCreditProbeTierMaxTokens
	}
	for round := 0; round <= maxEscalations; round++ {
		reservation := reservationFor(probe, maxTokens)
		status, body := m.sendOnce(ctx, base, apiKey, proxyURL, probe.Model, maxTokens, probe.CreditProbeTimeout())
		if status <= 0 {
			// A transport failure is not an answer from the provider; retry
			// once before giving up so a dropped handshake does not turn a
			// healthy credential into an unreadable one.
			status, body = m.sendOnce(ctx, base, apiKey, proxyURL, probe.Model, maxTokens, probe.CreditProbeTimeout())
		}
		if status == http.StatusPaymentRequired {
			if balance, ok := parseBalance(body); ok {
				cred.Balance = balance
				cred.Reservation = reservation
				cred.Exact = true
				cred.Unknown = false
				cred.Reason = ""
				cred.Tier = classifyTier(probe, balance)
				cred.TierModel = tierModel(probe, cred.Tier)
				return cred
			}
			cred.Reservation = reservation
			cred.Reason = "unparsable_402_body"
			return cred
		}
		if status >= 200 && status < 300 {
			// Affordable: the balance is at least this reservation. Record the
			// lower bound, then raise the ceiling to force a precise answer.
			cred.Balance = reservation
			cred.Reservation = reservation
			cred.Exact = false
			cred.Unknown = false
			cred.Reason = "at_least"
			cred.Tier = classifyTier(probe, reservation)
			cred.TierModel = tierModel(probe, cred.Tier)
			maxTokens *= escalationFactor
			continue
		}
		cred.Reservation = reservation
		cred.Reason = failureReason(status, body)
		return cred
	}
	return cred
}

// reservationFor is the worst-case cost of the probe request: the full
// completion ceiling at the probe model's output price.
func reservationFor(probe *config.OpenAICompatibilityCreditProbe, maxTokens int) float64 {
	rate := 0.0
	model := strings.ToLower(strings.TrimSpace(probe.Model))
	for _, tier := range probe.Tiers {
		if strings.ToLower(strings.TrimSpace(tier.Model)) == model {
			rate = tier.OutputPerM
			break
		}
	}
	if rate <= 0 || maxTokens <= 0 {
		return 0
	}
	return float64(maxTokens) * rate / 1_000_000
}

// classifyTier picks the first tier whose reservation the balance can afford.
func classifyTier(probe *config.OpenAICompatibilityCreditProbe, balance float64) int {
	for i := range probe.Tiers {
		if balance >= probe.Tiers[i].Reservation() {
			return i
		}
	}
	return -1
}

// tierModel maps a tier index back to its model name.
func tierModel(probe *config.OpenAICompatibilityCreditProbe, tier int) string {
	if probe == nil || tier < 0 || tier >= len(probe.Tiers) {
		return ""
	}
	return strings.TrimSpace(probe.Tiers[tier].Model)
}

// parseBalance extracts the reported available amount from a rejection body.
func parseBalance(body string) (float64, bool) {
	match := balancePattern.FindStringSubmatch(body)
	if len(match) < 2 {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(match[1], 64)
	if errParse != nil {
		return 0, false
	}
	return value, true
}

// parseRequired extracts the demanded reservation.
func parseRequired(body string) (float64, bool) {
	match := requiredPattern.FindStringSubmatch(body)
	if len(match) < 2 {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(match[1], 64)
	if errParse != nil {
		return 0, false
	}
	return value, true
}

// failureReason condenses a non-OK answer into a short stable label.
func failureReason(status int, body string) string {
	if status <= 0 {
		return "transport_error"
	}
	switch status {
	case http.StatusUnauthorized:
		return "invalid_key"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "model_unavailable"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	message := strings.TrimSpace(ojson.Get(body, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(ojson.Get(body, "message").String())
	}
	if message == "" {
		message = strings.TrimSpace(body)
	}
	if len(message) > 120 {
		message = message[:120]
	}
	return "status_" + strconv.Itoa(status) + ": " + message
}

// sendOnce issues one probe request and returns its status plus a bounded
// body. A non-positive status means the request never reached the provider.
func (m *Manager) sendOnce(ctx context.Context, base, apiKey, proxyURL, model string, maxTokens int, timeout time.Duration) (int, string) {
	payload := buildPayload(model, maxTokens)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, errNew := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/chat/completions", bytes.NewReader(payload))
	if errNew != nil {
		return 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	client, errClient := m.clientFor(proxyURL, timeout)
	if errClient != nil {
		return 0, errClient.Error()
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return 0, errDo.Error()
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("creditprobe: close response failed: %v", errClose)
		}
	}()
	buf := make([]byte, bodyLimit)
	read, errRead := io.ReadFull(resp.Body, buf)
	if read < 0 {
		read = 0
	}
	body := string(buf[:read])
	if errRead != nil && read == 0 {
		body = ""
	}
	return resp.StatusCode, body
}

// clientFor builds a client for one credential, honouring its proxy setting and
// caching per proxy URL so repeated sweeps stay cheap.
func (m *Manager) clientFor(proxyURL string, timeout time.Duration) (*http.Client, error) {
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return m.directClient(timeout), nil
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return nil, errParse
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proxied == nil {
		m.proxied = make(map[string]*http.Client)
	}
	if cached, ok := m.proxied[trimmed]; ok && cached != nil {
		return cached, nil
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyURL(parsed),
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   4,
	}}
	m.proxied[trimmed] = client
	return client, nil
}

// directClient reuses one shared transport for credentials without a proxy.
func (m *Manager) directClient(timeout time.Duration) *http.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.direct == nil || m.directTimeout != timeout {
		m.direct = &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{}).DialContext,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   8,
		}}
		m.directTimeout = timeout
	}
	return m.direct
}

// maskKey keeps credentials unreadable in management payloads.
func maskKey(apiKey string) string {
	value := strings.TrimSpace(apiKey)
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return "key:" + strconv.Itoa(len(value))
	}
	return value[:7] + "..." + value[len(value)-4:]
}

// round6 normalizes money to the six decimals the upstream reports.
func round6(value float64) float64 {
	sign := 1.0
	if value < 0 {
		sign = -1.0
	}
	return float64(int64(value*1e6+sign*0.5)) / 1e6
}

// cloneSummary returns a copy safe for serialization.
func cloneSummary(in *Summary) *Summary {
	if in == nil {
		return nil
	}
	out := *in
	if in.Tiers != nil {
		out.Tiers = append([]TierStat(nil), in.Tiers...)
	}
	if in.Credentials != nil {
		out.Credentials = append([]Credential(nil), in.Credentials...)
	}
	return &out
}
