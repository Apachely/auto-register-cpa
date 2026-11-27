package registerbot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	log "github.com/sirupsen/logrus"
)

// browserInstance is one pooled Chrome process plus its browser level CDP session.
type browserInstance struct {
	id        int
	proc      *os.Process
	control   *cdpClient
	transport *cdpTransport
	// port is the loopback DevTools endpoint; profileDir the isolated user data dir.
	port       int
	profileDir string
	browser    string
	mode       string
	// used counts accounts this instance already served.
	used     int
	lastUsed time.Time
	mu       sync.Mutex
	pidFile  string
	contexts int
	proxyURL string
	owned    bool
	metaFile string
}

// Alive reports whether the instance can still drive a page.
//
// The DevTools HTTP endpoint is the liveness signal: Chrome closes it when the
// process exits, so a answered /json/version means the instance is usable.
func (b *browserInstance) Alive(ctx context.Context) bool {
	if b == nil || b.transport == nil {
		return false
	}
	if b.control != nil && b.control.loadErr() != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, errReady := b.transport.versionReady(probeCtx)
	return errReady == nil
}

// ID identifies the pooled instance in events and evidence names.
func (b *browserInstance) ID() int {
	if b == nil {
		return -1
	}
	return b.id
}

// Mode reports which ladder position produced this instance.
func (b *browserInstance) Mode() string {
	if b == nil {
		return ""
	}
	return b.mode
}

// Browser describes the resolved build, for the panel header.
func (b *browserInstance) Browser() string {
	if b == nil {
		return ""
	}
	return b.browser
}

// Used reports how many accounts this instance already served.
func (b *browserInstance) Used() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// ProfileDir returns the isolated user data directory of this instance.
func (b *browserInstance) ProfileDir() string {
	if b == nil {
		return ""
	}
	return b.profileDir
}

// markUsed records one more served account.
func (b *browserInstance) markUsed() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.used++
	b.lastUsed = time.Now()
	b.mu.Unlock()
}

// Pool owns the embedded Chrome instances and the browser degradation ladder.
//
// The ladder is what guarantees the core capability keeps working:
//
//	embedded -> attach -> system -> manual
//
// embedded launches its own hidden Chrome with one isolated profile per pooled
// instance, so a bulk run never multiplies the operator's own browser windows.
// attach connects to a DevTools endpoint that is already listening. system reuses
// the installed browser through the same CDP path with its own profile. manual
// stops requiring a browser inside CPA and hands the step to the operator.
type Pool struct {
	settings func() Settings
	stateDir string

	mu        sync.Mutex
	acquireMu sync.Mutex
	instances map[int]*browserInstance
	busy      map[int]bool
	available chan struct{}
	order     []int
	nextID    int
	mode      string
	reason    string
	proxyURL  string
	recovered bool
}

// NewPool builds the browser pool. stateDir holds profiles and evidence.
func NewPool(settingsFunc func() Settings, stateDir string) *Pool {
	set := DefaultSettings()
	if settingsFunc != nil {
		set = settingsFunc()
	}
	return &Pool{
		settings:  settingsFunc,
		stateDir:  stateDir,
		instances: make(map[int]*browserInstance),
		busy:      make(map[int]bool),
		available: make(chan struct{}, 1),
		mode:      set.Browser.Mode,
		proxyURL:  set.Gateway.ProxyURL,
	}
}

// SetProxyURL selects forwarding for newly created isolated page contexts.
func (p *Pool) SetProxyURL(proxyURL string) { p.mu.Lock(); p.proxyURL = proxyURL; p.mu.Unlock() }

func (p *Pool) snapshotSettings() Settings {
	if p == nil || p.settings == nil {
		return DefaultSettings()
	}
	return p.settings()
}

// Mode reports the effective browser mode after any degradation.
func (p *Pool) Mode() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode, p.reason
}

// SetMode pins the ladder position, used by the panel override and by degradation.
func (p *Pool) SetMode(mode, reason string) error {
	switch mode {
	case BrowserModeEmbedded, BrowserModeAttach, BrowserModeSystem, BrowserModeManual:
	default:
		return fmt.Errorf("registerbot: unknown browser mode %q", mode)
	}
	p.mu.Lock()
	p.mode = mode
	p.reason = reason
	p.mu.Unlock()
	return nil
}

