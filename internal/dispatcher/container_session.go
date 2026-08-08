package dispatcher

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// containerSession implements backend.SandboxSession over a single docker
// container: one docker-attach connection feeding an in-memory transcript
// buffer + multi-subscriber fan-out (§決定 8/9's "1 attach 所有者 + memory
// buffer + fan-out" core — modeled directly on localRuntimeSession's
// readLoop/appendTranscript/subscribe in runtime_local_linux.go, the
// existing session layer §決定 8 calls out to extract and reuse rather than
// redesign), and one ContainerWait call feeding a `done` channel every
// Wait() caller selects on (§決定 7's "backend 内で一度だけ wait して exit
// future を fan-out").
//
// Full disk-spool persistence of the transcript (so `boid job log` survives
// container remove) is explicitly deferred to PR7 (docs/plans/
// phase6-container-backend.md §決定 8: "PR5 では transcript spool の実装は
// skeleton まで OK") — the in-memory buffer here satisfies live
// Subscribe/snapshot semantics for the lifetime of the containerBackend
// process but is not written to the runtime dir the way
// localRuntimeSession's transcriptFile is.
type containerSession struct {
	backend *containerBackend
	id      string
	api     dockerAPI
	tty     bool

	// specPath is removed unconditionally once the container exits (it
	// carries secrets — same retention contract as cleanupSandboxSpec for
	// the userns path: the spec is always deleted, runner-state.json is
	// retained for post-hoc diagnosis). Empty for Adopt-reconstructed
	// sessions, which never wrote one (mirrors usernsSession.prepared being
	// nil for Adopt — see sessionLocalArtifacts's doc comment).
	specPath string
	// specDir, when non-empty, is the per-job directory writeContainerSpec
	// created specPath/statePath under (<runtimeDir>/spec/<spec.ID> —
	// Blocker 1, PR7 codex review) and is removed wholesale (os.RemoveAll)
	// instead of specPath alone, so no empty directory accumulates under
	// runtimeDir/spec over the daemon's lifetime. Empty when
	// ContainerBackendOptions.RuntimeDir was unset (the pre-PR7 flat
	// /tmp/boid-<ID>-runner-*.json layout, where only the file itself is
	// ever removed) or for Adopt-reconstructed sessions.
	specDir string
	// dockerTLSDir is the per-job cert directory materializeDockerClientCert
	// wrote (§決定5), removed alongside specPath once the container exits.
	// Empty whenever LaunchOptions.DockerEnabled was false or no
	// ContainerBackendOptions.DockerTLSCA was configured — the overwhelming
	// majority of sessions today.
	dockerTLSDir string
	// brokerTLSDir is the per-job cert directory materializeBrokerClientCert
	// wrote (docs/plans/phase6-cutover-followups.md §⓪), removed alongside
	// specPath/dockerTLSDir once the container exits. Empty whenever no
	// ContainerBackendOptions.BrokerTLSCA was configured — every session
	// before this feature, and any deployment (including every unit test)
	// that never wires one in.
	brokerTLSDir string

	// transcriptFile / transcriptPath implement §決定8's "daemon 側が
	// attach stream を runtime storage へ逐次 spool" full-persistence
	// contract (PR7 — modeled directly on localRuntimeSession's own
	// transcriptFile/transcriptPath in runtime_local_linux.go, per §決定8's
	// own "現行 session 層の抽出・流用" instruction): every chunk
	// appendTranscript records to the in-memory buffer is also written here,
	// at <runtimeDir>/<containerID>/transcript.log — the exact path
	// ReadTranscript/StatTranscript (transcript.go, backend-neutral) already
	// read, and the exact filename (localRuntimeTranscriptFile) the userns
	// backend's own transcript.log uses. This is what lets `boid job log`
	// keep working after ContainerRemove: docker itself discards `docker
	// logs` history once a container is removed, but this file survives on
	// the host bind-mounted runtimes dir.
	//
	// Both are empty when ContainerBackendOptions.RuntimeDir was empty
	// (every pre-PR7 test/caller — see dockerTLSCertDir's identical
	// fallback) or when spool-file creation failed (advisory: a spool
	// failure degrades `boid job log` for this one job, it must never fail
	// Launch), and are ALWAYS empty for Adopt-reconstructed sessions — see
	// openTranscriptSpool's own doc comment for why re-spooling on Adopt is
	// deliberately not attempted yet.
	transcriptFile *os.File
	transcriptPath string

	connMu sync.Mutex
	hijack *client.HijackedResponse
	// stdinCloseOnce guards CloseInput's half-close against the CURRENT
	// generation's hijack, and is itself replaced with a fresh *sync.Once
	// every time attach() installs a new one (Opus review of PR #864,
	// N6). A single session-lifetime sync.Once here (the pre-fix shape)
	// would only ever half-close the FIRST generation's connection: once
	// CloseInput fired for generation 1, its Once.Do would never run its
	// function body again, so a caller that called CloseInput before a
	// mid-stream drop — then relies on it again after reattachIfLost
	// installs generation 2 — would silently get a no-op forever, with no
	// error to signal it. A pointer field (not a plain sync.Once value)
	// so CloseInput can snapshot the CURRENT generation's *sync.Once under
	// connMu and call Do on that local copy outside the lock, exactly the
	// same pattern this type already uses for hijack itself.
	stdinCloseOnce *sync.Once

	mu          sync.Mutex
	transcript  []byte
	subscribers map[int]chan []byte
	nextSubID   int
	running     bool
	exit        backend.RuntimeExit
	// attached reports whether the session currently has a live attach
	// connection with its own readLoop actively feeding appendTranscript —
	// i.e. whether there is anything for Subscribe to hand a new caller a
	// channel onto. This is INDEPENDENT of running (§決定 7's container-exit
	// state): doAdopt's own best-effort attach (its doc comment) can leave
	// behind a session that is running (ContainerWait is still blocked, the
	// container is genuinely alive) but not attached (the attach call itself
	// failed — e.g. the engine was unreachable at adoption time), and until
	// this field existed Subscribe() only ever checked running, so it
	// answered ok=true with a channel that would never receive anything
	// (Opus review of PR #857): the Web UI/WS ingress saw "connected" and
	// went silent forever, no matter how long the caller waited or how
	// quickly the engine recovered — only a daemon restart (which forces a
	// fresh doAdopt) could ever fix it. See Subscribe's and
	// reattachIfLost's own doc comments for the two halves of the fix: (1)
	// Subscribe now requires both running AND attached before returning
	// ok=true, so ingress gets an honest error instead of a dead channel,
	// and (2) Adopt's cache-hit path best-effort re-attaches a running,
	// not-yet-attached session so a later Subscribe can recover once the
	// engine comes back, without waiting for a daemon restart.
	//
	// Set true the moment attach() stores a fresh hijack (before its
	// readLoop goroutine even starts — so a Subscribe racing right after
	// Launch/doAdopt/reattachIfLost never sees a false negative), and false
	// again the moment attach() fails synchronously OR readLoop's own defer
	// runs (the stream — whatever ended it, container exit or a dropped
	// connection — is no longer live). Guarded by mu (not connMu) precisely
	// so Subscribe can read it atomically alongside running under one lock,
	// the same way it already read running alone before this field existed.
	attached bool
	// reattaching is non-nil exactly while one goroutine's best-effort
	// re-attach (reattachIfLost) is in flight, so concurrent cache-hit
	// Adopt callers for the SAME session (multiple WS ingress racing a
	// cache hit right as the engine recovers) share its outcome instead of
	// each firing their own ContainerAttach — the session-scoped analogue
	// of containerBackend.adopting's backend-scoped dedup for cache-miss
	// doAdopt calls. Closed (and reset to nil) once that one attempt
	// returns; every other concurrent caller just waits on it.
	reattaching chan struct{}

	done chan struct{}
	// readDone is the CURRENT attach generation's completion signal: each
	// call to attach() — the session's first (Launch/doAdopt) or a later
	// best-effort reattachIfLost retry — allocates and stores a brand new
	// channel here rather than reusing whatever the field already held.
	// That is deliberate, not an oversight: a naive design that kept one
	// readDone for the session's whole lifetime would panic the first time
	// a re-attach ran (close of an already-closed channel — the previous
	// generation's attach() error path or readLoop defer already closed
	// it), which is exactly the failure mode a first-draft fix for this
	// hit. Read together with `attached` under mu (see waitLoop's own
	// drain-select, which snapshots both under one lock before deciding
	// whether there is anything to drain at all) rather than referenced
	// directly, so a concurrent attach()/reattachIfLost swapping this
	// field never races waitLoop's own read of it.
	readDone chan struct{}
}

