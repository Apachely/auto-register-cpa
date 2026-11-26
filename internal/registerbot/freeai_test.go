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

func TestFreeAIOnboardingRequiresConfirmedCredit(t *testing.T) {
	for _, stage := range []string{"onboarding_plans", "onboarding_discord"} {
		t.Run(stage, func(t *testing.T) {
			client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
				if request.Method == "Runtime.evaluate" {
					return map[string]any{"result": map[string]any{"value": map[string]any{"url": "https://freeaiapikey.com/setup", "action": stage, "credit_confirmed": false}}}
				}
				if request.Method == "Input.dispatchMouseEvent" || request.Method == "Page.navigate" {
					t.Error("required onboarding was bypassed")
				}
				return map[string]any{}
			})
			task := newFreeAITestTask(t)
			task.session = &Session{client: client, sessionID: "session", targetID: "target", contextID: "context"}
			_, errRun := NewFreeAIDriver(nil).Run(context.Background(), task)
			if !errors.Is(errRun, ErrManualHandoff) || task.session == nil {
				t.Fatal("incomplete onboarding did not retain manual handoff")
			}
		})
	}
}

func TestFreeAIOnboardingFastPath(t *testing.T) {
	key := "sk-" + strings.Repeat("A", 43)
	var actions []string
	claimCount, finishCount, pendingPolls := 0, 0, 0
	pendingClick := ""
	client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
		value := func(v any) map[string]any { return map[string]any{"result": map[string]any{"value": v}} }
		switch request.Method {
		case "Runtime.evaluate":
			expression, _ := request.Params["expression"].(string)
			if expression == freeAIInspectScript {
				page := freeAIPage{URL: "https://freeaiapikey.com/setup", Action: "onboarding_profile", PageState: "survey"}
				if claimCount > 0 {
					pendingPolls++
					// Label changes and a transient DOM redraw must not submit twice.
					page.PageState = strings.Repeat("pending", pendingPolls)
					if pendingPolls >= 3 {
						page.Action, page.CreditConfirmed = "onboarding_credit", true
					}
				}
				if finishCount > 0 {
					// Even if the wallet has not refreshed, this session already
					// observed credit confirmation before clicking Finish later.
					page = freeAIPage{URL: "https://freeaiapikey.com/dashboard", Action: "onboarding_entry", Key: key}
				}
				return value(page)
			}
			if strings.HasPrefix(expression, "cpaPrepareSelector(") {
				switch {
				case strings.Contains(expression, "role") && strings.Contains(expression, "student"):
					pendingClick = "Student"
				case strings.Contains(expression, "source") && strings.Contains(expression, "google"):
					pendingClick = "Google search"
				default:
					t.Errorf("unexpected survey selector: %s", expression)
				}
				return value(map[string]any{"x": 10, "y": 10})
			}
			if strings.Contains(expression, "input.checked") {
				label := "Student"
				if strings.Contains(expression, "source") {
					label = "Google search"
				}
				return value(strings.Contains(strings.Join(actions, "|"), label))
			}
			if strings.HasPrefix(expression, "cpaFindButton(") {
				switch {
				case strings.Contains(expression, "Get my $5"):
					pendingClick = "Get my $5"
				case strings.Contains(expression, "Finish later and go to my dashboard"):
					pendingClick = "Finish later and go to my dashboard"
				default:
					t.Errorf("unexpected tutorial action: %s", expression)
				}
				return value(map[string]any{"x": 10, "y": 10})
			}
		case "Input.dispatchMouseEvent":
			if request.Params["type"] == "mouseReleased" {
				actions = append(actions, pendingClick)
				switch pendingClick {
				case "Get my $5":
					claimCount++
				case "Finish later and go to my dashboard":
					finishCount++
				}
			}
		case "Page.navigate":
			t.Error("completed survey was sent back to setup")
		}
		return map[string]any{}
	})
	task := newFreeAITestTask(t)
	task.Channel.OnboardingRole, task.Channel.OnboardingSource = "student", "google"
	task.session = &Session{client: client, sessionID: "session", targetID: "target", contextID: "context"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	harvest, err := NewFreeAIDriver(nil).Run(ctx, task)
	if err != nil || harvest.APIKey != key || !harvest.CreditConfirmed {
		t.Fatalf("harvest valid=%v credited=%v error=%v", harvest.APIKey == key, harvest.CreditConfirmed, err)
	}
	want := "Student|Google search|Get my $5|Finish later and go to my dashboard"
	if strings.Join(actions, "|") != want || claimCount != 1 || finishCount != 1 {
		t.Fatalf("actions=%v claim=%d finish=%d", actions, claimCount, finishCount)
	}
	t.Log("Student -> Google search -> Get my $5 (once) -> Finish later -> full Default Key; no setup loop")
}

func TestFreeAIExistingPaidDashboardDoesNotReopenSetup(t *testing.T) {
	key := "sk-" + strings.Repeat("A", 43)
	client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
		if request.Method == "Runtime.evaluate" {
			return map[string]any{"result": map[string]any{"value": freeAIPage{URL: "https://freeaiapikey.com/dashboard", Action: "key", CreditConfirmed: true, Key: key}}}
		}
		if request.Method == "Page.navigate" || request.Method == "Input.dispatchMouseEvent" {
			t.Error("ready dashboard must not repeat onboarding")
		}
		return map[string]any{}
	})
	task := newFreeAITestTask(t)
	task.session = &Session{client: client, sessionID: "session", targetID: "target", contextID: "context"}
	harvest, err := NewFreeAIDriver(nil).Run(context.Background(), task)
	if err != nil || harvest.APIKey != key || !harvest.CreditConfirmed {
		t.Fatalf("harvest valid=%v credited=%v error=%v", harvest.APIKey == key, harvest.CreditConfirmed, err)
	}
}

func TestFreeAIOrdinarySignInStallRetriesWithoutManualHandoff(t *testing.T) {
	client := newFreeAITestCDP(t, func(request cdpMessage) map[string]any {
		if request.Method == "Runtime.evaluate" {
			if request.Params["expression"] == freeAIInspectScript {
				return map[string]any{"result": map[string]any{"value": freeAIPage{URL: "https://accounts.google.com/v3/signin/identifier", Action: "email"}}}
			}
			return map[string]any{"result": map[string]any{"value": false}}
		}
		return map[string]any{}
	})
	task := newFreeAITestTask(t)
	task.session = &Session{client: client, sessionID: "session", targetID: "target", contextID: "context"}
	settings := DefaultSettings()
	settings.Browser.StepTimeoutSeconds = 1
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, errRun := NewFreeAIDriver(func() Settings { return settings }).Run(ctx, task)
	if errRun == nil || errors.Is(errRun, ErrManualHandoff) || task.session != nil {
		t.Fatalf("ordinary timeout did not close and retry: %v", errRun)
	}
}
