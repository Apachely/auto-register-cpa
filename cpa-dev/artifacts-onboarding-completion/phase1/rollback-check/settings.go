// Package registerbot implements the batch registration engine: channel isolated
// account provisioning, an embedded Chrome pool driven over CDP, a local proxy
// gateway layer for node selection, and a resumable live event stream.
//
// The engine never blocks the core capability: if the gateway or the embedded
// browser is unavailable the batch still runs through the degraded path, and the
// degradation reason is reported as a live event instead of a fatal error.
package registerbot

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Browser modes, ordered from preferred to most degraded.
const (
	BrowserModeEmbedded = "embedded"
	BrowserModeAttach   = "attach"
	BrowserModeSystem   = "system"
	BrowserModeManual   = "manual"
)

// Gateway modes, ordered from preferred to most degraded.
const (
	GatewayModeController = "controller"
	GatewayModeProxyOnly  = "proxy_only"
	GatewayModeDisabled   = "disabled"
)

// Gateway sharing strategies decide how channels contend for proxy nodes.
const (
	ShareModeLease      = "lease"
	ShareModePerChannel = "per_channel"
	ShareModeShared     = "shared"
)

// Settings is the persisted registerbot configuration. It lives in its own YAML
// file next to the CPA config file so that the upstream config schema stays intact.
type Settings struct {
	Enabled  bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	StateDir string `yaml:"state-dir,omitempty" json:"state_dir,omitempty"`
	Evidence bool   `yaml:"capture-evidence,omitempty" json:"capture_evidence,omitempty"`

	Gateway  GatewaySettings   `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	Browser  BrowserSettings   `yaml:"browser,omitempty" json:"browser,omitempty"`
	Import   ImportSettings    `yaml:"import,omitempty" json:"import,omitempty"`
	Channels []ChannelSettings `yaml:"channels,omitempty" json:"channels,omitempty"`
}

// GatewaySettings describes the local proxy gateway layer.
type GatewaySettings struct {
	// ProxyURL is the HTTP proxy every browser and API call uses.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy_url,omitempty"`
	// ControllerURL is the Clash compatible controller of the same process.
	// Empty means auto discover.
	ControllerURL string `yaml:"controller-url,omitempty" json:"controller_url,omitempty"`
	// AutoDiscover probes controller candidates derived from ProxyURL.
	AutoDiscover bool `yaml:"auto-discover,omitempty" json:"auto_discover,omitempty"`
	// Secret is the optional controller authorization token.
	Secret string `yaml:"secret,omitempty" json:"secret,omitempty"`
	// VerifyExitIP confirms a pin actually changed the observable exit address.
	VerifyExitIP bool `yaml:"verify-exit-ip,omitempty" json:"verify_exit_ip,omitempty"`
	// ExitIPURL returns the current exit address as JSON or plain text.
	ExitIPURL string `yaml:"exit-ip-url,omitempty" json:"exit_ip_url,omitempty"`
	// ShareMode decides channel contention on the gateway.
	ShareMode string `yaml:"share-mode,omitempty" json:"share_mode,omitempty"`
	// Groups restricts which proxy groups may be used. Empty means all.
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	// PinTimeoutSeconds bounds a node switch plus its exit IP confirmation.
	PinTimeoutSeconds int `yaml:"pin-timeout-seconds,omitempty" json:"pin_timeout_seconds,omitempty"`
	// ProbeTimeoutSeconds bounds a single controller request.
	ProbeTimeoutSeconds int `yaml:"probe-timeout-seconds,omitempty" json:"probe_timeout_seconds,omitempty"`
	// CircuitThreshold is how many consecutive gateway failures degrade the channel.
	CircuitThreshold int `yaml:"circuit-threshold,omitempty" json:"circuit_threshold,omitempty"`
	// CircuitCooldownSeconds is how long a degraded channel waits before retrying.
	CircuitCooldownSeconds int `yaml:"circuit-cooldown-seconds,omitempty" json:"circuit_cooldown_seconds,omitempty"`
}

