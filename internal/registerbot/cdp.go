package registerbot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/gorilla/websocket"
)

// cdpClient is a minimal Chrome DevTools Protocol client.
//
// One websocket session drives the whole browser: flattened target sessions carry
// a sessionId, so a single connection per pooled instance is enough. Keeping the
// surface small means the registerbot adds no dependency beyond the websocket
// client CPA already ships, and every CDP failure returns an error the engine can
// degrade on instead of panicking.
type cdpClient struct {
	conn      *websocket.Conn
	endpoint  string
	mu        sync.Mutex
	writeMu   sync.Mutex
	nextID    int
	pending   map[int]chan cdpMessage
	closeOnce sync.Once
	closed    chan struct{}
	err       error
	errMu     sync.Mutex

	handlersMu      sync.Mutex
	handlers        map[string][]cdpHandler
	nextSubscribeID int
}

type cdpHandler struct {
	// sessionID empty matches any session of a flattened target browser.
	sessionID string
	// token lets unsubscribe remove exactly this registration.
	token string
	call  func(cdpMessage)
}

type cdpMessage struct {
	ID        int            `json:"id"`
	Method    string         `json:"method"`
	Params    map[string]any `json:"params"`
	Result    map[string]any `json:"result"`
	SessionID string         `json:"sessionId"`
	Error     *cdpErrorBody  `json:"error"`
}

type cdpErrorBody struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// cdpError reports a protocol level failure.
type cdpError struct {
	Code    int
	Message string
}

func (e cdpError) Error() string {
	return fmt.Sprintf("cdp command failed (%d)", e.Code)
}

// dialCDP connects to a DevTools websocket endpoint.
func dialCDP(ctx context.Context, wsURL string) (*cdpClient, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, errDial := dialer.DialContext(ctx, wsURL, nil)
	if errDial != nil {
		return nil, fmt.Errorf("registerbot: cdp dial %s: %w", wsURL, errDial)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	client := &cdpClient{
		conn:     conn,
		endpoint: wsURL,
		nextID:   1,
		pending:  make(map[int]chan cdpMessage),
		closed:   make(chan struct{}),
		handlers: make(map[string][]cdpHandler),
	}
	go client.readLoop()
	return client, nil
}

func (c *cdpClient) setErr(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

func (c *cdpClient) loadErr() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// readLoop dispatches command responses and target events.
func (c *cdpClient) readLoop() {
	defer c.closePending(errors.New("cdp connection closed"))
	for {
		_, payload, errRead := c.conn.ReadMessage()
		if errRead != nil {
			c.setErr(errRead)
			return
		}
		var msg cdpMessage
		if errDecode := json.Unmarshal(payload, &msg); errDecode != nil {
			c.setErr(errors.New("invalid CDP message"))
			return
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			if ok {
				delete(c.pending, msg.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}
		if msg.Method == "" {
			continue
		}
		c.handlersMu.Lock()
		matched := make([]func(cdpMessage), 0, len(c.handlers[msg.Method]))
		for _, handler := range c.handlers[msg.Method] {
			if handler.sessionID == "" || handler.sessionID == msg.SessionID {
				matched = append(matched, handler.call)
			}
		}
		c.handlersMu.Unlock()
		for _, call := range matched {
			call(msg)
		}
	}
}

func (c *cdpClient) closePending(reason error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int]chan cdpMessage)
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- cdpMessage{Error: &cdpErrorBody{Message: reason.Error(), Code: -1}}:
		default:
		}
	}
	c.closeOnce.Do(func() { close(c.closed) })
}

// subscribe registers a handler for one event method of one session.
func (c *cdpClient) subscribe(method, sessionID string, call func(cdpMessage)) func() {
	c.handlersMu.Lock()
	token := fmt.Sprintf("%s#%d", method, c.nextSubscribeID)
	c.nextSubscribeID++
	c.handlers[method] = append(c.handlers[method], cdpHandler{sessionID: sessionID, token: token, call: call})
	c.handlersMu.Unlock()
	return func() {
		c.handlersMu.Lock()
		defer c.handlersMu.Unlock()
		kept := make([]cdpHandler, 0, len(c.handlers[method]))
		for _, existing := range c.handlers[method] {
			if existing.token != token {
				kept = append(kept, existing)
			}
		}
		c.handlers[method] = kept
	}
}

// Call sends one command with no session and waits for its response.
func (c *cdpClient) Call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	return c.roundTrip(ctx, method, params, "")
}

// roundTrip performs one command, tagged with sessionId when provided.
func (c *cdpClient) roundTrip(ctx context.Context, method string, params map[string]any, sessionID string) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	c.mu.Lock()
	if errLoad := c.loadErr(); errLoad != nil {
		c.mu.Unlock()
		return nil, errLoad
	}
	id := c.nextID
	c.nextID++
	ch := make(chan cdpMessage, 1)
	c.pending[id] = ch
	envelope := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		envelope["sessionId"] = sessionID
	}
	payload, errMarshal := json.Marshal(envelope)
	c.mu.Unlock()
	if errMarshal != nil {
		c.dropPending(id)
		return nil, fmt.Errorf("registerbot: encode cdp call: %w", errMarshal)
	}
	c.writeMu.Lock()
	errWrite := c.conn.WriteMessage(websocket.TextMessage, payload)
	c.writeMu.Unlock()
	if errWrite != nil {
		c.dropPending(id)
		return nil, fmt.Errorf("registerbot: cdp write: %w", errWrite)
	}
	select {
	case <-ctx.Done():
		c.dropPending(id)
		return nil, c.wrapContextErr(ctx.Err())
	case <-c.closed:
		c.dropPending(id)
		return nil, c.loadErr()
	case msg := <-ch:
		if msg.Error != nil {
			return nil, cdpError{Code: msg.Error.Code, Message: msg.Error.Message}
		}
		return msg.Result, nil
	}
}