// profileDir returns the isolated user data directory for one pooled instance.
func (p *Pool) profileDir(id int) string {
	set := p.snapshotSettings()
	base := strings.TrimSpace(set.Browser.ProfileBaseDir)
	if base == "" {
		base = filepath.Join(p.stateDir, "profiles")
	}
	if !filepath.IsAbs(base) {
		if abs, errAbs := filepath.Abs(base); errAbs == nil {
			base = abs
		}
	}
	slot := id
	if size := p.poolSize(); size > 0 {
		slot = id % size
	}
	return filepath.Join(base, fmt.Sprintf("instance-%d", slot))
}

// freePort reserves a loopback port for one DevTools listener.
func freePort() (int, error) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		return 0, errListen
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	port := 0
	if ok {
		port = addr.Port
	}
	if errClose := listener.Close(); errClose != nil {
		_ = errClose
	}
	if port == 0 {
		return 0, errors.New("registerbot: devtools port allocation failed")
	}
	return port, nil
}

// launchArgs builds the Chrome command line for one pooled instance.
func launchArgs(set Settings, profileDir string, port int) []string {
	width, height := set.Browser.WindowWidth, set.Browser.WindowHeight
	if width <= 0 {
		width = 1440
	}
	if height <= 0 {
		height = 900
	}
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + profileDir,
		fmt.Sprintf("--window-size=%d,%d", width, height),
		"--window-position=80,80",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--disable-features=Translate,MediaRouter",
		"--disable-session-crashed-bubble",
		"--disable-infobars",
		"--no-service-autorun",
		"--password-store=basic",
		"--test-type",
		"--lang=en-US",
	}
	if set.Browser.Headless {
		args = append(args, "--headless=new", "--disable-gpu")
	}
	if proxyURL := strings.TrimSpace(set.Gateway.ProxyURL); proxyURL != "" {
		args = append(args, "--proxy-server="+proxyURL)
	}
	args = append(args, set.Browser.ExtraArgs...)
	return append(args, "about:blank")
}

// resolveChrome returns the first existing configured browser binary.
func (p *Pool) resolveChrome() (string, error) {
	set := p.snapshotSettings()
	for _, candidate := range set.Browser.ChromePaths {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if stat, errStat := os.Stat(candidate); errStat == nil && !stat.IsDir() {
			return candidate, nil
		}
	}
	for _, name := range []string{"chrome", "chrome.exe", "chromium", "msedge"} {
		if found, errLook := exec.LookPath(name); errLook == nil {
			return found, nil
		}
	}
	return "", errors.New("registerbot: no Chrome/Chromium/Edge binary found")
}

// poolSize returns the configured live instance ceiling.
func (p *Pool) poolSize() int {
	size := p.snapshotSettings().Browser.PoolSize
	if size <= 0 {
		return 1
	}
	if size > 8 {
		return 8
	}
	return size
}

// Acquire hands out a live instance, launching one when the pool is short.
func (p *Pool) Acquire(ctx context.Context) (*browserInstance, error) {
	return p.acquire(ctx, true)
}

func (p *Pool) acquire(ctx context.Context, countUse bool) (*browserInstance, error) {
	for {
		instance, errAcquire := p.tryAcquire(ctx, countUse)
		if !errors.Is(errAcquire, errPoolBusy) {
			return instance, errAcquire
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.available:
		}
	}
}

// Warm starts isolated local browser processes before an account reaches the
// queue. Held slots force the pool to fill without sharing profile data with the
// operator's personal browser.
func (p *Pool) Warm(ctx context.Context, count int) error {
	if count <= 0 {
		return nil
	}
	if maximum := p.poolSize(); count > maximum {
		count = maximum
	}
	held := make([]*browserInstance, 0, count)
	defer func() {
		for _, instance := range held {
			p.Release(instance)
		}
	}()
	for len(held) < count {
		instance, errAcquire := p.acquire(ctx, false)
		if errAcquire != nil {
			return errAcquire
		}
		held = append(held, instance)
	}
	return nil
}

