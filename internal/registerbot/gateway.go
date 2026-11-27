package registerbot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
)

// Gateway is the unified gateway layer above the registration channels.
//
// It talks to the registration-only proxy (default http://127.0.0.1:11460) on two
// planes:
//   - the forwarding plane, used as the HTTP proxy for browsers and API checks;
//   - the control plane, a Clash compatible controller used to list groups and
//     pin one node so different channels register from different exit IPs.
//
// The control plane is optional by design. When it is unreachable the gateway
// degrades to proxy_only and the batch continues on one shared exit; when the
// forwarding plane is also down it degrades to disabled and the batch continues
// directly, because registering accounts matters more than node isolation.
type Gateway struct {
	settings func() Settings

	mu            sync.Mutex
	proxyClient   *http.Client
	leaseChanged  chan struct{}
	leaseRoute    string
	leaseReady    bool
	leaseResult   PinResult
	rotation      map[string]int
	controllerURL string
	controllerAt  time.Time
	mode          string
	lastError     string
	checkedAt     time.Time
	lastStatus    Status
	groups        map[string]Group
	leases        map[string]lease  // group|holder -> active account
	pinState      map[string]string // group -> node pinned by this process
}

type lease struct {
	channel string
	at      time.Time
}

// NewGateway builds the gateway layer. settingsFunc must return the live settings.
func NewGateway(settingsFunc func() Settings) *Gateway {
	return &Gateway{
		settings:     settingsFunc,
		mode:         GatewayModeDisabled,
		groups:       make(map[string]Group),
		leases:       make(map[string]lease),
		pinState:     make(map[string]string),
		leaseChanged: make(chan struct{}),
		rotation:     make(map[string]int),
		proxyClient: &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 8,
			},
		},
	}
}

// Group is one controller proxy group and its selectable nodes.
type Group struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Now   string   `json:"now"`
	Nodes []string `json:"nodes"`
}