func (c *cdpClient) dropPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// wrapContextErr keeps a cancelled run distinguishable from a dead socket.
func (c *cdpClient) wrapContextErr(err error) error {
	if err == nil {
		return nil
	}
	if live := c.loadErr(); live != nil {
		return live
	}
	return err
}

// Close ends the session.
func (c *cdpClient) Close() error {
	if c == nil {
		return nil
	}
	errClose := c.conn.Close()
	c.setErr(errors.New("cdp session closed"))
	c.closePending(errors.New("cdp session closed"))
	return errClose
}

// Session drives one page target inside a pooled browser instance.
type Session struct {
	client    *cdpClient
	sessionID string
	targetID  string
	openerID  string
	contextID string
	instance  *browserInstance
	closeOnce sync.Once
}

// call runs a command inside the flattened target session.
func (s *Session) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("registerbot: session detached")
	}
	return s.client.roundTrip(ctx, method, params, s.sessionID)
}

// cdpTransport resolves the HTTP endpoints of a DevTools listener.
type cdpTransport struct {
	base   string
	client *http.Client
}

func newCDPTransport(hostPort string) *cdpTransport {
	if strings.TrimSpace(hostPort) == "" {
		hostPort = "127.0.0.1:9333"
	}
	if strings.Contains(hostPort, "://") {
		hostPort = strings.TrimPrefix(strings.TrimPrefix(hostPort, "http://"), "https://")
	}
	return &cdpTransport{
		base: "http://" + hostPort,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 4,
			},
		},
	}
}

func (t *cdpTransport) getJSON(ctx context.Context, path string, out any) error {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, t.base+path, nil)
	if errNew != nil {
		return errNew
	}
	resp, errDo := t.client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			_ = errClose
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registerbot: devtools %s -> HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// versionReady confirms a DevTools listener answered and reports the build.
func (t *cdpTransport) versionReady(ctx context.Context) (string, error) {
	var doc struct {
		Browser         string `json:"Browser"`
		ProtocolVersion string `json:"Protocol-Version"`
	}
	if errGet := t.getJSON(ctx, "/json/version", &doc); errGet != nil {
		return "", errGet
	}
	if strings.TrimSpace(doc.ProtocolVersion) == "" {
		return "", errors.New("registerbot: devtools endpoint answered without a protocol version")
	}
	return doc.Browser, nil
}

// connectBrowser opens the browser level session used for target management.
func (t *cdpTransport) connectBrowser(ctx context.Context) (*cdpClient, error) {
	var doc struct {
		WebsocketURL string `json:"webSocketDebuggerUrl"`
	}
	if errGet := t.getJSON(ctx, "/json/version", &doc); errGet != nil {
		return nil, errGet
	}
	if strings.TrimSpace(doc.WebsocketURL) == "" {
		return nil, errors.New("registerbot: devtools endpoint has no browser websocket url")
	}
	return dialCDP(ctx, doc.WebsocketURL)
}

// OpenPage creates a fresh tab and returns a usable page session.
func (b *browserInstance) OpenPage(ctx context.Context, initialURL string) (*Session, error) {
	if b == nil || b.control == nil {
		return nil, errors.New("registerbot: browser instance unavailable")
	}
	b.mu.Lock()
	proxyURL := b.proxyURL
	b.mu.Unlock()
	if proxyURL == "" {
		proxyURL = "direct://"
	}
	contextResult, errContext := b.control.Call(ctx, "Target.createBrowserContext", map[string]any{"disposeOnDetach": true, "proxyServer": proxyURL})
	if errContext != nil {
		return nil, fmt.Errorf("registerbot: isolated context: %w", errContext)
	}
	contextID, _ := contextResult["browserContextId"].(string)
	if contextID == "" {
		return nil, errors.New("registerbot: no isolated context id")
	}
	cleanup := func() {
		_, _ = b.control.Call(ctx, "Target.disposeBrowserContext", map[string]any{"browserContextId": contextID})
	}
	result, errCall := b.control.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": contextID})
	if errCall != nil {
		cleanup()
		return nil, fmt.Errorf("registerbot: create target: %w", errCall)
	}
	targetID, _ := result["targetId"].(string)
	if strings.TrimSpace(targetID) == "" {
		cleanup()
		return nil, errors.New("registerbot: create target returned no target id")
	}
	attached, errAttach := b.control.Call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
	if errAttach != nil {
		cleanup()
		return nil, fmt.Errorf("registerbot: attach target: %w", errAttach)
	}
	sessionID, _ := attached["sessionId"].(string)
	session := &Session{client: b.control, sessionID: sessionID, targetID: targetID, contextID: contextID, instance: b}
	b.mu.Lock()
	b.contexts++
	b.mu.Unlock()
	for _, method := range []string{"Page.enable", "Runtime.enable", "Log.enable"} {
		if _, errEnable := session.call(ctx, method, nil); errEnable != nil {
			// A missing domain only narrows observation, never the run itself.
			_ = errEnable
		}
	}
	if _, errScript := session.call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": helperScript}); errScript != nil {
		_ = session.CloseTarget(ctx)
		return nil, errScript
	}
	if errInstall := session.InstallHelpers(ctx); errInstall != nil {
		_ = session.CloseTarget(ctx)
		return nil, errInstall
	}
	if errNavigate := session.Navigate(ctx, initialURL); errNavigate != nil {
		_ = session.CloseTarget(ctx)
		return nil, errNavigate
	}
	return session, nil
}

