// Run with BOID_PLAYWRIGHT_MODULE pointing at an installed playwright package.
const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');
const source = fs.readFileSync('web/templates/tasks.templ', 'utf8');
const component = source.slice(source.indexOf('templ TaskDetailLiveScript()'));
const script = component.match(/<script>([\s\S]*?)<\/script>/)[1];
(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.BOID_CHROMIUM });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    let fail = false;
    let revision = 0;
    await page.route('http://boid.test/**', async route => {
      const url = new URL(route.request().url());
      if (url.pathname.includes('/fragment')) {
        if (fail) return route.fulfill({ status: 503, body: 'Unavailable' });
        const kind = url.searchParams.get('kind');
        return route.fulfill({ contentType: 'text/html', body: `<div id="task-${kind}" data-task-id="test">Revision ${revision}<details class="description"><summary>Description</summary>Text</details></div>` });
      }
      return route.fulfill({ contentType: 'text/html', body: '<html><body></body></html>' });
    });
    await page.goto('http://boid.test/');
    await page.setContent(`<div id="task-status" data-task-id="test"></div><div id="task-pinned"></div><div id="task-timeline"></div><div id="task-live-status"><span id="task-live-message"></span><button id="task-live-retry">Refresh</button></div><textarea id="input">Keep this input</textarea>`);
    await page.evaluate(() => {
      window.EventSource = class {
        static CLOSED = 2;
        constructor() { this.readyState = 1; this.listeners = {}; window.stream = this; }
        addEventListener(name, fn) { this.listeners[name] = fn; }
        close() { this.readyState = 2; }
      };
    });
    await page.addScriptTag({ content: script });
    await page.evaluate(() => window.stream.listeners.open());
    await page.waitForFunction(() => document.querySelector('#task-status').textContent.includes('Revision 0'));
    await page.locator('#task-status details').evaluate(el => el.open = true);
    fail = true;
    await page.locator('#task-live-retry').click();
    await page.waitForFunction(() => document.querySelector('#task-live-status').dataset.stale === 'true');
    assert.match(await page.locator('#task-live-message').textContent(), /Updates interrupted/);
    assert.equal(await page.locator('#input').inputValue(), 'Keep this input');
    assert.equal(await page.locator('#task-status details').evaluate(el => el.open), true);
    fail = false;
    revision = 1;
    await page.locator('#task-live-retry').click();
    await page.waitForFunction(() => document.querySelector('#task-status').textContent.includes('Revision 1'));
    await page.waitForFunction(() => document.querySelector('#task-live-status').dataset.stale === 'false');
    assert.equal(await page.locator('#task-status details').evaluate(el => el.open), true);
    await page.evaluate(() => window.stream.listeners.error());
    assert.equal(await page.locator('#task-live-status').getAttribute('data-stale'), 'true');
    revision = 2;
    await page.evaluate(() => window.stream.listeners.open());
    await page.waitForFunction(() => document.querySelector('#task-status').textContent.includes('Revision 2'));
    assert.equal(await page.locator('#task-live-status').getAttribute('data-stale'), 'false');
    assert.deepEqual(errors, []);
    console.log('PASS: HTTP failure, retry recovery, SSE disconnect/reconnect, input and expansion preservation');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
