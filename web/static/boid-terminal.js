import { FitAddon } from '/static/vendor/xterm-5.x/addon-fit.mjs';

// Control character codes for the special keybar.
const KEY_CODES = {
  esc:   '\x1b',
  tab:   '\x09',
  up:    '\x1b[A',
  down:  '\x1b[B',
  right: '\x1b[C',
  left:  '\x1b[D',
};

function wsUrlFromPath(path) {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return proto + '//' + window.location.host + path;
}

// Apply Ctrl modifier to a single printable character.
function applyCtrl(data) {
  if (data.length !== 1) return data;
  const code = data.charCodeAt(0);
  if (code >= 64 && code <= 95) return String.fromCharCode(code - 64);  // @A-Z[\]^_
  if (code >= 97 && code <= 122) return String.fromCharCode(code - 96); // a-z
  return data;
}

function toBase64(str) {
  const bytes = new TextEncoder().encode(str);
  let binary = '';
  bytes.forEach(b => { binary += String.fromCharCode(b); });
  return btoa(binary);
}

/**
 * initBoidTerminal initialises an xterm.js terminal inside rootEl.
 *
 * @param {HTMLElement} rootEl - Container with .boid-terminal class.
 * @param {{ jobId: string, wsUrl: string }} opts
 * @returns {{ term: Terminal, disconnect: () => void }}
 */
