package sandbox

// CloneSpec is the runner-facing declaration of the sandbox-internal git
// clone + branch resolution launch sequence. It is the sandbox.Spec-level
// counterpart of orchestrator.CloneDeclaration: dispatcher's
// BuildSandboxSpec fills in the concrete gateway clone URL, the
// sandbox-internal mount paths, and the real (non-shimmed) git binary path
// that orchestrator has no business knowing about.
//
// The zero value (Enabled == false) is a complete no-op: the runner skips
// the whole sequence.
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
	// just without the object-sharing optimization — graceful degradation,
	// not a hard dependency.
	ReferenceDir string

	// TargetDir is the sandbox-internal neutral path to clone into. An
	// existing TargetDir is removed before cloning, so reopen (re-running
	// this same sequence) is idempotent by re-clone rather than
	// fetch-in-place.
	TargetDir string

	// RealGitBin is the sandbox-internal path of a real (non-shimmed) git
	// binary so the runner's own clone/branch-resolution git invocations
	// are not routed through the broker-dispatch git builtin. Empty falls
	// back to a bare "git" lookup on $PATH.
	RealGitBin string

	// Branch, BaseBranch, CheckoutOnly and BaseBranchForkPoint mirror
	// orchestrator.CloneDeclaration 1:1 — see that type's doc comments for
	// the exact resolution semantics.
	Branch              string
	BaseBranch          string
	CheckoutOnly        bool
	BaseBranchForkPoint string
}
