package dispatcher_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): Runner tracks a
// JobContextSnapshot per dispatched job so the `boid task env` / `boid task
// payload` broker RPCs can serve this exact job's env/payload data — without
// re-deriving job-scoped facts (allowed_domains + resolved host commands,
// the trait-filtered payload) that only exist at dispatch time.

func TestDispatch_TracksJobContext_EnvAndPayload(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()
	r.AllowedDomains = []string{"github.com", "example.com"}

	payload := json.RawMessage(`{"artifact":{"report":"ok"}}`)
	spec := &orchestrator.JobSpec{
		ProjectID:    "proj-1",
		Argv:         []string{"echo", "hi"},
		Kind:         orchestrator.JobKindHook,
		PrimaryInput: payload,
		HostCommands: map[string]orchestrator.CommandDef{
			"gh": {Name: "gh", AllowedSubcommands: []string{"pr"}},
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found after successful Dispatch", jobID)
	}
	if len(snap.Env.AllowedDomains) != 2 || snap.Env.AllowedDomains[0] != "github.com" {
		t.Errorf("Env.AllowedDomains = %v, want [github.com example.com]", snap.Env.AllowedDomains)
	}
	if len(snap.Env.HostCommands) != 1 || snap.Env.HostCommands[0].Name != "gh" {
		t.Errorf("Env.HostCommands = %+v, want 1 entry named gh", snap.Env.HostCommands)
	}
	if string(snap.Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", snap.Payload, payload)
	}
}

// TestDispatch_TracksJobContext_PayloadPatchAllowedTraits pins Phase 5b
// PR7's codex review Major 1 fix (wiring-seams.md #17):
// JobContextSnapshot.PayloadPatchAllowedTraits must come straight from
// spec.HookTraitsProduces (itself captured at PlanHook time from the firing
// hook's own Traits.Produces) — not re-derived from anything live — so
// `boid task update --payload-patch`'s allowedTraits gate can never observe
// a project-meta edit that happened after this exact job was dispatched.
func TestDispatch_TracksJobContext_PayloadPatchAllowedTraits(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()

	spec := &orchestrator.JobSpec{
		ProjectID:          "proj-1",
		Argv:               []string{"echo", "hi"},
		Kind:               orchestrator.JobKindHook,
		HookTraitsProduces: []orchestrator.TraitType{"artifact", "verification"},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found after successful Dispatch", jobID)
	}
	want := []orchestrator.TraitType{"artifact", "verification"}
	if len(snap.PayloadPatchAllowedTraits) != len(want) {
		t.Fatalf("PayloadPatchAllowedTraits = %v, want %v", snap.PayloadPatchAllowedTraits, want)
	}
	for i, tr := range want {
		if snap.PayloadPatchAllowedTraits[i] != tr {
			t.Errorf("PayloadPatchAllowedTraits[%d] = %q, want %q", i, snap.PayloadPatchAllowedTraits[i], tr)
		}
	}
}

// TestDispatch_TracksJobContext_PayloadPatchAllowedTraits_NilJobSpecFieldYieldsNil
// pins nil passthrough: a JobSpec with no HookTraitsProduces (the
// virtual/synthesized agent hook case, or any non-hook job) must leave the
// snapshot's PayloadPatchAllowedTraits nil — not an empty-but-non-nil slice,
// which MergePayloadPatch treats as "reject every trait" rather than
// "unrestricted".
func TestDispatch_TracksJobContext_PayloadPatchAllowedTraits_NilJobSpecFieldYieldsNil(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found after successful Dispatch", jobID)
	}
	if snap.PayloadPatchAllowedTraits != nil {
		t.Errorf("PayloadPatchAllowedTraits = %v, want nil", snap.PayloadPatchAllowedTraits)
	}
}

