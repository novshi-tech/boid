const { chromium } = require(process.env.BOID_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');
(async () => {
 const browser = await chromium.launch({headless:true, executablePath:process.env.BOID_CHROMIUM});
 try {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  let count = 0, mode = 'failure', body = '', release;
  await page.route('http://boid.test/**', async route => {
   if (route.request().method() !== 'POST') return route.fulfill({contentType:'text/html',body:'<body></body>'});
   count++; body = route.request().postData();
   if (mode === 'failure') return route.abort('failed');
   if (mode === 'pending') await new Promise(resolve => release = resolve);
   return route.fulfill({status:200, headers:{'X-Boid-Operation-ID':'result-1'}, contentType:'text/html', body:'<section id="task-operations"><div class="operation-result" data-operation-id="result-1">Accepted</div></section>'});
  });
  await page.goto('http://boid.test/');
  await page.setContent('<section id="task-operations"></section><div id="task-live-status"><button id="task-live-retry">Refresh</button></div><form method="post" action="/tasks/card/commands"><textarea name="instruction">Keep my input</textarea><input name="_csrf" value="csrf-test" type="hidden"><button type="submit" name="key" value="discuss">Discuss</button></form>');
  await page.addScriptTag({content:fs.readFileSync('web/static/boid-operation-submit.js','utf8')});
  await page.getByText('Discuss',{exact:true}).click();
  await page.waitForFunction(() => document.querySelector('#operation-submit-feedback')?.textContent.includes('could not be confirmed'));
  assert.equal(await page.locator('textarea').inputValue(),'Keep my input');
  assert.equal(new URLSearchParams(body).get('key'),'discuss');
  assert.equal(new URLSearchParams(body).get('_csrf'),'csrf-test');
  assert.equal(count,1);
  mode = 'pending';
  await page.getByText('Discuss',{exact:true}).click();
  await page.waitForFunction(() => document.querySelector('form').getAttribute('aria-busy') === 'true');
  assert.equal(await page.getByText('Discuss',{exact:true}).isDisabled(),true);
  await page.locator('form').evaluate(form => form.dispatchEvent(new SubmitEvent('submit',{bubbles:true,cancelable:true})));
  assert.equal(count,2);
  release();
  await page.waitForFunction(() => document.querySelector('#task-operations').textContent === 'Accepted');
  assert.equal(await page.locator('#operation-submit-feedback').count(),0);
  assert.equal(await page.locator('textarea').inputValue(),'Keep my input');
  assert.deepEqual(errors,[]);
  console.log('PASS: response loss, retained instruction/CSRF/command, duplicate suppression, correlated response');
 } finally { await browser.close(); }
})().catch(e => {console.error(e);process.exitCode=1;});