// BrowserSettings describes the embedded Chrome pool.
type BrowserSettings struct {
	Mode                  string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	ChromePaths           []string `yaml:"chrome-paths,omitempty" json:"chrome_paths,omitempty"`
	AttachURL             string   `yaml:"attach-url,omitempty" json:"attach_url,omitempty"`
	PoolSize              int      `yaml:"pool-size,omitempty" json:"pool_size,omitempty"`
	Headless              bool     `yaml:"headless,omitempty" json:"headless,omitempty"`
	WindowWidth           int      `yaml:"window-width,omitempty" json:"window_width,omitempty"`
	WindowHeight          int      `yaml:"window-height,omitempty" json:"window_height,omitempty"`
	ProfileBaseDir        string   `yaml:"profile-dir,omitempty" json:"profile_dir,omitempty"`
	ExtraArgs             []string `yaml:"extra-args,omitempty" json:"extra_args,omitempty"`
	StartupTimeoutSeconds int      `yaml:"startup-timeout-seconds,omitempty" json:"startup_timeout_seconds,omitempty"`
	StepTimeoutSeconds    int      `yaml:"step-timeout-seconds,omitempty" json:"step_timeout_seconds,omitempty"`
	HumanWaitSeconds      int      `yaml:"human-wait-seconds,omitempty" json:"human_wait_seconds,omitempty"`
	// RetireAfter restarts a pooled instance after this many accounts.
	RetireAfter int `yaml:"retire-after,omitempty" json:"retire_after,omitempty"`
	// RemoteDebugPort is the fixed debugging port used by the embedded pool.
	RemoteDebugPort int `yaml:"remote-debug-port,omitempty" json:"remote_debug_port,omitempty"`
}

// ImportSettings targets where a harvested key is written.
type ImportSettings struct {
	// Family is the config api-keys family that receives harvested keys.
	Family string `yaml:"family,omitempty" json:"family,omitempty"`
	// VerifyURL checks a harvested key before it is written to the config.
	VerifyURL string `yaml:"verify-url,omitempty" json:"verify_url,omitempty"`
	// SkipVerify turns off the live check of a harvested key.
	SkipVerify bool `yaml:"skip-verify,omitempty" json:"skip_verify,omitempty"`
}

// ChannelSettings is one isolated registration channel.
type ChannelSettings struct {
	ID   string `yaml:"id" json:"id"`
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Enabled channels are schedulable; disabled ones keep their history.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Provider is the CPA provider group name that receives harvested keys.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// ProxyURL is written into each harvested key entry; empty inherits the gateway.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy_url,omitempty"`
	// Concurrency is how many accounts of this channel run at once.
	Concurrency int `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	// Group is the gateway group this channel pins nodes in.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	// Nodes is the selectable node pool; empty means every node of Group.
	Nodes []string `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	// RotateNode switches node between accounts of this channel.
	RotateNode bool `yaml:"rotate-node,omitempty" json:"rotate_node,omitempty"`
	// IntervalSeconds is the pause between two accounts of the same channel.
	IntervalSeconds int `yaml:"interval-seconds,omitempty" json:"interval_seconds,omitempty"`
	// StepDelayMillis is the human pacing delay between UI steps.
	StepDelayMillis int `yaml:"step-delay-millis,omitempty" json:"step_delay_millis,omitempty"`
	// TargetKeys stops the run after this many harvested keys; zero means all.
	TargetKeys int `yaml:"target-keys,omitempty" json:"target_keys,omitempty"`
	// MaxAttempts bounds retries for one account.
	MaxAttempts int `yaml:"max-attempts,omitempty" json:"max_attempts,omitempty"`
	// StartURL overrides the channel default entry point.
	StartURL string `yaml:"start-url,omitempty" json:"start_url,omitempty"`
	// ManualMode keeps the batch running without the gateway or a browser.
	ManualMode bool `yaml:"manual-mode,omitempty" json:"manual_mode,omitempty"`
	// Onboarding answers are operator-supplied facts, never inferred from an account.
	OnboardingRole      string `yaml:"onboarding-role,omitempty" json:"onboarding_role,omitempty"`
	OnboardingSource    string `yaml:"onboarding-source,omitempty" json:"onboarding_source,omitempty"`
	SkipOptionalDiscord bool   `yaml:"skip-optional-discord,omitempty" json:"skip_optional_discord,omitempty"`
}

