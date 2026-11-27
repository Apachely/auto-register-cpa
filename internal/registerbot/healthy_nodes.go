package registerbot

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
)

// Healthy snapshot reasons. Every non-applied reason is a degradation, never a
// startup failure: the live controller sweep is the authoritative source.
const (
	healthyNodesApplied         = "applied"
	healthyNodesNotConfigured   = "not_configured"
	healthyNodesUnreadable      = "unreadable"
	healthyNodesInvalid         = "invalid"
	healthyNodesStale           = "stale"
	healthyNodesEmpty           = "empty"
	maxHealthyNodesDocumentSize = 1 << 20
)

type healthyNodeDocument struct {
	Version   int `json:"version"`
	CheckedAt int `json:"checked_at"`
	Nodes     []struct {
		Name    string `json:"name"`
		DelayMS int    `json:"delay_ms"`
		ExitIP  string `json:"exit_ip"`
	} `json:"nodes"`
}

// healthyNodesReport is the outcome of one snapshot read.
type healthyNodesReport struct {
	// Applied reports whether exit addresses were seeded from the snapshot.
	Applied bool
	// Reason is one of the healthyNodes* constants above.
	Reason string
	// Count is the number of usable entries in the snapshot.
	Count int
	// CheckedAt is the snapshot generation time in epoch milliseconds.
	CheckedAt int64
	// AgeSeconds is how old the snapshot was at read time, when measurable.
	AgeSeconds int64
	// Exits maps node name to its last measured exit address.
	Exits map[string]string
	// Missing counts configured nodes absent from the snapshot. Informational only:
	// configured nodes are never removed because of it.
	Missing int
}

// Summary is a sanitized one-line description safe for the queue journal.
func (r healthyNodesReport) Summary() string {
	switch r.Reason {
	case healthyNodesApplied:
		return fmt.Sprintf("healthy_nodes_seed count=%d missing=%d", r.Count, r.Missing)
	case healthyNodesStale:
		return fmt.Sprintf("healthy_nodes_ignored reason=stale age_seconds=%d", r.AgeSeconds)
	case healthyNodesNotConfigured:
		return "healthy_nodes_not_configured"
	default:
		return "healthy_nodes_ignored reason=" + r.Reason
	}
}

// Level maps the snapshot outcome onto a journal severity.
func (r healthyNodesReport) Level() string {
	switch r.Reason {
	case healthyNodesApplied, healthyNodesNotConfigured:
		return "info"
	default:
		return "warning"
	}
}

// applyHealthyNodes reads the generated health snapshot and seeds verified exit
// addresses only. It deliberately never rewrites ChannelSettings.Nodes: freezing
// the selectable pool to one startup snapshot made an expired file authoritative
// over fresher live data and could take the whole module down.
func applyHealthyNodes(settings *Settings, stateDir string, now time.Time) healthyNodesReport {
	report := healthyNodesReport{Reason: healthyNodesNotConfigured}
	if settings == nil || strings.TrimSpace(settings.Gateway.HealthyNodesFile) == "" {
		return report
	}
	path := strings.TrimSpace(settings.Gateway.HealthyNodesFile)
	if !filepath.IsAbs(path) {
		path = filepath.Join(stateDir, path)
	}
	data, errRead := os.ReadFile(filepath.Clean(path))
	if errRead != nil || len(data) == 0 || len(data) > maxHealthyNodesDocumentSize {
		report.Reason = healthyNodesUnreadable
		return report
	}
	var document healthyNodeDocument
	if errDecode := json.Unmarshal(data, &document); errDecode != nil || document.Version != 1 {
		report.Reason = healthyNodesInvalid
		return report
	}
	report.CheckedAt = int64(document.CheckedAt)
	ttl := settings.Gateway.HealthyNodesTTL()
	if document.CheckedAt <= 0 || now.Sub(time.UnixMilli(int64(document.CheckedAt))) > ttl {
		report.Reason = healthyNodesStale
		if document.CheckedAt > 0 {
			age := now.Sub(time.UnixMilli(int64(document.CheckedAt))).Seconds()
			if age > 0 {
				report.AgeSeconds = int64(age)
			}
		}
		return report
	}
	exits := make(map[string]string, len(document.Nodes))
	healthy := make([]string, 0, len(document.Nodes))
	seen := make(map[string]struct{}, len(document.Nodes))
	for _, node := range document.Nodes {
		name := strings.TrimSpace(node.Name)
		if name == "" || node.DelayMS <= 0 {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		address, errAddress := netip.ParseAddr(strings.TrimSpace(node.ExitIP))
		if errAddress != nil || !address.Is4() {
			continue
		}
		seen[name] = struct{}{}
		healthy = append(healthy, name)
		exits[name] = address.String()
	}
	report.Count = len(healthy)
	if len(healthy) == 0 {
		report.Reason = healthyNodesEmpty
		return report
	}
	if settings.Gateway.VerifiedExits == nil {
		settings.Gateway.VerifiedExits = make(map[string]string, len(exits))
	}
	for name, exitIP := range exits {
		settings.Gateway.VerifiedExits[name] = exitIP
	}
	// Report configured pools that the snapshot does not know about. The nodes stay
	// selectable; the live sweep measures them on the next idle pass.
	for index := range settings.Channels {
		channel := &settings.Channels[index]
		if !channel.Enabled || channel.ManualMode || len(channel.Nodes) == 0 {
			continue
		}
		for _, name := range channel.Nodes {
			if _, exists := seen[strings.TrimSpace(name)]; !exists {
				report.Missing++
			}
		}
	}
	report.Exits = exits
	report.Applied = true
	report.Reason = healthyNodesApplied
	return report
}
