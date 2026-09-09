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
    let fail = false, failKind = '', redirected = false;
    let revision = 0;
    let pinnedResponses = 0;
    await page.route('http://boid.test/**', async route => {
      const url = new URL(route.request().url());
      if (url.pathname.includes('/card-timeline/head')) return route.fulfill({ contentType: 'text/html', body: '' });
      if (url.pathname.includes('/fragment')) {
        if (redirected) return route.fulfill({ status: 302, headers: { location: '/login' } });
        const kind = url.searchParams.get('kind');
        if (fail || kind === failKind) return route.fulfill({ status: 503, body: 'Unavailable' });
        if (kind === 'pinned') {
          pinnedResponses++;
          return route.fulfill({ contentType: 'text/html', body: `<div id="task-pinned" data-response="${pinnedResponses}"><form method="post" action="/tasks/test/suggestion"><input type="hidden" name="answer" value="accept"><input type="hidden" name="verb" value="go"><input type="hidden" name="basis" value="current"><button type="submit">Accept</button></form></div>` });
        }
        return route.fulfill({ contentType: 'text/html', body: `<div id="task-${kind}" data-task-id="test">Revision ${revision}<details class="description"><summary>Description</summary>Text</details></div>` });
      }
      return route.fulfill({ contentType: 'text/html', body: '<html><body></body></html>' });
    });
    await page.goto('http://boid.test/');
    await page.setContent(`<div id="task-status" data-task-id="test"></div><div id="task-pinned"></div><div id="task-timeline"></div><div id="task-controls"></div><div id="task-live-status"><span id="task-live-message"></span><button id="task-live-retry">Refresh</button></div><section id="card-timeline"><div class="tab-empty">No history yet.</div></section><textarea id="input">Keep this input</textarea>`);
    await page.evaluate(() => {
      window.EventSource = class {
        static CONNECTING = 0;
        static OPEN = 1;
        static CLOSED = 2;
        constructor() {
          this.readyState = EventSource.CONNECTING;
          this.listeners = {};
          window.streams = window.streams || [];
          window.streams.push(this);
          window.stream = this;
        }
        addEventListener(name, fn) { this.listeners[name] = fn; }
        close() { this.readyState = 2; }
        emit(name) {
          if (name === 'open') this.readyState = EventSource.OPEN;
          if (name === 'error') this.readyState = EventSource.CONNECTING;
          this.listeners[name]();
        }
      };
    });
    await page.addScriptTag({ content: script });
    await page.evaluate(() => window.stream.emit('open'));
    await page.waitForFunction(() => document.querySelector('#task-status').textContent.includes('Revision 0'));
    await page.locator('#task-status details').evaluate(el => el.open = true);

    // A focused suggestion control must survive a pinned fragment replacement.
    await page.locator('#task-pinned button').focus();
    const pinnedBefore = await page.locator('#task-pinned').getAttribute('data-response');
    revision = 1;
    await page.evaluate(() => window.stream.emit('job'));
    await page.waitForFunction(previous => document.querySelector('#task-pinned')?.dataset.response !== previous, pinnedBefore);
    assert.equal(await page.evaluate(() => document.activeElement?.closest('#task-pinned') !== null), true);

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
    await page.evaluate(() => {
      window.oldStream = window.stream;
      window.oldStream.emit('error');
    });
    assert.equal(await page.locator('#task-live-status').getAttribute('data-stale'), 'true');
    revision = 2;
    await page.locator('#task-live-retry').click();
    assert.equal(await page.evaluate(() => window.stream !== window.oldStream), true);
    assert.equal(await page.evaluate(() => window.oldStream.readyState), 2);
    await page.evaluate(() => window.oldStream.emit('open'));
    assert.equal(await page.locator('#task-live-status').getAttribute('data-stale'), 'true');
    await page.evaluate(() => window.stream.emit('open'));
    await page.waitForFunction(() => document.querySelector('#task-status').textContent.includes('Revision 2'));
    assert.equal(await page.locator('#task-live-status').getAttribute('data-stale'), 'false');
    await page.evaluate(() => window.oldStream.emit('error'));
    assert.equal(await page.locator('#task-live-status').getAttribute('data-stale'), 'false');
    redirected = true;
    await page.locator('#task-live-retry').click();
    await page.waitForFunction(() => document.querySelector('#task-live-status').dataset.stale === 'true');
    assert.match(await page.locator('#task-status').textContent(), /Revision 2/);
    redirected = false;
    await page.locator('#task-live-retry').click();
    await page.waitForFunction(() => document.querySelector('#task-live-status').dataset.stale === 'false');
    assert.equal(await page.locator('#card-timeline .tab-empty').textContent(), 'No history yet.');

    // An empty history response clears rows that have moved into current work.
    await page.locator('#card-timeline').evaluate(el => {
      el.innerHTML = '<ul class="card-timeline-list"><li class="card-timeline-item">Old receipt</li></ul>';
    });
    await page.locator('#task-live-retry').click();
    await page.waitForFunction(() => document.querySelectorAll('#card-timeline .card-timeline-item').length === 0);

    // Once an HTMX tab swap removes Timeline, its old failure must no longer
    // make successfully refreshed visible sections appear stale.
    failKind = 'timeline';
    await page.locator('#task-live-retry').click();
    await page.waitForFunction(() => document.querySelector('#task-live-status').dataset.stale === 'true');
    failKind = '';
    await page.locator('#task-timeline').evaluate(el => el.remove());
    await page.evaluate(() => document.dispatchEvent(new CustomEvent('htmx:afterSwap')));
    await page.waitForFunction(() => document.querySelector('#task-live-status').dataset.stale === 'false');
    assert.deepEqual(errors, []);
    console.log('PASS: failures/recovery, focus and expansion preservation, controls refresh, removed Timeline, auth redirect and empty history');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