var _ backend.SandboxSession = (*containerSession)(nil)

func newContainerSession(b *containerBackend, id string, tty bool, specPath, dockerTLSDir, brokerTLSDir string) *containerSession {
	sess := &containerSession{
		backend:      b,
		id:           id,
		api:          b.api,
		tty:          tty,
		specPath:     specPath,
		dockerTLSDir: dockerTLSDir,
		brokerTLSDir: brokerTLSDir,
		subscribers:  make(map[int]chan []byte),
		running:      true,
		done:         make(chan struct{}),
		// readDone is deliberately NOT initialized here (Opus review of PR
		// #864, N7): both callers of this constructor (Launch, doAdopt)
		// always call attach() — which unconditionally sets readDone,
		// success or failure — before start() ever launches the waitLoop
		// goroutine that is readDone's only reader. A placeholder channel
		// here would be dead code (never read before being overwritten),
		// which is exactly what the pre-fix version of this field was.
		stdinCloseOnce: &sync.Once{},
	}
	if specPath != "" && b.runtimeDir != "" {
		sess.specDir = filepath.Dir(specPath)
	}
	return sess
}

func (s *containerSession) ID() string { return s.id }

// attach establishes the session's docker-attach connection for the CURRENT
// generation and starts a read loop that feeds appendTranscript. withLogs
// replays already-produced output before switching to the live stream —
// Adopt's post-restart recovery path (the closest this backend gets to a
// separate `docker logs` call); Launch passes false since nothing has been
// produced yet at create time.
//
// This is called more than once over a session's lifetime whenever the
// FIRST attach attempt fails: doAdopt's own best-effort attach (its doc
// comment) and reattachIfLost's later best-effort retry both funnel through
// here. Each call allocates a brand new readDone channel rather than
// reusing whatever s.readDone already held — see that field's own doc
// comment for why reusing it would panic (close of an already-closed
// channel) the moment a second generation's own close ran. attached is set
// (true on success, false on failure) in the SAME locked section as
// readDone, so a concurrent Subscribe or waitLoop reading both together
// under mu (see their own doc comments) never observes one updated without
// the other.
func (s *containerSession) attach(ctx context.Context, withLogs bool) error {
	readDone := make(chan struct{})

	result, err := s.api.ContainerAttach(ctx, s.id, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Logs:   withLogs,
	})
	if err != nil {
		close(readDone)
		s.mu.Lock()
		s.readDone = readDone
		s.attached = false
		s.mu.Unlock()
		return err
	}
	hijack := result.HijackedResponse
	s.connMu.Lock()
	staleHijack := s.hijack
	s.hijack = &hijack
	// A fresh *sync.Once per generation (N6): see stdinCloseOnce's own
	// doc comment for why reusing the previous generation's Once here
	// would permanently no-op CloseInput after the very first call.
	s.stdinCloseOnce = &sync.Once{}
	s.connMu.Unlock()
	// A stale hijack here (Opus review of PR #864, N1) is a PREVIOUS
	// generation's connection that reattachIfLost is replacing: its own
	// readLoop has already exited (attach()/reattachIfLost's gate
	// guarantees this attach call only runs when attached is currently
	// false), so nothing is still reading from it — closing it is safe
	// and does not race the new generation this call just installed.
	// Without this, every mid-stream-drop reattach leaked one fd (and the
	// moby HijackedResponse's own buffered *bufio.Reader) until process
	// exit or the Go net.Conn's finalizer happened to run. nil for every
	// session's FIRST attach (Launch/doAdopt) — closeConn's own nil check
	// makes that a no-op, not a special case here.
	if staleHijack != nil {
		staleHijack.Close()
	}

	s.mu.Lock()
	s.readDone = readDone
	s.attached = true
	s.mu.Unlock()

	go s.readLoop(&hijack, readDone)
	return nil
}