// CloseTarget drops the tab and its session.
func (s *Session) CloseTarget(ctx context.Context) error {
	if s == nil || s.client == nil || strings.TrimSpace(s.targetID) == "" {
		return nil
	}
	var errClose error
	s.closeOnce.Do(func() {
		if s.contextID != "" {
			_, errClose = s.client.Call(ctx, "Target.disposeBrowserContext", map[string]any{"browserContextId": s.contextID})
		} else {
			_, errClose = s.client.Call(ctx, "Target.closeTarget", map[string]any{"targetId": s.targetID})
		}
		if s.instance != nil {
			s.instance.mu.Lock()
			s.instance.contexts--
			s.instance.mu.Unlock()
		}
	})
	return errClose
}

// FollowAuthorizationPopup follows only a provider/Google popup opened by the
// current page inside this account's own browser context.
func (s *Session) FollowAuthorizationPopup(ctx context.Context) error {
	result, errTargets := s.client.Call(ctx, "Target.getTargets", nil)
	if errTargets != nil {
		return errTargets
	}
	infos, _ := result["targetInfos"].([]any)
	currentExists := false
	for _, raw := range infos {
		if info, ok := raw.(map[string]any); ok && info["targetId"] == s.targetID {
			currentExists = true
		}
	}
	for _, raw := range infos {
		info, ok := raw.(map[string]any)
		if !ok || info["type"] != "page" || info["browserContextId"] != s.contextID {
			continue
		}
		isChild := info["openerId"] == s.targetID
		isReturning := !currentExists && s.openerID != "" && info["targetId"] == s.openerID
		if !isChild && !isReturning {
			continue
		}
		address, _ := info["url"].(string)
		if !googleIdentityHost(address) && !freeAIHost(address) {
			continue
		}
		targetID, _ := info["targetId"].(string)
		attached, errAttach := s.client.Call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
		if errAttach != nil {
			return errAttach
		}
		s.sessionID, _ = attached["sessionId"].(string)
		if isChild {
			s.openerID = s.targetID
		} else {
			s.openerID = ""
		}
		s.targetID = targetID
		_, _ = s.call(ctx, "Page.enable", nil)
		_, _ = s.call(ctx, "Runtime.enable", nil)
		_, _ = s.call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": helperScript})
		return s.InstallHelpers(ctx)
	}
	return nil
}

// Navigate loads a URL and waits for the load event.
func (s *Session) Navigate(ctx context.Context, targetURL string) error {
	result, errCall := s.call(ctx, "Page.navigate", map[string]any{"url": normalizeURL(targetURL)})
	if errCall != nil {
		return fmt.Errorf("registerbot: navigate: %w", errCall)
	}
	if failure, _ := result["errorText"].(string); failure != "" {
		return navigationFailure(failure)
	}
	return s.WaitForLoad(ctx)
}

// NavigationError contains only a stable browser error code, never a URL or page
// content. The queue retries it rather than misclassifying a network failure as
// an identity challenge requiring human intervention.
type NavigationError struct{ Code string }

func (e *NavigationError) Error() string { return "registerbot: navigation failed (" + e.Code + ")" }

func navigationFailure(raw string) *NavigationError {
	code := strings.TrimPrefix(strings.TrimSpace(raw), "net::")
	switch code {
	case "ERR_CONNECTION_RESET", "ERR_CONNECTION_CLOSED", "ERR_CONNECTION_TIMED_OUT", "ERR_NAME_NOT_RESOLVED", "ERR_PROXY_CONNECTION_FAILED", "ERR_TUNNEL_CONNECTION_FAILED", "ERR_SSL_PROTOCOL_ERROR", "ERR_INTERNET_DISCONNECTED", "ERR_CONNECTION_REFUSED", "ERR_ABORTED", "ERR_NETWORK_CHANGED", "ERR_EMPTY_RESPONSE", "ERR_TIMED_OUT", "ERR_ADDRESS_UNREACHABLE", "ERR_CERT_AUTHORITY_INVALID", "ERR_CERT_DATE_INVALID":
		return &NavigationError{Code: code}
	default:
		return &NavigationError{Code: "ERR_FAILED"}
	}
}

