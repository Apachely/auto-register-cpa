package registerbot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestProxyPoolSettingsDefaultAndSafetyBounds(t *testing.T) {
	settings := DefaultSettings()
	if !settings.ProxyPool.Enabled() || settings.ProxyPool.MaxUsableAccountsPerIP != 3 {
		t.Fatalf("unexpected compatibility defaults: %+v", settings.ProxyPool)
	}
	if settings.Gateway.ProxyURL != "http://127.0.0.1:11460" || settings.Gateway.ControllerURL != "http://127.0.0.1:11461" || settings.Channels[0].ProxyURL != "http://127.0.0.1:11460" {
		t.Fatalf("registration traffic is not isolated from the system proxy: gateway=%+v channel=%+v", settings.Gateway, settings.Channels[0])
	}
	if settings.Browser.WarmPoolSize != 1 || settings.Browser.StartupTimeoutSeconds != 10 || settings.Browser.StepTimeoutSeconds != 10 || settings.Browser.RetireAfter != 32 {
		t.Fatalf("unexpected fast isolated-browser defaults: %+v", settings.Browser)
	}
	if settings.Channels[0].MaxAttempts != 1 {
		t.Fatalf("unattended channel retries failed accounts: %+v", settings.Channels[0])
	}
	settings.ProxyPool.Mode = ProxyPoolModeEnforce
	if errValidate := validateSettings(&settings); errValidate != nil {
		t.Fatal(errValidate)
	}
	settings.ProxyPool.MaxUsableAccountsPerIP = 4
	if errValidate := validateSettings(&settings); errValidate == nil {
		t.Fatal("per-IP usable-account limit exceeded the hard maximum")
	}
	settings.ProxyPool.MaxUsableAccountsPerIP = 3
	settings.Gateway.VerifyExitIP = false
	if errValidate := validateSettings(&settings); errValidate == nil {
		t.Fatal("enforced proxy pool accepted unverified exits")
	}
}

func TestAutomaticWaitsAreHardCappedAtTenSeconds(t *testing.T) {
	settings := DefaultSettings()
	settings.Gateway.PinTimeoutSeconds = 600
	settings.Browser.StartupTimeoutSeconds = 600
	settings.Browser.StepTimeoutSeconds = 600
	settings.Browser.HumanWaitSeconds = 600
	if settings.Gateway.PinTimeout() != automaticWaitLimit || settings.Browser.StartupTimeout() != automaticWaitLimit || settings.Browser.StepTimeout() != automaticWaitLimit || settings.Browser.HumanWait() != automaticWaitLimit {
		t.Fatal("automatic wait exceeded ten seconds")
	}
}

func TestBrowserWarmPoolIsBoundedByPoolSize(t *testing.T) {
	settings := DefaultSettings()
	settings.Browser.PoolSize = 1
	settings.Browser.WarmPoolSize = 8
	settings.Normalize()
	if settings.Browser.WarmPoolSize != 1 {
		t.Fatalf("warm pool exceeded the browser ceiling: %+v", settings.Browser)
	}
}

func TestFreeAIOnboardingDefaults(t *testing.T) {
	fresh := DefaultFreeAIChannel()
	if fresh.OnboardingRole != "student" || fresh.OnboardingSource != "google" {
		t.Fatal("new channel does not have unattended survey defaults")
	}
	for _, test := range []struct{ name, role, source, wantRole, wantSource string }{
		{"old channel", "", "", "student", "google"},
		{"missing source", "researcher", "", "researcher", "google"},
		{"missing role", "", "github", "student", "github"},
		{"explicit choices", "employee", "friend", "employee", "friend"},
	} {
		t.Run(test.name, func(t *testing.T) {
			set := DefaultSettings()
			set.Channels[0].OnboardingRole = test.role
			set.Channels[0].OnboardingSource = test.source
			set.Normalize()
			if set.Channels[0].OnboardingRole != test.wantRole || set.Channels[0].OnboardingSource != test.wantSource {
				t.Fatal("normalization lost defaults or explicit choices")
			}
		})
	}
}

func TestFreeAIOnboardingDefaultsPersistThroughChannelUpdate(t *testing.T) {
	e := newTestEngine(t, testSettings(), nil)
	settings := e.Settings()
	settings.Channels[0].OnboardingRole, settings.Channels[0].OnboardingSource = "", ""
	if errUpdate := e.UpdateSettings(settings); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	channel := e.Settings().Channels[0]
	if channel.OnboardingRole != "student" || channel.OnboardingSource != "google" {
		t.Fatal("saved empty survey settings were not normalized")
	}
}

func TestGatewayNodeHealthSettingsDefaultAndBounds(t *testing.T) {
	settings := DefaultSettings()
	if settings.Gateway.HealthyNodesTTL() != 30*time.Minute || settings.Gateway.NodeProbeInterval() != 90*time.Second {
		t.Fatalf("unexpected health defaults: %+v", settings.Gateway)
	}
	if settings.Gateway.NodeProbeTimeout() != 8*time.Second || settings.Gateway.NodeProbeLimit() != 12 {
		t.Fatalf("a probe budget this tight would mark healthy nodes dead: %+v", settings.Gateway)
	}
	settings.Gateway.HealthyNodesTTLMinutes, settings.Gateway.NodeProbeIntervalSeconds = 0, 0
	settings.Gateway.NodeProbeTimeoutSeconds, settings.Gateway.NodeProbeConcurrency = -3, 500
	settings.Normalize()
	if settings.Gateway.HealthyNodesTTL() != 30*time.Minute || settings.Gateway.NodeProbeInterval() != 90*time.Second {
		t.Fatalf("zero values did not fall back to defaults: %+v", settings.Gateway)
	}
	if settings.Gateway.NodeProbeTimeout() != 8*time.Second || settings.Gateway.NodeProbeLimit() != maxNodeProbeConcurrency {
		t.Fatalf("invalid probe values escaped normalization: %+v", settings.Gateway)
	}
	settings.Gateway.NodeProbeIntervalSeconds = -1
	if settings.Gateway.NodeProbeInterval() != 0 {
		t.Fatal("a negative interval must disable the background sweep")
	}
}

// A health snapshot must never shrink a configured node pool, and saving settings
// must not persist a startup snapshot back into the file.
func TestConfiguredNodesSurviveSaveDespiteHealthSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "healthy.json")
	body := `{"version":1,"checked_at":` + strconv.FormatInt(time.Now().UnixMilli(), 10) +
		`,"nodes":[{"name":"known","delay_ms":100,"exit_ip":"203.0.113.5"}]}`
	if errWrite := os.WriteFile(snapshot, []byte(body), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	settings := testSettings()
	settings.Gateway.HealthyNodesFile = snapshot
	settings.Channels[0].ManualMode = false
	settings.Channels[0].Nodes = []string{"known", "unknown-node"}
	engine, errNew := NewEngine(dir, settings, func(context.Context, ChannelSettings, Harvest) error { return nil })
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer func() {
		if errClose := engine.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	if nodes := engine.Settings().Channels[0].Nodes; len(nodes) != 2 || nodes[1] != "unknown-node" {
		t.Fatalf("snapshot narrowed the configured pool: %v", nodes)
	}
	if errUpdate := engine.UpdateSettings(engine.Settings()); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	data, errRead := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Contains(data, []byte("unknown-node")) {
		t.Fatalf("saved settings lost configured nodes:\n%s", data)
	}
	if bytes.Contains(data, []byte("verified_exits")) || bytes.Contains(data, []byte("203.0.113.5")) {
		t.Fatalf("runtime exit seeds leaked into persisted settings:\n%s", data)
	}
}