// reattachIfLost is Adopt's cache-hit companion to doAdopt's own best-effort
// attach: called every time a cache-HIT session is about to be handed back
// out, it best-effort re-attaches a session that is running (the container
// is genuinely alive — ContainerWait has not resolved) but not currently
// attached (attached is false — either the session's very first attach,
// doAdopt's, failed, or a previous successful attach's readLoop has since
// ended). A session that is not running, or already attached, returns
// immediately with no I/O — the overwhelming majority of calls, since most
// cache hits are for a session that has been streaming fine the whole time.
//
// Concurrent callers for the SAME session (multiple WS ingress cache-hitting
// it at once, right as the engine recovers) share ONE attach attempt rather
// than each firing their own ContainerAttach: the first caller to observe
// the not-attached state reserves s.reattaching and does the work alone;
// every other concurrent caller finds the reservation and just waits on it
// (or on ctx, so a caller with a bounded ctx — every real caller, see
// Runner.Subscribe's own doc comment — can never hang here longer than its
// own deadline even though the attempt it is waiting on was started by
// somebody else's ctx). This is the session-scoped analogue of
// containerBackend.adopting's backend-scoped dedup for cache-miss doAdopt
// calls (Adopt's own doc comment, PR5 review Major 5) — same shape, smaller
// scope.
//
// A failed re-attach (engine still down) is logged and otherwise swallowed:
// Adopt itself stays ok=true regardless (the session still supports
// signal/stop/wait, exactly as doAdopt's own best-effort attach already
// documented) — only Subscribe observes the difference (see its own doc
// comment).
func (s *containerSession) reattachIfLost(ctx context.Context) {
	s.mu.Lock()
	if !s.running || s.attached {
		s.mu.Unlock()
		return
	}
	if attempt := s.reattaching; attempt != nil {
		s.mu.Unlock()
		select {
		case <-attempt:
		case <-ctx.Done():
		}
		return
	}
	attempt := make(chan struct{})
	s.reattaching = attempt
	// replayLogs decides Logs:true vs Logs:false for the ContainerAttach
	// call below, and MUST be read in the same locked section that
	// reserved s.reattaching above, before releasing s.mu — otherwise a
	// concurrent appendTranscript (a stray write arriving on some other
	// path) could flip the answer between the check and the attach call
	// (Opus review of PR #864, B1).
	//
	// The gate this function reattaches under — running && !attached — is
	// reached by TWO distinct routes, and they need OPPOSITE Logs
	// behavior:
	//
	//  1. doAdopt's own first attach failed (its doc comment): this
	//     containerSession has never produced a single byte of
	//     transcript. Logs:true is not just safe here, it is the ONLY way
	//     to backfill whatever the container already emitted before this
	//     daemon ever knew about it — exactly what doAdopt's own direct
	//     attach(ctx, true) call does for the very same reason.
	//  2. A PREVIOUSLY successful attach's readLoop ended while the
	//     container kept running (an engine/socket-proxy hiccup, or a
	//     live-restore daemon restart) — this session's transcript
	//     already holds everything the container produced up to the
	//     drop. Logs:true here would make the engine replay that SAME
	//     history a second time, landing it in the transcript (and, for a
	//     Launched session, the on-disk spool file — the one thing `boid
	//     job log` can still read after ContainerRemove) as a literal
	//     duplicate. This is not hypothetical: it was measured directly
	//     (a fake engine's Logs:true replay produced "EARLY-OUTPUT" twice
	//     in the transcript after a reattach) before this check existed.
	//
	// Both routes reach this same running&&!attached gate with no other
	// distinguishing signal available except the transcript itself, so
	// "is the transcript still empty" is what selects between them. The
	// trade-off this choice makes (documented so a future change to it is
	// deliberate, not accidental): replaying zero-content for case 1 is
	// always safe (there is nothing to duplicate), but choosing Logs:
	// false for case 2 means whatever the container produced DURING the
	// disconnect window (between readLoop ending and this re-attach
	// succeeding) is permanently lost — it is never in the transcript and
	// Logs:false means the engine never replays it either. The
	// alternative (always Logs:true) trades that gap for corrupting the
	// transcript with a duplicated run of everything already captured,
	// which is worse for `boid job log`'s "this is the literal
	// transcript" contract than a bounded, one-time gap during a
	// reconnect window is — a duplicate is confusing and silently wrong
	// forever, while a gap is at least a clean edge. If a future need
	// makes that gap unacceptable, the fix is to track exactly how much
	// of the container's own output this session has already consumed
	// (a byte offset / `docker logs --since`) and request only the
	// remainder — not to fall back to blanket replay.
	replayLogs := len(s.transcript) == 0
	s.mu.Unlock()

	if err := s.attach(ctx, replayLogs); err != nil {
		slog.Warn("container backend: best-effort re-attach to a running session failed; it will keep supporting signal/stop/wait only until the next cache hit finds the engine reachable",
			"container_id", s.id, "error", err)
	}

	s.mu.Lock()
	s.reattaching = nil
	s.mu.Unlock()
	close(attempt)
}