// Status is the gateway view exposed to the panel.
type Status struct {
	Mode          string   `json:"mode"`
	ProxyURL      string   `json:"proxy_url"`
	ControllerURL string   `json:"controller_url,omitempty"`
	ShareMode     string   `json:"share_mode"`
	ExitIP        string   `json:"exit_ip,omitempty"`
	Groups        []Group  `json:"groups"`
	Selected      []string `json:"selected"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
	CheckedAt     int64    `json:"checked_at"`
}

// snapshotSettings returns live settings or defaults when the callback is missing.
func (g *Gateway) snapshotSettings() Settings {
	if g == nil || g.settings == nil {
		return DefaultSettings()
	}
	return g.settings()
}

// Mode reports the current degradation level of the gateway.
func (g *Gateway) Mode() string {
	if g == nil {
		return GatewayModeDisabled
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// ProxyURL returns the forwarding address used by browsers and API checks.
func (g *Gateway) ProxyURL() string {
	set := g.snapshotSettings()
	if strings.TrimSpace(set.Gateway.ProxyURL) == "" {
		return DefaultSettings().Gateway.ProxyURL
	}
	return strings.TrimSpace(set.Gateway.ProxyURL)
}

// setTransport points the shared client at the current forwarding plane.
func (g *Gateway) applyProxy(proxyURL string) error {
	client, errClient := gatewayHTTPClient(proxyURL)
	if errClient != nil {
		return errClient
	}
	g.mu.Lock()
	previous := g.proxyClient
	g.proxyClient = client
	g.mu.Unlock()
	if previous != nil {
		previous.CloseIdleConnections()
	}
	return nil
}

func gatewayHTTPClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext, MaxIdleConnsPerHost: 8}
	if proxyURL != "" {
		parsed, errParse := url.Parse(proxyURL)
		if errParse != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("registerbot: invalid HTTP proxy URL")
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport}, nil
}

// EffectiveProxyURL is empty only when the forwarding plane is unavailable.
func (g *Gateway) EffectiveProxyURL() string {
	if g.Mode() == GatewayModeDisabled {
		return ""
	}
	return g.ProxyURL()
}

// controllerClient builds a client that never chains through the proxy itself.
func (g *Gateway) controllerClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 2,
		},
	}
}

func (g *Gateway) controllerSecret() string {
	return strings.TrimSpace(g.snapshotSettings().Gateway.Secret)
}

// controllerGet issues a GET on the control plane.
func (g *Gateway) controllerGet(ctx context.Context, baseURL, path string, out any) error {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if errNew != nil {
		return errNew
	}
	if secret := g.controllerSecret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, errDo := g.controllerClient().Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			// Body close failures are not actionable here.
			_ = errClose
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registerbot: controller %s -> HTTP %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(out)
}

// candidateControllers derives control-plane addresses from the forwarding plane.
func candidateControllers(proxyURL string, explicit string) []string {
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		for _, existing := range out {
			if strings.EqualFold(existing, raw) {
				return
			}
		}
		out = append(out, raw)
	}
	add(explicit)
	if strings.TrimSpace(explicit) != "" {
		// An explicit controller is an isolation boundary. Never fall back to an
		// adjacent controller that may own the operator's system proxy.
		return out
	}
	parsed, errParse := url.Parse(strings.TrimSpace(proxyURL))
	if errParse != nil || parsed.Host == "" {
		return out
	}
	host := parsed.Hostname()
	port, errPort := net.LookupPort("tcp", parsed.Port())
	if errPort != nil {
		return out
	}
	// SakuraCat style launchers keep the controller next to the mixed port.
	for _, delta := range []int{-10, -2, -1, 1, 2, 10} {
		candidate := port + delta
		if candidate <= 0 || candidate > 65535 {
			continue
		}
		add(net.JoinHostPort(host, fmt.Sprint(candidate)))
	}
	for _, known := range []int{9090, 9097, 8888, 8080} {
		add(net.JoinHostPort(host, fmt.Sprint(known)))
	}
	return out
}

// discoverController probes candidates and caches the first live control plane.
func (g *Gateway) discoverController(ctx context.Context, force bool) (string, error) {
	g.mu.Lock()
	cached, cachedAt := g.controllerURL, g.controllerAt
	g.mu.Unlock()
	set := g.snapshotSettings()
	if !force && cached != "" && time.Since(cachedAt) < time.Minute {
		return cached, nil
	}
	var lastErr error
	for _, candidate := range candidateControllers(g.ProxyURL(), set.Gateway.ControllerURL) {
		var version struct {
			Version string `json:"version"`
		}
		probeCtx, cancel := context.WithTimeout(ctx, set.Gateway.ProbeTimeout())
		errProbe := g.controllerGet(probeCtx, candidate, "/version", &version)
		cancel()
		if errProbe != nil {
			lastErr = errProbe
			continue
		}
		if strings.TrimSpace(version.Version) == "" {
			lastErr = fmt.Errorf("registerbot: controller %s reported no version", candidate)
			continue
		}
		g.mu.Lock()
		g.controllerURL = candidate
		g.controllerAt = time.Now()
		g.mu.Unlock()
		return candidate, nil
	}
	if lastErr == nil {
		lastErr = errors.New("registerbot: no controller candidate configured")
	}
	return "", lastErr
}

// Probe refreshes the gateway view and picks the highest usable mode.
func (g *Gateway) Probe(ctx context.Context, force bool) Status {
	set := g.snapshotSettings()
	status := Status{ProxyURL: g.ProxyURL(), ShareMode: set.Gateway.ShareMode, Mode: GatewayModeDisabled, CheckedAt: time.Now().UnixMilli(), Groups: []Group{}, Selected: []string{}}
	forwarding := g.pingProxy(status.ProxyURL) == nil
	if forwarding {
		status.Mode = GatewayModeProxyOnly
	} else {
		status.Warnings = append(status.Warnings, "forwarding plane unreachable; direct browser mode")
	}
	if ctrl, errDiscover := g.discoverController(ctx, force); errDiscover == nil {
		status.ControllerURL = ctrl
		if groups, errList := g.listGroups(ctx, ctrl); errList == nil {
			status.Groups = groups
			if forwarding {
				status.Mode = GatewayModeController
			}
			g.mu.Lock()
			g.groups = make(map[string]Group, len(groups))
			for _, group := range groups {
				g.groups[group.Name] = group
				if group.Now != "" {
					status.Selected = append(status.Selected, group.Name+"="+group.Now)
				}
			}
			g.mu.Unlock()
		} else {
			status.Error = "controller proxy list unavailable"
		}
	} else {
		status.Error = "controller unavailable"
	}
	if status.Mode != GatewayModeController {
		status.Warnings = append(status.Warnings, "node isolation unavailable; batch continues with degraded routing")
	}
	if set.Gateway.VerifyExitIP && forwarding {
		if ip, errIP := g.ExitIP(ctx); errIP == nil {
			status.ExitIP = ip
		} else {
			status.Warnings = append(status.Warnings, "exit address could not be verified")
		}
	}
	g.mu.Lock()
	g.mode = status.Mode
	g.lastError = status.Error
	g.checkedAt = time.UnixMilli(status.CheckedAt)
	g.lastStatus = status
	g.mu.Unlock()
	return status
}

// Snapshot returns the last measured gateway state without network probes.
func (g *Gateway) Snapshot() Status {
	set := g.snapshotSettings()
	g.mu.Lock()
	defer g.mu.Unlock()
	status := g.lastStatus
	status.Mode = g.mode
	status.ControllerURL = g.controllerURL
	status.Groups = make([]Group, 0, len(g.groups))
	for _, group := range g.groups {
		group.Nodes = append([]string(nil), group.Nodes...)
		status.Groups = append(status.Groups, group)
	}
	sortGroups(status.Groups)
	status.Warnings = append([]string(nil), status.Warnings...)
	status.Selected = append([]string(nil), status.Selected...)
	if status.ShareMode == "" {
		status.ShareMode = set.Gateway.ShareMode
	}
	if status.ProxyURL == "" {
		status.ProxyURL = set.Gateway.ProxyURL
	}
	return status
}

type proxiesDocument struct {
	Proxies map[string]struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Now     string   `json:"now"`
		All     []string `json:"all"`
		History []struct {
			Delay int    `json:"delay"`
			Time  string `json:"time"`
		} `json:"history"`
	} `json:"proxies"`
}

// listGroups reads selectable groups, ordered as the controller reports them.
func (g *Gateway) listGroups(ctx context.Context, baseURL string) ([]Group, error) {
	var doc proxiesDocument
	if errGet := g.controllerGet(ctx, baseURL, "/proxies", &doc); errGet != nil {
		return nil, errGet
	}
	set := g.snapshotSettings()
	allowed := func(name string) bool {
		if len(set.Gateway.Groups) == 0 {
			return true
		}
		for _, want := range set.Gateway.Groups {
			if strings.EqualFold(strings.TrimSpace(want), name) {
				return true
			}
		}
		return false
	}
	var out []Group
	for name, item := range doc.Proxies {
		if !allowed(name) {
			continue
		}
		switch item.Type {
		case "Selector", "selector":
		default:
			continue
		}
		nodes := make([]string, 0, len(item.All))
		for _, node := range item.All {
			leaf := doc.Proxies[node]
			switch strings.ToLower(leaf.Type) {
			case "selector", "urltest", "fallback", "direct", "reject":
				continue
			}
			if strings.EqualFold(node, "DIRECT") || strings.EqualFold(node, "REJECT") {
				continue
			}
			nodes = append(nodes, node)
		}
		grp := Group{Name: name, Type: item.Type, Now: item.Now, Nodes: nodes}
		if grp.Now == "" {
			grp.Now = item.Now
		}
		out = append(out, grp)
	}
	// Stable order keeps the panel list from reshuffling between polls.
	sortGroups(out)
	return out, nil
}

func sortGroups(groups []Group) {
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0 && groups[j].Name < groups[j-1].Name; j-- {
			groups[j], groups[j-1] = groups[j-1], groups[j]
		}
	}
}

// Groups returns the last probed group table without touching the network.
func (g *Gateway) Groups() []Group {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Group, 0, len(g.groups))
	for _, grp := range g.groups {
		out = append(out, grp)
	}
	sortGroups(out)
	return out
}

// pinNode switches one group to a node on the control plane.
func (g *Gateway) pinNode(ctx context.Context, baseURL, group, node string) error {
	payload, errMarshal := json.Marshal(map[string]string{"name": node})
	if errMarshal != nil {
		return fmt.Errorf("registerbot: encode pin payload: %w", errMarshal)
	}
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/proxies/"+url.PathEscape(group), strings.NewReader(string(payload)))
	if errNew != nil {
		return errNew
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := g.controllerSecret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, errDo := g.controllerClient().Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			_ = errClose
		}
	}()
	// Clash replies 204 on success; sing-box may reply 200 with an empty body.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registerbot: pin %s -> %s failed (HTTP %d)", group, node, resp.StatusCode)
	}
	g.mu.Lock()
	g.pinState[group] = node
	g.mu.Unlock()
	return nil
}

// PinResult describes one completed node pin.
type PinResult struct {
	Group     string `json:"group"`
	Node      string `json:"node"`
	ExitIP    string `json:"exit_ip,omitempty"`
	Mode      string `json:"mode"`
	Changed   bool   `json:"changed"`
	Verified  bool   `json:"verified"`
	Degraded  string `json:"degraded,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// Pin makes group show node, then optionally confirms the observable exit changed.
//
// Node acquisition happens inside the credential acquisition window, so the pin
// is bounded by Gateway.PinTimeout.
func (g *Gateway) Pin(ctx context.Context, group, node string) (PinResult, error) {
	started := time.Now()
	set := g.snapshotSettings()
	result := PinResult{Group: group, Node: node, Mode: g.Mode()}
	if strings.TrimSpace(group) == "" || strings.TrimSpace(node) == "" {
		return result, errors.New("registerbot: group and node are required")
	}
	if result.Mode != GatewayModeController {
		result.Node = ""
		result.Degraded = "control plane unavailable; pin skipped"
		return result, nil
	}
	g.mu.Lock()
	previous := g.pinState[group]
	known := g.groups[group]
	g.mu.Unlock()
	if !containsNode(known.Nodes, node) {
		return result, errors.New("registerbot: node does not belong to the selected group")
	}
	pinCtx, cancel := context.WithTimeout(ctx, set.Gateway.PinTimeout())
	defer cancel()
	baseURL, errDiscover := g.discoverController(pinCtx, false)
	if errDiscover != nil {
		result.Mode = GatewayModeProxyOnly
		result.Node = ""
		result.Degraded = "controller lost; continuing with proxy"
		return result, nil
	}
	if errPin := g.pinNode(pinCtx, baseURL, group, node); errPin != nil {
		result.Mode = GatewayModeProxyOnly
		result.Node = ""
		result.Degraded = "node pin rejected; continuing with proxy"
		result.ElapsedMS = time.Since(started).Milliseconds()
		return result, nil
	}
	result.Changed = previous != node
	groups, errList := g.listGroups(pinCtx, baseURL)
	if errList == nil {
		for _, selected := range groups {
			if selected.Name == group {
				result.Verified = selected.Now == node
			}
		}
	}
	if !result.Verified {
		result.Degraded = "controller did not confirm the selected node"
	}
	if set.Gateway.VerifyExitIP {
		if verified, valid := normalizeExitIP(set.Gateway.VerifiedExits[node]); valid {
			result.ExitIP = verified
		} else if ip, errIP := g.ExitIP(pinCtx); errIP == nil {
			result.ExitIP = ip
		} else {
			result.Degraded = "node selected; exit address unverified"
		}
	}
	result.ElapsedMS = time.Since(started).Milliseconds()
	return result, nil
}

// NodePool resolves the nodes a channel may rotate over.
func (g *Gateway) NodePool(channel ChannelSettings) []string {
	g.mu.Lock()
	grp, ok := g.groups[channel.Group]
	g.mu.Unlock()
	pool := append([]string(nil), channel.Nodes...)
	if len(pool) == 0 && ok {
		pool = append(pool, grp.Nodes...)
	}
	if len(pool) == 0 {
		return nil
	}
	var out []string
	for _, node := range pool {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}
		if ok && !containsNode(grp.Nodes, node) {
			continue
		}
		out = append(out, node)
	}
	return out
}