var errPoolBusy = errors.New("registerbot: browser pool busy")

func (p *Pool) tryAcquire(ctx context.Context, countUse bool) (*browserInstance, error) {
	p.acquireMu.Lock()
	defer p.acquireMu.Unlock()
	if !p.recovered {
		p.recovered = true
		p.recoverManaged(ctx)
	}
	set := p.snapshotSettings()
	p.mu.Lock()
	mode := p.mode
	p.mu.Unlock()

	if mode == BrowserModeManual {
		return nil, ErrManualHandoff
	}

	// Reuse a live instance whose usage budget is not exhausted.
	for _, instance := range p.liveInstances() {
		p.mu.Lock()
		busy := p.busy[instance.id]
		p.mu.Unlock()
		if busy {
			continue
		}
		instance.mu.Lock()
		contexts := instance.contexts
		instance.mu.Unlock()
		if contexts == 0 && set.Browser.RetireAfter > 0 && instance.Used() >= set.Browser.RetireAfter {
			p.retire(instance)
			continue
		}
		if instance.Alive(ctx) {
			if countUse {
				instance.markUsed()
			}
			p.mu.Lock()
			p.busy[instance.id] = true
			instance.mu.Lock()
			instance.proxyURL = p.proxyURL
			instance.mu.Unlock()
			p.mu.Unlock()
			return instance, nil
		}
		p.retire(instance)
	}

	if len(p.liveInstances()) >= p.poolSize() {
		return nil, errPoolBusy
	}

	rungs := []string{BrowserModeEmbedded, BrowserModeAttach, BrowserModeSystem}
	for i, rung := range rungs {
		if rung == mode {
			rungs = rungs[i:]
			break
		}
	}
	var lastErr error
	seen := make(map[string]bool, len(rungs))
	for _, rung := range rungs {
		if rung == BrowserModeManual || seen[rung] {
			continue
		}
		seen[rung] = true
		instance, errLaunch := p.launchAs(ctx, set, rung)
		if errLaunch == nil {
			if rung != mode {
				_ = p.SetMode(rung, fmt.Sprintf("browser degraded from %s to %s: %v", mode, rung, lastErr))
				log.WithFields(log.Fields{"from": mode, "to": rung, "cause": errString(lastErr)}).
					Warn("registerbot: browser ladder degradation")
			}
			if countUse {
				instance.markUsed()
			}
			p.mu.Lock()
			p.busy[instance.id] = true
			instance.mu.Lock()
			instance.proxyURL = p.proxyURL
			instance.mu.Unlock()
			p.mu.Unlock()
			return instance, nil
		}
		lastErr = errLaunch
	}
	// Nothing can provide a browser: keep the batch alive by handing steps to the
	// operator instead of failing the whole run.
	_ = p.SetMode(BrowserModeManual, "browser ladder exhausted: "+errString(lastErr))
	return nil, ErrManualHandoff
}

// Release returns an exclusive process slot while retaining manual page contexts.
func (p *Pool) Release(instance *browserInstance) {
	if instance == nil {
		return
	}
	p.mu.Lock()
	delete(p.busy, instance.id)
	p.mu.Unlock()
	select {
	case p.available <- struct{}{}:
	default:
	}
}

// launchAs starts one instance forced to a specific ladder position.
func (p *Pool) launchAs(ctx context.Context, set Settings, mode string) (*browserInstance, error) {
	switch mode {
	case BrowserModeEmbedded, BrowserModeSystem:
		return p.launchProcess(ctx, set, mode)
	case BrowserModeAttach:
		return p.attach(ctx, set)
	default:
		return nil, fmt.Errorf("registerbot: mode %q does not provide a browser", mode)
	}
}

