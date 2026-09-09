// Generate fixture: BOID_UI_FIXTURE_DIR=/tmp/boid-ui-mobile go test ./web/templates -run TestWriteMobileChildTimelineFixture -count=1
const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');
(async () => {
 const browser = await chromium.launch({headless:true, executablePath:process.env.BOID_CHROMIUM});
 try {
  const page = await browser.newPage();
  const dir = process.env.BOID_UI_FIXTURE_DIR || '/tmp/boid-ui-mobile';
  await page.route('http://boid.test/**', route => {
   const path = new URL(route.request().url()).pathname;
   if (path === '/') return route.fulfill({contentType:'text/html', body:fs.readFileSync(dir+'/index.html','utf8')});
   if (path === '/static/style.css') return route.fulfill({contentType:'text/css', body:fs.readFileSync(dir+'/static/style.css','utf8')});
   return route.fulfill({status:404,body:''});
  });
  await page.addInitScript(() => { window.EventSource = class { addEventListener() {} close() {} }; });
  for (const width of [320,375,390,1024]) {
   await page.setViewportSize({width,height:850});
   await page.goto('http://boid.test/');
   for (const zoom of [1,2]) {
    await page.evaluate(zoom => document.documentElement.style.fontSize = (16*zoom)+'px',zoom);
    await page.locator('details:not(.action-menu):not(.tab-dropdown-toggle-wrap)').evaluateAll(els => els.forEach(el => el.open=true));
    const dimensions = await page.evaluate(() => ({viewport:innerWidth,document:document.documentElement.scrollWidth}));
    assert.ok(dimensions.document <= width,`overflow at ${width}px / ${zoom}x: ${JSON.stringify(dimensions)}`);
    const titles = page.locator('.detail-children-title');
    assert.ok(await titles.count() >= 2);
    for (const title of await titles.all()) {
     const box = await title.boundingBox();
     assert.ok(box.width >= Math.min(200,width-64),`compressed title ${box.width} at ${width}/${zoom}`);
    }
    const link = page.locator('a[href="/tasks/card-mobile/children/mobile"]').first();
    await link.focus();
    assert.equal(await link.evaluate(el => el === document.activeElement),true);
    assert.equal(await page.locator('a[href="/tasks/task-mobile/questions/question-mobile"]').count(),1);
    assert.equal(await page.locator('a[href="https://example.com/resource"]').getAttribute('target'),'_blank');
   }
   if (width === 375) await page.screenshot({path:'/tmp/boid-ui-mobile-375.png',fullPage:true});
  }
  console.log('PASS: full card at 320/375/390/1024 px, 100%/200% text, open description/diagnostics, child title widths, question/resource links, keyboard focus');
 } finally { await browser.close(); }
})().catch(e=>{console.error(e);process.exitCode=1;});