// WaitForLoad waits for Page.loadEventFired, or for readyState when it already passed.
func (s *Session) WaitForLoad(ctx context.Context) error {
	if ready, errReady := s.readyState(ctx); errReady == nil && ready == "complete" {
		return nil
	}
	done := make(chan struct{}, 1)
	stop := s.client.subscribe("Page.loadEventFired", s.sessionID, func(cdpMessage) {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	defer stop()
	for {
		if ready, errReady := s.readyState(ctx); errReady == nil && ready == "complete" {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (s *Session) readyState(ctx context.Context) (string, error) {
	value, errEval := s.Evaluate(ctx, "document.readyState", false)
	if errEval != nil {
		return "", errEval
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	return "", nil
}

// CurrentURL reads location.href.
func (s *Session) CurrentURL(ctx context.Context) string {
	value, errEval := s.Evaluate(ctx, "location.href", false)
	if errEval != nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// PageTitle reads document.title.
func (s *Session) PageTitle(ctx context.Context) string {
	value, errEval := s.Evaluate(ctx, "document.title", false)
	if errEval != nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// Evaluate runs an expression and decodes a primitive result.
func (s *Session) Evaluate(ctx context.Context, expression string, awaitPromise bool) (any, error) {
	result, errCall := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
		"userGesture":   true,
	})
	if errCall != nil {
		return nil, errCall
	}
	if exception, ok := result["exceptionDetails"].(map[string]any); ok && exception != nil {
		text, _ := exception["text"].(string)
		if text == "" {
			if desc, okDesc := exception["description"].(string); okDesc {
				text = desc
			}
		}
		return nil, errors.New("registerbot: page script evaluation failed")
	}
	inner, _ := result["result"].(map[string]any)
	if inner == nil {
		return nil, nil
	}
	if value, ok := inner["value"]; ok {
		return value, nil
	}
	return nil, nil
}

// EvaluateString runs an expression and requires a string result.
func (s *Session) EvaluateString(ctx context.Context, expression string, awaitPromise bool) (string, error) {
	value, errEval := s.Evaluate(ctx, expression, awaitPromise)
	if errEval != nil {
		return "", errEval
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	return "", nil
}

// waitFor polls an expression until it is truthy or the context expires.
func (s *Session) waitFor(ctx context.Context, expression string, interval time.Duration) error {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if value, errEval := s.Evaluate(ctx, expression, false); errEval == nil && truthy(value) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("registerbot: page condition timed out: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed != "" && typed != "false" && typed != "null"
	case float64:
		return typed != 0
	case nil:
		return false
	default:
		return true
	}
}

// WaitForSelector polls until a CSS selector resolves to a visible element.
func (s *Session) WaitForSelector(ctx context.Context, selector string) error {
	return s.waitFor(ctx, fmt.Sprintf("cpaVisible(%s)", jsString(selector)), 200*time.Millisecond)
}

// WaitForText polls until a visible control carries the given label.
func (s *Session) WaitForText(ctx context.Context, text string) error {
	return s.waitFor(ctx, fmt.Sprintf("cpaTextVisible(%s)", jsString(text)), 250*time.Millisecond)
}

// InstallHelpers defines the DOM helpers used by every interaction step.
func (s *Session) InstallHelpers(ctx context.Context) error {
	if _, errEval := s.Evaluate(ctx, helperScript, false); errEval != nil {
		return fmt.Errorf("registerbot: install page helpers: %w", errEval)
	}
	return nil
}

// helperScript is injected once per target. It locates visible controls, reports
// page state for stall diagnostics, and never redefines itself after navigation
// because the execution context is recreated with the document.
const helperScript = `(function () {
  if (window.__cpaHelpers) { return true; }
  window.__cpaHelpers = true;
  var norm = function (v) { return String(v == null ? '' : v).replace(/\s+/g, ' ').trim().toLowerCase(); };
  var visible = function (el) {
    if (!el) { return false; }
    var r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) { return false; }
    var st = window.getComputedStyle(el);
    if (st.visibility === 'hidden' || st.display === 'none' || parseFloat(st.opacity) < 0.05) { return false; }
    return true;
  };
  var shadowSearch = function (sel) {
    var matches = document.querySelectorAll(sel);
    for (var k = 0; k < matches.length; k++) { if (visible(matches[k])) { return matches[k]; } }
    var hosts = document.querySelectorAll('*');
    for (var i = 0; i < hosts.length; i++) {
      if (!hosts[i].shadowRoot) { continue; }
      var inner = hosts[i].shadowRoot.querySelectorAll(sel);
      for (var j = 0; j < inner.length; j++) { if (visible(inner[j])) { return inner[j]; } }
    }
    return null;
  };
  window.cpaVisible = function (sel) {
    var el = shadowSearch(sel);
    if (!el) { return false; }
    if (visible(el)) { return true; }
    var pool = el.querySelectorAll ? el.querySelectorAll('*') : [];
    for (var i = 0; i < pool.length; i++) { if (visible(pool[i])) { return true; } }
    return false;
  };
  window.cpaTextVisible = function (text) {
    var want = norm(text);
    if (!want) { return false; }
    var nodes = document.querySelectorAll('button, [role="button"], a, input[type=submit], label, [jsaction]');
    for (var i = 0; i < nodes.length; i++) {
      var label = norm(nodes[i].innerText || nodes[i].textContent || nodes[i].value || nodes[i].getAttribute('aria-label') || '');
      if (label && label.indexOf(want) >= 0 && visible(nodes[i])) { return true; }
    }
    return false;
  };
  var lastTarget = null, inputTarget = null;
  var mark = function (el) {
    lastTarget = el;
    var prev = document.querySelector('[data-cpa-target]');
    if (prev) { prev.removeAttribute('data-cpa-target'); }
    el.setAttribute('data-cpa-target', '1');
    var r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2, text: norm(el.innerText || el.getAttribute('aria-label')), id: el.id || '' };
  };
  window.cpaFindButton = function (text) {
    var want = norm(text);
    if (!want) { return null; }
    var nodes = document.querySelectorAll('button, [role="button"], a, input[type=submit], [jsaction]');
    var best = null, bestScore = 0;
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      var label = norm(el.innerText || el.textContent || el.value || el.getAttribute('aria-label') || '');
      if (!label) { continue; }
      var score = 0;
      if (label === want) { score = 3; } else if (label.indexOf(want) === 0) { score = 2; } else if (label.indexOf(want) >= 0) { score = 1; }
      if (score > bestScore && visible(el) && !el.disabled) { bestScore = score; best = el; }
    }
    if (!best) { return null; }
    return mark(best);
  };
  window.cpaFindInput = function (selector, label) {
    var el = selector ? shadowSearch(selector) : null;
    if (el && !visible(el)) { el = null; }
    if (!el && label) {
      var want = norm(label);
      var inputs = document.querySelectorAll('input, textarea');
      for (var j = 0; j < inputs.length; j++) {
        var cand = inputs[j];
        var hint = norm(cand.getAttribute('aria-label') || cand.getAttribute('placeholder') || cand.name || cand.id || '');
        if (hint && hint.indexOf(want) >= 0 && visible(cand)) { el = cand; break; }
      }
    }
    if (!el) { return null; }
    return mark(el);
  };
  var editable = function (el) {
    return !!el && el.isConnected && visible(el) && !el.disabled && !el.readOnly && el.getAttribute('aria-disabled') !== 'true' && /^(INPUT|TEXTAREA)$/.test(el.tagName);
  };
  window.cpaPrepareInput = async function (selector, label) {
    inputTarget = null;
    if (!window.cpaFindInput(selector, label)) { return null; }
    var el = lastTarget;
    if (!editable(el)) { return null; }
    el.scrollIntoView({block: 'center', inline: 'nearest', behavior: 'instant'});
    await new Promise(function (resolve) { setTimeout(resolve, 150); });
    if (!editable(el)) { return null; }
    var r = el.getBoundingClientRect();
    var left = Math.max(0, r.left), right = Math.min(innerWidth, r.right), top = Math.max(0, r.top), bottom = Math.min(innerHeight, r.bottom);
    if (right - left < 2 || bottom - top < 2) { return null; }
    var x = (left + right) / 2, y = (top + bottom) / 2;
    var root = el.getRootNode(), hit = root.elementFromPoint(x, y);
    if (hit !== el && !el.contains(hit)) { return null; }
    inputTarget = el;
    return {x: x, y: y};
  };
  window.cpaInputFocused = function () {
    return editable(inputTarget) && inputTarget.getRootNode().activeElement === inputTarget;
  };
  window.cpaInputMatches = function (expected) {
    return editable(inputTarget) && inputTarget.value === expected;
  };
  window.cpaReadAll = function () {
    var text = document.body ? document.body.innerText : '';
    return String(text) + '\n' + document.documentElement.outerHTML;
  };
  window.cpaState = function () {
    var alerts = [];
    document.querySelectorAll('[role="alert"], .ErOBK, .RLvSzb, [jsname="LgwsAd"]').forEach(function (n) {
      var t = norm(n.innerText || n.textContent);
      if (t) { alerts.push(t); }
    });
    var buttons = [];
    document.querySelectorAll('button, [role="button"], input[type=submit]').forEach(function (n) {
      if (!visible(n)) { return; }
      var t = norm(n.innerText || n.value || n.getAttribute('aria-label'));
      if (t) { buttons.push((n.disabled ? '~' : '') + t); }
    });
    var fields = [];
    document.querySelectorAll('input, textarea').forEach(function (n) {
      if (!visible(n)) { return; }
      fields.push({ id: n.id || '', name: n.name || '', type: n.type || '', label: norm(n.getAttribute('aria-label') || n.getAttribute('placeholder') || '') });
    });
    return {
      url: location.href,
      title: document.title,
      ready: document.readyState,
      busy: !!document.querySelector('[aria-busy="true"]'),
      errors: alerts.slice(0, 4),
      buttons: buttons.slice(0, 20),
      fields: fields.slice(0, 10)
    };
  };
  return true;
})()`

func normalizeURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "about:blank"
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "about:") || strings.HasPrefix(value, "data:") {
		return value
	}
	return "https://" + value
}

// jsString renders a Go string as a single quoted JavaScript literal.
func jsString(value string) string {
	builder := strings.Builder{}
	builder.WriteByte('\'')
	for _, r := range value {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '\'':
			builder.WriteString(`\'`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('\'')
	return builder.String()
}

type clickPoint struct {
	x float64
	y float64
}

func asPoint(value any) (clickPoint, bool) {
	doc, ok := value.(map[string]any)
	if !ok || doc == nil {
		return clickPoint{}, false
	}
	x, okX := doc["x"].(float64)
	y, okY := doc["y"].(float64)
	if !okX || !okY {
		return clickPoint{}, false
	}
	return clickPoint{x: x, y: y}, true
}

// locateText resolves the first visible control matching one of the labels.
func (s *Session) locateText(ctx context.Context, texts []string) (clickPoint, string, error) {
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		value, errEval := s.Evaluate(ctx, fmt.Sprintf("cpaFindButton(%s)", jsString(text)), false)
		if errEval != nil {
			return clickPoint{}, "", errEval
		}
		if point, ok := asPoint(value); ok {
			return point, text, nil
		}
	}
	return clickPoint{}, "", fmt.Errorf("registerbot: no enabled control labelled %q", strings.Join(texts, " / "))
}

// ClickText performs a real mouse click on the first visible enabled control
// whose label matches one of the given texts, in order.
func (s *Session) ClickText(ctx context.Context, texts ...string) error {
	point, _, errLocate := s.locateText(ctx, texts)
	if errLocate != nil {
		return errLocate
	}
	return s.clickAt(ctx, point.x, point.y)
}

// ClickSelector performs a real mouse click on the element matching a selector.
func (s *Session) ClickSelector(ctx context.Context, selector string) error {
	value, errEval := s.Evaluate(ctx, fmt.Sprintf("cpaFindInput(%s, '')", jsString(selector)), false)
	if errEval != nil {
		return errEval
	}
	point, ok := asPoint(value)
	if !ok {
		return fmt.Errorf("registerbot: selector %q matched nothing visible", selector)
	}
	return s.clickAt(ctx, point.x, point.y)
}

// clickAt dispatches a real press and release through the Input domain.
func (s *Session) clickAt(ctx context.Context, x, y float64) error {
	if _, errMove := s.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}); errMove != nil {
		return fmt.Errorf("registerbot: mouse move: %w", errMove)
	}
	base := map[string]any{"x": x, "y": y, "button": "left", "clickCount": 1, "buttons": 1, "pointerType": "mouse"}
	pressed := map[string]any{"type": "mousePressed"}
	for key, value := range base {
		pressed[key] = value
	}
	if _, errPress := s.call(ctx, "Input.dispatchMouseEvent", pressed); errPress != nil {
		return fmt.Errorf("registerbot: mouse press: %w", errPress)
	}
	released := map[string]any{"type": "mouseReleased", "buttons": 0}
	for key, value := range base {
		released[key] = value
	}
	released["buttons"] = 0
	if _, errRelease := s.call(ctx, "Input.dispatchMouseEvent", released); errRelease != nil {
		return fmt.Errorf("registerbot: mouse release: %w", errRelease)
	}
	return nil
}

var (
	errInputNotReady = errors.New("registerbot: input is not yet editable or unobstructed")
	errInputFocus    = errors.New("registerbot: input focus was not acquired")
	errInputValue    = errors.New("registerbot: input value was not retained; retrying the field")
	errSubmitReady   = errors.New("registerbot: Google Next control is not yet ready")
)

// TypeInto confirms focus and the resulting value instead of treating accepted
// CDP commands as evidence that the page consumed the input. Readback returns
// only boolean comparisons, never the field contents.
func (s *Session) TypeInto(ctx context.Context, selector, label, value string) error {
	result, errEval := s.Evaluate(ctx, fmt.Sprintf("cpaPrepareInput(%s, %s)", jsString(selector), jsString(label)), true)
	if errEval != nil {
		return errEval
	}
	point, ok := asPoint(result)
	if !ok {
		return errInputNotReady
	}
	if errClick := s.clickAt(ctx, point.x, point.y); errClick != nil {
		return errClick
	}
	focused, errFocus := s.Evaluate(ctx, "cpaInputFocused()", false)
	if errFocus != nil {
		return errFocus
	}
	if focused != true {
		return errInputFocus
	}
	matches, errMatch := s.Evaluate(ctx, fmt.Sprintf("cpaInputMatches(%s)", jsString(value)), false)
	if errMatch != nil {
		return errMatch
	}
	if matches == true {
		return nil
	}
	if _, errSelect := s.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type": "rawKeyDown", "key": "a", "code": "KeyA", "modifiers": 2,
		"windowsVirtualKeyCode": 65, "nativeVirtualKeyCode": 65,
	}); errSelect != nil {
		return fmt.Errorf("registerbot: select all: %w", errSelect)
	}
	if _, errUp := s.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type": "keyUp", "key": "a", "code": "KeyA", "modifiers": 2,
		"windowsVirtualKeyCode": 65, "nativeVirtualKeyCode": 65,
	}); errUp != nil {
		return fmt.Errorf("registerbot: select all release: %w", errUp)
	}
	if errClear := s.PressKey(ctx, "Backspace"); errClear != nil {
		return fmt.Errorf("registerbot: clear field: %w", errClear)
	}
	cleared, errCleared := s.Evaluate(ctx, "cpaInputFocused() && cpaInputMatches('')", false)
	if errCleared != nil {
		return errCleared
	}
	if cleared != true {
		return errInputValue
	}
	if _, errInsert := s.call(ctx, "Input.insertText", map[string]any{"text": value}); errInsert != nil {
		return fmt.Errorf("registerbot: insert text: %w", errInsert)
	}
	retained, errRetained := s.Evaluate(ctx, fmt.Sprintf(`(async function(){
		await new Promise(function(resolve){setTimeout(resolve,150);});
		return cpaInputFocused() && cpaInputMatches(%s);
	})()`, jsString(value)), true)
	if errRetained != nil {
		return errRetained
	}
	if retained != true {
		return errInputValue
	}
	return nil
}