func containsNode(nodes []string, want string) bool {
	for _, node := range nodes {
		if node == want {
			return true
		}
	}
	return false
}

// Acquire shares an already confirmed fixed route across concurrent accounts.
// Route changes remain exclusive: a single forwarding port cannot use two
// selected nodes simultaneously, regardless of the configured share-mode label.
func (g *Gateway) Acquire(ctx context.Context, channel ChannelSettings, holder string) (PinResult, error) {
	set := g.snapshotSettings()
	if channel.ManualMode || !set.Enabled {
		return PinResult{Mode: g.Mode(), Degraded: "gateway skipped by channel settings"}, nil
	}
	key := channel.Group + "|" + holder
	var mode, route string
	var pool []string
	for {
		if errContext := ctx.Err(); errContext != nil {
			return PinResult{}, errContext
		}
		mode, pool = g.Mode(), g.NodePool(channel)
		route = ""
		if mode != GatewayModeController {
			route = "forwarding:" + g.ProxyURL()
		} else if !channel.RotateNode {
			node := "<current>"
			if len(pool) > 0 {
				node = pool[0]
			}
			route = channel.Group + "\x00" + node
		}
		g.mu.Lock()
		if g.mode != mode {
			g.mu.Unlock()
			continue
		}
		if len(g.leases) == 0 {
			g.leases[key] = lease{channel: channel.ID, at: time.Now()}
			g.leaseRoute, g.leaseReady = route, false
			g.mu.Unlock()
			break
		}
		if route != "" && route == g.leaseRoute && g.leaseReady {
			g.leases[key] = lease{channel: channel.ID, at: time.Now()}
			result := g.leaseResult
			g.mu.Unlock()
			return result, nil
		}
		changed := g.leaseChanged
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return PinResult{}, ctx.Err()
		case <-changed:
		}
	}
	finish := func(result PinResult, errPin error) (PinResult, error) {
		if errPin != nil {
			g.Release(channel.Group, holder)
			return result, errPin
		}
		g.mu.Lock()
		g.leaseResult, g.leaseReady = result, true
		close(g.leaseChanged)
		g.leaseChanged = make(chan struct{})
		g.mu.Unlock()
		return result, nil
	}
	if mode != GatewayModeController {
		return finish(PinResult{Mode: mode, Degraded: "controller unavailable; shared forwarding retained"}, nil)
	}
	if len(pool) == 0 {
		return finish(PinResult{Group: channel.Group, Mode: mode, Degraded: "no selectable nodes; current proxy route retained"}, nil)
	}
	g.mu.Lock()
	index := 0
	if channel.RotateNode {
		index = g.rotation[channel.ID]
	}
	chosen := pool[index%len(pool)]
	if channel.RotateNode {
		g.rotation[channel.ID] = index + 1
	}
	g.mu.Unlock()
	result, errPin := g.Pin(ctx, channel.Group, chosen)
	return finish(result, errPin)
}

