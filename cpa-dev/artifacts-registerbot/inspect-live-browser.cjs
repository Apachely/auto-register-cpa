const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('C:/Users/443/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright');
(async () => {
  const root = path.resolve(__dirname, '../..');
  const metadata = JSON.parse(fs.readFileSync(path.join(root, 'registerbot/browser-0.json'), 'utf8'));
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${metadata.port}`);
  const redact = (value) => value.replace(/sk-[A-Za-z0-9_-]{16,}/g, '[redacted-key]').replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/g, '[account]');
  for (const context of browser.contexts()) for (const page of context.pages()) {
    if (page.url() === 'about:blank') continue;
    const observation = await page.evaluate(() => ({
      url: location.origin + location.pathname,
      title: document.title,
      text: (document.body?.innerText || '').slice(0, 4000),
      buttons: Array.from(document.querySelectorAll('button,[role=button],a')).map((el) => el.textContent?.trim()).filter(Boolean).slice(0, 25),
      inputs: Array.from(document.querySelectorAll('input')).map((el) => ({type:el.type,name:el.name,id:el.id}))
    }));
    console.log(redact(JSON.stringify(observation)));
  }
  // Exit only disconnects this diagnostic client; the engine owns Chrome.
  process.exit(0);
})().catch((error) => {console.error(error.message);process.exit(1);});
