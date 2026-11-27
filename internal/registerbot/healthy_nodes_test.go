package registerbot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func itoa64(value int64) string { return strconv.FormatInt(value, 10) }

func writeHealthySnapshot(t *testing.T, path, body string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(body), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func TestApplyHealthyNodesSeedsExitsWithoutNarrowingChannels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "healthy.json")
	checked := time.Now().Add(-2 * time.Minute).UnixMilli()
	writeHealthySnapshot(t, path, `{"version":1,"checked_at":`+itoa64(checked)+`,"nodes":[`+
		`{"name":"fast-a","delay_ms":120,"exit_ip":"203.0.113.10"},`+
		`{"name":"fast-b","delay_ms":180,"exit_ip":"203.0.113.11"},`+
		`{"name":"fast-b","delay_ms":190,"exit_ip":"203.0.113.99"},`+
		`{"name":"","delay_ms":1,"exit_ip":"203.0.113.12"},`+
		`{"name":"slow","delay_ms":0,"exit_ip":"203.0.113.13"},`+
		`{"name":"ipv6-only","delay_ms":10,"exit_ip":"2001:db8::1"}]}`)
	settings := DefaultSettings()
	settings.Gateway.HealthyNodesFile = path
	settings.Channels[0].Nodes = []string{"fast-a", "ghost-node"}
	report := applyHealthyNodes(&settings, dir, time.Now())
	if !report.Applied || report.Reason != healthyNodesApplied {
		t.Fatalf("report=%+v", report)
	}
	if report.Count != 2 {
		t.Fatalf("count=%d report=%+v", report.Count, report)
	}
	// The snapshot seeds exit addresses only. A configured pool is never narrowed,
	// so a stale or partial file cannot shrink the selectable node set.
	if len(settings.Channels[0].Nodes) != 2 || settings.Channels[0].Nodes[1] != "ghost-node" {
		t.Fatalf("channel nodes were rewritten: %v", settings.Channels[0].Nodes)
	}
	if settings.Gateway.VerifiedExits["fast-a"] != "203.0.113.10" || settings.Gateway.VerifiedExits["fast-b"] != "203.0.113.11" {
		t.Fatalf("unexpected verified exits: %v", settings.Gateway.VerifiedExits)
	}
	if report.Missing != 1 {
		t.Fatalf("missing=%d report=%+v", report.Missing, report)
	}
}

func TestApplyHealthyNodesIgnoresStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "healthy.json")
	stale := time.Now().Add(-3 * time.Hour).UnixMilli()
	writeHealthySnapshot(t, path, `{"version":1,"checked_at":`+itoa64(stale)+`,"nodes":[{"name":"old","delay_ms":120,"exit_ip":"203.0.113.10"}]}`)
	settings := DefaultSettings()
	settings.Gateway.HealthyNodesFile = path
	report := applyHealthyNodes(&settings, dir, time.Now())
	if report.Applied || report.Reason != healthyNodesStale {
		t.Fatalf("report=%+v", report)
	}
	if len(settings.Gateway.VerifiedExits) != 0 {
		t.Fatalf("stale exits were seeded: %v", settings.Gateway.VerifiedExits)
	}
	if report.AgeSeconds < 3*3600-5 {
		t.Fatalf("age=%d", report.AgeSeconds)
	}
	if report.Level() != "warning" {
		t.Fatalf("level=%s", report.Level())
	}
}

func TestApplyHealthyNodesDegradesInsteadOfFailing(t *testing.T) {
	for name, prepare := range map[string]func(*testing.T, *Settings, string) string{
		"missing": func(_ *testing.T, _ *Settings, dir string) string { return filepath.Join(dir, "absent.json") },
		"empty": func(t *testing.T, _ *Settings, dir string) string {
			path := filepath.Join(dir, "empty.json")
			writeHealthySnapshot(t, path, "")
			return path
		},
		"bad json": func(t *testing.T, _ *Settings, dir string) string {
			path := filepath.Join(dir, "bad.json")
			writeHealthySnapshot(t, path, "not json")
			return path
		},
		"no healthy": func(t *testing.T, _ *Settings, dir string) string {
			path := filepath.Join(dir, "none.json")
			writeHealthySnapshot(t, path, `{"version":1,"checked_at":`+itoa64(time.Now().UnixMilli())+`,"nodes":[{"name":"x","delay_ms":0,"exit_ip":"203.0.113.9"}]}`)
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			settings := DefaultSettings()
			settings.Gateway.HealthyNodesFile = prepare(t, &settings, dir)
			report := applyHealthyNodes(&settings, dir, time.Now())
			if report.Applied {
				t.Fatalf("stale/unusable snapshot was applied: %+v", report)
			}
			if len(settings.Gateway.VerifiedExits) != 0 {
				t.Fatalf("exits seeded from unusable snapshot: %v", settings.Gateway.VerifiedExits)
			}
		})
	}
}

func TestApplyHealthyNodesRelativePath(t *testing.T) {
	dir := t.TempDir()
	writeHealthySnapshot(t, filepath.Join(dir, "healthy.json"), `{"version":1,"checked_at":`+itoa64(time.Now().UnixMilli())+`,"nodes":[{"name":"near","delay_ms":50,"exit_ip":"203.0.113.7"}]}`)
	settings := DefaultSettings()
	settings.Gateway.HealthyNodesFile = "healthy.json"
	report := applyHealthyNodes(&settings, dir, time.Now())
	if !report.Applied || settings.Gateway.VerifiedExits["near"] != "203.0.113.7" {
		t.Fatalf("report=%+v exits=%v", report, settings.Gateway.VerifiedExits)
	}
}

// A broken or expired snapshot must never take the whole module down.
func TestNewEngineStartsDespiteUnusableHealthySnapshot(t *testing.T) {
	settings := testSettings()
	settings.Gateway.HealthyNodesFile = filepath.Join(t.TempDir(), "absent.json")
	engine, errNew := NewEngine(t.TempDir(), settings, func(context.Context, ChannelSettings, Harvest) error { return nil })
	if errNew != nil {
		t.Fatalf("engine refused to start: %v", errNew)
	}
	defer func() {
		if errClose := engine.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	found := false
	for _, event := range engine.Events().Tail(20, "") {
		if event.Stage == StageNodeHealth {
			found = true
			if event.Level != "warning" {
				t.Fatalf("severity=%s message=%s", event.Level, event.Message)
			}
		}
	}
	if !found {
		t.Fatal("node health snapshot outcome was never reported")
	}
}