// SubmitGoogleCredential resolves the current form's enabled submit control,
// checking the field again after layout settles. It never clicks a broad
// jsaction ancestor merely because that ancestor contains the word "Next".
func (s *Session) SubmitGoogleCredential(ctx context.Context, stage, value string) error {
	container := ""
	switch stage {
	case "email":
		container = "#identifierNext"
	case "password":
		container = "#passwordNext"
	default:
		return errSubmitReady
	}
	result, errEval := s.Evaluate(ctx, fmt.Sprintf(`(async function(){
		if(location.origin!=='https://accounts.google.com'||!cpaInputMatches(%s))return null;
		var visible=function(el){var r=el.getBoundingClientRect(),st=getComputedStyle(el);return el.isConnected&&r.width>1&&r.height>1&&st.display!=='none'&&st.visibility!=='hidden'&&Number(st.opacity)!==0;};
		var enabled=function(el){return visible(el)&&!el.disabled&&!el.closest('[aria-disabled="true"],[aria-busy="true"]');};
		var container=document.querySelector(%s),button=null;
		if(container){
			button=container.matches('button,[role="button"],input[type="submit"]')?container:container.querySelector('button,[role="button"],input[type="submit"]');
		}else{
			button=Array.from(document.querySelectorAll('button,[role="button"],input[type="submit"]')).find(function(el){
				var label=String(el.innerText||el.value||el.getAttribute('aria-label')||'').replace(/\s+/g,' ').trim();
				return /^(next|continue|下一步|继续)$/i.test(label)&&enabled(el);
			});
		}
		if(!button||!enabled(button))return null;
		button.scrollIntoView({block:'center',inline:'nearest',behavior:'instant'});
		await new Promise(function(resolve){setTimeout(resolve,150);});
		if(location.origin!=='https://accounts.google.com'||!enabled(button)||!cpaInputMatches(%s))return null;
		var r=button.getBoundingClientRect(),left=Math.max(0,r.left),right=Math.min(innerWidth,r.right),top=Math.max(0,r.top),bottom=Math.min(innerHeight,r.bottom);
		if(right-left<2||bottom-top<2)return null;
		var x=(left+right)/2,y=(top+bottom)/2,hit=document.elementFromPoint(x,y);
		if(!hit||(hit!==button&&!button.contains(hit)))return null;
		return {x:x,y:y};
	})()`, jsString(value), jsString(container), jsString(value)), true)
	if errEval != nil {
		return errEval
	}
	point, ok := asPoint(result)
	if !ok {
		return errSubmitReady
	}
	return s.clickAt(ctx, point.x, point.y)
}