export function initBoidTerminal(rootEl, { jobId, wsUrl }) {
  const xtermWrap   = rootEl.querySelector('.boid-terminal-xterm-wrap');
  const xtermRoot   = rootEl.querySelector('.boid-terminal-xterm');
  const statusDot   = rootEl.querySelector('.boid-terminal-status');
  const disconnectOverlay = rootEl.querySelector('.boid-terminal-disconnect-overlay');
  const reconnectBtn      = rootEl.querySelector('.boid-terminal-reconnect');
  const ctrlBtn      = rootEl.querySelector('.boid-terminal-keybar-ctrl');

  const term = new window.Terminal({
    fontFamily: "'IBM Plex Mono', 'Menlo', 'Monaco', 'Courier New', monospace",
    fontSize: 14,
    // Keep aligned with maxSnapshotScrollback in runtime_local_linux.go: the
    // connect snapshot prepends up to that many scrolled-off history lines, so
    // xterm must retain at least as many for the user to scroll back to them.
    scrollback: 2000,
  });
  const fitAddon = new FitAddon();
  term.loadAddon(fitAddon);
  term.open(xtermRoot);
  resizeToViewport();
  fitAddon.fit();
  document.fonts.ready.then(function () { scheduleFit(); });
  window.addEventListener('resize', function () {
    resizeToViewport();
  });

  let ws = null;
  let ctrlActive = false;

  // --- status indicator ---
  const STATUS_TITLES = {
    connecting:   '接続中',
    connected:    '接続済み',
    disconnected: '切断',
  };
  function setStatus(state) {
    statusDot.className = 'boid-terminal-status boid-terminal-status-' + state;
    statusDot.title = STATUS_TITLES[state] || state;
  }

  // Fit the terminal to the remaining viewport space. The Terminal component's
  // flex-based sizing only works when the parent is an explicit-height flex
  // column (true for /jobs/:id/terminal, but not for the embedded widget on
  // the job detail page). Measuring rootEl.top each time handles both cases:
  // flex parents give us a stable top, block parents give us whatever layout
  // pushed rootEl down to.
  function resizeToViewport() {
    const rect = rootEl.getBoundingClientRect();
    const bottomGap = 8;
    const height = Math.max(200, window.innerHeight - rect.top - bottomGap);
    rootEl.style.height = height + 'px';
  }

  // --- WS send helpers ---
  function sendInput(data) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'input', data: toBase64(data) }));
  }

  function sendResize(cols, rows) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'resize', cols, rows }));
  }

  // --- connect / reconnect ---
  function connect() {
    setStatus('connecting');
    disconnectOverlay.hidden = true;

    const url = wsUrl.startsWith('ws') ? wsUrl : wsUrlFromPath(wsUrl);
    ws = new WebSocket(url);

    ws.onopen = function () {
      setStatus('connected');
      // The server's first output message is a freshly-rendered screen grid
      // (see runtime_local_linux.go subscribe / renderTerminalSnapshot), not a
      // replay of the whole transcript. It's a full-screen paint, so wipe the
      // terminal first — on reconnect this clears the previous session's frame
      // so the snapshot lands on a clean screen.
      term.reset();
      const dims = fitAddon.proposeDimensions();
      if (dims) sendResize(dims.cols, dims.rows);
    };

    ws.onmessage = function (e) {
      let msg;
      try { msg = JSON.parse(e.data); } catch (_) { return; }
      if (msg.type === 'output') {
        const bytes = Uint8Array.from(atob(msg.data), c => c.charCodeAt(0));
        term.write(bytes);
      } else if (msg.type === 'exit') {
        term.write('\r\n\x1b[90m[プロセス終了: ' + msg.code + ']\x1b[0m\r\n');
        ws.close();
      } else if (msg.type === 'error') {
        term.write('\r\n\x1b[31m[エラー: ' + msg.message + ']\x1b[0m\r\n');
      }
    };

    ws.onclose = function () {
      setStatus('disconnected');
      disconnectOverlay.hidden = false;
    };

    ws.onerror = function () {
      setStatus('disconnected');
    };
  }

  // --- xterm input → WS ---
  term.onData(function (data) {
    if (ctrlActive) {
      sendInput(applyCtrl(data));
      ctrlActive = false;
      ctrlBtn.classList.remove('boid-terminal-keybar-ctrl-on');
    } else {
      sendInput(data);
    }
  });

  // --- ResizeObserver: fit + resize frame (debounced via rAF) ---
  let prevCols = 0, prevRows = 0;
  let fitRafId = null;

  function scheduleFit() {
    if (fitRafId) return;
    fitRafId = requestAnimationFrame(function () {
      fitRafId = null;
      fitAddon.fit();
      const dims = fitAddon.proposeDimensions();
      if (!dims) return;
      if (dims.cols !== prevCols || dims.rows !== prevRows) {
        // Clear the screen before propagating the new size to the PTY. Most
        // TUIs (claude code, vim, ...) repaint by cursor-up + erase relative
        // to the old frame; when cols change, that math is wrong and leftover
        // characters pile up. Resetting xterm makes those erases land on an
        // empty screen, and the next frame draws cleanly.
        // Skip the very first fit (prevCols == 0), where there's nothing to
        // clear and we'd risk dropping the initial output.
        if (prevCols !== 0) {
          term.reset();
        }
        prevCols = dims.cols;
        prevRows = dims.rows;
        sendResize(dims.cols, dims.rows);
      }
    });
  }

  // Observe the wrap (parent), not xtermRoot. xterm sets explicit width/height
  // on xtermRoot via fitAddon.fit(), so observing it would only react to our
  // own writes — never to outer layout changes (e.g. site-main max-width
  // flipping at the 768px media query). The wrap's width is driven by the
  // surrounding flex/block layout, so its size mirrors what fit() should target.
  const ro = new ResizeObserver(scheduleFit);
  ro.observe(xtermWrap);

  // visualViewport: only refit when soft keyboard appears (large height reduction).
  // URL bar show/hide causes small resize events that should not trigger PTY resize.
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', function () {
      const diff = window.innerHeight - window.visualViewport.height;
      if (diff > 150) {
        scheduleFit();
      }
    });
  }

  // --- mobile touch scroll (Step B) ---
  // xterm.js の .xterm-viewport はネイティブ scroll を使うが、タッチ慣性の
  // 高頻度 pixel delta と相性が悪くスクロールが詰まる。
  // Touch Events で delta を自前計算し term.scrollLines() に変換する。
  (function attachTouchScroll() {
    const viewport = xtermRoot.querySelector('.xterm-viewport');
    if (!viewport) return;

    let startY = 0;
    let lastY = 0;
    let lastT = 0;
    let velocityY = 0;  // px/ms
    let rafId = null;
    let remainder = 0;  // 端数行の持ち越し (touchmove/touchend で共有)

    function cellHeight() {
      // getBoundingClientRect ベースで 1 行の高さを推定する
      const rows = xtermRoot.querySelector('.xterm-rows');
      if (rows && rows.children.length > 0) {
        return rows.children[0].getBoundingClientRect().height || 17;
      }
      const totalRows = term.buffer.active.length || 1;
      return viewport.scrollHeight / totalRows;
    }

    viewport.addEventListener('touchstart', function (e) {
      if (rafId) { cancelAnimationFrame(rafId); rafId = null; }
      startY = e.touches[0].clientY;
      lastY  = startY;
      lastT  = e.timeStamp;
      velocityY = 0;
      remainder = 0;
    }, { passive: true });

    viewport.addEventListener('touchmove', function (e) {
      const y  = e.touches[0].clientY;
      const dt = e.timeStamp - lastT || 1;
      const dy = lastY - y;  // 正 = 上スワイプ = 過去へスクロール

      velocityY = dy / dt;  // px/ms

      // remainder を持ち越して sub-cell delta を捨てない
      remainder += dy / cellHeight();
      const rows = Math.trunc(remainder);
      remainder -= rows;
      if (rows !== 0) term.scrollLines(rows);

      lastY = y;
      lastT = e.timeStamp;
      e.preventDefault();
    }, { passive: false });

    viewport.addEventListener('touchend', function () {
      // 慣性減衰スクロール: velocityY (px/ms) を行数に換算しながら減衰させる
      // remainder は touchmove からの端数を引き継ぐ
      const ch = cellHeight();
      let vel = velocityY;  // px/ms

      const FRICTION = 0.92;  // フレームごとの速度減衰率
      const MIN_VEL  = 0.02;  // この速度以下になったら停止 (px/ms)

      function step() {
        vel *= FRICTION;
        if (Math.abs(vel) < MIN_VEL) { rafId = null; return; }

        // 16ms/frame 相当の移動量
        const dy = vel * 16;
        remainder += dy / ch;
        const rows = Math.trunc(remainder);
        remainder -= rows;
        if (rows !== 0) term.scrollLines(rows);

        rafId = requestAnimationFrame(step);
      }

      if (Math.abs(vel) >= MIN_VEL) {
        rafId = requestAnimationFrame(step);
      }
    }, { passive: true });
  })();

  // --- reconnect button ---
  reconnectBtn.addEventListener('click', function () {
    connect();
  });

  // --- special keybar ---
  rootEl.querySelectorAll('.boid-terminal-keybar-btn').forEach(function (btn) {
    btn.addEventListener('click', function () {
      const key = btn.dataset.key;
      if (key === 'ctrl') {
        ctrlActive = !ctrlActive;
        ctrlBtn.classList.toggle('boid-terminal-keybar-ctrl-on', ctrlActive);
        return;
      }
      const code = KEY_CODES[key];
      if (!code) return;
      if (ctrlActive) {
        sendInput(applyCtrl(code));
        ctrlActive = false;
        ctrlBtn.classList.remove('boid-terminal-keybar-ctrl-on');
      } else {
        sendInput(code);
      }
      term.focus();
    });
  });

  // Initial connection
  connect();

  return {
    term,
    disconnect: function () { if (ws) ws.close(); },
  };
}