// Release keeps the selected exit stable until every operation of the task ends.
func (g *Gateway) Release(group, holder string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := group + "|" + holder
	if _, exists := g.leases[key]; exists {
		delete(g.leases, key)
		if len(g.leases) == 0 {
			g.leaseReady = false
			g.leaseRoute = ""
		}
		close(g.leaseChanged)
		g.leaseChanged = make(chan struct{})
	}
}

// exitIPDirect reads the local exit address without the forwarding plane.
func (g *Gateway) exitIPDirect(ctx context.Context) (string, error) {
	direct := &http.Client{
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext,
		},
	}
	return g.readExitIP(ctx, direct)
}

// ExitIP reads the exit address the forwarding plane currently presents.
func (g *Gateway) ExitIP(ctx context.Context) (string, error) {
	set := g.snapshotSettings()
	client, errClient := gatewayHTTPClient(g.ProxyURL())
	if errClient != nil {
		return "", errClient
	}
	defer client.CloseIdleConnections()
	probeCtx, cancel := context.WithTimeout(ctx, set.Gateway.ProbeTimeout()+5*time.Second)
	defer cancel()
	return g.readExitIP(probeCtx, client)
}

// pingProxy confirms a TCP session to the forwarding plane.
func (g *Gateway) pingProxy(proxyURL string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(proxyURL))
	if errParse != nil {
		return errParse
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	conn, errDial := net.DialTimeout("tcp", net.JoinHostPort(parsed.Hostname(), port), 3*time.Second)
	if errDial != nil {
		return errDial
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			_ = errClose
		}
	}()
	return nil
}