func (s *containerSession) closeConn() {
	s.connMu.Lock()
	hijack := s.hijack
	s.connMu.Unlock()
	if hijack != nil {
		hijack.Close()
	}
}

// start kicks off the session's single ContainerWait owner (§決定 7).
func (s *containerSession) start() {
	go s.waitLoop()
}

// readLoop is the session's one and only reader of ITS generation's attach
// connection — hijack/readDone are this specific attach() call's own values
// (passed explicitly, not re-read from the session's own possibly-already-
// reassigned fields), so a concurrent reattachIfLost swapping s.hijack/
// s.readDone for a NEW generation can never cause this, the OLD
// generation's reader, to operate on (or close) the wrong channel. Non-TTY
// containers multiplex stdout/stderr with docker's 8-byte-header framing
// (demuxDockerFrame); both streams are combined into a single transcript
// exactly like the userns backend's combined pipe (§決定 8: "TTY/非 TTY と
// も単一結合で stdout/stderr 分離は意図的に無い").
func (s *containerSession) readLoop(hijack *client.HijackedResponse, readDone chan struct{}) {
	defer func() {
		// attached=false BEFORE close(readDone): a concurrent waitLoop or
		// reattachIfLost that wakes up on readDone closing should already
		// see the up to date attached value if it happens to also read
		// mu-guarded state around the same moment (not load-bearing for
		// correctness — both are only ever read together as one snapshot,
		// see their own doc comments — but keeping state-then-signal
		// ordering here matches the pattern used by every doc comment
		// referencing this file's "close as final act" convention).
		s.mu.Lock()
		s.attached = false
		s.mu.Unlock()
		close(readDone)
	}()

	if s.tty {
		buf := make([]byte, 4096)
		for {
			n, err := hijack.Reader.Read(buf)
			if n > 0 {
				s.appendTranscript(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}

	for {
		chunk, err := demuxDockerFrame(hijack.Reader)
		if len(chunk) > 0 {
			s.appendTranscript(chunk)
		}
		if err != nil {
			return
		}
	}
}

// demuxDockerFrame reads one frame of docker's non-TTY attach multiplexed
// stream format: an 8-byte header (byte 0 = stream type [stdout/stderr],
// bytes 1-3 reserved, bytes 4-7 = big-endian uint32 payload size) followed
// by that many payload bytes. This is a small, stable, publicly documented
// wire format (the same one github.com/moby/moby/pkg/stdcopy implements) —
// reimplemented directly here rather than importing that package, which
// lives in the full github.com/moby/moby module and would drag in far more
// than this PR's minimum-dependency mandate allows for ~15 lines of framing
// logic.
func demuxDockerFrame(r *bufio.Reader) ([]byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[4:8])
	if size == 0 {
		return nil, nil
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *containerSession) appendTranscript(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transcript = append(s.transcript, chunk...)
	// Disk spool (§決定8, PR7): mirrors localRuntimeSession.appendTranscript's
	// own `s.transcriptFile.Write(chunk)` — nil (spooling disabled or an
	// Adopt-reconstructed session, see openTranscriptSpool's doc comment)
	// is the overwhelming majority of PR5-vintage callers and a no-op here.
	if s.transcriptFile != nil {
		if _, err := s.transcriptFile.Write(chunk); err != nil {
			slog.Warn("container backend: write transcript spool failed", "container_id", s.id, "error", err)
		}
	}
	for id, ch := range s.subscribers {
		copyChunk := append([]byte(nil), chunk...)
		select {
		case ch <- copyChunk:
		default:
			close(ch)
			delete(s.subscribers, id)
		}
	}
}

// Subscribe mirrors LocalRuntime.SubscribeRuntime's contract exactly
// (including its not-obviously-symmetric ok=false case): a snapshot is
// always returned, even when the session has already exited — a late
// connect after exit still gets the final transcript — but ok is false and
// no channel/cancel is handed back so callers don't wait for output that
// will never arrive.
//
// ok requires BOTH running AND attached (Opus review of PR #857): running
// alone used to be the whole check, which meant a session doAdopt attached
// to best-effort (its doc comment) — running (the container is genuinely
// alive) but never actually attached (the attach call itself failed) —
// answered ok=true with a channel that would then never receive a single
// byte. That is exactly the "connected but the terminal stays blank
// forever" symptom the Web UI/WS ingress hit: the caller had no way to
// distinguish "attached and just quiet" from "will never speak", so it kept
// waiting either way. Requiring attached too means that case now honestly
// reports ok=false.
//
// finished (SandboxSession.Subscribe's own doc comment has the full
// rationale — Opus review of PR #864, B2) is what actually lets
// a caller act on that ok=false correctly: it is simply !running, checked
// under the SAME lock as ok so the two are never observed inconsistently.
// running==false means the container genuinely exited — ingress should
// treat this as "job done", the pre-existing behavior. running==true (this
// is the running-but-not-attached case above) means finished=false — ingress
// must NOT treat this as "job done"; it should surface an error and let
// Adopt's cache-hit path (reattachIfLost) get a chance to fix the
// underlying cause on a LATER call, without needing a daemon restart. The
// first version of this fix set finished implicitly by leaving ingress's
// pre-existing "ok=false → job done" branch untouched, which was itself
// wrong (an unconditional false positive is a worse diagnostic than the
// dead-channel hang it replaced) — the finished return value is what closes
// that gap.
func (s *containerSession) Subscribe() ([]byte, <-chan []byte, func(), bool, bool) {
	s.mu.Lock()
	snapshot := append([]byte(nil), s.transcript...)
	running := s.running
	live := running && s.attached
	var subID int
	var ch chan []byte
	if live {
		subID = s.nextSubID
		s.nextSubID++
		ch = make(chan []byte, 64)
		s.subscribers[subID] = ch
	}
	s.mu.Unlock()

	finished := !running
	if !live {
		return snapshot, nil, func() {}, false, finished
	}
	return snapshot, ch, func() { s.unsubscribe(subID) }, true, false
}

func (s *containerSession) unsubscribe(subID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subscribers[subID]; ok {
		close(ch)
		delete(s.subscribers, subID)
	}
}

func (s *containerSession) closeSubscribersLocked() {
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
}

func (s *containerSession) WriteInput(data []byte) error {
	s.connMu.Lock()
	hijack := s.hijack
	s.connMu.Unlock()
	if hijack == nil {
		return ErrRuntimeUnsupported
	}
	_, err := hijack.Conn.Write(data)
	return err
}

// CloseInput half-closes the attach connection's write side exactly once
// (HijackedResponse.CloseWrite — a no-op, not an error, when the
// underlying net.Conn doesn't support half-close, matching that method's
// own documented fallback). This does not close the output stream (current
// contract, preserved as-is — same as the userns backend's
// LocalRuntime.CloseInputRuntime).
func (s *containerSession) CloseInput() error {
	// Snapshot the CURRENT generation's hijack AND its own *sync.Once
	// together under connMu (N6): attach() replaces both atomically in
	// the same critical section, so this pairing is never observed
	// half-updated. Calling Do on the local copy outside the lock avoids
	// holding connMu across the CloseWrite call itself.
	s.connMu.Lock()
	once := s.stdinCloseOnce
	hijack := s.hijack
	s.connMu.Unlock()
	once.Do(func() {
		if hijack == nil {
			return
		}
		_ = hijack.CloseWrite()
	})
	return nil
}

// sessionControlCallTimeout is defined in runner.go (moved there, Opus
// review of PR #857, Nit 7: most of its consumers are Runner methods, not
// containerSession ones) — see its doc comment there for the full
// rationale. Resize below is the one containerSession-side consumer.

func (s *containerSession) Resize(size backend.TerminalSize) error {
	if size.Rows <= 0 || size.Cols <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	_, err := s.api.ContainerResize(ctx, s.id, client.ContainerResizeOptions{
		Height: uint(size.Rows),
		Width:  uint(size.Cols),
	})
	return err
}

// Wait blocks until the session's single waitLoop (started once, by
// Launch/Adopt's call to start()) observes container exit and closes done —
// §決定 7's single-owner fan-out: however many goroutines call Wait
// concurrently (Runner.watchRuntime and Runner.cleanupSandboxAfterWait both
// do, on the very same session — see launchSandbox's doc comment), exactly
// one ContainerWait API call is ever made.
func (s *containerSession) Wait(ctx context.Context) (backend.RuntimeExit, error) {
	select {
	case <-ctx.Done():
		return backend.RuntimeExit{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		exit := s.exit
		s.mu.Unlock()
		return exit, nil
	}
}

// waitLoop is the session's single ContainerWait owner. Ordering after
// detecting exit follows §決定 7/8's "diagnostics before resource teardown"
// contract: drain the read loop (readDone) so the transcript buffer is
// final, THEN finalize exit state and close done (unblocking Wait
// callers), THEN run the diagnostics collector (if any — see
// ContainerBackendOptions.DiagnosticsCollector's doc comment) to
// completion, THEN — strictly after all of that — remove the container and
// the secret-carrying host spec file. Because container removal happens
// last, both after Wait has already returned to every caller and after the
// diagnostics collector has finished, no caller — nor the collector — can
// observe a removed container through this session's own state.
//
// Removal itself tries without Force first: the container already exited
// (ContainerWait resolved), so a plain remove should succeed; Force is
// reserved for the retry after an error, rather than being applied
// unconditionally on every removal (a "silent force" masks whatever made
// the plain remove fail).
func (s *containerSession) waitLoop() {
	waitRes := s.api.ContainerWait(context.Background(), s.id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	var exitCode int
	// engineError mirrors the RuntimeExit.EngineError this session reports
	// from Wait — see that field's doc comment (internal/sandbox/backend/
	// backend.go) for the full contract. Left empty on the ordinary
	// "the engine gave us a real StatusCode" path below; only the two
	// engine-fault branches (wait response carrying an Error, and the
	// ContainerWait call itself failing) set it, from the ONE place either
	// error's message is still available — the container is gone, and its
	// logs with it, by the time anything downstream of Wait() runs.
	var engineError string
	select {
	case res := <-waitRes.Result:
		// A response is not the same thing as an exit status: container.WaitResponse
		// carries two independent facts, and only StatusCode is one. When the engine
		// could not report an exit status at all (the runtime failed to wait, the
		// shim died, the container never started), it answers with StatusCode: 0 and
		// a non-nil Error — the SDK does not distinguish this from an actual "exited
		// 0" (moby/moby/client@v0.5.0 container_wait.go forwards the body verbatim;
		// see waitResponseEngineError's doc comment in
		// container_backend_workspace_init.go).
		//
		// This is this layer's own correctness question — exitCode is the value
		// this session reports as ITS exit status, and an engine that never filled
		// in StatusCode must not be read as "exited 0" here regardless of what any
		// caller happens to do with that value afterward. In today's one caller,
		// Runner.watchRuntime (runner.go) independently coerces a zero exit code to
		// 1 and unconditionally marks the job JobStatusFailed on this path (a
		// container exiting without the in-container runner's own "job done"
		// report), so this fix does not change whether that job ends up failed —
		// it changes whether the failure is reported honestly (exit_code in the
		// job's own output stops contradicting job.ExitCode) and whether
		// NewDefaultDiagnosticsCollector's exit.ExitCode != 0 gate — the collector
		// that exists specifically for the "container didn't run right" case —
		// actually fires for this one. Depending on watchRuntime's coercion for the
		// task-done consequence would be relying on a property of a different layer
		// that could drift independently of this one; this fix keeps it correct
		// here on its own terms as well.
		if eerr := waitResponseEngineError(res); eerr != nil {
			slog.Warn("container backend: ContainerWait response carried an engine error", "container_id", s.id, "error", eerr)
			exitCode = 1
			engineError = eerr.Error()
		} else {
			exitCode = int(res.StatusCode)
		}
	case err := <-waitRes.Error:
		exitCode = 1
		// waitChannelError (container_backend_workspace_init.go) covers the
		// nil case this branch would otherwise panic on; see its doc comment.
		//
		// Resolved BEFORE the Warn, not after: the Warn used to fire above
		// the nil guard, so the one case the guard exists for was logged as
		// `error=<nil>` while job.Output and diagnostics.json both got the
		// fallback wording — the daemon log, which is where an operator looks
		// first, was the only place that said nothing
		// (next-session-container-backend-followups.md #3).
		engineError = waitChannelError(err).Error()
		slog.Warn("container backend: ContainerWait failed", "container_id", s.id, "error", engineError)
	}

	// The container process has exited, but its attach stream can still
	// deliver a final burst of already-produced output for a short window
	// afterward. Prefer letting readLoop drain it naturally — it returns
	// (closing readDone) once the daemon itself closes the stream — rather
	// than closing our side immediately, which could truncate exactly that
	// final burst. Only force-close via closeConn if draining hasn't
	// finished within attachDrainGracePeriod: this bounds the wait and
	// still guarantees readDone closes even if the daemon is slow.
	//
	// attached/readDone are snapshotted together under mu — not read as
	// `s.readDone` directly — for two reasons (Opus review of PR #857).
	// First, readDone is no longer a fixed, once-set field: reattachIfLost
	// can swap it for a fresh generation at any point while the container
	// is still running (concurrently with this very goroutine, which spends
	// most of its life blocked in the ContainerWait select above), so a
	// bare field read here would race that write. Second, and more
	// fundamentally, whether there is anything to drain at all is no longer
	// implied by readDone alone: a session that was NEVER successfully
	// attached (doAdopt's best-effort attach failed and no later cache-hit
	// reattachIfLost fixed it before the container happened to exit) has
	// attached=false, and skips the select/drain entirely rather than
	// relying on the old "attach's error path already closed readDone
	// synchronously, so the select falls through immediately" indirection —
	// simpler to read and no longer even true once readDone is per-
	// generation (an old, already-closed generation's channel would make
	// the select fall through for the WRONG reason if reattachIfLost had
	// since installed a new, live, not-yet-closed one).
	s.mu.Lock()
	attached := s.attached
	readDone := s.readDone
	s.mu.Unlock()
	if attached {
		select {
		case <-readDone:
			s.closeConn()
		case <-time.After(attachDrainGracePeriod):
			s.closeConn()
			<-readDone
		}
	} else {
		// No live stream to drain. closeConn is still called — a no-op
		// when s.hijack is nil (attach's own error path never sets it),
		// but a real (if likely redundant) close in the rarer case where
		// attached went false because a PREVIOUS generation's readLoop
		// already ended (a dropped connection reattachIfLost never got a
		// chance to retry before the container exited) and its hijack is
		// still sitting there unclosed.
		s.closeConn()
	}

	// Close (and flush) the disk transcript spool now: readLoop — the sole
	// writer via appendTranscript — has already returned (readDone closed
	// above), so no further writes can race this Close in the common case.
	// Doing this BEFORE finalizing exit state / closing s.done means a
	// diagnostics collector that reads transcript.log from disk (§決定8's
	// silent-exit classification) always sees the complete file, and
	// BEFORE ContainerRemove means the file is guaranteed durable before
	// the container itself (and any `docker logs` fallback) is gone.
	//
	// [N3, Opus review of PR #864]: "no further writes can race this" is
	// no longer a PURE structural guarantee of this file's own logic the
	// way it was before reattachIfLost existed — it now also leans on the
	// engine's own behavior. s.running only flips false a few lines below
	// this point, strictly AFTER this drain-select and this Sync/Close —
	// so a cache-hit Adopt racing exactly this window still sees
	// running==true and (once the drain-select above has finished)
	// attached==false, and will fire a best-effort reattachIfLost against
	// a container that has, in fact, already exited (ContainerWait already
	// resolved) but has not been ContainerRemove'd yet (that happens even
	// later in this same function). If that stray ContainerAttach were to
	// SUCCEED, it would start a brand new readLoop that could then write
	// to s.transcriptFile concurrently with the Sync()/Close() calls right
	// below — a real race, not merely a wasted API call. In measured
	// practice this window is not empty (26-30 out of 30 induced exit-vs-
	// reattach races land a stray attach attempt in it, each logged as a
	// WARN) but is harmless BECAUSE both docker and podman refuse to
	// attach to a non-running container, so the stray attach fails and no
	// second readLoop ever starts — the invariant holds today because of
	// that engine behavior, not because this window is provably empty on
	// this file's own terms. If a future engine (or a mocked/fake one in a
	// test) ever allowed an attach against an already-exited-but-not-yet-
	// removed container, this comment's claim would stop being true.
	//
	// [Major 9, PR7 codex review]: Sync() runs BEFORE Close(), not just
	// Close() alone. Close() flushes the process's own userspace buffers to
	// the kernel but makes no durability guarantee beyond that — a power
	// loss between Close() and the data actually reaching disk could still
	// lose the tail of a job's transcript right as its container is
	// removed, at precisely the moment `boid job log`'s only remaining
	// source of truth. A Sync failure is escalated to Error (louder than
	// the general Warn used elsewhere in this file) since it is the
	// durability guarantee §決定8's "full 永続" contract depends on; Close
	// still runs (and ContainerRemove still proceeds) even when Sync fails
	// — blocking container teardown indefinitely on a persistent disk error
	// would leak the container itself and defeat the reap contract, a worse
	// outcome than a possibly-incomplete transcript tail.
	if s.transcriptFile != nil {
		if err := s.transcriptFile.Sync(); err != nil {
			slog.Error("container backend: sync transcript spool failed; the transcript tail may not survive a crash before it reaches disk",
				"container_id", s.id, "path", s.transcriptPath, "error", err)
		}
		if err := s.transcriptFile.Close(); err != nil {
			slog.Warn("container backend: close transcript spool failed", "container_id", s.id, "path", s.transcriptPath, "error", err)
		}
	}

	s.mu.Lock()
	s.running = false
	s.exit = backend.RuntimeExit{ExitCode: exitCode, TranscriptPath: s.transcriptPath, EngineError: engineError}
	s.closeSubscribersLocked()
	exit := s.exit
	s.mu.Unlock()
	close(s.done)

	if collector := s.backend.diagnosticsCollector; collector != nil {
		collector(context.Background(), s.id, exit)
	}

	s.backend.forgetSession(s.id)
	// Bounded, like every other teardown removal in this file (codex round 2 of
	// PR8, Major 3 — see containerCleanupContext). This one blocks no caller (the
	// exit is already published and s.done already closed), so an unbounded
	// version leaks a goroutine rather than wedging a dispatch; it is bounded
	// anyway so that "a teardown ContainerRemove in this package always has a
	// deadline" is a property of the file rather than of each site. The base is
	// Background() rather than a request context because waitLoop outlives every
	// request that could have started it.
	removeCtx, cancelRemove := containerCleanupContext(context.Background())
	if _, err := s.api.ContainerRemove(removeCtx, s.id, client.ContainerRemoveOptions{RemoveVolumes: true}); err != nil {
		slog.Warn("container backend: remove exited container failed; retrying with Force", "container_id", s.id, "error", err)
		if _, ferr := s.api.ContainerRemove(removeCtx, s.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); ferr != nil {
			slog.Warn("container backend: force remove exited container failed", "container_id", s.id, "error", ferr)
		}
	}
	cancelRemove()
	if s.specDir != "" {
		// Blocker 1 (PR7 codex review): a runtimeDir-scoped spec lives in its
		// own per-job directory (<runtimeDir>/spec/<spec.ID>/) — remove it
		// wholesale rather than just specPath, matching Launch's cleanupFiles
		// on the error path.
		_ = os.RemoveAll(s.specDir)
	} else if s.specPath != "" {
		_ = os.Remove(s.specPath)
	}
	if s.dockerTLSDir != "" {
		_ = os.RemoveAll(s.dockerTLSDir)
	}
	if s.brokerTLSDir != "" {
		_ = os.RemoveAll(s.brokerTLSDir)
	}
}

// Stop requests graceful termination: docker stop sends the container's
// configured stop signal (SIGTERM by default) and waits up to a timeout
// (docker's own default, 10s — not overridden here) before SIGKILL.
func (s *containerSession) Stop(ctx context.Context) error {
	_, err := s.api.ContainerStop(ctx, s.id, client.ContainerStopOptions{})
	return err
}

// Signal delivers sig to the container's PID 1 (docker-init, §決定 3) via
// `docker kill --signal=<sig>` — no SIGKILL follow-up, matching the
// interface contract. docker-init forwards signals to its child (the boid
// runner-container entrypoint), whose harness adapters' own
// sigutil.ForwardAndWait reacts to SIGUSR1 exactly as the userns path's
// SIG_IGN'd-then-adapter-handled chain does (see the plan doc's §決定 3).
func (s *containerSession) Signal(ctx context.Context, sig syscall.Signal) error {
	name := unix.SignalName(sig)
	if name == "" {
		name = strconv.Itoa(int(sig))
	}
	_, err := s.api.ContainerKill(ctx, s.id, client.ContainerKillOptions{Signal: name})
	return err
}
