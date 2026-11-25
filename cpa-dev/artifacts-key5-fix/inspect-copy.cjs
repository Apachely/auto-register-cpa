const fs = require('node:fs');
const path = require('node:path');
const readline = require('node:readline');
const { chromium } = require('C:/Users/443/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright');
const root = path.resolve(__dirname, '../..');
const source = fs.readFileSync(path.join(root, 'cpa-dev/cpa-backend/internal/registerbot/freeai.go'), 'utf8');
const start = source.indexOf('const freeAIInspectScript = `') + 'const freeAIInspectScript = `'.length;
const inspect = source.slice(start, source.indexOf('`', start));
const redact = (value) => value.replace(/sk-[A-Za-z0-9_.~+/=-]+/g, (key) => `[key length=${key.length}]`).replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/g, '[account]');
(async () => {
  const credential = process.env.CPA_REGISTERBOT_ACCOUNT;
  if (!credential) throw new Error('Missing process credential');
  delete process.env.CPA_REGISTERBOT_ACCOUNT;
  const split = credential.indexOf('|');
  const account = credential.slice(0, split), password = credential.slice(split + 1);
  const metadata = JSON.parse(fs.readFileSync(path.join(root, 'registerbot/browser-0.json'), 'utf8'));
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${metadata.port}`);
  const context = await browser.newContext({ proxy: { server: 'http://127.0.0.1:12450' } });
  let page = await context.newPage();
  await page.goto('https://freeaiapikey.com/register', { waitUntil: 'domcontentloaded' });
  let previous = '', actedFor = '';
  for (let step = 0; step < 160; step++) {
    const pages = context.pages().filter((item) => !item.isClosed());
    page = pages.find((item) => item.url().includes('accounts.google.com')) || pages.find((item) => item.url().includes('/dashboard')) || page;
    if (page.url().startsWith('https://freeaiapikey.com/dashboard') || page.url().startsWith('https://www.freeaiapikey.com/dashboard')) break;
    let state;
    try { state = await page.evaluate(inspect); } catch { await new Promise((resolve) => setTimeout(resolve, 500)); continue; }
    if (state.action !== previous) { console.log(JSON.stringify({stage:state.action})); previous = state.action; actedFor = ''; }
    if (actedFor === state.action) { await new Promise((resolve) => setTimeout(resolve, 700)); continue; }
    actedFor = state.action;
    try {
    if (state.action === 'email' && new URL(page.url()).hostname === 'accounts.google.com') {
      await page.locator('#identifierId,input[name=identifier],input[type=email]').filter({visible:true}).first().fill(account);
      await page.getByRole('button', {name:/^(Next|下一步|继续)$/}).click();
    } else if (state.action === 'password' && new URL(page.url()).hostname === 'accounts.google.com') {
      await page.locator('input[type=password]').filter({visible:true}).first().fill(password);
      await page.getByRole('button', {name:/^(Next|下一步|继续)$/}).click();
    } else if (state.action === 'google') {
      await page.getByText(/Continue with Google|Sign in with Google/, {exact:false}).first().click();
    } else if (state.action === 'consent') {
      const next = page.getByRole('button', {name:/^(I understand|我了解|I agree|Allow|Continue|继续|允许|Accept|Next|下一步)$/}).first();
      if (await next.isVisible()) await next.click();
    } else if (['challenge','blocked','invalid_credentials'].includes(state.action)) {
      if (!page.url().includes('/challenge/pwd')) throw new Error(`Manual step required: ${state.action}`);
    }
    } catch (error) {
      if (!new URL(page.url()).pathname.startsWith('/dashboard')) throw error;
    }
    await new Promise((resolve) => setTimeout(resolve, 700));
  }
  if (!new URL(page.url()).pathname.startsWith('/dashboard')) throw new Error('Dashboard not reached');
  const observe = async () => {
  const observation = await page.evaluate(() => ({
    url:location.origin+location.pathname,
    text:(document.body?.innerText || '').slice(0,7000),
    buttons:Array.from(document.querySelectorAll('button')).map((element,index)=>({index,text:element.innerText,title:element.title,label:element.getAttribute('aria-label'),onclick:element.getAttribute('onclick'),data:Object.fromEntries(Object.entries(element.dataset))})),
    keyElements:Array.from(document.querySelectorAll('code,input')).filter((element)=>(element.value||element.textContent||'').includes('sk-')).map((element)=>({tag:element.tagName,id:element.id,text:element.value||element.textContent})),
    keyValueDefined:typeof keyValue !== 'undefined',
    keyValueLength:typeof keyValue === 'string'?keyValue.length:0
  }));
  console.log('DASHBOARD '+redact(JSON.stringify(observation)));
  };
  if (process.env.CPA_KEY5_REPAIR !== '1') await observe();
  if (process.env.CPA_KEY5_REPAIR === '1') {
    const admin = fs.readFileSync(path.join(root,'admin-key.txt'),'utf8').trim();
    const management = 'http://127.0.0.1:8317/v8/management';
    const headers = {Authorization:`Bearer ${admin}`,'Content-Type':'application/json'};
    const read = async()=>{
      const response=await fetch(management+'/config/api-keys/openai-compatibility',{headers});
      if(response.status!==200)throw new Error('Configuration read failed');
      return {groups:await response.json(),etag:response.headers.get('etag')};
    };
    const before=await read();
    const provider=before.groups.find((entry)=>entry.name==='FreeAi');
    if(!provider||provider.keys.length!==5)throw new Error('Provider layout changed');
    const old=provider.keys[4]['api-key'];
    const copiedCandidates=await page.evaluate(()=>Array.from(document.querySelectorAll('button')).map((element,index)=>({index,code:element.getAttribute('onclick')})).filter((entry)=>entry.code?.startsWith('copyKey(')));
    const matches=copiedCandidates.flatMap(({index,code})=>{const match=code.match(/^copyKey\(['"](sk-[A-Za-z0-9_-]+)['"]\)/);return match&&match[1].startsWith(old)&&match[1].length>old.length?[{index,key:match[1]}]:[];});
    if(matches.length!==1)throw new Error('Expected one complete copy value matching the truncated key');
    const balance=await page.evaluate(()=>({text:document.body.innerText,onboardingPending:!!document.querySelector('a[href="/setup"]')}));
    if(balance.onboardingPending)throw new Error('Onboarding is not complete');
    await page.evaluate(()=>{window.__cpaCopiedKey='';navigator.clipboard.writeText=async(value)=>{window.__cpaCopiedKey=String(value);};});
    await page.locator('button').nth(matches[0].index).click();
    const full=await page.evaluate(()=>window.__cpaCopiedKey);
    if(full!==matches[0].key)throw new Error('Copied value does not match the selected key');
    require('node:child_process').execFileSync('C:/Users/443/.cache/codex-runtimes/codex-primary-runtime/dependencies/native/powershell/pwsh.exe',['-NoProfile','-NonInteractive','-Command',"$value=[Console]::In.ReadToEnd(); $secure=ConvertTo-SecureString -String $value -AsPlainText -Force; ConvertFrom-SecureString $secure | Set-Content -LiteralPath 'E:\\LY\\Tool\\CPA\\registerbot\\key5-repair.dpapi' -Encoding ASCII"],{input:full,windowsHide:true,stdio:['pipe','pipe','pipe']});
    console.log(JSON.stringify({completeCopyProtected:true,length:full.length,oldWasExactPrefix:full.startsWith(old)}));
    const probe=async()=>{
      const response=await fetch(management+'/requests/api-call',{method:'POST',headers,body:JSON.stringify({method:'POST',url:provider['base-url'].replace(/\/$/,'')+'/chat/completions',proxy_url:'http://127.0.0.1:12450',header:{Authorization:`Bearer ${full}`,'Content-Type':'application/json'},data:JSON.stringify({model:provider.models[0].name,messages:[{role:'user',content:'Hi'}],stream:false,max_tokens:5})})});
      const result=await response.json();let body;try{body=JSON.parse(result.body);}catch{}
      return {http:result.status_code,choices:body?.choices?.length||0,error:typeof body?.error==='string'?body.error:body?.error?.message,htmlTitle:typeof result.body==='string'?(result.body.match(/<title>([^<]+)<\/title>/i)||[])[1]:undefined};
    };
    let verified;
    for(let attempt=1;attempt<=3;attempt++){
      verified=await probe();console.log('PROBE '+redact(JSON.stringify({attempt,...verified})));
      if(verified.http===200&&verified.choices>0)break;
      if(verified.http<500)break;
      await new Promise((resolve)=>setTimeout(resolve,2000));
    }
    if(verified.http!==200||verified.choices<1)throw new Error(`Complete key inference rejected: HTTP ${verified.http}`);
    const fresh=await read();
    if(fresh.etag!==before.etag)throw new Error('Configuration changed; no write performed');
    const target=fresh.groups.find((entry)=>entry.name==='FreeAi');
    if(target.keys[4]['api-key']!==old)throw new Error('Fifth key changed; no write performed');
    target.keys[4]={...target.keys[4],'api-key':full};
    const write=await fetch(management+'/config/api-keys/openai-compatibility',{method:'PUT',headers:{...headers,'If-Match':fresh.etag},body:JSON.stringify(fresh.groups)});
    if(write.status!==200)throw new Error(`Protected replacement rejected: HTTP ${write.status}`);
    const after=await read();const current=after.groups.find((entry)=>entry.name==='FreeAi');
    if(current.keys.length!==5||current.keys[4]['api-key']!==full||current.keys.slice(0,4).some((entry,index)=>entry['api-key']!==provider.keys[index]['api-key']))throw new Error('Provider readback mismatch');
    const result={replacedIndex:5,oldLength:old.length,newLength:full.length,oldWasExactPrefix:full.startsWith(old),keyCount:current.keys.length,firstFourUnchanged:true,model:provider.models[0].name,proxy:current.keys[4]['proxy-url'],inference:verified,onboardingCompleted:!balance.onboardingPending,normalCreditsBeforeTest:(balance.text.match(/NORMAL CREDITS\s*\$([\d.]+)/)||[])[1],hint:full.slice(0,3)+'…'+full.slice(-4)};
    fs.writeFileSync(path.join(__dirname,'repair-result.json'),JSON.stringify(result,null,2));
    console.log('REPAIRED '+JSON.stringify(result));await context.close();process.exit(0);
  }
  console.log('READY_FOR_COPY_INDEX');
  const lines = readline.createInterface({input:process.stdin,terminal:false});
  for await (const line of lines) {
    const command = line.trim();
    if (command === 'close') { await context.close(); process.exit(0); }
    if (command === 'inspect') {await observe();continue;}
    if (command.startsWith('click ')) {
      const button=page.locator('button').nth(Number(command.slice(6)));
      const label=await button.innerText();
      if(/skip|revoke|delete|new key/i.test(label)) throw new Error('Action is outside onboarding completion');
      await button.click();
      await new Promise((resolve)=>setTimeout(resolve,600));
      await observe();
      continue;
    }
    if (!command.startsWith('copy ')) continue;
    const index = Number(command.slice(5));
    await page.evaluate(() => {
      window.__cpaCopiedKey = '';
      navigator.clipboard.writeText = async (value) => {window.__cpaCopiedKey = String(value);};
    });
    await page.locator('button').nth(index).click();
    const full = await page.evaluate(() => window.__cpaCopiedKey);
    if (typeof full !== 'string' || !/^sk-[A-Za-z0-9_-]{16,509}$/.test(full)) throw new Error('Copy did not yield an entire API key');
    // Persist only a current-user DPAPI blob while the configuration repair is verified.
    const { execFileSync } = require('node:child_process');
    execFileSync('powershell.exe',['-NoProfile','-NonInteractive','-Command',"$value=[Console]::In.ReadToEnd(); $secure=ConvertTo-SecureString -String $value -AsPlainText -Force; ConvertFrom-SecureString $secure | Set-Content -LiteralPath 'E:\\LY\\Tool\\CPA\\registerbot\\key5-repair.dpapi' -Encoding ASCII"],{input:full,windowsHide:true,stdio:['pipe','pipe','pipe']});
    console.log(JSON.stringify({copied:true,length:full.length,hint:full.slice(0,3)+'…'+full.slice(-4)}));
    await context.close();
    process.exit(0);
  }
})().catch((error)=>{console.error(redact(error.message.split('\n')[0]));process.exit(1);});