// Duration accessors keep the YAML surface as plain integers, as in CPA config.

// PinTimeout bounds a node switch plus exit IP confirmation.
func (g GatewaySettings) PinTimeout() time.Duration {
	return secondsDuration(g.PinTimeoutSeconds, 20)
}

// ProbeTimeout bounds a single controller request.
func (g GatewaySettings) ProbeTimeout() time.Duration {
	return secondsDuration(g.ProbeTimeoutSeconds, 5)
}

// CircuitThreshold is how many consecutive gateway failures degrade a channel.
func (g GatewaySettings) FailureThreshold() int {
	if g.CircuitThreshold > 0 {
		return g.CircuitThreshold
	}
	return 3
}

// CircuitCooldown is how long a degraded channel waits before retrying.
func (g GatewaySettings) CircuitCooldown() time.Duration {
	return secondsDuration(g.CircuitCooldownSeconds, 60)
}

// StartupTimeout bounds one embedded browser launch plus CDP handshake.
func (b BrowserSettings) StartupTimeout() time.Duration {
	return secondsDuration(b.StartupTimeoutSeconds, 40)
}

// StepTimeout bounds one page interaction step.
func (b BrowserSettings) StepTimeout() time.Duration {
	return secondsDuration(b.StepTimeoutSeconds, 120)
}

// HumanWait bounds how long the engine waits for manual intervention.
func (b BrowserSettings) HumanWait() time.Duration {
	return secondsDuration(b.HumanWaitSeconds, 300)
}

// Interval is the pause between two accounts of a channel.
func (c ChannelSettings) Interval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

// StepDelay is the human pacing delay between UI steps.
func (c ChannelSettings) StepDelay() time.Duration {
	if c.StepDelayMillis > 0 {
		return time.Duration(c.StepDelayMillis) * time.Millisecond
	}
	return 600 * time.Millisecond
}

func secondsDuration(value, fallback int) time.Duration {
	if value > 0 {
		return time.Duration(value) * time.Second
	}
	return time.Duration(fallback) * time.Second
}

// DefaultSettings returns the shipped defaults for a fresh install.
func DefaultSettings() Settings {
	return Settings{
		Enabled:  true,
		StateDir: "registerbot",
		Evidence: true,
		Gateway: GatewaySettings{
			ProxyURL:               "http://127.0.0.1:12450",
			AutoDiscover:           true,
			VerifyExitIP:           true,
			ExitIPURL:              "https://api.ipify.org?format=json",
			ShareMode:              ShareModeLease,
			PinTimeoutSeconds:      20,
			ProbeTimeoutSeconds:    5,
			CircuitThreshold:       3,
			CircuitCooldownSeconds: 60,
		},
		Browser: BrowserSettings{
			Mode:                  BrowserModeEmbedded,
			PoolSize:              2,
			WindowWidth:           1440,
			WindowHeight:          900,
			StartupTimeoutSeconds: 40,
			StepTimeoutSeconds:    120,
			HumanWaitSeconds:      300,
			RetireAfter:           8,
			RemoteDebugPort:       9333,
		},
		Import: ImportSettings{
			Family:    "openai-compatibility",
			VerifyURL: "https://api.freeaiapikey.com/v1/models",
		},
		Channels: []ChannelSettings{DefaultFreeAIChannel()},
	}
}

