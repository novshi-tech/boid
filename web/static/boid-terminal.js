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
  const xtermRoot   = rootEl.querySelector('.boid-terminal-xterm');
  const statusDot   = rootEl.querySelector('.boid-terminal-status');
  const disconnectOverlay = rootEl.querySelector('.boid-terminal-disconnect-overlay');
  const reconnectBtn      = rootEl.querySelector('.boid-terminal-reconnect');
  const imeOverlay   = rootEl.querySelector('.boid-terminal-ime-overlay');
  const imeTextarea  = rootEl.querySelector('.boid-terminal-ime-textarea');
  const imeSendBtn   = rootEl.querySelector('.boid-terminal-ime-send');
  const imeCloseBtn  = rootEl.querySelector('.boid-terminal-ime-close');
  const ctrlBtn      = rootEl.querySelector('.boid-terminal-keybar-ctrl');

  const term = new window.Terminal({
    fontFamily: "'IBM Plex Mono', 'Menlo', 'Monaco', 'Courier New', monospace",
    fontSize: 14,
    scrollback: 1000,
  });
  const fitAddon = new FitAddon();
  term.loadAddon(fitAddon);
  term.open(xtermRoot);
  fitAddon.fit();

  let ws = null;
  let ctrlActive = false;

  // --- status indicator ---
  function setStatus(state) {
    statusDot.className = 'boid-terminal-status boid-terminal-status-' + state;
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

  // --- ResizeObserver: fit + resize frame ---
  let prevCols = 0, prevRows = 0;
  const ro = new ResizeObserver(function () {
    fitAddon.fit();
    const dims = fitAddon.proposeDimensions();
    if (!dims) return;
    if (dims.cols !== prevCols || dims.rows !== prevRows) {
      prevCols = dims.cols;
      prevRows = dims.rows;
      sendResize(dims.cols, dims.rows);
    }
  });
  ro.observe(xtermRoot);

  // visualViewport: adjust xterm container height when soft keyboard appears.
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', function () {
      const container = rootEl.querySelector('.boid-terminal-xterm-wrap');
      if (container) {
        container.style.height = window.visualViewport.height + 'px';
      }
      fitAddon.fit();
      const dims = fitAddon.proposeDimensions();
      if (dims && (dims.cols !== prevCols || dims.rows !== prevRows)) {
        prevCols = dims.cols;
        prevRows = dims.rows;
        sendResize(dims.cols, dims.rows);
      }
    });
  }

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
      if (key === 'ime') {
        openIME();
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

  // --- IME modal ---
  function openIME() {
    term.blur();
    imeOverlay.hidden = false;
    imeTextarea.focus();
  }

  function closeIME() {
    imeOverlay.hidden = true;
    term.focus();
  }

  imeSendBtn.addEventListener('click', function () {
    const text = imeTextarea.value;
    if (text) {
      sendInput(text);
      imeTextarea.value = '';
    }
  });

  imeCloseBtn.addEventListener('click', closeIME);

  imeOverlay.addEventListener('click', function (e) {
    if (e.target === imeOverlay) closeIME();
  });

  imeTextarea.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeIME();
    }
  });

  // Initial connection
  connect();

  return {
    term,
    disconnect: function () { if (ws) ws.close(); },
  };
}
