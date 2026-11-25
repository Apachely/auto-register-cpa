const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('C:/Users/443/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright');

async function main() {
  const root = path.resolve(__dirname, '../..');
  const key = fs.readFileSync(path.join(root, 'admin-key.txt'), 'utf8').trim();
  const browser = await chromium.launch({ channel: 'chrome', headless: true });
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, reducedMotion: 'reduce' });
  const page = await context.newPage();
  const errors = [];
  const responses = [];
  page.on('pageerror', () => errors.push('page_runtime_error'));
  page.on('response', (response) => {
    if (response.url().includes('/registerbot')) {
      const url = new URL(response.url());
      responses.push({ path: url.pathname, status: response.status() });
    }
  });
  try {
    await page.goto('http://127.0.0.1:8317/management.html#/registerbot');
    await page.locator('input[name="cpa-management-key"]').fill(key);
    await page.locator('button[type="submit"]').click();
    await page.waitForURL((url) => !url.hash.includes('login'), { timeout: 30000 });
    await page.goto('http://127.0.0.1:8317/management.html#/registerbot');
    await page.locator('textarea').waitFor({ state: 'visible', timeout: 30000 });
    await page.waitForResponse((response) => response.url().includes('/registerbot/events') && response.status() === 200, { timeout: 15000 }).catch(() => {
      if (!responses.some((response) => response.path.endsWith('/events') && response.status === 200)) throw new Error('SSE did not connect');
    });
    const desktopOverflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
    await page.screenshot({ path: path.join(__dirname, 'registerbot-desktop.jpg'), type: 'jpeg', quality: 75 });
    const headings = await page.locator('h1,h2').allTextContents();
    const live = JSON.parse(fs.readFileSync(path.join(__dirname, 'verification/live-run.json'), 'utf8'));
    const runId = live.id || live.run?.id;
    if (runId) {
      const runCard = page.locator('article').filter({ hasText: runId });
      await runCard.screenshot({ path: path.join(__dirname, 'registerbot-success.jpg'), type: 'jpeg', quality: 75 });
      await page.evaluate(() => scrollTo(0, 0));
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await page.waitForTimeout(700);
    const mobileOverflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
    await page.screenshot({ path: path.join(__dirname, 'registerbot-mobile.jpg'), type: 'jpeg', quality: 75 });
    const result = { headings, desktopOverflow, mobileOverflow, pageErrors: errors, responses };
    fs.writeFileSync(path.join(__dirname, 'verification/panel-browser.json'), JSON.stringify(result, null, 2));
    console.log(JSON.stringify(result));
    if (errors.length || desktopOverflow || mobileOverflow) process.exitCode = 1;
  } finally {
    await browser.close();
  }
}
main().catch((error) => { console.error(error.message); process.exitCode = 1; });