// DefaultFreeAIChannel is the first shipped channel kind.
func DefaultFreeAIChannel() ChannelSettings {
	return ChannelSettings{
		ID:              "freeaiapikey",
		Kind:            ChannelKindFreeAIApiKey,
		Name:            "FreeAIapikey",
		Enabled:         true,
		Provider:        "FreeAi",
		ProxyURL:        "http://127.0.0.1:12450",
		Concurrency:     1,
		Group:           "节点选择",
		RotateNode:      true,
		IntervalSeconds: 45,
		StepDelayMillis: 900,
		MaxAttempts:     2,
	}
}

// Normalize fills defaults and clamps unsafe values.
func (s *Settings) Normalize() {
	def := DefaultSettings()
	if s.StateDir == "" {
		s.StateDir = def.StateDir
	}
	if s.Gateway.ProxyURL == "" {
		s.Gateway.ProxyURL = def.Gateway.ProxyURL
	}
	if s.Gateway.ShareMode == "" {
		s.Gateway.ShareMode = def.Gateway.ShareMode
	}
	if s.Gateway.ExitIPURL == "" {
		s.Gateway.ExitIPURL = def.Gateway.ExitIPURL
	}
	if s.Gateway.PinTimeoutSeconds == 0 {
		s.Gateway.PinTimeoutSeconds = def.Gateway.PinTimeoutSeconds
	}
	if s.Gateway.ProbeTimeoutSeconds == 0 {
		s.Gateway.ProbeTimeoutSeconds = def.Gateway.ProbeTimeoutSeconds
	}
	if s.Gateway.CircuitThreshold == 0 {
		s.Gateway.CircuitThreshold = def.Gateway.CircuitThreshold
	}
	if s.Gateway.CircuitCooldownSeconds == 0 {
		s.Gateway.CircuitCooldownSeconds = def.Gateway.CircuitCooldownSeconds
	}
	if s.Browser.Mode == "" {
		s.Browser.Mode = def.Browser.Mode
	}
	if s.Browser.PoolSize <= 0 {
		s.Browser.PoolSize = def.Browser.PoolSize
	}
	if s.Browser.PoolSize > 8 {
		s.Browser.PoolSize = 8
	}
	if s.Browser.WindowWidth <= 0 {
		s.Browser.WindowWidth = def.Browser.WindowWidth
	}
	if s.Browser.WindowHeight <= 0 {
		s.Browser.WindowHeight = def.Browser.WindowHeight
	}
	if s.Browser.StartupTimeoutSeconds == 0 {
		s.Browser.StartupTimeoutSeconds = def.Browser.StartupTimeoutSeconds
	}
	if s.Browser.StepTimeoutSeconds == 0 {
		s.Browser.StepTimeoutSeconds = def.Browser.StepTimeoutSeconds
	}
	if s.Browser.HumanWaitSeconds == 0 {
		s.Browser.HumanWaitSeconds = def.Browser.HumanWaitSeconds
	}
	if s.Browser.RetireAfter <= 0 {
		s.Browser.RetireAfter = def.Browser.RetireAfter
	}
	if s.Browser.RemoteDebugPort <= 0 || s.Browser.RemoteDebugPort > 65535 {
		s.Browser.RemoteDebugPort = def.Browser.RemoteDebugPort
	}
	if s.Import.Family == "" {
		s.Import.Family = def.Import.Family
	}
	if s.Import.VerifyURL == "" {
		s.Import.VerifyURL = def.Import.VerifyURL
	}
	if len(s.Browser.ChromePaths) == 0 {
		s.Browser.ChromePaths = detectChromePaths()
	}
	for i := range s.Channels {
		ch := &s.Channels[i]
		if ch.ID == "" {
			ch.ID = slugChannelName(ch.Name)
		}
		if ch.Kind == "" {
			ch.Kind = ChannelKindFreeAIApiKey
		}
		if ch.Name == "" {
			ch.Name = ch.ID
		}
		if ch.Concurrency <= 0 {
			ch.Concurrency = 1
		}
		if ch.Concurrency > 8 {
			ch.Concurrency = 8
		}
		if ch.MaxAttempts <= 0 {
			ch.MaxAttempts = 1
		}
		if ch.MaxAttempts > 10 {
			ch.MaxAttempts = 10
		}
		if ch.Group == "" {
			ch.Group = def.Channels[0].Group
		}
		if ch.IntervalSeconds < 0 {
			ch.IntervalSeconds = 0
		}
		if ch.StepDelayMillis == 0 {
			ch.StepDelayMillis = 600
		}
		if ch.Provider == "" {
			ch.Provider = def.Channels[0].Provider
		}
		if ch.ProxyURL == "" {
			ch.ProxyURL = s.Gateway.ProxyURL
		}
	}
	sort.SliceStable(s.Channels, func(i, j int) bool { return s.Channels[i].ID < s.Channels[j].ID })
}

