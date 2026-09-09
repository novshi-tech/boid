// Run with Node.js and Playwright installed: node --test web/terminal_resize.test.cjs
// PLAYWRIGHT_CHROMIUM_EXECUTABLE optionally selects an existing Chromium binary.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { test } = require('node:test');
const { chromium } = require('playwright');

const fixture = `<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="/static/style.css">
<link rel="stylesheet" href="/static/assets/xterm-5.x/xterm.css">
<style>body { margin: 0; padding: 0; } main { padding-top: 90px; }</style>
<script src="/static/assets/xterm-5.x/xterm.js"></script>
<main><div class="boid-terminal">
  <div class="boid-terminal-status-bar"><span class="boid-terminal-status"></span></div>
  <div class="boid-terminal-xterm-wrap">
    <div class="boid-terminal-xterm"></div>
    <div class="boid-terminal-disconnect-overlay" hidden>
      <span class="boid-terminal-disconnect-msg"></span>
      <button class="boid-terminal-reconnect">Reconnect</button>
    </div>
    <div class="boid-terminal-copy-toast" hidden>
      <button class="boid-terminal-copy-btn">
        <span class="boid-terminal-copy-label"></span><span class="boid-terminal-copy-preview"></span>
      </button>
      <button class="boid-terminal-copy-dismiss">Dismiss</button>
    </div>
  </div>
  <div class="boid-terminal-keybar">
    <button class="boid-terminal-keybar-btn" data-key="esc">Esc</button>
    <button class="boid-terminal-keybar-btn boid-terminal-keybar-ctrl" data-key="ctrl">Ctrl</button>
  </div>
</div></main>
<script type="module">
  import { initBoidTerminal } from '/static/boid-terminal.js';
  window.widget = initBoidTerminal(document.querySelector('.boid-terminal'), { jobId: 'test', wsUrl: '/fake' });
</script>`;

async function screen(page) {
  return page.evaluate(() => {
    const t = widget.term;
    return {
      cols: t.cols, rows: t.rows, buffer: t.buffer.active.type, modes: t.modes,
      cursor: [t.buffer.active.cursorX, t.buffer.active.cursorY],
      lines: Array.from({ length: t.rows }, (_, i) => t.buffer.active.getLine(i).translateToString(true)).filter(Boolean),
      resize: framesSent.filter(f => f.type === 'resize').at(-1),
    };
  });
}

async function output(page, data) {
  await page.evaluate(async data => {
    socket.onmessage({ data: JSON.stringify({ type: 'output', data: btoa(data) }) });
    await new Promise(resolve => widget.term.write('', resolve));
  }, data);
}

test('terminal preserves state across keyboard, URL bar and width changes', async t => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
  });
  t.after(() => browser.close());
  for (const buffer of ['normal', 'alternate']) {
    await t.test(buffer, async t => {
      const page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
      t.after(() => page.close());
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await page.route('http://boid.test/**', async route => {
        const pathname = new URL(route.request().url()).pathname;
        if (pathname === '/') return route.fulfill({ contentType: 'text/html', body: fixture });
        return route.fulfill({
          contentType: pathname.endsWith('.css') ? 'text/css' : 'text/javascript',
          body: await fs.readFile(path.join(__dirname, pathname)),
        });
      });
      await page.addInitScript(() => {
        window.framesSent = [];
        window.WebSocket = class {
          static OPEN = 1;
          static CONNECTING = 0;
          constructor() {
            this.readyState = 1;
            window.socket = this;
            setTimeout(() => this.onopen?.(), 0);
          }
          send(frame) { framesSent.push(JSON.parse(frame)); }
          close() {}
        };
        // Desktop automation cannot open an OS keyboard; emulate its viewport events.
        window.testViewport = new EventTarget();
        testViewport.height = 844;
        testViewport.offsetTop = 0;
        Object.defineProperty(window, 'visualViewport', { value: testViewport });
      });
      await page.goto('http://boid.test/');
      await page.waitForFunction(() => window.widget && framesSent.some(f => f.type === 'resize'));
      await page.evaluate(async () => {
        await document.fonts.ready;
        for (let i = 0; i < 5; i++) await new Promise(requestAnimationFrame);
      });
      await output(page, (buffer === 'alternate' ? '\x1b[?1049h' : '') +
        '\x1b[?2004h\x1b[?1h\x1b[?1002h\x1b[2J\x1b[HHistory: keep me\r\nPrompt: ');
      const before = await screen(page);
      assert.equal(before.buffer, buffer);
      assert.deepEqual(before.lines, ['History: keep me', 'Prompt: ']);

      for (const height of [500, 600, 844, 784]) {
        const previous = await screen(page);
        await page.evaluate(height => {
          testViewport.height = height;
          testViewport.dispatchEvent(new Event('resize'));
        }, height);
        await page.waitForFunction(rows => widget.term.rows !== rows, previous.rows);
        const after = await screen(page);
        assert.equal(after.cols, before.cols);
        assert.deepEqual(after.lines, before.lines, `screen lost at viewport height ${height}`);
        assert.equal(after.buffer, before.buffer);
        assert.deepEqual(after.modes, before.modes);
        assert.deepEqual(after.cursor, before.cursor);
        assert.deepEqual(after.resize, { type: 'resize', cols: after.cols, rows: after.rows });
      }

      for (const width of [800, 320, 390]) {
        const previous = await screen(page);
        await page.setViewportSize({ width, height: 844 });
        await page.waitForFunction(cols => widget.term.cols !== cols, previous.cols);
        const after = await screen(page);
        assert.deepEqual(after.lines, before.lines);
        assert.equal(after.buffer, before.buffer);
        assert.deepEqual(after.modes, before.modes);
        assert.deepEqual(after.resize, { type: 'resize', cols: after.cols, rows: after.rows });
      }

      await page.evaluate(() => widget.term.focus());
      await page.keyboard.type('abc');
      assert.deepEqual(await page.evaluate(() => framesSent.filter(f => f.type === 'input').map(f => atob(f.data))), ['a', 'b', 'c']);
      await output(page, '\x1b[?1002h\r\x1b[2KPrompt: abc');
      const afterInput = await screen(page);
      assert.deepEqual(afterInput.lines, ['History: keep me', 'Prompt: abc']);
      assert.deepEqual(afterInput.modes, before.modes);
      assert.deepEqual(errors, []);
    });
  }
});