// launchProcess starts a local browser binary and waits for its DevTools endpoint.
func (p *Pool) launchProcess(ctx context.Context, set Settings, mode string) (*browserInstance, error) {
	exe, errResolve := p.resolveChrome()
	if errResolve != nil {
		return nil, errResolve
	}
	port, errPort := freePort()
	if errPort != nil {
		return nil, errPort
	}
	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.mu.Unlock()

	profileDir := p.profileDir(id)
	if errMkdir := os.MkdirAll(profileDir, 0o700); errMkdir != nil {
		return nil, fmt.Errorf("registerbot: create profile dir: %w", errMkdir)
	}

	cmd := exec.Command(exe, launchArgs(set, profileDir, port)...)
	hideWindow(cmd)
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		return nil, fmt.Errorf("registerbot: browser stdout pipe: %w", errStdout)
	}
	stderr, errStderr := cmd.StderrPipe()
	if errStderr != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("registerbot: browser stderr pipe: %w", errStderr)
	}
	if errStart := cmd.Start(); errStart != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("registerbot: launch browser: %w", errStart)
	}
	go drainBrowserLog(stdout, id)
	go drainBrowserLog(stderr, id)

	instance := &browserInstance{
		id:         id,
		proc:       cmd.Process,
		port:       port,
		profileDir: profileDir,
		transport:  newCDPTransport(fmt.Sprintf("127.0.0.1:%d", port)),
		browser:    filepath.Base(exe),
		mode:       mode,
		owned:      true,
		lastUsed:   time.Now(),
	}
	if instance.proc != nil {
		instance.pidFile = filepath.Join(p.stateDir, fmt.Sprintf("browser-%d.pid", id))
		_ = os.WriteFile(instance.pidFile, []byte(fmt.Sprint(instance.proc.Pid)), 0o600)
	}

	waitCtx, cancel := context.WithTimeout(ctx, set.Browser.StartupTimeout())
	defer cancel()
	if errWait := waitForDevTools(waitCtx, instance.transport); errWait != nil {
		instance.shutdown()
		return nil, errWait
	}
	controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
	defer controlCancel()
	control, errConnect := instance.transport.connectBrowser(controlCtx)
	if errConnect != nil {
		instance.shutdown()
		return nil, errConnect
	}
	instance.control = control
	instance.metaFile = filepath.Join(p.stateDir, fmt.Sprintf("browser-%d.json", id))
	meta := managedBrowser{ID: id, Port: port, Browser: instance.browser, Mode: mode, Endpoint: control.endpoint, ProfileDir: profileDir}
	if encoded, errEncode := json.Marshal(meta); errEncode == nil {
		if errWrite := os.WriteFile(instance.metaFile+".tmp", encoded, 0o600); errWrite == nil {
			_ = os.Rename(instance.metaFile+".tmp", instance.metaFile)
		}
	}
	if version, errVersion := instance.transport.versionReady(controlCtx); errVersion == nil && version != "" {
		instance.browser = version
	}

	p.mu.Lock()
	p.instances[id] = instance
	p.order = append(p.order, id)
	p.mu.Unlock()
	log.WithFields(log.Fields{
		"instance": id, "mode": mode, "port": port, "profile": profileDir,
	}).Debug("registerbot: browser instance ready")
	return instance, nil
}

// attach connects to a DevTools endpoint that is already listening.
func (p *Pool) attach(ctx context.Context, set Settings) (*browserInstance, error) {
	endpoint := strings.TrimSpace(set.Browser.AttachURL)
	if endpoint == "" {
		return nil, errors.New("registerbot: no attach endpoint configured")
	}
	transport := newCDPTransport(endpoint)
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if errWait := waitForDevTools(waitCtx, transport); errWait != nil {
		return nil, errWait
	}
	controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
	defer controlCancel()
	control, errConnect := transport.connectBrowser(controlCtx)
	if errConnect != nil {
		return nil, errConnect
	}
	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.mu.Unlock()
	instance := &browserInstance{
		id: id, control: control, transport: transport,
		browser: "attached endpoint", mode: BrowserModeAttach, lastUsed: time.Now(),
	}
	p.mu.Lock()
	p.instances[id] = instance
	p.order = append(p.order, id)
	p.mu.Unlock()
	return instance, nil
}