// Channel looks up a channel by id.
func (s *Settings) Channel(id string) (*ChannelSettings, bool) {
	want := strings.TrimSpace(id)
	for i := range s.Channels {
		if strings.EqualFold(s.Channels[i].ID, want) {
			return &s.Channels[i], true
		}
	}
	return nil, false
}

func slugChannelName(name string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		case r == ' ', r == '.', r == '/', r == '@':
			builder.WriteRune('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "channel"
	}
	return out
}

// store owns the on-disk settings document.
type store struct {
	mu        sync.Mutex
	path      string
	settings  Settings
	lastSaved time.Time
}

func newStore(path string) *store {
	return &store{path: path, settings: DefaultSettings()}
}

// Load reads the settings file once, creating a documented default when absent.
func (st *store) Load() (Settings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	data, err := os.ReadFile(st.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return st.settings, fmt.Errorf("registerbot: read settings: %w", err)
		}
		st.settings = DefaultSettings()
		st.settings.Normalize()
		if errWrite := st.writeLocked(); errWrite != nil {
			return st.settings, errWrite
		}
		return st.settings, nil
	}
	var parsed Settings
	if errUnmarshal := yaml.Unmarshal(data, &parsed); errUnmarshal != nil {
		return st.settings, fmt.Errorf("registerbot: parse settings: %w", errUnmarshal)
	}
	merged := parsed
	defaults := DefaultSettings()
	if len(merged.Channels) == 0 {
		merged.Channels = defaults.Channels
	}
	merged.Normalize()
	st.settings = merged
	st.lastSaved = time.Now()
	return merged, nil
}

// Update applies mutate to the live settings and persists the result.
func (st *store) Update(mutate func(*Settings)) (Settings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := cloneSettings(st.settings)
	if mutate != nil {
		mutate(&next)
	}
	next.Normalize()
	if errValidate := validateSettings(&next); errValidate != nil {
		return st.settings, errValidate
	}
	previous := st.settings
	st.settings = next
	if errWrite := st.writeLocked(); errWrite != nil {
		st.settings = previous
		return previous, errWrite
	}
	return next, nil
}

// Snapshot returns a copy of the current settings.
func (st *store) Snapshot() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return cloneSettings(st.settings)
}

func cloneSettings(in Settings) Settings {
	out := in
	out.Gateway.Groups = append([]string(nil), in.Gateway.Groups...)
	out.Browser.ChromePaths = append([]string(nil), in.Browser.ChromePaths...)
	out.Browser.ExtraArgs = append([]string(nil), in.Browser.ExtraArgs...)
	out.Channels = make([]ChannelSettings, len(in.Channels))
	for i, ch := range in.Channels {
		dup := ch
		dup.Nodes = append([]string(nil), ch.Nodes...)
		out.Channels[i] = dup
	}
	return out
}

