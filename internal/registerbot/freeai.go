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
	URL            string `json:"url"`
	Action         string `json:"action"`
	Key            string `json:"key"`
	NetworkCode    string `json:"network_code"`
	HumanChallenge bool   `json:"human_challenge"`
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
	lastProgress := time.Now()
	acted := false
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
		if page.Action != lastAction && page.Action != "loading" {
			lastAction, lastProgress, acted = page.Action, time.Now(), false
			task.Report(page.Action, "info", freeAIStageMessage(page.Action))
		}
		if page.Action == "key" && freeAIHost(page.URL) && keyPattern.MatchString(page.Key) {
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
			case "email":
				if !googleIdentityHost(page.URL) {
					return harvest, errors.New("registerbot: refusing credentials outside Google identity origin")
				}
				errAction = session.TypeInto(acquireCtx, "#identifierId,input[name=identifier],input[type=email]", "email", task.Account.Email)
				if errAction == nil {
					errAction = session.ClickText(acquireCtx, "Next", "下一步", "继续")
				}
			case "password":
				if !googleIdentityHost(page.URL) {
					return harvest, errors.New("registerbot: refusing credentials outside Google identity origin")
				}
				errAction = session.TypeInto(acquireCtx, "input[type=password]", "password", task.Account.Password)
				if errAction == nil {
					errAction = session.ClickText(acquireCtx, "Next", "下一步", "继续")
				}
			case "choose_account":
				errAction = session.ClickText(acquireCtx, task.Account.Email, "Use another account", "使用其他帐号", "使用其他账号")
			case "consent":
				_ = session.ScrollToEnd(acquireCtx)
				errAction = session.ClickText(acquireCtx, "I understand", "我了解", "我知道了", "我明白了", "I agree", "我同意", "Allow", "允许", "Continue", "继续", "Accept", "接受", "Next", "下一步")
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
				lastProgress = time.Now()
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
	case "email":
		return "entering Google account"
	case "password":
		return "entering Google password"
	case "choose_account":
		return "selecting Google account"
	case "consent":
		return "confirming Google consent or Workspace terms"
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
 var host=location.hostname, path=location.pathname, action='loading', key='', networkCode='';
 var humanChallenge=has('iframe[src*="recaptcha"],input[name=totpPin],input[name=idvPin]')||text.includes('verify it’s you')||text.includes("verify it's you")||text.includes('2-step verification');
 var networkMatch=bodyText.match(/\bERR_[A-Z_]+\b/);
 var chromeError=location.protocol==='chrome-error:'||location.origin==='null'||has('#main-frame-error,.neterror');
 var google=host==='accounts.google.com';
 var free=host==='freeaiapikey.com'||host==='www.freeaiapikey.com';
 if(chromeError&&networkMatch){action='network_error';networkCode=networkMatch[0];}
 else if(google){
  if(text.includes('wrong password')||text.includes('密码错误')||text.includes("couldn’t find your google account")||text.includes("couldn't find your google account")){action='invalid_credentials';}
  else if(has('input[type=password]')){action='password';}
  else if(has('#identifierId,input[name=identifier],input[type=email]')){action='email';}
  else if(text.includes('this browser or app may not be secure')||text.includes('此浏览器或应用可能不安全')){action='blocked';}
  else if(path.includes('/challenge/')||humanChallenge){action='challenge';}
  else if(path.includes('speedbump')||path.includes('consent')||text.includes('i understand')||text.includes('我了解')||text.includes('我同意')||text.includes('wants access')||text.includes('sign in to')){action='consent';}
  else if(text.includes('choose an account')||text.includes('选择帐号')||text.includes('选择账号')){action='choose_account';}
  else if(Array.from(document.querySelectorAll('button,[role=button],input[type=submit]')).some(function(e){return visible(e)&&/^(continue|allow|i agree|next|继续|允许|我同意)$/i.test((e.innerText||e.value||'').trim());})){action='consent';}
 } else if(free){
  if(path.startsWith('/dashboard')){
   var content=(document.body?document.body.innerText:'')+'\n'+Array.from(document.querySelectorAll('input,textarea')).map(function(e){return e.value||'';}).join('\n');
   var match=content.match(/sk-[A-Za-z0-9_-]{16,}/);
   if(!match){var scripts=Array.from(document.scripts).map(function(e){return e.textContent||'';}).join('\n');var assigned=scripts.match(/keyValue\s*=\s*["'](sk-[A-Za-z0-9_-]{16,})["']/);if(assigned)match=assigned;}
   if(match){key=match[1]||match[0];action='key';}
   else if(/show api key|show key|reveal|显示|查看密钥/.test(text)){action='reveal';}
  } else if(/continue with google|sign in with google|使用 google/.test(text)){action='google';}
  else if(path!=='/register'&&path!=='/login'){action='dashboard';}
 }
 return {url:location.href,action:action,key:key,network_code:networkCode,human_challenge:humanChallenge};
})()`