func (g *Gateway) readExitIP(ctx context.Context, client *http.Client) (string, error) {
	set := g.snapshotSettings()
	target := strings.TrimSpace(set.Gateway.ExitIPURL)
	if target == "" {
		return "", errors.New("registerbot: exit ip probe url not configured")
	}
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("User-Agent", "cpa-registerbot/1.0")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			_ = errClose
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registerbot: exit ip probe HTTP %d", resp.StatusCode)
	}
	var body strings.Builder
	buf := make([]byte, 512)
	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			_, _ = body.Write(buf[:n])
		}
		if errRead != nil {
			break
		}
		if body.Len() > 4096 {
			break
		}
	}
	raw := strings.TrimSpace(body.String())
	if raw == "" {
		return "", errors.New("registerbot: exit ip probe returned no body")
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal([]byte(raw), &doc); errUnmarshal == nil {
		for _, field := range []string{"ip", "query", "Ip", "IP", "origin"} {
			if value, ok := doc[field]; ok {
				if text, okString := value.(string); okString && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text), nil
				}
			}
		}
	}
	// Plain text services such as ifconfig.me return a bare address.
	if fields := strings.Fields(raw); len(fields) == 1 {
		return fields[0], nil
	}
	return "", errors.New("registerbot: exit ip probe body not recognised")
}

// HealthReport is the machine-readable gateway health for the panel header.
type HealthReport struct {
	OK      bool   `json:"ok"`
	Level   string `json:"level"`
	Message string `json:"message,omitempty"`
}

// Health summarises the gateway state for degradation banners.
func (g *Gateway) Health() HealthReport {
	g.mu.Lock()
	mode, errText := g.mode, g.lastError
	g.mu.Unlock()
	switch mode {
	case GatewayModeController:
		return HealthReport{OK: true, Level: "full"}
	case GatewayModeProxyOnly:
		return HealthReport{OK: true, Level: "degraded", Message: "node pin disabled: " + errText}
	default:
		message := errText
		if message == "" {
			message = "gateway unavailable"
		}
		return HealthReport{OK: true, Level: "fallback", Message: message}
	}
}
