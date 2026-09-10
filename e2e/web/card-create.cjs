const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');

(async () => {
  const dir = process.env.BOID_UI_FIXTURE_DIR;
  const browser = await chromium.launch({headless: true, executablePath: process.env.BOID_CHROMIUM});
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/*', route => {
      const path = new URL(route.request().url()).pathname;
      if (path === '/tasks/new') return route.fulfill({contentType: 'text/html', body: fs.readFileSync(dir + '/card-new.html', 'utf8')});
      if (path === '/static/style.css') return route.fulfill({contentType: 'text/css', body: fs.readFileSync(dir + '/static/style.css', 'utf8')});
      return route.fulfill({status: 404, body: ''});
    });
    for (const width of [320, 375, 1024]) {
      await page.setViewportSize({width, height: 850});
      await page.goto('http://boid.test/tasks/new');
      assert.equal(await page.locator('#workspace').inputValue(), 'default');
      assert.equal(await page.locator('#project_id option:enabled').count(), 1);
      await page.locator('#title').fill('相談したいこと');
      await page.locator('#description').fill('まだ具体化できていない目的');
      await page.locator('#workspace').selectOption('team');
      assert.equal(await page.locator('#project_id option:enabled').count(), 2);
      assert.equal(await page.locator('#project_id option:checked').getAttribute('data-default'), 'true');
      await page.locator('#project_id').selectOption('custom');
      const data = await page.locator('form').evaluate(form => Object.fromEntries(new FormData(form)));
      assert.deepEqual(data, {title: '相談したいこと', workspace: 'team', project_id: 'custom', description: 'まだ具体化できていない目的'});
      await page.locator('#workspace').selectOption('default');
      assert.equal(await page.locator('#project_id option:checked').getAttribute('data-workspace'), 'default');
      assert.equal(await page.locator('#title').inputValue(), '相談したいこと');
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `overflow at ${width}px`);
      if (width === 375) await page.screenshot({path: dir + '/card-new-375.png', fullPage: true});
    }
    assert.deepEqual(errors, []);
    console.log('PASS: Card creation fields, workspace filtering, default selection, preserved input, and mobile layout');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
