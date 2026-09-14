const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');

(async () => {
  const dir = process.env.BOID_UI_FIXTURE_DIR;
  const browser = await chromium.launch({headless: true, executablePath: process.env.BOID_CHROMIUM});
  try {
    const page = await browser.newPage();
    await page.route('**/*', route => {
      const path = new URL(route.request().url()).pathname;
      if (path === '/') return route.fulfill({contentType: 'text/html', body: fs.readFileSync(dir + '/card-list.html', 'utf8')});
      if (path === '/static/style.css') return route.fulfill({contentType: 'text/css', body: fs.readFileSync(dir + '/static/style.css', 'utf8')});
      return route.fulfill({status: 404, body: ''});
    });
    for (const width of [320, 375, 1024]) {
      await page.setViewportSize({width, height: 850});
      await page.goto('http://boid.test/');
      for (const scale of [1, 2]) {
        await page.evaluate(scale => { document.documentElement.style.fontSize = (16 * scale) + 'px'; }, scale);
        for (const [id, text] of [['awaiting', '入力待ち'], ['discuss', 'Discuss'], ['go', '稼働中']]) {
          const row = page.locator(`[data-task-id="${id}"]`);
          const badge = row.locator('.list-row-execution');
          assert.ok(await badge.isVisible());
          assert.ok((await badge.textContent()).includes(text));
          const bounds = await badge.boundingBox();
          const summary = await row.locator('.list-row-line3').boundingBox();
          assert.ok(bounds.y + bounds.height <= summary.y + 1, `badge overlaps summary at ${width}px / ${scale}`);
        }
        assert.equal(await page.locator('[data-task-id="idle"] .list-row-execution').count(), 0);
        assert.equal(await page.locator('[data-task-id="awaiting"] .list-row-execution-running').count(), 0);
        assert.equal(await page.locator('.list-row-rollup').count(), 0);
        // Scope enlarged-text overflow to the list; the existing site nav
        // overflows at 320px / 200% independently of the card rows.
        const fits = await page.locator('#task-list-rows').evaluate(list => {
          const bounds = list.getBoundingClientRect();
          return bounds.right <= innerWidth && list.scrollWidth <= list.clientWidth;
        });
        assert.ok(fits, `card list overflow at ${width}px / ${scale}`);
        if (width === 375 && scale === 1) await page.screenshot({path: dir + '/card-list-375.png', fullPage: true});
      }
    }
    console.log('PASS: Card execution states remain visible with long summaries at mobile/desktop widths and 200% text size');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