func splitChunks(value string, size int) []string {
	runes := []rune(value)
	var out []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// PressKey sends one named key chord such as Enter.
func (s *Session) PressKey(ctx context.Context, key string) error {
	defined, ok := keyDefinitions[key]
	if !ok {
		return fmt.Errorf("registerbot: unsupported key %q", key)
	}
	if _, errDown := s.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type": "rawKeyDown", "key": defined.key, "code": defined.code,
		"windowsVirtualKeyCode": defined.virtual, "nativeVirtualKeyCode": defined.virtual,
	}); errDown != nil {
		return fmt.Errorf("registerbot: key down %s: %w", key, errDown)
	}
	if defined.text != "" {
		if _, errChar := s.call(ctx, "Input.dispatchKeyEvent", map[string]any{
			"type": "char", "key": defined.key, "code": defined.code, "text": defined.text,
			"unmodifiedText":        defined.text,
			"windowsVirtualKeyCode": defined.virtual, "nativeVirtualKeyCode": defined.virtual,
		}); errChar != nil {
			return fmt.Errorf("registerbot: key char %s: %w", key, errChar)
		}
	}
	if _, errUp := s.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type": "keyUp", "key": defined.key, "code": defined.code,
		"windowsVirtualKeyCode": defined.virtual, "nativeVirtualKeyCode": defined.virtual,
	}); errUp != nil {
		return fmt.Errorf("registerbot: key up %s: %w", key, errUp)
	}
	return nil
}

