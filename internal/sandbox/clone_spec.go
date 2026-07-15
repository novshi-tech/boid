package sandbox

// CloneSpec is the runner-inner-child-facing declaration of the sandbox-
// internal git clone + branch resolution launch sequence
// (docs/plans/git-gateway-cutover.md PR5: 「runner の clone 実行機構 + branch
// 宣言」). It is the sandbox.Spec-level counterpart of
// orchestrator.CloneDeclaration: dispatcher's BuildSandboxSpec fills in the
// concrete gateway clone URL, the sandbox-internal mount paths, and the real
// (non-shimmed) git binary path that orchestrator has no business knowing
// about.
//
// The zero value (Enabled == false) is a complete no-op: runner-inner-child
// skips the whole sequence and the existing worktree bind-mount path runs
// exactly as it did before PR5. Only test callers construct a CloneSpec with
// Enabled == true until the PR6 cutover switches dispatch over to this path
// by default.
type CloneSpec struct {
	// Enabled gates the entire sequence.
	Enabled bool

	// URL is the full gateway clone URL, e.g.
	// "http://10.0.2.2:<port>/j/<job-token>/<host>/<owner>/<repo>.git". It
	// carries a live job token — see runner/state.go's redactCloneURLToken,
	// which strips the token before this value (or anything derived from
	// it) is written to runner-state.json.
	URL string

	// ReferenceDir is the sandbox-internal path of the RO bind-mounted host
	// repo `.git` used as `git clone --reference`. Empty (or missing on
	// disk at clone time) disables --reference: the clone still succeeds,
	// just without the object-sharing optimization
	// (docs/plans/container-based-boid.md: 「--reference を欠いても遅くなる
	// だけで正しく動く」— graceful degradation, not a hard dependency).
	ReferenceDir string

	// TargetDir is the sandbox-internal neutral path to clone into
	// (docs/plans/git-gateway-cutover.md: 「clone 先は sandbox 内の中立 path
	// /workspace」). An existing TargetDir is removed before cloning, so
	// reopen (re-running this same sequence) is idempotent by re-clone
	// rather than fetch-in-place.
	TargetDir string

	// RealGitBin is the sandbox-internal path of a real (non-shimmed) git
	// binary, bind-mounted read-only alongside boid's git-shim overlay so
	// the runner's own clone/branch-resolution git invocations are not
	// routed through the broker-dispatch git builtin. PR6 removes the shim
	// entirely; until then this is the only genuine git binary inside the
	// sandbox. Empty falls back to a bare "git" lookup on $PATH (which, in
	// production dispatch, would resolve to the shim — harmless today since
	// production JobSpecs never set Enabled).
	RealGitBin string

	// Branch, BaseBranch, CheckoutOnly and BaseBranchForkPoint mirror
	// orchestrator.CloneDeclaration 1:1 — see that type's doc comments for
	// the exact resolution semantics.
	Branch              string
	BaseBranch          string
	CheckoutOnly        bool
	BaseBranchForkPoint string
}
