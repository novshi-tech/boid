// Generate with TestWriteMobileExecTimelineFixture and TestWriteMobileChildTimelineFixture.
const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');
(async () => {
 const browser = await chromium.launch({headless:true, executablePath:process.env.BOID_CHROMIUM});
 try {
  const page = await browser.newPage();
  const dir = process.env.BOID_UI_FIXTURE_DIR;
  await page.route('http://boid.test/**', route => {
   const path = new URL(route.request().url()).pathname;
   if (path === '/') return route.fulfill({contentType:'text/html', body:fs.readFileSync(dir+'/exec.html','utf8')});
   if (path === '/static/style.css') return route.fulfill({contentType:'text/css', body:fs.readFileSync(dir+'/static/style.css','utf8')});
   return route.fulfill({status:404,body:''});
  });
  await page.addInitScript(() => { window.EventSource = class { addEventListener() {} close() {} }; });
  for (const width of [320,375,390,1024]) {
   await page.setViewportSize({width,height:850});
   await page.goto('http://boid.test/');
   assert.equal(await page.locator('#tabs, #tab-panel').count(), 0);
   assert.equal(await page.locator('.card-timeline #task-timeline').count(), 1);
   assert.equal(await page.locator('#task-timeline a[href="/tasks/child-mobile/questions/question-mobile"]').count(),1);
   assert.equal(await page.locator('.timeline-row-child').count(),1);
   assert.equal(await page.locator('.detail-child-tree').count(),0);
   assert.equal(await page.locator('.timeline-row-child a[href="/tasks/child-mobile/questions/question-mobile"]').count(),1);
   assert.equal(await page.locator('details[open]').count(),0);
   for (const zoom of [1,2]) {
    await page.evaluate(zoom => document.documentElement.style.fontSize = (16*zoom)+'px',zoom);
    await page.locator('details:not(.action-menu)').evaluateAll(els => els.forEach(el => el.open=true));
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `overflow at ${width}px / ${zoom}x`);
    assert.ok(await page.locator('.action-bar').isVisible());
   }
   if (width === 375) {
    await page.evaluate(() => document.documentElement.style.fontSize = '16px');
    await page.screenshot({path:dir+'/exec-375.png',fullPage:true});
   }
  }
  console.log('PASS: exec layout at 320/375/390/1024 px, 100%/200% text, disclosures, child history, question link, action bar');
 } finally { await browser.close(); }
})().catch(e=>{console.error(e);process.exitCode=1;});
