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
	PageState       string `json:"page_state"`
	Key             string `json:"key"`
	NetworkCode     string `json:"network_code"`
	HumanChallenge  bool   `json:"human_challenge"`
	CreditConfirmed bool   `json:"credit_confirmed"`
	SurveyCompleted bool   `json:"survey_completed"`
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

func selectFreeAIRadio(ctx context.Context, session *Session, name, value string) error {
	selector := "input[type=radio][name=" + jsString(name) + "][value=" + jsString(value) + "]"
	checked := "(function(){var input=document.querySelector(" + jsString(selector) + ");return !!input&&input.checked;})()"
	if current, errRead := session.Evaluate(ctx, checked, false); errRead == nil && current == true {
		return nil
	}
	if errClick := session.ClickSelector(ctx, "label:has("+selector+")"); errClick != nil {
		return errClick
	}
	current, errRead := session.Evaluate(ctx, checked, false)
	if errRead != nil {
		return errRead
	}
	if current != true {
		return errInputValue
	}
	return nil
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
	actionAttempts := 0
	var lastDispatch time.Time
	lastActionIssue := ""
	sawOnboarding, creditConfirmed := false, false
	surveyCompleted := false
	claimAttempted := false
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
		creditConfirmed = creditConfirmed || page.CreditConfirmed
		surveyCompleted = surveyCompleted || page.SurveyCompleted || page.CreditConfirmed
		// The dashboard keeps its optional setup banner after Finish later. A
		// confirmed survey must not send this same session back into that guide.
		if page.Action == "onboarding_entry" && surveyCompleted && page.Key != "" {
			page.Action = "key"
		}
		if strings.HasPrefix(page.Action, "onboarding_") {
			sawOnboarding = true
		}
		actionPath := ""
		if parsed, errURL := url.Parse(page.URL); errURL == nil {
			actionPath = parsed.Hostname() + parsed.Path + "#" + page.PageState
		}
		if (page.Action != lastAction || actionPath != lastActionPath) && page.Action != "loading" {
			lastAction, lastProgress, acted = page.Action, time.Now(), false
			lastActionPath = actionPath
			actionAttempts = 0
			lastActionIssue = ""
			task.Report(page.Action, "info", freeAIStageMessage(page.Action))
		}
		// Completion is observed through the next page state, not a CDP ack.
		// Only repeat navigation/continuation actions; never toggle a revealed
		// key or submit the onboarding credit claim twice automatically.
		credentialStep := page.Action == "email" || page.Action == "password"
		if acted && retryFreeAIAction(page.Action) && actionAttempts < 3 && time.Since(lastDispatch) >= 4*time.Second {
			acted = false
			task.Report(page.Action, "warn", "action_retry")
		}
		if page.Action == "key" && freeAIHost(page.URL) && keyPattern.MatchString(page.Key) {
			if sawOnboarding {
				if !surveyCompleted {
					task.Report("onboarding_profile_required", "warn", "survey completion was not confirmed; retain the guide before capture")
					return harvest, ErrManualHandoff
				}
				message := "required survey completed; full Default Key captured"
				if !creditConfirmed {
					message = "required survey completed; provider did not award welcome credit; full Default Key captured"
				}
				task.Report("onboarding_complete", "info", message)
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
		// The submit label/fingerprint changes while the credit request is in
		// flight. Do not interpret that change as permission to claim again.
		if page.Action == "onboarding_profile" && claimAttempted {
			acted = true
		}
		if !acted && errInspect == nil {
			var errAction error
			switch page.Action {
			case "onboarding_entry":
				errAction = session.Navigate(acquireCtx, "https://freeaiapikey.com/setup")
			case "onboarding_profile":
				role, source := task.Channel.onboardingAnswers()
				errAction = selectFreeAIRadio(acquireCtx, session, "role", role)
				if errAction == nil {
					errAction = selectFreeAIRadio(acquireCtx, session, "source", source)
				}
				if errAction == nil {
					claimAttempted = true
					errAction = session.ClickText(acquireCtx, "Get my $5")
					if errors.Is(errAction, errControlNotReady) {
						claimAttempted = false // No input was dispatched; safely wait for the control.
					}
				}
			case "onboarding_credit", "onboarding_tool", "onboarding_test", "onboarding_discord", "onboarding_plans":
				if !surveyCompleted {
					task.Report("onboarding_profile_required", "warn", "survey completion has not been confirmed; retain the survey before capture")
					return harvest, ErrManualHandoff
				}
				errAction = session.ClickText(acquireCtx, "Finish later and go to my dashboard", "Open my dashboard")
			case "onboarding_unknown":
				// Redirects can expose /setup before its controls are rendered.
				// Wait within the existing step deadline, not an immediate handoff.
				errAction = errControlNotReady
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
				errAction = session.ClickText(acquireCtx, "Continue", "继续", "Allow", "允许", "I agree", "我同意", "Accept", "接受", "Next", "下一步", "I understand", "我了解", "我知道了", "我明白了")
			case "speedbump":
				errAction = session.AcceptWorkspaceTerms(acquireCtx)
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
				actionAttempts++
				lastDispatch = time.Now()
				lastActionIssue = ""
				message := "action_dispatched"
				if credentialStep {
					message = "credential_submitted"
				}
				task.Report(page.Action, "info", message)
				if actionAttempts == 1 {
					lastProgress = time.Now()
				}
			} else if page.Action != "loading" {
				issue := "action_not_ready"
				switch {
				case errors.Is(errAction, errInputFocus):
					issue = "input_focus_pending"
				case errors.Is(errAction, errInputValue):
					issue = "input_value_pending"
				}
				if issue != lastActionIssue {
					lastActionIssue = issue
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
			if googleIdentityHost(page.URL) && !page.HumanChallenge && page.Action != "blocked" && page.Action != "challenge" {
				// A stalled ordinary sign-in page is not an identity challenge.
				// Close this context and let the queue's bounded retry policy run.
				task.Report(page.Action, "warn", "sign-in page timed out; restarting this account automatically")
				return harvest, errors.New("registerbot: ordinary sign-in page timed out")
			}
			task.Report("manual", "warn", "page did not advance; retained browser is ready for operator continuation")
			return harvest, ErrManualHandoff
		}
		select {
		case <-acquireCtx.Done():
		case <-ticker.C:
		}
	}
}

func retryFreeAIAction(action string) bool {
	switch action {
	case "email", "password", "consent", "speedbump", "choose_account", "google", "dashboard", "onboarding_entry", "onboarding_credit", "onboarding_tool", "onboarding_test", "onboarding_discord", "onboarding_plans":
		return true
	default:
		return false
	}
}

func freeAIStageMessage(stage string) string {
	switch stage {
	case "onboarding_entry":
		return "opening required onboarding"
	case "onboarding_profile":
		return "completing the configured onboarding survey"
	case "onboarding_credit":
		return "survey response confirmed; finishing the guide later and opening the dashboard"
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
 var host=location.hostname, path=location.pathname, action='loading', key='', networkCode='', creditConfirmed=false, surveyCompleted=false;
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
  var normal=(bodyText.match(/Normal(?:\s+credits)?\s+\$([0-9,.]+)/i)||[])[1];
  creditConfirmed=!!normal&&Number(normal.replace(/,/g,''))>=5;
  if(path==='/setup'||path.startsWith('/setup/')){
   var step=(bodyText.match(/\bStep\s*([1-5])\s*of\s*5\b/i)||[])[1];
   creditConfirmed=creditConfirmed||(has('#surveyDone')&&text.includes('$5.00 is in your balance.'));
   surveyCompleted=creditConfirmed||(has('#surveyDone')&&text.includes('your answers are saved'));
   if(surveyCompleted){action='onboarding_credit';}
   else if(has('#surveySubmit')||has('#surveyForm')||step==='1'){action='onboarding_profile';}
   else if(step==='2'){action='onboarding_tool';}
   else if(step==='3'){action='onboarding_test';}
   else if(step==='4'){action='onboarding_discord';}
   else if(step==='5'){action='onboarding_plans';}
   else{action='onboarding_unknown';}
  } else if(path==='/dashboard'||path.startsWith('/dashboard/')){
   // Display text is truncated to 28 characters followed by an ellipsis. Only
   // the complete literal passed to the provider's Copy key action is usable.
   var copies=Array.from(document.querySelectorAll('button[title="Copy key"]')).filter(visible);
   for(var i=0;i<copies.length;i++){
    // A dashboard may have several keys. Scope the copy action to the named
    // Default Key row, never whichever Copy key button happens to be first.
    var row=copies[i].parentElement, isDefault=false;
    for(var depth=0;row&&row!==document.body&&depth<5;depth++,row=row.parentElement){
     if(row.querySelectorAll('button[title="Copy key"]').length>1)break;
     if(Array.from(row.querySelectorAll('p')).some(function(e){return visible(e)&&/^default key$/i.test((e.innerText||'').trim());})){isDefault=true;break;}
    }
    if(!isDefault)continue;
    var copied=(copies[i].getAttribute('onclick')||'').match(/^\s*copyKey\(\s*(['"])(sk-[A-Za-z0-9_-]{16,509})\1\s*\)\s*;?\s*$/);
    if(copied){key=copied[2];break;}
   }
   if(has('a[href="/setup"]')&&!creditConfirmed){action='onboarding_entry';}
   else if(key){action='key';}
   else if(/show api key|show key|reveal|显示|查看密钥/.test(text)){action='reveal';}
  } else if(/continue with google|sign in with google|使用 google/.test(text)){action='google';}
  else if(path!=='/register'&&path!=='/login'){action='dashboard';}
 }
 // Google can replace consent screens without changing the path. Compare a
 // semantic fingerprint of headings and controls, excluding input values,
 // query strings, disabled states and transient loading indicators.
 var signature=Array.from(document.querySelectorAll('h1,h2,[role="heading"],button,[role="button"],input[type="submit"]')).filter(visible).map(function(el){return String(el.innerText||el.value||el.getAttribute('aria-label')||'').replace(/\s+/g,' ').trim().toLowerCase();}).join('|');
 var hash=2166136261;for(var s=0;s<signature.length;s++){hash=Math.imul(hash^signature.charCodeAt(s),16777619);}
 return {url:location.href,action:action,page_state:(hash>>>0).toString(16),key:key,network_code:networkCode,human_challenge:humanChallenge,credit_confirmed:creditConfirmed,survey_completed:surveyCompleted};
})()`