// TestDispatch_TracksJobContext_Instructions_MatchesJobSpec verifies
// JobContextSnapshot.Instructions is populated straight from
// spec.Instruction — the same value contextFiles would have written to
// instructions.yaml for this exact job.
func TestDispatch_TracksJobContext_Instructions_MatchesJobSpec(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Instruction: &orchestrator.RoutedInstruction{
			Agent:   "claude-code",
			Message: "do the thing",
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found", jobID)
	}
	if len(snap.Instructions) != 1 || snap.Instructions[0].Agent != "claude-code" || snap.Instructions[0].Message != "do the thing" {
		t.Errorf("Instructions = %+v, want the single routed instruction from spec.Instruction", snap.Instructions)
	}
}

// TestDispatch_TracksJobContext_Instructions_NilJobSpecInstructionYieldsEmpty
// is the direct regression guard for the codex-review finding on PR #797:
// orchestrator.Evaluator can fire two agent-kind hooks for different agents
// from the same task in one round (extractInstructionAgents matches any
// agent in the instruction history, not just the active/last entry), but
// only the hook whose agent equals the *last* history entry gets a non-nil
// spec.Instruction (selectInstruction/FilterInstructions only look at the
// last entry) — the other hook's JobSpec.Instruction is nil, and its
// instructions.yaml file is correspondingly never written. A job whose
// spec.Instruction is nil must track an EMPTY instructions list, not
// something re-derived from the task row (which would incorrectly hand it
// the other hook's agent's instruction).
func TestDispatch_TracksJobContext_Instructions_NilJobSpecInstructionYieldsEmpty(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()

	// Simulates the claude-code hook's JobSpec when the task's instruction
	// history is [claude-code, codex] (active/last entry is codex): the
	// evaluator still matches and fires the claude-code hook (its agent
	// appears in the history), but selectInstruction returns nil for it
	// since it doesn't match the *active* (last) entry.
	spec := &orchestrator.JobSpec{
		ProjectID:   "proj-1",
		Argv:        []string{"echo", "hi"},
		Kind:        orchestrator.JobKindHook,
		Instruction: nil,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found", jobID)
	}
	if len(snap.Instructions) != 0 {
		t.Errorf("Instructions = %+v, want empty (spec.Instruction was nil) — must NOT be re-derived from the task row", snap.Instructions)
	}
}

func TestDispatch_TracksJobContext_NilPrimaryInput(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindExec,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found", jobID)
	}
	if len(snap.Payload) != 0 {
		t.Errorf("Payload = %s, want empty for a job with no PrimaryInput", snap.Payload)
	}
}

func TestUnregisterJob_RemovesJobContext(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Backend = newStatefulBackend()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, ok := r.JobContext(jobID); !ok {
		t.Fatalf("JobContext(%q) should be tracked right after Dispatch", jobID)
	}

	r.UnregisterJob(jobID)

	if _, ok := r.JobContext(jobID); ok {
		t.Errorf("JobContext(%q) should be gone after UnregisterJob", jobID)
	}
}

func TestJobContext_UnknownJobID_ReturnsFalse(t *testing.T) {
	r := &dispatcher.Runner{}
	if _, ok := r.JobContext("no-such-job"); ok {
		t.Error("expected ok=false for an untracked job id")
	}
}

// TestDispatch_TracksJobContext_WorkspacePeerAdvertise pins the `boid
// project list` peer-discovery feature: a clone-mode job dispatched with a
// workspace peer present must have that peer's PeerAdvertise (clone URL,
// reference path, clone dir — the same value fed into
// SandboxRuntimeInfo.WorkspacePeerAdvertise via Runner.buildPeerAdvertise)
// show up in JobContextSnapshot.WorkspacePeerAdvertise, keyed by peer
// project ID. This is the seam BoidOpProjectList reads from
// (internal/server/boid_executor.go) to answer `boid project list` with
// peer clone info.
func TestDispatch_TracksJobContext_WorkspacePeerAdvertise(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/host/self", UpstreamURL: "https://github.com/owner/self.git",
	}); err != nil {
		t.Fatalf("create self project: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "peer-1", WorkDir: "/host/peer", UpstreamURL: "https://github.com/owner/peer.git",
	}); err != nil {
		t.Fatalf("create peer project: %v", err)
	}
	// WorkspaceID lives in the separate project_workspaces join table
	// (project_catalog.go's CreateProject never writes it, even when set on
	// the *Project struct passed in — see GetProject's own LEFT JOIN).
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-1"); err != nil {
		t.Fatalf("set self project workspace: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "peer-1", "ws-1"); err != nil {
		t.Fatalf("set peer project workspace: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	r := &dispatcher.Runner{
		DB:          d.Conn,
		Projects:    orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:     &capturingSandboxBackend{},
		BoidBinary:  "/boid",
		GitGateway:  gitgateway.NewRegistry(),
		GatewayURL:  &gwURL,
		RuntimesDir: t.TempDir(),
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			ProjectDir: "/host/self",
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found after successful Dispatch", jobID)
	}
	adv, ok := snap.WorkspacePeerAdvertise["peer-1"]
	if !ok {
		t.Fatalf("WorkspacePeerAdvertise = %#v, want an entry for peer-1", snap.WorkspacePeerAdvertise)
	}
	if adv.CloneURL == "" {
		t.Error("peer advertise CloneURL is empty, want a gateway clone URL")
	}
	if adv.Name != "peer" {
		t.Errorf("peer advertise Name = %q, want %q", adv.Name, "peer")
	}
}

