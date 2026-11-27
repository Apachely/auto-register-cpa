package registerbot

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/MoeForge/moego/json"
)

type healthyNodeDocument struct {
	Version int `json:"version"`
	Nodes   []struct {
		Name    string `json:"name"`
		DelayMS int    `json:"delay_ms"`
		ExitIP  string `json:"exit_ip"`
	} `json:"nodes"`
}

func applyHealthyNodes(settings *Settings, stateDir string) error {
	if settings == nil || strings.TrimSpace(settings.Gateway.HealthyNodesFile) == "" {
		return nil
	}
	path := strings.TrimSpace(settings.Gateway.HealthyNodesFile)
	if !filepath.IsAbs(path) {
		path = filepath.Join(stateDir, path)
	}
	data, errRead := os.ReadFile(filepath.Clean(path))
	if errRead != nil {
		return fmt.Errorf("registerbot: read healthy nodes: %w", errRead)
	}
	if len(data) == 0 || len(data) > 1024*1024 {
		return errors.New("registerbot: healthy node file is invalid")
	}
	var document healthyNodeDocument
	if errDecode := json.Unmarshal(data, &document); errDecode != nil || document.Version != 1 {
		return errors.New("registerbot: healthy node file is invalid")
	}
	healthy := make([]string, 0, len(document.Nodes))
	settings.Gateway.VerifiedExits = make(map[string]string, len(document.Nodes))
	seen := make(map[string]struct{}, len(document.Nodes))
	for _, node := range document.Nodes {
		name := strings.TrimSpace(node.Name)
		exitIP := strings.TrimSpace(node.ExitIP)
		if name == "" || node.DelayMS <= 0 {
			continue
		}
		address, errAddress := netip.ParseAddr(exitIP)
		if errAddress != nil || !address.Is4() {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		healthy = append(healthy, name)
		settings.Gateway.VerifiedExits[name] = address.String()
	}
	if len(healthy) == 0 {
		return errors.New("registerbot: no healthy registration nodes")
	}
	for index := range settings.Channels {
		channel := &settings.Channels[index]
		if !channel.Enabled || channel.ManualMode {
			continue
		}
		if len(channel.Nodes) == 0 {
			channel.Nodes = append([]string(nil), healthy...)
			continue
		}
		allowed := make([]string, 0, len(channel.Nodes))
		for _, name := range channel.Nodes {
			if _, exists := seen[name]; exists {
				allowed = append(allowed, name)
			}
		}
		if len(allowed) == 0 {
			return fmt.Errorf("registerbot: channel %s has no healthy registration nodes", channel.ID)
		}
		channel.Nodes = allowed
	}
	return nil
}
