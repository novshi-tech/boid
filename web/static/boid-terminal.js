import { FitAddon } from '/static/assets/xterm-5.x/addon-fit.mjs';

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
  const disconnectMsg     = rootEl.querySelector('.boid-terminal-disconnect-msg');
  const ctrlBtn      = rootEl.querySelector('.boid-terminal-keybar-ctrl');
  const copyToast    = rootEl.querySelector('.boid-terminal-copy-toast');
  const copyBtn      = rootEl.querySelector('.boid-terminal-copy-btn');
  const copyLabel    = rootEl.querySelector('.boid-terminal-copy-label');
  const copyPreview  = rootEl.querySelector('.boid-terminal-copy-preview');
  const copyDismiss  = rootEl.querySelector('.boid-terminal-copy-dismiss');

  const term = new window.Terminal({
    fontFamily: "'IBM Plex Mono', 'Menlo', 'Monaco', 'Courier New', monospace",
    fontSize: 14,
    // Keep aligned with MaxScrollbackLines in internal/vtsnapshot: a rendered
    // connect snapshot prepends up to that many scrolled-off history lines, so
    // xterm must retain at least as many for the user to scroll back to them.
    scrollback: 2000,
    // Force-select escape hatch on macOS. While a TUI has mouse reporting on,
    // xterm.js disables its SelectionService and only re-admits a drag through
    // SelectionService.shouldForceSelection, which is `event.shiftKey`
    // everywhere except macOS — there it is `altKey && macOptionClickForcesSelection`,
    // and that option defaults to false. Without this line a Mac user has no
    // way to select terminal text at all, and the copy toast below never
    // appears for them.
    macOptionClickForcesSelection: true,
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
  let exitReceived = false;

  // --- mouse-tracking mode dedup ---
  //
  // Claude Code's TUI re-asserts DECSET mouse tracking (\x1b[?1000h etc.) on
  // every screen repaint, not once at startup: 161 occurrences of each of
  // 1000/1002/1003/1006 measured on a single 13MB production transcript.
  // xterm.js's CoreMouseService.activeProtocol setter fires onProtocolChange
  // unconditionally, even when re-set to the value it already holds, and
  // xterm's own onProtocolChange handler calls SelectionService.disable(),
  // which calls clearSelection(). So every repaint silently wipes whatever
  // selection the user is mid-drag on — including a Shift-forced one, the
  // escape hatch a few lines below exists for exactly this TUI. A drag has to
  // start and finish entirely between two repaints or it never survives to
  // mouseup, which is why the copy toast can appear to never fire even with
  // Shift held.
  //
  // The fix can't live in vendored xterm.js, so it lives here: track the
  // mouse protocol client-side and strip a DECSET/DECRST that would not
  // actually change it before term.write() ever sees it, so xterm's own
  // CoreMouseService never re-fires for a no-op re-assertion. Real changes
  // (mouse tracking genuinely turning on/off) still pass through untouched.
  const MOUSE_PROTOCOL_MODES = { '9': 'X10', '1000': 'VT200', '1002': 'DRAG', '1003': 'ANY' };
  const MOUSE_MODE_RE = /\x1b\[\?([0-9;]+)([hl])/g;
  // Matches an as-yet-unterminated CSI "ESC [ ? params" opener sitting at
  // the very end of a chunk, i.e. one that hasn't seen its final h/l byte.
  const INCOMPLETE_CSI_TAIL_RE = /\x1b(?:\[(?:\?[0-9;]*)?)?$/;
  const TAIL_WINDOW = 20; // longer than any realistic DECSET/DECRST opener
  let mouseProtocol = 'NONE';
  // A trailing incomplete opener held back from the previous chunk, to be
  // prepended once the rest of it arrives. See the note below.
  let mouseModeCarry = '';

  function stripRedundantMouseModeAssertions(bytes) {
    let bin = mouseModeCarry;
    mouseModeCarry = '';
    for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);

    // The container backend's PTY reader chunks output at a fixed byte
    // boundary (internal/dispatcher), which has nothing to do with escape
    // sequence boundaries: a burst like "\x1b[?1002h" can and does land
    // split across two 'output' messages. Regex-matching per chunk would
    // then either miss the sequence or misfire on a bogus partial, silently
    // desyncing this tracker from what xterm.js's own stateful parser will
    // actually apply once the rest arrives — which can wedge mouse
    // reporting off for the rest of the session (same failure shape as the
    // reset desync above). Hold back a still-open opener instead of
    // stripping/forwarding it prematurely; term.reset() clears this too, a
    // reset being a discontinuity that makes any pending partial moot.
    const tail = bin.slice(-TAIL_WINDOW);
    const tailMatch = tail.match(INCOMPLETE_CSI_TAIL_RE);
    if (tailMatch) {
      mouseModeCarry = tailMatch[0];
      bin = bin.slice(0, bin.length - tailMatch[0].length);
    }

    // First pass, no mutation: replay every mouse-mode token in this chunk
    // exactly as xterm's CoreMouseService would (last one wins) to find the
    // NET protocol this chunk settles on. A burst that cycles through
    // VT200 -> DRAG -> ANY on every repaint never repeats a value
    // consecutively, so comparing token-by-token against the prior token
    // would never catch it as redundant — only the chunk's final resting
    // value, compared against the value already active before the chunk,
    // says whether the whole burst changed anything.
    let finalProtocol = mouseProtocol;
    let m;
    MOUSE_MODE_RE.lastIndex = 0;
    while ((m = MOUSE_MODE_RE.exec(bin))) {
      m[1].split(';').forEach(function (n) {
        if (n in MOUSE_PROTOCOL_MODES) finalProtocol = m[2] === 'h' ? MOUSE_PROTOCOL_MODES[n] : 'NONE';
      });
    }

    if (finalProtocol === mouseProtocol) {
      // The whole burst is a no-op: drop just the mouse-relevant numbers
      // from each token (a token can bundle unrelated modes, e.g. bracketed
      // paste, in the same param list) so nothing about this chunk reaches
      // xterm's CoreMouseService and re-fires onProtocolChange.
      bin = bin.replace(MOUSE_MODE_RE, function (whole, params, hl) {
        const rest = params.split(';').filter(function (n) { return !(n in MOUSE_PROTOCOL_MODES); });
        if (rest.length === params.split(';').length) return whole;
        return rest.length ? '\x1b[?' + rest.join(';') + hl : '';
      });
    } else {
      mouseProtocol = finalProtocol;
    }

    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // Transcript bytes already painted. Handed back as ?replay_offset= on the
  // next connect so a reconnect resumes mid-stream instead of repainting the
  // whole session from the top.
  let replayOffset = 0;
  let reconnectAttempt = 0;
  let reconnectTimer = null;
  // Set by disconnect(): a deliberate teardown must not trigger a retry.
  let closedByUser = false;

  // Auto-reconnect pacing. An idle PTY produces no traffic, and every
  // intermediary (Cloudflare Tunnel, mobile NAT, a phone suspending the tab)
  // eventually reaps such a connection — so a drop is far more often the
  // network than the job ending, and the terminal should heal itself.
  const RECONNECT_MAX_ATTEMPTS = 8;
  const RECONNECT_BASE_MS = 1000;
  const RECONNECT_MAX_MS = 15000;

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
  // On mobile, use visualViewport.height so the terminal shrinks when the soft
  // keyboard opens (window.innerHeight does not shrink on iOS/Android Chrome).
  //
  // rootEl is .boid-terminal, which has `flex: 1 1 0` inside the explicit-height
  // flex column .site-main. A flex item with flex-basis:0 + flex-grow:1 IGNORES
  // its `height` (flex-grow stretches it to fill the column regardless), so the
  // soft keyboard would otherwise leave the terminal — and the keybar at its
  // bottom — at full height, hidden behind the keyboard. `max-height` DOES clamp
  // flex-grow, so set it too: when the keyboard shrinks visualViewport, max-height
  // pulls the terminal (and the keybar) up into the visible area. `height` is kept
  // for any future block-parent embedding where flex-grow is not in play.
  //
  // Do NOT clamp the result to a fixed minimum (this used to floor it at 200px).
  // A floor taller than the available space makes rootEl overflow the viewport,
  // and .site-main's `overflow:hidden` then clips its bottom — the keybar — back
  // out of sight. This bites whenever the visible area is short: small phones,
  // landscape, a tall header pushing rect.top down, or a soft keyboard shrinking
  // visualViewport below rect.top + 200. The keybar (flex-shrink:0) and status
  // bar stay visible without a floor because the xterm viewport (flex:1 1 0,
  // min-height:0) absorbs the shrink instead. Clamp only at 0 to avoid a
  // negative height.
  function resizeToViewport() {
    const rect = rootEl.getBoundingClientRect();
    const bottomGap = 8;
    const vh = window.visualViewport ? window.visualViewport.height : window.innerHeight;
    const height = Math.max(0, vh - rect.top - bottomGap);
    rootEl.style.height = height + 'px';
    rootEl.style.maxHeight = height + 'px';
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
  function showOverlay(message) {
    if (disconnectMsg) disconnectMsg.textContent = message;
    disconnectOverlay.hidden = false;
  }

  function connect() {
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
    exitReceived = false;
    closedByUser = false;
    setStatus('connecting');
    disconnectOverlay.hidden = true;

    let url = wsUrl.startsWith('ws') ? wsUrl : wsUrlFromPath(wsUrl);
    if (replayOffset > 0) {
      url += (url.indexOf('?') === -1 ? '?' : '&') + 'replay_offset=' + replayOffset;
    }
    ws = new WebSocket(url);

    ws.onopen = function () {
      setStatus('connected');
      reconnectAttempt = 0;
      const dims = fitAddon.proposeDimensions();
      if (dims) sendResize(dims.cols, dims.rows);
    };

    ws.onmessage = function (e) {
      let msg;
      try { msg = JSON.parse(e.data); } catch (_) { return; }
      if (msg.type === 'attach') {
        // The server states where the replay that follows starts, in RAW
        // transcript bytes — including when what it actually sends is a
        // rendered screen, so this counter stays comparable with the live
        // deltas that follow and with the ?replay_offset we hand back.
        //
        // Two shapes need the screen wiped first, and they are not the same
        // condition: offset 0 means a full raw repaint from the top (a first
        // connect to a non-PTY job, or a daemon that rebuilt a shorter
        // transcript), while rendered means a resolved screen dump, which
        // must land on a clear terminal or it would be drawn under whatever
        // is already there. Anything else is a reconnect splice onto a screen
        // we still have.
        const offset = msg.offset || 0;
        // term.reset() resets xterm's own CoreMouseService.activeProtocol to
        // NONE (see coreMouseService.reset() in xterm.js's Terminal.reset()),
        // so the dedup tracker above must follow it back to NONE or the next
        // real mouse-mode assertion after this reset would be wrongly
        // dropped as a no-op, leaving xterm's mouse reporting stuck off.
        // mouseModeCarry is also stale across this discontinuity — a raw
        // splice picks up mid-stream at a different position than whatever
        // byte offset the carry was measured from.
        if (offset === 0 || msg.rendered) { term.reset(); mouseProtocol = 'NONE'; mouseModeCarry = ''; }
        replayOffset = offset;
      } else if (msg.type === 'output') {
        const bytes = Uint8Array.from(atob(msg.data), c => c.charCodeAt(0));
        term.write(stripRedundantMouseModeAssertions(bytes));
        replayOffset += bytes.length;
      } else if (msg.type === 'exit') {
        exitReceived = true;
        term.write('\r\n\x1b[90m[プロセス終了: ' + msg.code + ']\x1b[0m\r\n');
        ws.close();
      } else if (msg.type === 'error') {
        term.write('\r\n\x1b[31m[エラー: ' + msg.message + ']\x1b[0m\r\n');
      }
    };

    ws.onclose = function () {
      setStatus('disconnected');
      if (exitReceived || closedByUser) {
        if (!exitReceived) showOverlay('接続が切断されました');
        return;
      }
      scheduleReconnect();
    };

    ws.onerror = function () {
      setStatus('disconnected');
    };
  }

  function scheduleReconnect() {
    if (reconnectAttempt >= RECONNECT_MAX_ATTEMPTS) {
      showOverlay('接続が切断されました');
      return;
    }
    const wait = Math.min(RECONNECT_BASE_MS * Math.pow(2, reconnectAttempt), RECONNECT_MAX_MS);
    reconnectAttempt++;
    showOverlay('接続が切れました。再接続中... (' + reconnectAttempt + '/' + RECONNECT_MAX_ATTEMPTS + ')');
    setStatus('connecting');
    reconnectTimer = setTimeout(connect, wait);
  }

  // Coming back to a backgrounded tab is the single most common way this
  // terminal is found disconnected on a phone: the OS suspends the tab, the
  // socket dies unnoticed, and the backoff has long since given up by the
  // time the user looks again. Treat regaining visibility as a fresh start.
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState !== 'visible') return;
    if (exitReceived || closedByUser) return;
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
    reconnectAttempt = 0;
    connect();
  });

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
          // Same coupling as the attach-frame term.reset() above: this also
          // resets xterm's CoreMouseService.activeProtocol to NONE, so the
          // dedup tracker must follow or a repaint burst right after a
          // resize gets wrongly stripped as a no-op and mouse reporting
          // wedges off for the rest of the session.
          term.reset();
          mouseProtocol = 'NONE';
          mouseModeCarry = '';
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

  // visualViewport: resize/scroll updates container height on every change
  // (covers URL bar show/hide and soft keyboard open/close).
  // PTY resize (scheduleFit) is guarded to >150px changes only — smaller
  // events from URL bar transitions should not send a new PTY resize message.
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', function () {
      resizeToViewport();
      const diff = window.innerHeight - window.visualViewport.height;
      if (diff > 150) {
        scheduleFit();
      }
    });
    // scroll fires on iOS when the page shifts to keep a focused element visible
    window.visualViewport.addEventListener('scroll', function () {
      resizeToViewport();
    });
  }

  // --- mobile touch scroll (Step A + B) ---
  //
  // Step A — why term.scrollLines() alone can never work here:
  // Claude Code is a full-screen TUI; it enters the alternate screen once at
  // startup and never leaves it (see internal/vtsnapshot/vtsnapshot.go). An
  // alt-screen buffer has no scrollback (`buffer.active.ybase === 0`), and
  // xterm's own scrollLines() is `ydisp = clamp(ydisp + n, 0, ybase)` — with
  // ybase pinned at 0 that is a permanent no-op. No amount of fixing *how*
  // the touch event reaches this code changes that; the call it reaches was
  // dead the whole time. vendored xterm.js's own wheel handler
  // (bindMouse()/registerEvents() in xterm.js) already gets this right by
  // branching three ways on every wheel event:
  //   1. mouse tracking active (coreMouseService.areMouseEventsActive) →
  //      report the wheel to the app as a mouse event (button 4). Claude
  //      Code keeps DECSET 1000/1002/1003 asserted, so in practice this is
  //      the common case for it.
  //   2. no scrollback, i.e. alt screen (`!buffer.hasScrollback`) → translate
  //      into repeated cursor-key escapes (Up/Down), so the *app* scrolls
  //      its own view.
  //   3. otherwise (plain shell) → term.viewport.handleWheel(), the normal
  //      scrollback path term.scrollLines() also exercises.
  // Re-implementing that branch here would mean reaching into
  // coreMouseService/viewport private fields that vendored xterm.js does not
  // export, and re-deriving CoreMouseService's mouse-report encoding by
  // hand. Instead we synthesize a real WheelEvent from the touch delta and
  // dispatch it at .xterm-screen (a descendant of xterm.js's own root
  // element, so it bubbles straight into the exact listener above) and let
  // xterm.js itself pick the branch. This is the one point where this file
  // depends on vendored xterm.js DOM structure (the .xterm-screen class
  // name and the fact that a dispatched 'wheel' event reaches the same
  // listener a real one would) rather than a documented public API — if a
  // future xterm.js upgrade renames that class or stops registering its
  // wheel listener on an ancestor of it, dispatchSyntheticWheel() below is
  // the only place that needs to change.
  //
  // Step B — why Touch Events alone cannot reliably track the gesture:
  // xterm.js's DOM renderer replaces the child <span> of a row on every
  // repaint that touches it (renderRows() does rowElement.replaceChildren(...)
  // — the row <div> is reused, but its text spans are always rebuilt, even
  // when unchanged). A touch that started on a character lands on that
  // span. Per the Touch Events spec, subsequent touchmove/touchend events
  // for that touch keep targeting the *original* element even after it is
  // removed from the document — and once a target is detached, dispatch has
  // no path from the document down to it, so ancestor listeners (including
  // this file's own xtermWrap capture-phase listener below) simply stop
  // firing, capture phase included. Because term.scrollLines()'s
  // refresh(0, rows-1) after every successful scroll re-triggers exactly
  // that repaint, this used to break after the very first scrolled line
  // even while the finger kept moving.
  //
  // Pointer Events sidestep this: Pointer.setPointerCapture() redirects
  // every later pointer event for that pointerId straight to the capturing
  // element regardless of hit-testing or whether the original target is
  // still attached, so xtermWrap keeps receiving pointermove no matter how
  // many times xterm.js rebuilds the spans underneath. This is a standard
  // public Web API, not a vendored-library dependency.
  //
  // What Pointer Events do NOT replace: xterm.js's own vendored bindMouse()
  // registers raw Touch Events (touchstart/touchmove) directly on
  // this.element, independent of anything this file does with Pointer
  // Events — pointer capture only affects Pointer Events, so those keep
  // firing in parallel. When coreMouseService.areMouseEventsActive is false
  // (the DECSET-reset window right after term.reset(), see the
  // mouse-tracking-mode-dedup comment above), xterm's own touchmove handler
  // drives viewport.handleTouchMove() and would fight this code's scrolling
  // over the same gesture. The touchmove capture-phase listener below with
  // stopPropagation() is what prevents that — it is NOT superseded by the
  // Pointer Events migration and must stay. Its preventDefault() is equally
  // load-bearing for an unrelated reason: without it, touch-action:none
  // (style.css) makes iOS synthesize mousedown/mousemove/mouseup for the
  // gesture, and xterm's SelectionService starts a text selection on every
  // swipe, popping the copy toast on every scroll. Do not drop either call.
  //
  // Pointer type scoping: only pointerType === 'touch' is handled here.
  // Capturing a mouse pointerdown would break desktop drag-to-select (the
  // copy toast feature below) — mouse gestures are left entirely to
  // xterm.js's own SelectionService.
  (function attachTouchScroll() {
    const screenEl = xtermRoot.querySelector('.xterm-screen');
    if (!screenEl) return;

    let activePointerId = null;
    let startX = 0, startY = 0;
    let lastX = 0, lastY = 0;
    let lastT = 0;
    let velocityY = 0;  // px/ms
    let rafId = null;
    let inertiaFrames = 0;
    // Hard cap on synthetic wheel events fired from a single inertia run.
    // In branches 1/2 above (mouse report / cursor keys) each dispatched
    // wheel event becomes real input sent to the remote process, so
    // inertia must not be able to fire an unbounded stream of it — FRICTION
    // decay below already bounds this to roughly 60-90 frames for any
    // realistic flick velocity, but this is a hard backstop independent of
    // that math.
    const MAX_INERTIA_FRAMES = 180;

    // コピートーストや切断オーバーレイは xtermWrap の子だが、これらの上での
    // タッチはボタン操作 (コピー/閉じる/再接続) 用であって端末スクロール
    // ジェスチャーではない。ここで拾ってしまうと、ボタンへのタップが
    // 端末スクロールとして誤処理されうる。
    function isOnOverlay(e) {
      return (copyToast && copyToast.contains(e.target)) ||
        (disconnectOverlay && !disconnectOverlay.hidden && disconnectOverlay.contains(e.target));
    }

    // dy>0 means an upward swipe (finger moving toward smaller clientY),
    // which under direct-manipulation scrolling reveals content further
    // forward (more recent lines) — i.e. the same direction xterm.js's own
    // scrollLines(+n)/handleWheel(deltaY>0) move in, so no sign flip is
    // needed when handing dy straight to a WheelEvent's deltaY.
    function dispatchSyntheticWheel(dy, clientX, clientY) {
      if (dy === 0) return;
      screenEl.dispatchEvent(new WheelEvent('wheel', {
        deltaY: dy,
        deltaMode: WheelEvent.DOM_DELTA_PIXEL,
        clientX: clientX,
        clientY: clientY,
        bubbles: true,
        cancelable: true,
      }));
    }

    function stopInertia() {
      if (rafId) { cancelAnimationFrame(rafId); rafId = null; }
      inertiaFrames = 0;
    }

    xtermWrap.addEventListener('pointerdown', function (e) {
      if (e.pointerType !== 'touch') return;
      if (isOnOverlay(e)) return;
      stopInertia();
      activePointerId = e.pointerId;
      startX = lastX = e.clientX;
      startY = lastY = e.clientY;
      lastT = e.timeStamp;
      velocityY = 0;
      xtermWrap.setPointerCapture(e.pointerId);
    }, { capture: true });

    xtermWrap.addEventListener('pointermove', function (e) {
      if (e.pointerId !== activePointerId) return;
      const dt = e.timeStamp - lastT || 1;
      const dy = lastY - e.clientY;
      velocityY = dy / dt;  // px/ms
      dispatchSyntheticWheel(dy, e.clientX, e.clientY);
      lastX = e.clientX;
      lastY = e.clientY;
      lastT = e.timeStamp;
    }, { capture: true });

    function endGesture(e, withInertia) {
      if (e.pointerId !== activePointerId) return;
      activePointerId = null;
      if (xtermWrap.hasPointerCapture(e.pointerId)) {
        xtermWrap.releasePointerCapture(e.pointerId);
      }
      if (!withInertia) { stopInertia(); return; }

      // 慣性減衰スクロール: velocityY (px/ms) を減衰させながら合成 wheel を送る
      let vel = velocityY;
      const cx = lastX, cy = lastY;
      const FRICTION = 0.92;  // フレームごとの速度減衰率
      const MIN_VEL  = 0.02;  // この速度以下になったら停止 (px/ms)
      inertiaFrames = 0;

      function step() {
        vel *= FRICTION;
        inertiaFrames++;
        if (Math.abs(vel) < MIN_VEL || inertiaFrames > MAX_INERTIA_FRAMES) {
          rafId = null;
          return;
        }
        // 16ms/frame 相当の移動量
        dispatchSyntheticWheel(vel * 16, cx, cy);
        rafId = requestAnimationFrame(step);
      }

      if (Math.abs(vel) >= MIN_VEL) {
        rafId = requestAnimationFrame(step);
      }
    }

    xtermWrap.addEventListener('pointerup', function (e) {
      if (isOnOverlay(e)) { activePointerId = null; return; }
      // tap-to-focus: consolidated here (not a separate touchend handler)
      // so it shares state with the scroll gesture above instead of running
      // its own touchstart/touchend pair — see the top-of-function comment.
      const dx = e.clientX - startX, dy = e.clientY - startY;
      const wasTap = Math.abs(dx) < 10 && Math.abs(dy) < 10;
      endGesture(e, /* withInertia */ true);
      if (wasTap) term.focus();
    }, { capture: true });

    // pointercancel (browser-initiated gesture takeover, e.g. an OS
    // edge-swipe) must stop any in-flight inertia rAF loop, not start one —
    // there is no reliable end velocity to trust here.
    xtermWrap.addEventListener('pointercancel', function (e) {
      endGesture(e, /* withInertia */ false);
    }, { capture: true });

    // --- raw Touch Events: preventDefault/stopPropagation only ---
    // These do not compute scroll anymore (Pointer Events above own that);
    // they exist solely to (a) stop xterm.js's own competing
    // touchmove-driven scroll during the areMouseEventsActive===false
    // window, and (b) preventDefault so iOS does not synthesize mouse
    // events that would start a SelectionService drag-select. See the
    // top-of-function comment for why both still matter.
    xtermWrap.addEventListener('touchstart', function () {
      // Intentionally not stopped: see the "Step B" comment above.
    }, { passive: true, capture: true });

    xtermWrap.addEventListener('touchmove', function (e) {
      if (isOnOverlay(e)) return;
      e.preventDefault();
      e.stopPropagation();
    }, { passive: false, capture: true });

    xtermWrap.addEventListener('touchend', function () {
      // Intentionally not stopped: see the "Step B" comment above.
    }, { passive: true, capture: true });
  })();

  // --- copy toast ---
  //
  // Why a toast instead of a plain Ctrl+C: a TUI (claude code, vim, ...) turns
  // on mouse reporting, and xterm.js reacts by disabling its own
  // SelectionService and forwarding mouse events to the application. Shift
  // (Option on macOS, with macOptionClickForcesSelection) still forces a
  // selection through SelectionService.shouldForceSelection — but only for the
  // mousedown. The mouseup that ends the drag is still reported to the
  // application, and xterm.js sends mouse reports as *user input*:
  //
  //   triggerMouseEvent(e) { ... this._coreService.triggerDataEvent(report, true) }
  //
  // while SelectionService drops the selection on any user input:
  //
  //   this._coreService.onUserInput(() => { this.hasSelection && this.clearSelection() })
  //
  // So the selection dies the instant the user lets go of the mouse: the
  // highlight vanishes and term.getSelection() returns "". Two consequences
  // shape everything below. First, the text has to be captured *while the
  // selection still exists* — onSelectionChange fires repeatedly during the
  // drag, so the last non-empty value it reports is the finished selection.
  // Reading it lazily at copy time is too late. Second, Ctrl+C is deliberately
  // left alone as SIGINT: with the highlight already gone the user would be
  // copying something invisible, and a Ctrl+C meant to interrupt a runaway
  // process must never silently turn into a copy.
  //
  // The click is also load-bearing, not just UI garnish: it is the user
  // gesture navigator.clipboard.writeText demands on Safari/iOS, where a write
  // from a stray async callback is rejected outright.
  const COPY_TOAST_TIMEOUT_MS = 8000;
  const COPY_PREVIEW_MAX = 48;
  let pendingCopy = null;
  let copyToastTimer = null;

  function hideCopyToast() {
    if (copyToastTimer) { clearTimeout(copyToastTimer); copyToastTimer = null; }
    pendingCopy = null;
    copyToast.hidden = true;
    copyToast.classList.remove('boid-terminal-copy-toast-done');
  }

  function showCopyToast(text) {
    if (copyToastTimer) clearTimeout(copyToastTimer);
    pendingCopy = text;
    copyLabel.textContent = 'コピー';
    // Collapse newlines so a multi-line selection stays a one-line preview.
    const flat = text.replace(/\s+/g, ' ').trim();
    copyPreview.textContent = flat.length > COPY_PREVIEW_MAX
      ? flat.slice(0, COPY_PREVIEW_MAX) + '…'
      : flat;
    copyToast.classList.remove('boid-terminal-copy-toast-done');
    copyToast.hidden = false;
    copyToastTimer = setTimeout(hideCopyToast, COPY_TOAST_TIMEOUT_MS);
  }

  // execCommand('copy') is deprecated but irreplaceable here: navigator.clipboard
  // does not exist outside a secure context, so a plain-HTTP LAN origin
  // (http://192.168.x.x:8080, neither TLS nor localhost) has no async clipboard
  // API to fall back to.
  function copyViaExecCommand(text) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '0';
    ta.style.left = '0';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
    document.body.removeChild(ta);
    return ok;
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(
        function () { return true; },
        function () { return copyViaExecCommand(text); }
      );
    }
    return Promise.resolve(copyViaExecCommand(text));
  }

  function finishCopy(ok) {
    if (copyToastTimer) clearTimeout(copyToastTimer);
    copyLabel.textContent = ok ? 'コピーしました' : 'コピーできませんでした';
    copyPreview.textContent = '';
    copyToast.classList.add('boid-terminal-copy-toast-done');
    copyToastTimer = setTimeout(hideCopyToast, 1500);
    term.focus();
  }

  // The selection is gone by the time any click lands (see above), so capture
  // every non-empty value onSelectionChange reports during the drag.
  term.onSelectionChange(function () {
    const text = term.getSelection();
    if (!text || !text.trim()) return;
    showCopyToast(text);
  });

  // Clicking a button steals focus from xterm's hidden textarea, which would
  // leave the terminal unable to receive keystrokes. Suppressing mousedown
  // keeps focus where it is; finishCopy() re-focuses as a belt-and-braces
  // measure for the touch path, where no mousedown is involved.
  copyToast.addEventListener('mousedown', function (e) { e.preventDefault(); });

  copyBtn.addEventListener('click', function () {
    if (!pendingCopy) return;
    const text = pendingCopy;
    copyText(text).then(finishCopy);
  });

  copyDismiss.addEventListener('click', function () {
    hideCopyToast();
    term.focus();
  });

  // --- reconnect button ---
  reconnectBtn.addEventListener('click', function () {
    reconnectAttempt = 0;
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
    disconnect: function () {
      closedByUser = true;
      if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
      if (ws) ws.close();
    },
  };
}
