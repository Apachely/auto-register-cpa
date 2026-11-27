package registerbot

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
)

// The real driver, DOM inspection and CDP input run in an isolated browser.
// Every request is intercepted before network access; no provider account,
// credit or real credential is used by this regression.
func TestFreeAIFreshSurveyBrowser(t *testing.T) {
	runFreshSurveyBrowsers(t, 1, true)
}

func TestFreeAIParallelSurveyBrowser(t *testing.T) {
	runFreshSurveyBrowsers(t, 2, true)
}

func TestFreeAISurveyWithoutWelcomeCredit(t *testing.T) {
	runFreshSurveyBrowsers(t, 1, false)
}

// TestFreeAIGoogleAutomationBrowser drives every automatic registration stage
// in a real isolated Chrome process. Fetch interception serves local fixtures
// before any provider or identity request can leave the browser.
func TestFreeAIGoogleAutomationBrowser(t *testing.T) {
	if os.Getenv("REGISTERBOT_BROWSER_SMOKE") != "1" {
		t.Skip("set REGISTERBOT_BROWSER_SMOKE=1 for the installed-browser regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	settings := DefaultSettings()
	settings.Normalize()
	settings.Browser.Headless = true
	settings.Browser.PoolSize = 1
	settings.Gateway.ProxyURL = ""
	pool := NewPool(func() Settings { return settings }, t.TempDir())
	t.Cleanup(func() { _ = pool.Shutdown() })
	instance, errAcquire := pool.Acquire(ctx)
	if errAcquire != nil {
		t.Fatal(errAcquire)
	}
	defer pool.Release(instance)
	session, errSession := instance.OpenPage(ctx, "about:blank")
	if errSession != nil {
		t.Fatal(errSession)
	}
	defer func() { _ = session.CloseTarget(context.Background()) }()

	var emailOK, passwordOK, terms, consent, claims, dashboards, unexpected atomic.Int32
	fixtureKey := "sk-" + strings.Repeat("G", 43)
	stop := instance.control.subscribe("Fetch.requestPaused", session.sessionID, func(event cdpMessage) {
		go func() {
			request, _ := event.Params["request"].(map[string]any)
			address, _ := request["url"].(string)
			parsed, errURL := url.Parse(address)
			body, contentType := "", "text/html"
			if errURL != nil {
				unexpected.Add(1)
			} else {
				switch parsed.Hostname() + parsed.Path {
				case "freeaiapikey.com/register":
					body = `<!doctype html><body><button style="display:none">Continue with Google</button><button>Continue with Google account later</button><button id="google" onclick="location.href='https://accounts.google.com/v3/signin/identifier'">Continue with Google</button>`
				case "accounts.google.com/v3/signin/identifier":
					body = `<!doctype html><body><input type="password" style="display:none"><button disabled>Next</button><form id="identifierForm"><label>Email or phone<input id="identifierId" name="identifier" type="email"></label><div id="identifierNext"><button type="submit">Next</button></div></form><script>identifierForm.onsubmit=async e=>{e.preventDefault();await fetch('/fixture-email?value='+encodeURIComponent(identifierId.value));location.href='/v3/signin/challenge/pwd'}</script>`
				case "accounts.google.com/fixture-email":
					if parsed.Query().Get("value") == "fixture@example.invalid" {
						emailOK.Add(1)
					}
					body, contentType = `{"ok":true}`, "application/json"
				case "accounts.google.com/v3/signin/challenge/pwd":
					body = `<!doctype html><body><input name="Passwd" type="password" style="display:none"><form id="passwordForm"><label>Password<input id="passwordField" name="Passwd" type="password"></label><div id="passwordNext"><button type="submit">Next</button></div></form><script>passwordForm.onsubmit=async e=>{e.preventDefault();await fetch('/fixture-password?value='+encodeURIComponent(passwordField.value));location.href='/speedbump/workspacetermsofservice'}</script>`
				case "accounts.google.com/fixture-password":
					if parsed.Query().Get("value") == "fixture-secret" {
						passwordOK.Add(1)
					}
					body, contentType = `{"ok":true}`, "application/json"
				case "accounts.google.com/speedbump/workspacetermsofservice":
					terms.Add(1)
					body = `<!doctype html><body style="min-height:1200px"><button disabled>I understand</button><button style="margin-top:900px" onclick="location.href='/o/oauth2/v2/auth/consent'">I understand</button>`
				case "accounts.google.com/o/oauth2/v2/auth/consent":
					consent.Add(1)
					body = `<!doctype html><body><h1>FreeAIapikey wants access</h1><button disabled>Continue</button><button>Continue later</button><button onclick="location.href='https://freeaiapikey.com/setup'">Continue</button>`
				case "freeaiapikey.com/setup":
					body = `<!doctype html><body><main id="app">Loading</main><script>setTimeout(()=>{document.getElementById('app').innerHTML='<div>Step 1 of 5</div><div id="wallet">Normal $0.00</div><form id="surveyForm"><label><input type="radio" name="role" value="student">Student</label><label><input type="radio" name="source" value="google">Google search</label><button id="surveySubmit" disabled>Get my $5</button></form><a href="/dashboard">Finish later and go to my dashboard</a>';document.getElementById('surveyForm').onchange=()=>{document.getElementById('surveySubmit').disabled=!(document.querySelector('input[name=role]').checked&&document.querySelector('input[name=source]').checked)};document.getElementById('surveyForm').onsubmit=async e=>{e.preventDefault();await fetch('/fixture-claim');document.getElementById('surveyForm').outerHTML='<div id="surveyDone">$5.00 is in your balance.</div>';document.getElementById('wallet').textContent='Normal $5.00'}},100)</script>`
				case "freeaiapikey.com/fixture-claim":
					claims.Add(1)
					body, contentType = `{"ok":true}`, "application/json"
				case "freeaiapikey.com/dashboard":
					dashboards.Add(1)
					body = `<!doctype html><body><div>NORMAL CREDITS $5.00</div><div><p>Default Key</p><button title="Copy key" onclick="copyKey('` + fixtureKey + `')">Copy key</button></div>`
				case "freeaiapikey.com/favicon.ico", "accounts.google.com/favicon.ico":
					body = ""
				default:
					unexpected.Add(1)
				}
			}
			_, _ = session.call(ctx, "Fetch.fulfillRequest", map[string]any{"requestId": event.Params["requestId"], "responseCode": 200, "responseHeaders": []map[string]string{{"name": "Content-Type", "value": contentType}}, "body": base64.StdEncoding.EncodeToString([]byte(body))})
		}()
	})
	defer stop()
	if _, errEnable := session.call(ctx, "Fetch.enable", map[string]any{"patterns": []map[string]string{{"urlPattern": "*", "requestStage": "Request"}}}); errEnable != nil {
		t.Fatal(errEnable)
	}
	if errNavigate := session.Navigate(ctx, "https://freeaiapikey.com/register"); errNavigate != nil {
		t.Fatal(errNavigate)
	}
	task := newFreeAITestTask(t)
	task.session = session
	task.Channel.OnboardingRole, task.Channel.OnboardingSource = "student", "google"
	started := time.Now()
	harvest, errRun := NewFreeAIDriver(func() Settings { return settings }).Run(ctx, task)
	elapsed := time.Since(started)
	if errRun != nil || harvest.APIKey != fixtureKey || !harvest.CreditConfirmed {
		t.Fatalf("full browser automation failed: key=%v credit=%v error=%v events=%+v", harvest.APIKey == fixtureKey, harvest.CreditConfirmed, errRun, task.bus.Tail(100, task.Channel.ID))
	}
	if emailOK.Load() != 1 || passwordOK.Load() != 1 || terms.Load() != 1 || consent.Load() != 1 || claims.Load() != 1 || dashboards.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("email=%d password=%d terms=%d consent=%d claims=%d dashboards=%d unexpected=%d", emailOK.Load(), passwordOK.Load(), terms.Load(), consent.Load(), claims.Load(), dashboards.Load(), unexpected.Load())
	}
	if elapsed >= automaticWaitLimit {
		t.Fatalf("local full-flow automation exceeded ten seconds: %v", elapsed)
	}
	wantStages := []string{"google", "email", "password", "speedbump", "consent", "onboarding_profile", "onboarding_credit", "key"}
	events := task.bus.Tail(100, task.Channel.ID)
	position := 0
	for _, event := range events {
		if position < len(wantStages) && event.Stage == wantStages[position] {
			position++
		}
	}
	if position != len(wantStages) {
		t.Fatalf("stage recognition incomplete: matched=%d/%d events=%v", position, len(wantStages), events)
	}
	t.Logf("PASS full isolated Chrome flow in %v: Google -> email -> password -> terms -> consent -> onboarding -> key", elapsed)
}

func runFreshSurveyBrowsers(t *testing.T, parallel int, credit bool) {
	t.Helper()
	if os.Getenv("REGISTERBOT_BROWSER_SMOKE") != "1" {
		t.Skip("set REGISTERBOT_BROWSER_SMOKE=1 for the installed-browser regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := DefaultSettings()
	settings.Normalize()
	settings.Browser.Headless = true
	settings.Browser.PoolSize = parallel
	settings.Gateway.ProxyURL = ""
	pool := NewPool(func() Settings { return settings }, t.TempDir())
	t.Cleanup(func() { _ = pool.Shutdown() })
	var claims, dashboards, badAnswers, unexpected atomic.Int32
	claimsReady := make(chan struct{})
	type result struct {
		valid bool
		err   error
	}
	results := make(chan result, parallel)
	for index := 0; index < parallel; index++ {
		instance, errAcquire := pool.Acquire(ctx)
		if errAcquire != nil {
			t.Fatal(errAcquire)
		}
		created, errCreate := instance.control.Call(ctx, "Target.createBrowserContext", map[string]any{"disposeOnDetach": true})
		if errCreate != nil {
			t.Fatal(errCreate)
		}
		contextID, _ := created["browserContextId"].(string)
		target, errTarget := instance.control.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": contextID})
		if errTarget != nil {
			t.Fatal(errTarget)
		}
		targetID, _ := target["targetId"].(string)
		attached, errAttach := instance.control.Call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
		if errAttach != nil {
			t.Fatal(errAttach)
		}
		sessionID, _ := attached["sessionId"].(string)
		session := &Session{client: instance.control, sessionID: sessionID, targetID: targetID, contextID: contextID}
		_, _ = session.call(ctx, "Page.enable", nil)
		_, _ = session.call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": helperScript})
		fixtureKey := "sk-" + strings.Repeat(string(rune('A'+index)), 43)
		stop := instance.control.subscribe("Fetch.requestPaused", sessionID, func(event cdpMessage) {
			go func() {
				request, _ := event.Params["request"].(map[string]any)
				address, _ := request["url"].(string)
				parsed, errURL := url.Parse(address)
				body, contentType := "", "text/html"
				if errURL == nil && parsed.Hostname() == "freeaiapikey.com" {
					switch parsed.Path {
					case "/setup":
						body = `<!doctype html><style>label,button,a{display:block;margin:20px;padding:10px}form{display:contents}input{opacity:0}</style><body><main id="app">Loading</main><script>
setTimeout(()=>{
document.getElementById('app').innerHTML='<div>Step<span>1</span>of<span>5</span></div><div id="wallet">Normal $0.00</div><form id="surveyForm"><label><input type="radio" name="role" value="student">Student</label><label><input type="radio" name="source" value="google">Google search</label><button id="surveySubmit" disabled>Get my $5</button></form><a href="/dashboard">Finish later and go to my dashboard</a>';
document.getElementById('surveyForm').onchange=()=>{document.getElementById('surveySubmit').disabled=!(document.querySelector('input[name=role]').checked&&document.querySelector('input[name=source]').checked)};
document.getElementById('surveyForm').onsubmit=async e=>{e.preventDefault();let button=document.getElementById('surveySubmit');button.disabled=true;button.textContent='Adding your $5...';await fetch('/fixture-claim?role='+document.querySelector('input[name=role]:checked')?.value+'&source='+document.querySelector('input[name=source]:checked')?.value);document.getElementById('wallet').textContent='Normal $5.00';document.getElementById('surveyForm').outerHTML='<div id="surveyDone">$5.00 is in your balance.</div>'};
},1500);</script>`
					case "/fixture-claim":
						if claims.Add(1) == int32(parallel) {
							close(claimsReady)
						}
						// Both browser contexts must submit before either may finish.
						select {
						case <-claimsReady:
						case <-ctx.Done():
							return
						}
						if parsed.Query().Get("role") != "student" || parsed.Query().Get("source") != "google" {
							badAnswers.Add(1)
						}
						body, contentType = `{"ok":true}`, "application/json"
					case "/dashboard":
						dashboards.Add(1)
						body = `<!doctype html><body><a href="/setup">Finish setup</a><div>NORMAL CREDITS $5.00</div><div><p>Default Key</p><div><code>sk-shortened...</code><button title="Copy key" onclick="copyKey('` + fixtureKey + `')">Copy key</button></div></div>`
					case "/favicon.ico":
						body = ""
					default:
						unexpected.Add(1)
					}
				} else {
					unexpected.Add(1)
				}
				if !credit {
					body = strings.ReplaceAll(body, "Normal $5.00", "Normal $0.00")
					body = strings.ReplaceAll(body, "NORMAL CREDITS $5.00", "NORMAL CREDITS $0.00")
					body = strings.ReplaceAll(body, "$5.00 is in your balance.", "Thanks, your answers are saved. This account starts at $0.")
				}
				_, _ = session.call(ctx, "Fetch.fulfillRequest", map[string]any{"requestId": event.Params["requestId"], "responseCode": 200, "responseHeaders": []map[string]string{{"name": "Content-Type", "value": contentType}}, "body": base64.StdEncoding.EncodeToString([]byte(body))})
			}()
		})
		defer stop()
		if _, errEnable := session.call(ctx, "Fetch.enable", map[string]any{"patterns": []map[string]string{{"urlPattern": "*", "requestStage": "Request"}}}); errEnable != nil {
			t.Fatal(errEnable)
		}
		if errNavigate := session.Navigate(ctx, "https://freeaiapikey.com/setup"); errNavigate != nil {
			t.Fatal(errNavigate)
		}
		task := newFreeAITestTask(t)
		task.session = session
		// Empty persisted answers simulate a new/older channel needing defaults.
		task.Channel.OnboardingRole, task.Channel.OnboardingSource = "", ""
		go func() {
			harvest, errRun := NewFreeAIDriver(func() Settings { return settings }).Run(ctx, task)
			results <- result{valid: harvest.APIKey == fixtureKey, err: errRun}
		}()
	}
	for i := 0; i < parallel; i++ {
		select {
		case result := <-results:
			if result.err != nil || !result.valid {
				t.Fatalf("fresh survey did not finish: key=%v error=%v", result.valid, result.err)
			}
		case <-ctx.Done():
			t.Fatal("parallel browser fixture timed out")
		}
	}
	if claims.Load() != int32(parallel) || dashboards.Load() != int32(parallel) || badAnswers.Load() != 0 || unexpected.Load() != 0 {
		t.Fatalf("claims=%d dashboards=%d bad_answers=%d unexpected_requests=%d", claims.Load(), dashboards.Load(), badAnswers.Load(), unexpected.Load())
	}
	t.Logf("PASS real browsers: parallel=%d credit=%v claims=%d dashboards=%d; delayed survey -> Student -> Google search -> Finish later -> distinct complete Default Keys; no manual input or provider network", parallel, credit, claims.Load(), dashboards.Load())
}
