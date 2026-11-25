package registerbot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gorilla/websocket"
)

func newFreeAITestCDP(t *testing.T, respond func(cdpMessage) map[string]any) *cdpClient {
	t.Helper()
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
			result := respond(request)
			payload, errEncode := json.Marshal(map[string]any{"id": request.ID, "result": result})
			if errEncode != nil {
				t.Error(errEncode)
				return
			}
			if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, errDial := dialCDP(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestNavigateReportsCDPNetworkFailureImmediately(t *testing.T) {
	client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
		if request.Method != "Page.navigate" {
			t.Errorf("unexpected command after navigation failure: %s", request.Method)
		}
		return map[string]any{"errorText": "net::ERR_CONNECTION_RESET"}
	})
	session := &Session{client: client, sessionID: "test"}
	errNavigate := session.Navigate(context.Background(), "https://freeaiapikey.com/register")
	var networkError *NavigationError
	if !errors.As(errNavigate, &networkError) || networkError.Code != "ERR_CONNECTION_RESET" {
		t.Fatalf("navigation error=%v", errNavigate)
	}
	if errors.Is(errNavigate, ErrManualHandoff) {
		t.Fatal("network failure became manual handoff")
	}
}

func newFreeAITestTask(t *testing.T) *Task {
	t.Helper()
	bus, errBus := NewBus(t.TempDir(), 100)
	if errBus != nil {
		t.Fatal(errBus)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return &Task{Channel: DefaultFreeAIChannel(), Run: "test-run", RowID: "test-row", Account: Account{Email: "fixture@example.invalid", Password: "fixture-secret"}, bus: bus}
}

func TestFreeAIOpenNetworkFailureRetriesInsteadOfManual(t *testing.T) {
	client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
		switch request.Method {
		case "Target.createBrowserContext":
			return map[string]any{"browserContextId": "context"}
		case "Target.createTarget":
			return map[string]any{"targetId": "target"}
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "session"}
		case "Runtime.evaluate":
			return map[string]any{"result": map[string]any{"value": "complete"}}
		case "Page.navigate":
			return map[string]any{"errorText": "net::ERR_PROXY_CONNECTION_FAILED"}
		default:
			return map[string]any{}
		}
	})
	task := newFreeAITestTask(t)
	task.browser = &browserInstance{control: client}
	_, errRun := NewFreeAIDriver(nil).Run(context.Background(), task)
	var networkError *NavigationError
	if !errors.As(errRun, &networkError) || networkError.Code != "ERR_PROXY_CONNECTION_FAILED" {
		t.Fatalf("driver error=%v", errRun)
	}
	if errors.Is(errRun, ErrManualHandoff) || task.session != nil {
		t.Fatal("network failure retained a manual page")
	}
}

func TestFreeAIChromeInterstitialRetriesInsteadOfWaiting(t *testing.T) {
	client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
		if request.Method == "Runtime.evaluate" {
			return map[string]any{"result": map[string]any{"value": map[string]any{"url": "https://freeaiapikey.com/register", "action": "network_error", "network_code": "ERR_CONNECTION_RESET"}}}
		}
		return map[string]any{}
	})
	task := newFreeAITestTask(t)
	task.session = &Session{client: client, sessionID: "session", targetID: "target", contextID: "context"}
	_, errRun := NewFreeAIDriver(nil).Run(context.Background(), task)
	var networkError *NavigationError
	if !errors.As(errRun, &networkError) || networkError.Code != "ERR_CONNECTION_RESET" {
		t.Fatalf("interstitial error=%v", errRun)
	}
	if task.session != nil {
		t.Fatal("failed interstitial was retained instead of retried")
	}
}

func TestFreeAIPasswordTransitionDoesNotTriggerHumanVerification(t *testing.T) {
	for _, test := range []struct {
		name, url string
		human     bool
		want      string
	}{
		{"password transition", "https://accounts.google.com/v3/signin/challenge/pwd", false, "loading"},
		{"password captcha", "https://accounts.google.com/v3/signin/challenge/pwd", true, "challenge"},
		{"totp challenge", "https://accounts.google.com/v3/signin/challenge/totp", false, "challenge"},
		{"untrusted origin", "https://example.invalid/challenge/pwd", false, "challenge"},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := normalizeFreeAIPage(freeAIPage{URL: test.url, Action: "challenge", HumanChallenge: test.human})
			if page.Action != test.want {
				t.Fatalf("action=%s want=%s", page.Action, test.want)
			}
		})
	}
}

func TestNavigationFailureNeverExposesArbitraryBrowserText(t *testing.T) {
	if failure := navigationFailure("https://example.invalid/?token=private-value"); failure.Code != "ERR_FAILED" || strings.Contains(failure.Error(), "private-value") {
		t.Fatalf("unsafe error=%v", failure)
	}
}