// TestDispatch_TracksJobContext_WorkspacePeerAdvertise_NilWhenNotCloneMode
// pins the non-clone-mode degrade: buildPeerAdvertise is only computed
// inside the clone-mode branch of dispatchProjectSection
// (gitgateway_wire.go), so a job with Visibility.Clone == nil must leave
// WorkspacePeerAdvertise nil even though a workspace peer exists and the
// gateway is wired — mirrors GatewayCloneURL's own non-clone-mode empty
// behavior.
func TestDispatch_TracksJobContext_WorkspacePeerAdvertise_NilWhenNotCloneMode(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/host/self", UpstreamURL: "https://github.com/owner/self.git",
	}); err != nil {
		t.Fatalf("create self project: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "peer-1", WorkDir: "/host/peer", UpstreamURL: "https://github.com/owner/peer.git",
	}); err != nil {
		t.Fatalf("create peer project: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-1"); err != nil {
		t.Fatalf("set self project workspace: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "peer-1", "ws-1"); err != nil {
		t.Fatalf("set peer project workspace: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	r := &dispatcher.Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    newStatefulBackend(),
		BoidBinary: "/boid",
		GitGateway: gitgateway.NewRegistry(),
		GatewayURL: &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		// Visibility.Clone deliberately left nil.
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found after successful Dispatch", jobID)
	}
	if snap.WorkspacePeerAdvertise != nil {
		t.Errorf("WorkspacePeerAdvertise = %#v, want nil for a non-clone-mode job", snap.WorkspacePeerAdvertise)
	}
}

// TestDispatch_CloneMode_MissingUpstreamURL_NoJobContextTracked pins the
// ordering guarantee the peer-advertise wiring depends on: moving
// trackJobContext to after the project-registry-guarded section
// (dispatchProjectSection) means a job that fails inside that section (e.g.
// missing upstream_url, TestDispatch_CloneMode_MissingUpstreamURL_FailsLoud
// in gitgateway_wire_test.go) must never have a JobContextSnapshot tracked
// at all — Dispatch returns an error before trackJobContext runs.
func TestDispatch_CloneMode_MissingUpstreamURL_NoJobContextTracked(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	r := &dispatcher.Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    newStatefulBackend(),
		BoidBinary: "/boid",
	}
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			Clone: &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err == nil {
		t.Fatal("Dispatch: expected error when the project has no captured upstream_url")
	}

	// Dispatch returns "" as its job id on this failure path, so recover the
	// real (DB-assigned) job id directly to check nothing was tracked for it.
	var realJobID string
	if err := d.Conn.QueryRow(`SELECT id FROM jobs WHERE project_id = ?`, "proj-1").Scan(&realJobID); err != nil {
		t.Fatalf("look up the failed job's id: %v", err)
	}
	if _, ok := r.JobContext(realJobID); ok {
		t.Errorf("JobContext(%q) should not be tracked when Dispatch fails inside the project-registry-guarded section", realJobID)
	}
}