// waitForDevTools polls a DevTools endpoint until it answers.
func waitForDevTools(ctx context.Context, transport *cdpTransport) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, lastErr = transport.versionReady(probeCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("registerbot: devtools endpoint did not come up: %v (last: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// drainBrowserLog forwards browser output to debug logs only, never to the
// account event stream, so tokens in URLs cannot leak into the panel.
func drainBrowserLog(reader io.Reader, id int) {
	// Browser diagnostics may contain URLs or page data. Drain without logging.
	_, _ = io.Copy(io.Discard, reader)
}

func redactBrowserLine(text string) string {
	if len(text) > 400 {
		return text[:400] + "..."
	}
	return text
}

// liveInstances returns pooled instances in launch order.
func (p *Pool) liveInstances() []*browserInstance {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*browserInstance, 0, len(p.order))
	for _, id := range p.order {
		if instance := p.instances[id]; instance != nil {
			out = append(out, instance)
		}
	}
	return out
}

// retire drops one instance from the pool and stops it.
func (p *Pool) retire(instance *browserInstance) {
	if instance == nil {
		return
	}
	p.mu.Lock()
	delete(p.instances, instance.id)
	delete(p.busy, instance.id)
	for i, id := range p.order {
		if id == instance.id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	if errStop := instance.shutdown(); errStop != nil {
		log.WithError(errStop).Debug("registerbot: instance shutdown reported an error")
	}
}

// Retire marks an instance for restart, used after a hard page stall.
func (p *Pool) Retire(instance *browserInstance) {
	p.retire(instance)
}

// Shutdown stops every pooled instance.
func (p *Pool) Shutdown() error {
	var firstErr error
	for _, instance := range p.liveInstances() {
		p.retire(instance)
	}
	p.mu.Lock()
	p.instances = make(map[int]*browserInstance)
	p.order = nil
	p.mu.Unlock()
	return firstErr
}

// PoolStatus is the pool view exposed to the panel.
type PoolStatus struct {
	Mode       string           `json:"mode"`
	Reason     string           `json:"reason,omitempty"`
	PoolSize   int              `json:"pool_size"`
	Live       int              `json:"live"`
	Headless   bool             `json:"headless"`
	Instances  []InstanceStatus `json:"instances"`
	Candidates []string         `json:"chrome_candidates,omitempty"`
	Ladder     []LadderStep     `json:"ladder"`
}

// InstanceStatus describes one live browser instance.
type InstanceStatus struct {
	ID       int    `json:"id"`
	PID      int    `json:"pid,omitempty"`
	Port     int    `json:"port,omitempty"`
	Browser  string `json:"browser,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Used     int    `json:"used"`
	Attached bool   `json:"attached"`
}

// LadderStep is one ladder position with its measured readiness.
type LadderStep struct {
	Mode  string `json:"mode"`
	Ready bool   `json:"ready"`
	Note  string `json:"note,omitempty"`
}

// Status summarises the pool and probes every ladder position.
func (p *Pool) Status(ctx context.Context) PoolStatus {
	set := p.snapshotSettings()
	mode, reason := p.Mode()
	instances := p.liveInstances()
	status := PoolStatus{
		Mode:       mode,
		Reason:     reason,
		PoolSize:   p.poolSize(),
		Live:       len(instances),
		Headless:   set.Browser.Headless,
		Candidates: set.Browser.ChromePaths,
	}
	for _, instance := range instances {
		item := InstanceStatus{
			ID: instance.id, Port: instance.port, Browser: instance.browser,
			Mode: instance.mode, Used: instance.Used(), Attached: instance.proc == nil && !instance.owned,
		}
		if instance.proc != nil {
			item.PID = instance.proc.Pid
		}
		status.Instances = append(status.Instances, item)
	}
	_, errResolve := p.resolveChrome()
	browserFound := errResolve == nil
	for _, rung := range []string{BrowserModeEmbedded, BrowserModeAttach, BrowserModeSystem, BrowserModeManual} {
		entry := LadderStep{Mode: rung}
		switch rung {
		case BrowserModeEmbedded:
			entry.Ready = browserFound
			if !browserFound {
				entry.Note = "no local Chrome binary"
			}
		case BrowserModeAttach:
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, errReady := newCDPTransport(set.Browser.AttachURL).versionReady(probeCtx)
			cancel()
			entry.Ready = errReady == nil
			if !entry.Ready {
				entry.Note = "no DevTools endpoint at " + set.Browser.AttachURL
			}
		case BrowserModeSystem:
			entry.Ready = browserFound
		case BrowserModeManual:
			entry.Ready = true
			entry.Note = "operator completes the step in their own window"
		}
		status.Ladder = append(status.Ladder, entry)
	}
	return status
}

// kill stops the process without a graceful CDP close.
func (b *browserInstance) kill() {
	if b == nil {
		return
	}
	if b.control != nil && b.proc != nil {
		_ = b.control.Close()
		b.control = nil
	}
	if b.control != nil {
		_ = b.control.Close()
		b.control = nil
	}
	if b.proc != nil {
		_ = b.proc.Kill()
		_, _ = b.proc.Wait()
		b.proc = nil
	}
	if b.pidFile != "" {
		_ = os.Remove(b.pidFile)
	}
}

// shutdown asks the browser to exit gracefully, then kills it.
func (b *browserInstance) shutdown() error {
	if b == nil {
		return nil
	}
	var errCall error
	if b.control != nil {
		if b.proc != nil || b.owned {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, errCall = b.control.Call(ctx, "Browser.close", nil)
			cancel()
		}
		_ = b.control.Close()
		b.control = nil
	}
	if b.proc != nil {
		waited := make(chan struct{})
		go func() {
			defer close(waited)
			_, _ = b.proc.Wait()
		}()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-waited:
		case <-timer.C:
			_ = b.proc.Kill()
			<-waited
		}
		b.proc = nil
	}
	if b.pidFile != "" {
		_ = os.Remove(b.pidFile)
	}
	if b.metaFile != "" {
		_ = os.Remove(b.metaFile)
	}
	return errCall
}

type managedBrowser struct {
	ID         int    `json:"id"`
	Port       int    `json:"port"`
	Browser    string `json:"browser"`
	Mode       string `json:"mode"`
	Endpoint   string `json:"endpoint"`
	ProfileDir string `json:"profile_dir"`
}

// recoverManaged reconnects only an endpoint whose browser UUID still matches
// our persisted ownership record; a reused port cannot become an owned browser.
func (p *Pool) recoverManaged(ctx context.Context) {
	paths, _ := filepath.Glob(filepath.Join(p.stateDir, "browser-*.json"))
	for _, path := range paths {
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		var meta managedBrowser
		if json.Unmarshal(data, &meta) != nil || meta.Port < 1 || meta.Port > 65535 || meta.Endpoint == "" {
			p.removeManagedRecord(path)
			continue
		}
		if meta.ID >= p.nextID {
			p.nextID = meta.ID + 1
		}
		transport := newCDPTransport(fmt.Sprintf("127.0.0.1:%d", meta.Port))
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var version struct {
			Endpoint string `json:"webSocketDebuggerUrl"`
		}
		errVersion := transport.getJSON(probeCtx, "/json/version", &version)
		if errVersion != nil || version.Endpoint != meta.Endpoint {
			cancel()
			p.removeManagedRecord(path)
			continue
		}
		control, errConnect := transport.connectBrowser(probeCtx)
		cancel()
		if errConnect != nil {
			p.removeManagedRecord(path)
			continue
		}
		instance := &browserInstance{id: meta.ID, port: meta.Port, mode: meta.Mode, browser: meta.Browser, profileDir: meta.ProfileDir, control: control, transport: transport, owned: true, metaFile: path, proxyURL: p.proxyURL}
		p.mu.Lock()
		p.instances[meta.ID] = instance
		p.order = append(p.order, meta.ID)
		p.mu.Unlock()
	}
}

func (p *Pool) removeManagedRecord(path string) {
	_ = os.Remove(path)
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + ".pid"
	_ = os.Remove(filepath.Join(p.stateDir, name))
}
