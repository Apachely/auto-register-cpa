package registerbot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyHealthyNodesRestrictsAutomaticChannels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "healthy.json")
	if errWrite := os.WriteFile(path, []byte(`{"version":1,"nodes":[{"name":"fast-a","delay_ms":120,"exit_ip":"203.0.113.10"},{"name":"fast-b","delay_ms":180,"exit_ip":"203.0.113.11"},{"name":"","delay_ms":1,"exit_ip":"203.0.113.12"},{"name":"slow","delay_ms":0,"exit_ip":"203.0.113.13"}]}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	settings := DefaultSettings()
	settings.Gateway.HealthyNodesFile = path
	if errApply := applyHealthyNodes(&settings, dir); errApply != nil {
		t.Fatal(errApply)
	}
	if len(settings.Channels[0].Nodes) != 2 || settings.Channels[0].Nodes[0] != "fast-a" || settings.Channels[0].Nodes[1] != "fast-b" {
		t.Fatalf("unexpected healthy nodes: %v", settings.Channels[0].Nodes)
	}
	if settings.Gateway.VerifiedExits["fast-a"] != "203.0.113.10" || settings.Gateway.VerifiedExits["fast-b"] != "203.0.113.11" {
		t.Fatalf("unexpected verified exits: %v", settings.Gateway.VerifiedExits)
	}
}

func TestApplyHealthyNodesFailsClosed(t *testing.T) {
	settings := DefaultSettings()
	settings.Gateway.HealthyNodesFile = filepath.Join(t.TempDir(), "missing.json")
	if errApply := applyHealthyNodes(&settings, t.TempDir()); errApply == nil {
		t.Fatal("missing health data did not fail closed")
	}
}
