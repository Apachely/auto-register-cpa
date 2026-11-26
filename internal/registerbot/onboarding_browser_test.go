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