type keyDefinition struct {
	key     string
	code    string
	text    string
	virtual int
}

var keyDefinitions = map[string]keyDefinition{
	"Enter":     {key: "Enter", code: "Enter", text: "\r", virtual: 13},
	"Tab":       {key: "Tab", code: "Tab", text: "\t", virtual: 9},
	"Escape":    {key: "Escape", code: "Escape", virtual: 27},
	"Backspace": {key: "Backspace", code: "Backspace", virtual: 8},
	"PageDown":  {key: "PageDown", code: "PageDown", virtual: 34},
	"End":       {key: "End", code: "End", virtual: 35},
}

// AcceptWorkspaceTerms scrolls the document and nested terms containers, then
// resolves an unobstructed in-viewport control before dispatching a real click.
func (s *Session) AcceptWorkspaceTerms(ctx context.Context) error {
	value, errEval := s.Evaluate(ctx, `(async function(){
		if(location.hostname!=='accounts.google.com'||!location.pathname.includes('/speedbump/'))return null;
		var visible=function(el){var r=el.getBoundingClientRect(),st=getComputedStyle(el);return r.width>1&&r.height>1&&st.display!=='none'&&st.visibility!=='hidden'&&Number(st.opacity)!==0;};
		var roots=[document.scrollingElement,document.documentElement,document.body];
		roots.forEach(function(el){if(el)el.scrollTo({top:el.scrollHeight,behavior:'instant'});});
		Array.from(document.querySelectorAll('*')).forEach(function(el){
			if(el.clientHeight>0&&el.scrollHeight>el.clientHeight+2&&visible(el)&&/auto|scroll|overlay|hidden/.test(getComputedStyle(el).overflowY)){
				el.scrollTo({top:el.scrollHeight,behavior:'instant'});
			}
		});
		await new Promise(function(resolve){setTimeout(resolve,250);});
		var labels=['i understand','i undetstand','我了解','我理解','我知道了','我明白了'];
		var controls=Array.from(document.querySelectorAll('button,[role="button"],input[type="submit"],input[type="button"]'));
		var button=controls.find(function(el){
			var label=String(el.innerText||el.value||el.getAttribute('aria-label')||el.textContent||'').replace(/\s+/g,' ').trim().toLowerCase();
			return labels.includes(label)&&visible(el)&&!el.disabled&&el.getAttribute('aria-disabled')!=='true';
		});
		if(!button)return null;
		button.scrollIntoView({block:'center',inline:'nearest',behavior:'instant'});
		await new Promise(function(resolve){setTimeout(resolve,100);});
		if(!button.isConnected||button.disabled||button.getAttribute('aria-disabled')==='true')return null;
		var rect=button.getBoundingClientRect();
		var left=Math.max(0,rect.left),right=Math.min(innerWidth,rect.right),top=Math.max(0,rect.top),bottom=Math.min(innerHeight,rect.bottom);
		if(right-left<2||bottom-top<2)return null;
		var x=(left+right)/2,y=(top+bottom)/2,hit=document.elementFromPoint(x,y);
		if(!hit||(hit!==button&&!button.contains(hit)))return null;
		return {x:x,y:y};
	})()`, true)
	if errEval != nil {
		return errEval
	}
	point, ok := asPoint(value)
	if !ok {
		return errors.New("registerbot: Workspace terms confirmation is not yet ready")
	}
	return s.clickAt(ctx, point.x, point.y)
}