func validateSettings(s *Settings) error {
	if len(s.Channels) > 32 {
		return errors.New("registerbot: at most 32 channels are allowed")
	}
	seen := make(map[string]struct{}, len(s.Channels))
	for _, ch := range s.Channels {
		if strings.TrimSpace(ch.ID) == "" || slugChannelName(ch.ID) != ch.ID {
			return errors.New("registerbot: channel id is required")
		}
		if _, dup := seen[ch.ID]; dup {
			return fmt.Errorf("registerbot: duplicate channel id %q", ch.ID)
		}
		seen[ch.ID] = struct{}{}
		if !KnownChannelKind(ch.Kind) {
			return fmt.Errorf("registerbot: unknown channel kind %q", ch.Kind)
		}
		if ch.OnboardingRole != "" && !oneOf(ch.OnboardingRole, "student", "indie", "founder", "employee", "researcher", "exploring") {
			return errors.New("registerbot: invalid onboarding role")
		}
		if ch.OnboardingSource != "" && !oneOf(ch.OnboardingSource, "google", "ai", "youtube", "instagram", "reddit", "x", "discord", "github", "friend", "other") {
			return errors.New("registerbot: invalid onboarding source")
		}
	}
	switch s.Gateway.ShareMode {
	case ShareModeLease, ShareModePerChannel, ShareModeShared:
	default:
		return fmt.Errorf("registerbot: unknown gateway share mode %q", s.Gateway.ShareMode)
	}
	switch s.Browser.Mode {
	case BrowserModeEmbedded, BrowserModeAttach, BrowserModeSystem, BrowserModeManual:
	default:
		return fmt.Errorf("registerbot: unknown browser mode %q", s.Browser.Mode)
	}
	return nil
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

// writeLocked persists settings with an atomic replace. Caller holds st.mu.
func (st *store) writeLocked() error {
	dir := filepath.Dir(st.path)
	if errMkdir := os.MkdirAll(dir, 0o750); errMkdir != nil {
		return fmt.Errorf("registerbot: create state dir: %w", errMkdir)
	}
	data, errMarshal := yaml.Marshal(&st.settings)
	if errMarshal != nil {
		return fmt.Errorf("registerbot: encode settings: %w", errMarshal)
	}
	if errWrite := atomicWrite(st.path, data); errWrite != nil {
		return fmt.Errorf("registerbot: write settings: %w", errWrite)
	}
	st.lastSaved = time.Now()
	return nil
}

// detectChromePaths returns candidate installed Chrome/Chromium binaries.
func detectChromePaths() []string {
	var out []string
	push := func(candidate string) {
		if candidate == "" {
			return
		}
		if abs, err := filepath.Abs(candidate); err == nil {
			candidate = abs
		}
		for _, existing := range out {
			if strings.EqualFold(existing, candidate) {
				return
			}
		}
		out = append(out, candidate)
	}
	envKeys := []string{"PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA", "PROGRAMW6432"}
	relatives := []string{
		filepath.Join("Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join("Google", "Chrome Bin", "chrome.exe"),
		filepath.Join("Chromium", "Application", "chrome.exe"),
		filepath.Join("Chromium", "Application", "chromium.exe"),
		filepath.Join("Microsoft", "Edge", "Application", "msedge.exe"),
	}
	for _, key := range envKeys {
		root := os.Getenv(key)
		if root == "" {
			continue
		}
		for _, relative := range relatives {
			candidate := filepath.Join(root, relative)
			if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
				push(candidate)
			}
		}
	}
	for _, onPath := range []string{"chrome", "chrome.exe", "chromium", "msedge"} {
		if found, err := exec.LookPath(onPath); err == nil {
			push(found)
		}
	}
	if len(out) == 0 {
		// Keep the conventional default so the settings file stays self-documenting.
		out = append(out, filepath.Join(`C:\Program Files`, "Google", "Chrome", "Application", "chrome.exe"))
	}
	return out
}
