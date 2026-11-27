package registerbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gorilla/websocket"
)

func TestGatewaySelectorLeaseCoversCompleteAccount(t *testing.T) {
	var mu sync.Mutex
	node := "node-a"
	pins := 0
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"version":"test"}`))
		case r.URL.Path == "/proxies":
			_, _ = fmt.Fprintf(w, `{"proxies":{"group":{"type":"Selector","now":%q,"all":["node-a","node-b"]}}}`, node)
		case r.Method == http.MethodPut:
			var request struct {
				Name string `json:"name"`
			}
			if errDecode := json.NewDecoder(r.Body).Decode(&request); errDecode != nil {
				t.Error(errDecode)
			}
			node = request.Name
			pins++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer controller.Close()
	set := DefaultSettings()
	set.Gateway.ControllerURL = controller.URL
	set.Gateway.ProxyURL = controller.URL
	set.Gateway.VerifyExitIP = false
	gateway := NewGateway(func() Settings { return set })
	status := gateway.Probe(context.Background(), true)
	if status.Mode != GatewayModeController {
		t.Fatalf("mode=%s", status.Mode)
	}
	channel := ChannelSettings{ID: "one", Group: "group", RotateNode: true}
	first, errFirst := gateway.Acquire(context.Background(), channel, "first")
	if errFirst != nil || first.Node != "node-a" || !first.Verified {
		t.Fatalf("first=%+v error=%v", first, errFirst)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errAcquire := gateway.Acquire(ctx, channel, "second"); !errors.Is(errAcquire, context.Canceled) {
		t.Fatalf("overlapping lease=%v", errAcquire)
	}
	mu.Lock()
	count := pins
	mu.Unlock()
	if count != 1 {
		t.Fatalf("selector changed during active account: %d", count)
	}
	gateway.Release("group", "first")
	second, errSecond := gateway.Acquire(context.Background(), channel, "second")
	if errSecond != nil || second.Node != "node-b" {
		t.Fatalf("second=%+v error=%v", second, errSecond)
	}
	gateway.Release("group", "second")
}

func TestGatewayControllerCannotHideFailedForwardingPlane(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"version":"test"}`))
			return
		}
		_, _ = w.Write([]byte(`{"proxies":{}}`))
	}))
	defer controller.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	set := DefaultSettings()
	set.Gateway.ControllerURL = controller.URL
	set.Gateway.ProxyURL = deadURL
	set.Gateway.VerifyExitIP = false
	gateway := NewGateway(func() Settings { return set })
	status := gateway.Probe(context.Background(), true)
	if status.Mode != GatewayModeDisabled || gateway.EffectiveProxyURL() != "" {
		t.Fatalf("failed forwarding plane reported %+v", status)
	}
}

func TestGatewayControllerCandidatesPreferConfiguredPort(t *testing.T) {
	candidates := candidateControllers("http://127.0.0.1:11460", "http://127.0.0.1:11461")
	if len(candidates) != 1 || candidates[0] != "http://127.0.0.1:11461" {
		t.Fatalf("candidates=%v", candidates)
	}
}

func TestBrowserPoolExclusivelyReusesReleasedProcess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Browser":"test","Protocol-Version":"1.3"}`))
	}))
	defer server.Close()
	set := DefaultSettings()
	set.Browser.PoolSize = 2
	pool := NewPool(func() Settings { return set }, t.TempDir())
	for i := 0; i < 2; i++ {
		pool.instances[i] = &browserInstance{id: i, transport: newCDPTransport(server.URL), mode: BrowserModeEmbedded}
		pool.order = append(pool.order, i)
	}
	first, errFirst := pool.Acquire(context.Background())
	second, errSecond := pool.Acquire(context.Background())
	if errFirst != nil || errSecond != nil || first == second {
		t.Fatalf("nonexclusive slots: %v %v", errFirst, errSecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errAcquire := pool.Acquire(ctx); !errors.Is(errAcquire, context.Canceled) {
		t.Fatalf("full pool ignored cancellation: %v", errAcquire)
	}
	pool.Release(first)
	again, errAgain := pool.Acquire(context.Background())
	if errAgain != nil || again != first {
		t.Fatalf("released process not reused: %v", errAgain)
	}
}

func TestIsolatedContextUsesProxyAndNeverClosesAttachedBrowser(t *testing.T) {
	var mu sync.Mutex
	methods := []string{}
	proxy := ""
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, raw, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			var request cdpMessage
			if errDecode := json.Unmarshal(raw, &request); errDecode != nil {
				t.Error(errDecode)
				return
			}
			mu.Lock()
			methods = append(methods, request.Method)
			mu.Unlock()
			result := map[string]any{}
			switch request.Method {
			case "Target.createBrowserContext":
				result["browserContextId"] = "context"
				mu.Lock()
				proxy, _ = request.Params["proxyServer"].(string)
				mu.Unlock()
			case "Target.createTarget":
				if request.Params["browserContextId"] != "context" {
					t.Error("target lacks isolated context")
				}
				result["targetId"] = "target"
			case "Target.attachToTarget":
				result["sessionId"] = "session"
			case "Runtime.evaluate":
				result["result"] = map[string]any{"value": "complete"}
			}
			payload, _ := json.Marshal(map[string]any{"id": request.ID, "result": result})
			if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, errDial := dialCDP(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if errDial != nil {
		t.Fatal(errDial)
	}
	instance := &browserInstance{control: client, proxyURL: "http://127.0.0.1:12450"}
	session, errOpen := instance.OpenPage(ctx, "https://freeaiapikey.com/register")
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if errClose := session.CloseTarget(ctx); errClose != nil {
		t.Fatal(errClose)
	}
	if errStop := instance.shutdown(); errStop != nil {
		t.Fatal(errStop)
	}
	mu.Lock()
	defer mu.Unlock()
	if proxy != "http://127.0.0.1:12450" {
		t.Fatalf("proxy=%s", proxy)
	}
	foundDispose := false
	for _, method := range methods {
		if method == "Browser.close" {
			t.Fatal("attached browser was closed")
		}
		if method == "Target.disposeBrowserContext" {
			foundDispose = true
		}
	}
	if !foundDispose || instance.contexts != 0 {
		t.Fatal("isolated context was not disposed")
	}
}

func TestFreeAIRequiresExactCredentialAndProviderOrigins(t *testing.T) {
	for _, raw := range []string{"https://accounts.google.com.evil.invalid/", "http://accounts.google.com/", "https://evil.invalid/?next=accounts.google.com"} {
		if googleIdentityHost(raw) {
			t.Fatalf("unsafe identity origin %s", raw)
		}
	}
	if !googleIdentityHost("https://accounts.google.com/v3/signin/identifier") {
		t.Fatal("valid identity origin rejected")
	}
	for _, raw := range []string{"https://freeaiapikey.com.evil.invalid/dashboard", "http://freeaiapikey.com/dashboard"} {
		if freeAIHost(raw) {
			t.Fatalf("unsafe provider origin %s", raw)
		}
	}
	if !freeAIHost("https://www.freeaiapikey.com/dashboard") {
		t.Fatal("valid provider origin rejected")
	}
}