// ScrollToEnd pages a long consent screen so its accept control becomes usable.
func (s *Session) ScrollToEnd(ctx context.Context) error {
	_, errEval := s.Evaluate(ctx, `(function(){
		window.scrollTo(0, document.body.scrollHeight);
		var boxes = document.querySelectorAll('[style*="overflow"], [class*="scroll"], [jsname]');
		for (var i = 0; i < boxes.length; i++) { boxes[i].scrollTop = boxes[i].scrollHeight; }
		return true;
	})()`, false)
	if errEval != nil {
		return fmt.Errorf("registerbot: scroll page: %w", errEval)
	}
	return nil
}

// Screenshot captures the viewport as PNG bytes for stall evidence.
func (s *Session) Screenshot(ctx context.Context) ([]byte, error) {
	result, errCall := s.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if errCall != nil {
		return nil, errCall
	}
	data, _ := result["data"].(string)
	if strings.TrimSpace(data) == "" {
		return nil, errors.New("registerbot: screenshot returned no data")
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(data)
	if errDecode != nil {
		return nil, fmt.Errorf("registerbot: decode screenshot: %w", errDecode)
	}
	return decoded, nil
}

// PageText returns the visible text plus markup, used for key harvesting.
func (s *Session) PageText(ctx context.Context) (string, error) {
	return s.EvaluateString(ctx, "cpaReadAll()", false)
}

// State returns a compact page snapshot used for stall diagnostics.
func (s *Session) State(ctx context.Context) (map[string]any, error) {
	value, errEval := s.Evaluate(ctx, "cpaState()", false)
	if errEval != nil {
		return nil, errEval
	}
	if doc, ok := value.(map[string]any); ok && doc != nil {
		return doc, nil
	}
	return nil, errors.New("registerbot: page state was not an object")
}

// SetDownloadBehavior lets a pooled instance save files into its own directory.
func (b *browserInstance) SetDownloadBehavior(ctx context.Context, dir string) error {
	if b == nil || b.control == nil {
		return errors.New("registerbot: browser instance unavailable")
	}
	_, errCall := b.control.Call(ctx, "Browser.setDownloadBehavior", map[string]any{"behavior": "allow", "downloadPath": dir})
	if errCall != nil {
		return fmt.Errorf("registerbot: set download behaviour: %w", errCall)
	}
	return nil
}
