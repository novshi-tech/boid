const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.BOID_CHROMIUM });
  try {
    const page = await browser.newPage();
    await page.setViewportSize({ width: 375, height: 667 });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));

    let count = 0;
    let mode = 'failure';
    let body = '';
    let release;
    await page.route('http://boid.test/**', async route => {
      if (route.request().method() !== 'POST') {
        return route.fulfill({ contentType: 'text/html', body: '<body></body>' });
      }
      count++;
      body = route.request().postData();
      if (mode === 'failure') {
        await new Promise(resolve => { release = resolve; });
        return route.abort('failed');
      }
      if (mode === 'old') {
        return route.fulfill({
          contentType: 'text/html',
          body: '<section id="task-operations"><div class="operation-result" data-operation-id="old" data-operation-result="accepted">Earlier operation</div></section>',
        });
      }
      if (mode === 'pending') await new Promise(resolve => { release = resolve; });
      const rejected = mode === 'rejected';
      const started = mode === 'started';
      const id = rejected ? 'result-rejected' : `result-${count}`;
      const outcome = rejected ? 'rejected' : started ? 'started' : 'accepted';
      const label = rejected ? 'Rejected' : started ? 'Started' : 'Accepted';
      return route.fulfill({
        status: 200,
        headers: { 'X-Boid-Operation-ID': id },
        contentType: 'text/html',
        body: `<section id="task-operations"><div class="operation-result" data-operation-id="${id}" data-operation-result="${outcome}">${label}</div></section>`,
      });
    });

    await page.goto('http://boid.test/');
    await page.setContent('<div style="height: 900px" aria-hidden="true"></div><section id="task-operations"></section><div id="task-live-status"><button id="task-live-retry">Refresh</button></div><div style="height: 900px" aria-hidden="true"></div><form method="post" action="/tasks/card/commands"><textarea name="instruction">Keep my input</textarea><input name="_csrf" value="csrf-test" type="hidden"><button type="submit" name="key" value="discuss">Discuss</button></form>');
    await page.addScriptTag({ content: fs.readFileSync('web/static/boid-operation-submit.js', 'utf8') });

    async function assertFocusedAndVisible(locator, label) {
      const state = await locator.evaluate(element => {
        const rect = element.getBoundingClientRect();
        return {
          active: document.activeElement === element,
          bottom: rect.bottom,
          top: rect.top,
          viewportHeight: window.innerHeight,
        };
      });
      assert.equal(state.active, true, `${label} should receive focus`);
      assert.ok(state.top >= 0 && state.bottom <= state.viewportHeight,
        `${label} should be visible in the viewport: ${JSON.stringify(state)}`);
    }

    // A transport failure is result-unknown: preserve all submitted input.
    await page.getByText('Discuss', { exact: true }).click();
    await page.waitForFunction(() => document.querySelector('form').getAttribute('aria-busy') === 'true');
    await page.locator('form').scrollIntoViewIfNeeded();
    assert.ok(await page.evaluate(() => window.scrollY > 1200), 'the mobile form should begin well below the outcome region');
    release();
    await page.waitForFunction(() => document.querySelector('#operation-submit-feedback')?.textContent.includes('could not be confirmed'));
    await assertFocusedAndVisible(page.locator('#operation-submit-feedback'), 'unknown-result feedback');
    assert.equal(await page.locator('textarea').inputValue(), 'Keep my input');
    assert.equal(new URLSearchParams(body).get('key'), 'discuss');
    assert.equal(new URLSearchParams(body).get('_csrf'), 'csrf-test');
    assert.equal(count, 1);

    // A correlated accepted result clears the exact instruction that was sent,
    // while a second submit during the pending request remains suppressed.
    mode = 'pending';
    await page.getByText('Discuss', { exact: true }).click();
    await page.waitForFunction(() => document.querySelector('form').getAttribute('aria-busy') === 'true');
    assert.equal(await page.getByText('Discuss', { exact: true }).isDisabled(), true);
    await page.locator('form').evaluate(form => form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true })));
    assert.equal(count, 2);
    await page.locator('form').scrollIntoViewIfNeeded();
    release();
    await page.waitForFunction(() => document.querySelector('#task-operations').textContent === 'Accepted');
    await assertFocusedAndVisible(page.locator('.operation-result[data-operation-id="result-2"]'), 'accepted result');
    assert.equal(await page.locator('#operation-submit-feedback').count(), 0);
    assert.equal(await page.locator('textarea').inputValue(), '');

    // "started" is the other confirmed-success outcome and clears likewise.
    await page.locator('textarea').fill('Start this work');
    mode = 'started';
    await page.getByText('Discuss', { exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#task-operations').textContent === 'Started');
    assert.equal(await page.locator('textarea').inputValue(), '');

    // Do not erase a new draft typed while the accepted request is in flight.
    await page.locator('textarea').fill('Instruction sent to the server');
    mode = 'pending';
    await page.getByText('Discuss', { exact: true }).click();
    await page.waitForFunction(() => document.querySelector('form').getAttribute('aria-busy') === 'true');
    await page.locator('textarea').fill('New draft typed while waiting');
    release();
    await page.waitForFunction(() => document.querySelector('.operation-result')?.dataset.operationId === 'result-4');
    assert.equal(await page.locator('textarea').inputValue(), 'New draft typed while waiting');

    // A correlated rejection is known, but it did not start work: retain input.
    mode = 'rejected';
    await page.getByText('Discuss', { exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#task-operations').textContent === 'Rejected');
    assert.equal(await page.locator('textarea').inputValue(), 'New draft typed while waiting');

    // An unrelated history response cannot replace the latest correlated row.
    mode = 'old';
    await page.getByText('Discuss', { exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#operation-submit-feedback')?.textContent.includes('could not be confirmed'));
    assert.equal(await page.locator('#task-operations').textContent(), 'Rejected');
    assert.equal(await page.locator('textarea').inputValue(), 'New draft typed while waiting');
    assert.equal(count, 6);
    assert.deepEqual(errors, []);
    console.log('PASS: unknown/rejected input retention, accepted clearing, in-flight edits, CSRF/command retention, duplicate suppression and correlated responses');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
