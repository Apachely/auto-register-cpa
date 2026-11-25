package registerbot

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
)

// FreeAIDriver follows the provider's visible Google sign-in flow. Challenges
// remain resumable manual work; no challenge or identity checks are bypassed.
type FreeAIDriver struct{ settings func() Settings }

func NewFreeAIDriver(settings func() Settings) Driver { return &FreeAIDriver{settings: settings} }
func (d *FreeAIDriver) Kind() string                  { return ChannelKindFreeAIApiKey }

func (d *FreeAIDriver) Probe(ctx context.Context) error {
	set := DefaultSettings()
	if d.settings != nil {
		set = d.settings()
	}
	client, errClient := gatewayHTTPClient(set.Gateway.ProxyURL)
	if errClient != nil {
		return errClient
	}
	defer client.CloseIdleConnections()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://freeaiapikey.com/register", nil)
	if errRequest != nil {
		return errRequest
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return errors.New("registerbot: provider entry point unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return errors.New("registerbot: provider entry point returned an error")
	}
	return nil
}

type freeAIPage struct {
	URL             string `json:"url"`
	Action          string `json:"action"`
	Key             string `json:"key"`
	NetworkCode     string `json:"network_code"`
	HumanChallenge  bool   `json:"human_challenge"`
	CreditConfirmed bool   `json:"credit_confirmed"`
}

func normalizeFreeAIPage(page freeAIPage) freeAIPage {
	if page.Action == "challenge" && !page.HumanChallenge {
		parsed, errParse := url.Parse(page.URL)
		if errParse == nil && googleIdentityHost(page.URL) && (strings.HasSuffix(parsed.Path, "/challenge/pwd") || strings.HasSuffix(parsed.Path, "/challenge/password")) {
			page.Action = "loading"
		}
	}
	return page
}

func freeAIHost(raw string) bool {
	parsed, errParse := url.Parse(raw)
	return errParse == nil && (parsed.Hostname() == "freeaiapikey.com" || parsed.Hostname() == "www.freeaiapikey.com") && parsed.Scheme == "https"
}

func googleIdentityHost(raw string) bool {
	parsed, errParse := url.Parse(raw)
	return errParse == nil && parsed.Hostname() == "accounts.google.com" && parsed.Scheme == "https"
}

// Run retains an isolated page on manual handoff so the operator can resolve a
// challenge and resume the same registration without submitting credentials again.
func (d *FreeAIDriver) Run(ctx context.Context, task *Task) (harvest Harvest, runErr error) {
	if task == nil {
		return harvest, errors.New("registerbot: missing registration task")
	}
	if task.Channel.ManualMode {
		task.Report("manual", "warn", "manual completion requested")
		return harvest, ErrManualHandoff
	}
	start := strings.TrimSpace(task.Channel.StartURL)
	if start == "" {
		start = "https://freeaiapikey.com/register"
	}
	if !freeAIHost(start) {
		return harvest, errors.New("registerbot: FreeAI start URL must use the provider HTTPS origin")
	}
	set := DefaultSettings()
	if d.settings != nil {
		set = d.settings()
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	session, errOpen := task.OpenSession(acquireCtx, start)
	if errOpen != nil {
		if ctx.Err() != nil {
			return harvest, ctx.Err()
		}
		if errors.Is(errOpen, ErrManualHandoff) {
			task.Report("manual", "warn", "browser unavailable; complete registration and supply the API key")
			return harvest, ErrManualHandoff
		}
		task.Report("network_error", "warn", "browser navigation failed; queue retry follows")
		return harvest, errOpen
	}
	defer func() {
		if errors.Is(runErr, ErrManualHandoff) {
			return
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = session.CloseTarget(closeCtx)
		task.session = nil
	}()
	started := time.Now()
	lastAction := ""
	lastActionPath := ""
	lastProgress := time.Now()
	acted := false
	workspaceClicks := 0
	var lastWorkspaceClick time.Time
	credentialSubmissions := 0
	var lastCredentialSubmit time.Time
	lastCredentialIssue := ""
	sawOnboarding, creditConfirmed := false, false
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()
	for {
		if errContext := acquireCtx.Err(); errContext != nil {
			if ctx.Err() != nil {
				return harvest, ctx.Err()
			}
			task.Report("manual", "warn", "registration is awaiting operator completion")
			return harvest, ErrManualHandoff
		}
		var page freeAIPage
		_ = session.FollowAuthorizationPopup(acquireCtx)
		value, errInspect := session.Evaluate(acquireCtx, freeAIInspectScript, false)
		if errInspect == nil {
			encoded, errEncode := json.Marshal(value)
			if errEncode == nil {
				errInspect = json.Unmarshal(encoded, &page)
			}
		}
		if errInspect != nil {
			if session.client.loadErr() != nil {
				return harvest, errors.New("registerbot: browser connection lost")
			}
			page.Action = "loading"
		}
		page = normalizeFreeAIPage(page)
		if strings.HasPrefix(page.Action, "onboarding_") {
			sawOnboarding = true
			creditConfirmed = creditConfirmed || page.CreditConfirmed
		}
		actionPath := ""
		if parsed, errURL := url.Parse(page.URL); errURL == nil {
			actionPath = parsed.Hostname() + parsed.Path
		}
		if (page.Action != lastAction || actionPath != lastActionPath) && page.Action != "loading" {
			lastAction, lastProgress, acted = page.Action, time.Now(), false
			lastActionPath = actionPath
			workspaceClicks = 0
			credentialSubmissions = 0
			lastCredentialIssue = ""
			task.Report(page.Action, "info", freeAIStageMessage(page.Action))
		}
		// A dispatched click does not prove that the terms page advanced. Retry
		// briefly while it remains visible, without extending the stall budget.
		if page.Action == "speedbump" && acted && workspaceClicks < 3 && time.Since(lastWorkspaceClick) >= 2*time.Second {
			acted = false
		}
		credentialStep := page.Action == "email" || page.Action == "password"
		if credentialStep && acted && credentialSubmissions < 3 && time.Since(lastCredentialSubmit) >= 4*time.Second {
			acted = false
			task.Report(page.Action, "warn", "Google sign-in page did not advance; retrying the current form submission")
		}
		if page.Action == "key" && freeAIHost(page.URL) && keyPattern.MatchString(page.Key) {
			if sawOnboarding {
				if !creditConfirmed {
					task.Report("onboarding_credit_pending", "warn", "onboarding credit was not confirmed; complete the guide before import")
					return harvest, ErrManualHandoff
				}
				task.Report("onboarding_complete", "info", "onboarding completed and initial $5 credit confirmed")
			}
			return Harvest{APIKey: page.Key, Account: task.Account.Email, Node: task.node, ExitIP: task.exitIP, Browser: task.browserLabel(), StartedAt: started, FinishedAt: time.Now()}, nil
		}
		if page.Action == "invalid_credentials" {
			return harvest, errors.New("registerbot: Google rejected the supplied account credentials")
		}
		if page.Action == "network_error" {
			failure := navigationFailure(page.NetworkCode)
			task.Report("network_error", "warn", "browser navigation failed: "+failure.Code+"; queue retry follows")
			return harvest, failure
		}
		if page.Action == "challenge" || page.Action == "blocked" {
			task.Report("manual", "warn", "Google requires operator verification; finish in the retained browser and resume")
			return harvest, ErrManualHandoff
		}
		if !acted && errInspect == nil {
			var errAction error
			switch page.Action {
			case "onboarding_entry":
				errAction = session.Navigate(acquireCtx, "https://freeaiapikey.com/setup")
			case "onboarding_profile":
				if task.Channel.OnboardingRole == "" || task.Channel.OnboardingSource == "" {
					task.Report("onboarding_profile_required", "warn", "configure the channel onboarding answers or complete the survey in the retained browser")
					return harvest, ErrManualHandoff
				}
				errAction = session.ClickSelector(acquireCtx, `label:has(input[name=role][value="`+task.Channel.OnboardingRole+`"])`)
				if errAction == nil {
					errAction = session.ClickSelector(acquireCtx, `label:has(input[name=source][value="`+task.Channel.OnboardingSource+`"])`)
				}
				if errAction == nil {
					errAction = session.ClickText(acquireCtx, "Get my $5")
				}
			case "onboarding_credit", "onboarding_tool", "onboarding_test":
				errAction = session.ClickText(acquireCtx, "Continue")
			case "onboarding_discord":
				if !task.Channel.SkipOptionalDiscord {
					task.Report("onboarding_discord", "warn", "optional Discord step awaits the operator; enable its skip preference to continue without linking Discord")
					return harvest, ErrManualHandoff
				}
				errAction = session.ClickText(acquireCtx, "Skip for now")
			case "onboarding_plans":
				if !creditConfirmed {
					task.Report("onboarding_credit_pending", "warn", "initial $5 credit has not been confirmed; onboarding remains incomplete")
					return harvest, ErrManualHandoff
				}
				errAction = session.ClickText(acquireCtx, "Open my dashboard")
			case "onboarding_unknown":
				task.Report("onboarding_unknown", "warn", "unrecognized onboarding step; continue in the retained browser")
				return harvest, ErrManualHandoff
			case "email":
				if !googleIdentityHost(page.URL) {
					return harvest, errors.New("registerbot: refusing credentials outside Google identity origin")
				}
				errAction = session.TypeInto(acquireCtx, "#identifierId,input[name=identifier],input[type=email]", "email", task.Account.Email)
				if errAction == nil {
					errAction = session.SubmitGoogleCredential(acquireCtx, "email", task.Account.Email)
				}
			case "password":
				if !googleIdentityHost(page.URL) {
					return harvest, errors.New("registerbot: refusing credentials outside Google identity origin")
				}
				errAction = session.TypeInto(acquireCtx, "input[type=password]", "password", task.Account.Password)
				if errAction == nil {
					errAction = session.SubmitGoogleCredential(acquireCtx, "password", task.Account.Password)
				}
			case "choose_account":
				errAction = session.ClickText(acquireCtx, task.Account.Email, "Use another account", "使用其他帐号", "使用其他账号")
			case "consent":
				_ = session.ScrollToEnd(acquireCtx)
				errAction = session.ClickText(acquireCtx, "I understand", "我了解", "我知道了", "我明白了", "I agree", "我同意", "Allow", "允许", "Continue", "继续", "Accept", "接受", "Next", "下一步")
			case "speedbump":
				errAction = session.AcceptWorkspaceTerms(acquireCtx)
				if errAction == nil {
					workspaceClicks++
					lastWorkspaceClick = time.Now()
					task.Report("speedbump", "info", "Workspace terms button clicked; waiting for navigation")
				}
			case "google":
				errAction = session.ClickText(acquireCtx, "Continue with Google", "Sign in with Google", "使用 Google", "Google")
			case "reveal":
				errAction = session.ClickText(acquireCtx, "Show API key", "Show key", "Reveal", "显示", "查看密钥")
			case "dashboard":
				errAction = session.Navigate(acquireCtx, "https://freeaiapikey.com/dashboard")
			default:
				errAction = errors.New("waiting for page")
			}
			if errAction == nil {
				acted = true
				if credentialStep {
					credentialSubmissions++
					lastCredentialSubmit = time.Now()
					lastCredentialIssue = ""
					task.Report(page.Action, "info", "Google sign-in field confirmed and Next clicked; waiting for page transition")
				}
				if (!credentialStep || credentialSubmissions == 1) && (page.Action != "speedbump" || workspaceClicks == 1) {
					lastProgress = time.Now()
				}
			} else if credentialStep {
				issue := "Google sign-in control changed during interaction; waiting for the current form"
				for _, expected := range []error{errInputNotReady, errInputFocus, errInputValue, errSubmitReady} {
					if errors.Is(errAction, expected) {
						issue = expected.Error()
						break
					}
				}
				if issue != lastCredentialIssue {
					lastCredentialIssue = issue
					task.Report(page.Action, "warn", issue)
				}
			}
			var networkError *NavigationError
			if errors.As(errAction, &networkError) {
				task.Report("network_error", "warn", "browser navigation failed: "+networkError.Code+"; queue retry follows")
				return harvest, networkError
			}
		}
		if time.Since(lastProgress) >= set.Browser.StepTimeout() {
			task.Report("manual", "warn", "page did not advance; retained browser is ready for operator continuation")
			return harvest, ErrManualHandoff
		}
		select {
		case <-acquireCtx.Done():
		case <-ticker.C:
		}
	}
}

func freeAIStageMessage(stage string) string {
	switch stage {
	case "onboarding_entry":
		return "opening required onboarding"
	case "onboarding_profile":
		return "completing the configured onboarding survey"
	case "onboarding_credit":
		return "initial $5 onboarding credit confirmed"
	case "onboarding_tool":
		return "reviewing tool connection instructions"
	case "onboarding_test":
		return "reviewing API setup; authenticated inference is required before import"
	case "onboarding_discord":
		return "reviewing the optional additional Discord reward"
	case "onboarding_plans":
		return "finishing onboarding with the existing plan"
	case "email":
		return "entering Google account"
	case "password":
		return "entering Google password"
	case "choose_account":
		return "selecting Google account"
	case "consent":
		return "confirming Google consent or Workspace terms"
	case "speedbump":
		return "scrolling Workspace terms and confirming I understand"
	case "google":
		return "opening Google authorization"
	case "key":
		return "API key found; validation follows"
	case "reveal":
		return "revealing provider API key"
	case "dashboard":
		return "opening provider dashboard"
	case "challenge":
		return "additional verification requested"
	case "blocked":
		return "Google sign-in needs operator assistance"
	case "invalid_credentials":
		return "Google rejected credentials"
	case "network_error":
		return "browser network failure detected"
	default:
		return "waiting for registration page"
	}
}

const freeAIInspectScript = `(function(){
 var visible=function(e){if(!e)return false;var r=e.getBoundingClientRect();return r.width>1&&r.height>1&&getComputedStyle(e).visibility!=='hidden';};
 var has=function(s){return Array.from(document.querySelectorAll(s)).some(visible);};
 var bodyText=document.body?document.body.innerText:'';
 var text=bodyText.toLowerCase();
 var host=location.hostname, path=location.pathname, action='loading', key='', networkCode='', creditConfirmed=false;
 var humanChallenge=has('iframe[src*="recaptcha"],input[name=totpPin],input[name=idvPin]')||text.includes('verify it’s you')||text.includes("verify it's you")||text.includes('2-step verification');
 var networkMatch=bodyText.match(/\bERR_[A-Z_]+\b/);
 var chromeError=location.protocol==='chrome-error:'||location.origin==='null'||has('#main-frame-error,.neterror');
 var google=host==='accounts.google.com';
 var free=host==='freeaiapikey.com'||host==='www.freeaiapikey.com';
 if(chromeError&&networkMatch){action='network_error';networkCode=networkMatch[0];}
 else if(google){
  if(path.includes('/speedbump/')&&path.includes('workspacetermsofservice')){action='speedbump';}
  else if(text.includes('wrong password')||text.includes('密码错误')||text.includes("couldn’t find your google account")||text.includes("couldn't find your google account")){action='invalid_credentials';}
  else if(has('input[type=password]')){action='password';}
  else if(has('#identifierId,input[name=identifier],input[type=email]')){action='email';}
  else if(text.includes('this browser or app may not be secure')||text.includes('此浏览器或应用可能不安全')){action='blocked';}
  else if(path.includes('/challenge/')||humanChallenge){action='challenge';}
  else if(path.includes('speedbump')||path.includes('consent')||text.includes('i understand')||text.includes('我了解')||text.includes('我同意')||text.includes('wants access')||text.includes('sign in to')){action='consent';}
  else if(text.includes('choose an account')||text.includes('选择帐号')||text.includes('选择账号')){action='choose_account';}
  else if(Array.from(document.querySelectorAll('button,[role=button],input[type=submit]')).some(function(e){return visible(e)&&/^(continue|allow|i agree|next|继续|允许|我同意)$/i.test((e.innerText||e.value||'').trim());})){action='consent';}
 } else if(free){
  if(path==='/setup'||path.startsWith('/setup/')){
   var step=(bodyText.match(/Step\s+([1-5])\s+of\s+5/i)||[])[1];
   var normal=(bodyText.match(/Normal\s+\$([0-9,.]+)/i)||[])[1];
   creditConfirmed=!!normal&&Number(normal.replace(/,/g,''))>=5;
   if(step==='1'){action=text.includes('$5.00 is in your balance.')&&creditConfirmed?'onboarding_credit':'onboarding_profile';}
   else if(step==='2'){action='onboarding_tool';}
   else if(step==='3'){action='onboarding_test';}
   else if(step==='4'){action='onboarding_discord';}
   else if(step==='5'){action='onboarding_plans';}
   else{action='onboarding_unknown';}
  } else if(path.startsWith('/dashboard')&&has('a[href="/setup"]')){action='onboarding_entry';}
  else if(path.startsWith('/dashboard')){
   // Display text is truncated to 28 characters followed by an ellipsis. Only
   // the complete literal passed to the provider's Copy key action is usable.
   var copies=Array.from(document.querySelectorAll('button[title="Copy key"]')).filter(visible);
   for(var i=0;i<copies.length;i++){
    var copied=(copies[i].getAttribute('onclick')||'').match(/^\s*copyKey\(\s*(['"])(sk-[A-Za-z0-9_-]{16,509})\1\s*\)\s*;?\s*$/);
    if(copied){key=copied[2];break;}
   }
   if(key){action='key';}
   else if(/show api key|show key|reveal|显示|查看密钥/.test(text)){action='reveal';}
  } else if(/continue with google|sign in with google|使用 google/.test(text)){action='google';}
  else if(path!=='/register'&&path!=='/login'){action='dashboard';}
 }
 return {url:location.href,action:action,key:key,network_code:networkCode,human_challenge:humanChallenge,credit_confirmed:creditConfirmed};
})()`
