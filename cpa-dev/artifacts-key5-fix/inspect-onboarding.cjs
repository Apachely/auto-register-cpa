const fs = require('node:fs');
const path = require('node:path');
const {chromium} = require('C:/Users/443/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright');
const root = path.resolve(__dirname,'../..');
(async()=>{
 const meta=JSON.parse(fs.readFileSync(path.join(root,'registerbot/browser-0.json'),'utf8'));
 const browser=await chromium.connectOverCDP(`http://127.0.0.1:${meta.port}`);
 const pages=browser.contexts().flatMap((context)=>context.pages()).filter((page)=>page.url().startsWith('https://freeaiapikey.com/'));
 if(pages.length!==1)throw new Error('Expected one managed provider page');
 const page=pages[0];
 if(process.argv[2]==='open') await page.locator('a[href="/setup"]').click();
 if(process.argv[2]==='profile') {
  await page.getByText('Developer at a company',{exact:true}).click();
  await page.getByText('Google search',{exact:true}).click();
  await page.getByRole('button',{name:'Get my $5',exact:true}).click();
  await page.waitForFunction(()=>document.body.innerText.includes('$5.00 is in your balance.'),{},{timeout:15000});
 }
 if(process.argv[2]==='next') { await page.getByRole('button',{name:'Continue',exact:true}).click(); await page.waitForTimeout(500); }
 if(process.argv[2]==='discord-optional') { await page.getByRole('button',{name:'Skip for now',exact:true}).click(); await page.waitForTimeout(500); }
 if(process.argv[2]==='complete') { await page.getByRole('button',{name:'Open my dashboard',exact:true}).click(); await page.waitForURL('**/dashboard'); }
 const info=await page.evaluate(()=>({
  url:location.origin+location.pathname,text:(document.body?.innerText||'').slice(0,7000),
  links:Array.from(document.querySelectorAll('a')).filter((element)=>/\$5|onboard|credit|start|continue/i.test(element.innerText)).map((element)=>({text:element.innerText,href:new URL(element.href).pathname})),
  forms:Array.from(document.forms).map((form)=>({id:form.id,action:new URL(form.action).pathname,method:form.method,fields:Array.from(form.querySelectorAll('input,select,textarea')).map((element)=>({tag:element.tagName,name:element.name,type:element.type,placeholder:element.placeholder,value:['radio','checkbox'].includes(element.type)?element.value:undefined,options:element.options?Array.from(element.options).map((option)=>({value:option.value,text:option.text})):undefined}))})),
  buttons:Array.from(document.querySelectorAll('button')).filter((element)=>{const r=element.getBoundingClientRect();return r.width>0&&r.height>0;}).map((element)=>({text:element.innerText,title:element.title,type:element.type})),
  copies:Array.from(document.querySelectorAll('button')).map((element,index)=>({index,code:element.getAttribute('onclick')})).filter((entry)=>entry.code?.startsWith('copyKey('))
 }));
 const admin=fs.readFileSync(path.join(root,'admin-key.txt'),'utf8').trim();
 const response=await fetch('http://127.0.0.1:8317/v8/management/config/api-keys/openai-compatibility',{headers:{Authorization:`Bearer ${admin}`}});
 const provider=(await response.json()).find((entry)=>entry.name==='FreeAi');
 const old=provider.keys[4]['api-key'];
 info.copies=info.copies.map(({index,code})=>{const match=code.match(/^copyKey\(['"]([^'"]+)['"]\)/);return {index,keyLength:match?.[1].length,prefixMatchesOld:match?.[1].startsWith(old)};});
 const text=JSON.stringify(info).replace(/sk-[A-Za-z0-9_.~+/=-]+/g,(key)=>`[key length=${key.length}]`).replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/g,'[account]');
 console.log(text);process.exit(0);
})().catch((error)=>{console.error(error.message.split('\n')[0]);process.exit(1);});
