package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/creditprobe"
)

// creditRefreshBudget bounds one explicit panel-triggered sweep.
const creditRefreshBudget = 2 * time.Minute

// creditProbeSnapshot returns the cached balance summary for one provider
// entry, kicking off a background refresh when the cache is missing or stale.
// It is called while h.mu is held, so it only touches the internally
// synchronized probe manager.
func (h *Handler) creditProbeSnapshot(entry *config.OpenAICompatibility, index int) *creditprobe.Summary {
	if h == nil || entry == nil || h.creditProbe == nil {
		return nil
	}
	if entry.CreditProbe == nil || !entry.CreditProbe.Enabled {
		return nil
	}
	return h.creditProbe.Snapshot(entry, index)
}

// creditProbeTargets resolves the provider entries eligible for an explicit
// refresh. An empty index selects every opted-in provider.
func (h *Handler) creditProbeTargets(index string) []config.OpenAICompatibility {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	normalized := normalizedOpenAICompatibilityEntries(cfg.OpenAICompatibility)
	wanted := -1
	if trimmed := strings.TrimSpace(index); trimmed != "" {
		parsed, errParse := strconv.Atoi(trimmed)
		if errParse != nil {
			return nil
		}
		wanted = parsed
	}
	out := make([]config.OpenAICompatibility, 0, len(normalized))
	for i := range normalized {
		if normalized[i].CreditProbe == nil || !normalized[i].CreditProbe.Enabled {
			continue
		}
		if wanted >= 0 && wanted != i {
			continue
		}
		out = append(out, normalized[i])
	}
	return out
}

// GetOpenAICompatCredits reports the measured upstream balances of every
// provider that opted into the credit probe. The response is served from cache
// and a refresh is scheduled for stale entries, so this endpoint is cheap to
// poll from the panel.
//
//	GET /v8/management/observability/credits/openai-compatibility
func (h *Handler) GetOpenAICompatCredits(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"type":    "internal_error",
			"code":    "config_unavailable",
			"message": "configuration is not available",
		}})
		return
	}
	normalized := normalizedOpenAICompatibilityEntries(cfg.OpenAICompatibility)
	items := make([]gin.H, 0, len(normalized))
	for i := range normalized {
		entry := normalized[i]
		if entry.CreditProbe == nil || !entry.CreditProbe.Enabled {
			continue
		}
		summary := h.creditProbeSnapshot(&entry, i)
		if summary == nil {
			continue
		}
		items = append(items, gin.H{
			"index":  i,
			"name":   entry.Name,
			"credit": summary,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// RefreshOpenAICompatCredits performs a synchronous measurement so the panel
// can show fresh numbers right after a user asks for them.
//
//	POST /v8/management/observability/credits/openai-compatibility/refresh
//
// An optional "index" query parameter limits the sweep to one provider.
func (h *Handler) RefreshOpenAICompatCredits(c *gin.Context) {
	targets := h.creditProbeTargets(c.Query("index"))
	if len(targets) == 0 {
		c.JSON(http.StatusOK, gin.H{"items": []gin.H{}, "refreshed": 0})
		return
	}
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"type":    "internal_error",
			"code":    "config_unavailable",
			"message": "configuration is not available",
		}})
		return
	}
	normalized := normalizedOpenAICompatibilityEntries(cfg.OpenAICompatibility)
	items := make([]gin.H, 0, len(targets))
	refreshed := 0
	ctx := c.Request.Context()
	for i := range normalized {
		entry := normalized[i]
		if entry.CreditProbe == nil || !entry.CreditProbe.Enabled {
			continue
		}
		match := false
		for j := range targets {
			if targets[j].BaseURL == entry.BaseURL && strings.EqualFold(strings.TrimSpace(targets[j].Name), strings.TrimSpace(entry.Name)) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		timeout := entry.CreditProbe.CreditProbeTimeout() * time.Duration(len(entry.APIKeyEntries)+1)
		if timeout > creditRefreshBudget {
			timeout = creditRefreshBudget
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		summary := h.creditProbe.Refresh(reqCtx, &entry, i)
		cancel()
		if summary == nil {
			continue
		}
		refreshed++
		items = append(items, gin.H{"index": i, "name": entry.Name, "credit": summary})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "refreshed": refreshed})
}
