package dispatcher

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// fakeGetOriginURL is a shared ResolveHostCommands getOriginURL stub for
// tests in this file that don't exercise ${boid:repo_slug} expansion; none
// of their Env values contain the placeholder, so it is never actually
// invoked, but every call site still needs a non-nil func.
func fakeGetOriginURL(string) (string, error) {
	return "", fmt.Errorf("getOriginURL should not be called")
}

// Interactive=true の hook job は PTY 上で動かす必要があるため、 PrimaryInput を
// stdin に pipe したり stdout を capture file へ落としたりすると claude code 等の
// TUI が isatty() で非対話判定して落ちる。 Interactive 時は両方とも抑止し、
// PrimaryInput は context file 経路 ($HOME/.boid/context/payload.json) で渡す。
func TestBuildSandboxSpec_InteractiveDisablesStdinAndStdoutCapture(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Interactive:  true,
		PrimaryInput: []byte(`{"payload":"x"}`),
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: false})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if len(result.StdinBytes) != 0 {
		t.Errorf("StdinBytes = %q, want empty (Interactive jobs must not pipe stdin)", string(result.StdinBytes))
	}
	if result.StdoutCaptureFile != "" {
		t.Errorf("StdoutCaptureFile = %q, want empty (Interactive jobs must not redirect stdout to a file)", result.StdoutCaptureFile)
	}
	if !result.TTY {
		t.Errorf("TTY = false, want true (Interactive=true should request a PTY)")
	}
}

// 非 Interactive な non-foreground hook は従来どおり PrimaryInput を stdin に流し、
// stdout を /tmp/boid-output に capture する。
func TestBuildSandboxSpec_NonInteractiveKeepsStdinPipeAndStdoutCapture(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Interactive:  false,
		PrimaryInput: []byte(`{"payload":"x"}`),
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: false})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if string(result.StdinBytes) != `{"payload":"x"}` {
		t.Errorf("StdinBytes = %q, want PrimaryInput", string(result.StdinBytes))
	}
	if result.StdoutCaptureFile != "/tmp/boid-output" {
		t.Errorf("StdoutCaptureFile = %q, want /tmp/boid-output", result.StdoutCaptureFile)
	}
}

// git gateway cutover: boid exec は Runner.Dispatch() 経由の JobKindExec job に
// なった。非対話 (パイプ) exec でも live streaming が必要なので、hook と違い
// stdout capture file には落とさない — see sandbox_builder.go's stdoutCapture
// comment for the full rationale.
func TestBuildSandboxSpec_ExecNonInteractiveSkipsStdoutCapture(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Kind:        orchestrator.JobKindExec,
		Interactive: false,
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: false})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if result.StdoutCaptureFile != "" {
		t.Errorf("StdoutCaptureFile = %q, want empty (exec must stream live, not capture to a file)", result.StdoutCaptureFile)
	}
}

// Hook jobs (Kind unset / JobKindHook) keep the pre-existing batch-capture
// behavior untouched by the JobKindExec carve-out above.
func TestBuildSandboxSpec_HookNonInteractiveStillCapturesStdout(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Kind:        orchestrator.JobKindHook,
		Interactive: false,
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: false})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if result.StdoutCaptureFile != "/tmp/boid-output" {
		t.Errorf("StdoutCaptureFile = %q, want /tmp/boid-output (hook behavior must be unchanged)", result.StdoutCaptureFile)
	}
}

// TTY はspec.Interactive のみで決まる。Instruction の有無や PrimaryInput(stdin)
// は Phase 2 以降では TTY に影響しない。
func TestBuildSandboxSpec_TTYFollowsInteractiveOnly(t *testing.T) {
	cases := []struct {
		name        string
		interactive bool
		hasInst     bool
		hasStdin    bool
		wantTTY     bool
	}{
		{name: "Interactive=true → TTY=true", interactive: true, wantTTY: true},
		{name: "Interactive=false + Instruction → TTY=false", interactive: false, hasInst: true, wantTTY: false},
		{name: "Interactive=false + stdin → TTY=false", interactive: false, hasStdin: true, wantTTY: false},
		{name: "Interactive=false, nothing else → TTY=false", interactive: false, wantTTY: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &orchestrator.JobSpec{Interactive: tc.interactive}
			if tc.hasInst {
				spec.Instruction = &orchestrator.RoutedInstruction{}
			}
			if tc.hasStdin {
				spec.PrimaryInput = []byte(`{"key":"value"}`)
			}
			result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
			if err != nil {
				t.Fatalf("BuildSandboxSpec: %v", err)
			}
			if result.TTY != tc.wantTTY {
				t.Errorf("TTY = %v, want %v", result.TTY, tc.wantTTY)
			}
		})
	}
}

// sandbox 内の git は default で credential prompt を出す。 git-gateway 経由の
// clone/fetch で upstream が 401 を返すと sandbox 内 TUI が
// `Username for 'http://10.0.2.2:...':` で hang して Ctrl-C するまで解けない。
// 主対策は gateway 側 fail-fast (docs/plans/gitgateway-credential-fail-fast.md
// PR-B) だが、gateway 外の直リンク origin や upstream 側 PAT 失効経路の 401
// でも hang しないよう defense-in-depth として env に GIT_TERMINAL_PROMPT=0 +
// GIT_ASKPASS=/bin/false を default 注入する (PR-C)。
func TestBuildSandboxSpec_InjectsGitPromptSuppressionByDefault(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if got := result.Env["GIT_TERMINAL_PROMPT"]; got != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0 (defense-in-depth against sandbox git prompt hang)", got)
	}
	if got := result.Env["GIT_ASKPASS"]; got != "/bin/false" {
		t.Errorf("GIT_ASKPASS = %q, want /bin/false (block askpass helper path)", got)
	}
}

// spec.Env で明示的に GIT_TERMINAL_PROMPT / GIT_ASKPASS を指定した場合は
// default 注入で上書きしない。 これは例えば hook job で独自の askpass helper を
// 使いたいユースケース (現状は無いが将来のため) を潰さないためのガード。
func TestBuildSandboxSpec_RespectsExplicitGitPromptOverride(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Env: map[string]string{
			"GIT_TERMINAL_PROMPT": "1",
			"GIT_ASKPASS":         "/usr/local/bin/my-askpass",
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if got := result.Env["GIT_TERMINAL_PROMPT"]; got != "1" {
		t.Errorf("GIT_TERMINAL_PROMPT overridden by default: got %q, want 1 (spec.Env should win)", got)
	}
	if got := result.Env["GIT_ASKPASS"]; got != "/usr/local/bin/my-askpass" {
		t.Errorf("GIT_ASKPASS overridden by default: got %q, want /usr/local/bin/my-askpass", got)
	}
}

// BOID_HOST_IP はサンドボックス NS からホスト localhost に向かう pasta gateway
// (10.0.2.2) を指す。proxy の有無に関わらず常に注入して、サンドボックス内プロセ
// スが http_proxy のパース等に頼らず直接 IP を引けるようにする。
func TestBuildSandboxSpec_BoidHostIPAlwaysInjected(t *testing.T) {
	cases := []struct {
		name      string
		proxyPort int
	}{
		{"proxy disabled", 0},
		{"proxy enabled", 8888},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &orchestrator.JobSpec{}
			rt := SandboxRuntimeInfo{ProxyPort: tc.proxyPort}
			result, err := BuildSandboxSpec(spec, rt)
			if err != nil {
				t.Fatalf("BuildSandboxSpec: %v", err)
			}
			if got := result.Env["BOID_HOST_IP"]; got != "10.0.2.2" {
				t.Errorf("BOID_HOST_IP = %q, want 10.0.2.2", got)
			}
		})
	}
}

// TestBuildSandboxSpec_WorkspaceSlugEnv pins the Phase 4 PR3 wiring (docs/plans/
// home-workspace-volume.md): the fail-fast "CLI not found" error each
// adapter's Run() returns (claude/codex/opencode run.go) needs to name the
// workspace whose init.sh the user must edit, and it reads that name from
// BOID_WORKSPACE_SLUG in RunContext.Env — which is just spec.Env, so it has
// to be set here in BuildSandboxSpec from SandboxRuntimeInfo.WorkspaceSlug.
func TestBuildSandboxSpec_WorkspaceSlugEnv(t *testing.T) {
	t.Run("set when rt.WorkspaceSlug is non-empty", func(t *testing.T) {
		spec := &orchestrator.JobSpec{}
		rt := SandboxRuntimeInfo{WorkspaceSlug: "myws"}
		result, err := BuildSandboxSpec(spec, rt)
		if err != nil {
			t.Fatalf("BuildSandboxSpec: %v", err)
		}
		if got := result.Env["BOID_WORKSPACE_SLUG"]; got != "myws" {
			t.Errorf("BOID_WORKSPACE_SLUG = %q, want %q", got, "myws")
		}
	})
	t.Run("absent when rt.WorkspaceSlug is empty", func(t *testing.T) {
		spec := &orchestrator.JobSpec{}
		result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
		if err != nil {
			t.Fatalf("BuildSandboxSpec: %v", err)
		}
		if _, ok := result.Env["BOID_WORKSPACE_SLUG"]; ok {
			t.Errorf("BOID_WORKSPACE_SLUG should be absent for test wiring that never resolved a workspace, got %q", result.Env["BOID_WORKSPACE_SLUG"])
		}
	})
}

// behavior は canonical 名 (executor/supervisor) で BOID_INVOKED_BEHAVIOR に渡す。
// 現在の agent runner は behavior に依らず /boid-task を起動するが、 deprecated 別名
// (dev/plan) は canonical 化してから渡す慣習を残しているのでテストも維持する。 旧
// BOID_INVOKED_TYPE は instruction の phase 種別 (常に "execution") を運んでいて
// behavior と取り違えられていたため廃止する。
func TestBuildSandboxSpec_InvokedBehaviorIsCanonical(t *testing.T) {
	cases := []struct {
		behavior string
		want     string
	}{
		{"executor", "executor"},
		{"supervisor", "supervisor"},
		{"dev", "executor"},    // deprecated alias → canonical
		{"plan", "supervisor"}, // deprecated alias → canonical
	}
	for _, tc := range cases {
		t.Run(tc.behavior, func(t *testing.T) {
			spec := &orchestrator.JobSpec{
				Instruction: &orchestrator.RoutedInstruction{Agent: "claude-code"},
				Task:        &orchestrator.TaskSnapshot{Behavior: tc.behavior},
			}
			result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
			if err != nil {
				t.Fatalf("BuildSandboxSpec: %v", err)
			}
			if got := result.Env["BOID_INVOKED_BEHAVIOR"]; got != tc.want {
				t.Errorf("BOID_INVOKED_BEHAVIOR = %q, want %q", got, tc.want)
			}
			if _, ok := result.Env["BOID_INVOKED_TYPE"]; ok {
				t.Errorf("BOID_INVOKED_TYPE must be gone (replaced by BOID_INVOKED_BEHAVIOR)")
			}
		})
	}
}

// TestBuildSandboxSpec_BoidBinaryBoundAtShimBinDir is the 5a-3 cutover
// rewrite of the pre-5a-3 "BoidBinaryBindMountOnly" test
// (docs/plans/phase5-shim-and-task-context.md, "5a: shim 固定ディレクトリ化"
// PR3). It pins two facts at once:
//   - the boid binary is bind-mounted at the fixed sandbox path
//     `/run/boid/bin/boid` (sandboxShimBinDir + "/boid"), never at its host
//     path any more — the pre-5a-3 host-path identity bind was retired in
//     the same change;
//   - the retired git-shim overlay (/usr/bin/git, /bin/git bound to the
//     boid binary — docs/plans/git-gateway-cutover.md PR6) is still absent.
func TestBuildSandboxSpec_BoidBinaryBoundAtShimBinDir(t *testing.T) {
	const boidBin = "/usr/local/bin/boid"
	const wantTarget = sandboxShimBinDir + "/boid"
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{BoidBinary: boidBin}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	var boidMount *sandbox.Mount
	for i := range result.Mounts {
		m := &result.Mounts[i]
		switch m.Target {
		case wantTarget:
			boidMount = m
		case "/usr/bin/git", "/bin/git":
			t.Errorf("git-shim overlay mount must not exist post-cutover (docs/plans/git-gateway-cutover.md PR6): target=%q", m.Target)
		case boidBin:
			t.Errorf("pre-5a-3 host-path identity bind must not exist post-cutover: target=%q", m.Target)
		}
	}

	if boidMount == nil {
		t.Fatalf("boid binary mount not found at target %q", wantTarget)
	}
	if boidMount.Source != boidBin {
		t.Errorf("boid binary source = %q, want %q", boidMount.Source, boidBin)
	}
	if !boidMount.ReadOnly {
		t.Error("boid binary mount should be ReadOnly")
	}

	// BoidBinary 未設定時も同様に git override mount が存在しないことを確認。
	noGit, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec (no BoidBinary): %v", err)
	}
	for _, m := range noGit.Mounts {
		if m.Target == "/usr/bin/git" || m.Target == "/bin/git" {
			t.Errorf("unexpected git override mount when BoidBinary is empty: target=%q", m.Target)
		}
	}
}

// writable project では .git が ro 再 bind されることを確認する。
// これにより sandbox 内プロセスが .git/config 等を直接書き換えられない。
func TestProjectVisibilityMounts_GitROBind_Writable(t *testing.T) {
	const effectiveDir = "/home/user/project"
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, "/home/user", "", true, nil)

	var gitMount *sandbox.Mount
	for i := range mounts {
		if mounts[i].Target == effectiveDir+"/.git" {
			gitMount = &mounts[i]
			break
		}
	}
	if gitMount == nil {
		t.Fatal(".git ro re-bind mount not found in writable project mounts")
	}
	if !gitMount.ReadOnly {
		t.Error(".git re-bind mount must be ReadOnly")
	}
	if gitMount.Source != effectiveDir+"/.git" {
		t.Errorf("source = %q, want %q", gitMount.Source, effectiveDir+"/.git")
	}
	if !gitMount.DetectType {
		t.Error(".git re-bind mount must have DetectType=true (handles file and directory)")
	}
	if gitMount.Guard == "" {
		t.Error(".git re-bind mount must have a Guard")
	}
	if gitMount.Type != sandbox.MountBind {
		t.Errorf("type = %v, want MountBind", gitMount.Type)
	}
}

// read-only project では .git の ro re-bind は追加しない（既に親が read-only）。
func TestProjectVisibilityMounts_GitROBind_ReadOnly(t *testing.T) {
	const effectiveDir = "/home/user/project"
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, "/home/user", "", false, nil)

	for _, m := range mounts {
		if m.Target == effectiveDir+"/.git" && m.ReadOnly && m.DetectType {
			t.Error(".git ro re-bind should not be added for read-only project")
		}
	}
}

// .boid bind: project の .boid は origProjectDir から effectiveDir/.boid に
// bind される。writable タスクでは書き込み可、readonly タスクでは ro。
func TestProjectVisibilityMounts_BoidBind(t *testing.T) {
	const origProject = "/home/user/project"

	findBoid := func(mounts []sandbox.Mount) *sandbox.Mount {
		for i := range mounts {
			if mounts[i].Target == origProject+"/.boid" {
				return &mounts[i]
			}
		}
		return nil
	}

	// writable タスク: .boid は origProjectDir から bind され書き込み可。
	wMounts := projectVisibilityMounts(origProject, origProject, "/home/user", "", true, nil)
	w := findBoid(wMounts)
	if w == nil {
		t.Fatal(".boid bind not found in writable project mounts")
	}
	if w.Source != origProject+"/.boid" {
		t.Errorf(".boid source = %q, want %q", w.Source, origProject+"/.boid")
	}
	if w.Type != sandbox.MountBind {
		t.Errorf(".boid type = %v, want MountBind", w.Type)
	}
	if w.ReadOnly {
		t.Error(".boid bind must be writable for a writable task")
	}
	if w.Guard == "" {
		t.Error(".boid bind must have a Guard")
	}

	// readonly タスク: .boid は依然 bind されるが ro。
	roMounts := projectVisibilityMounts(origProject, origProject, "/home/user", "", false, nil)
	ro := findBoid(roMounts)
	if ro == nil {
		t.Fatal(".boid bind not found in read-only project mounts")
	}
	if !ro.ReadOnly {
		t.Error(".boid bind must be ReadOnly for a read-only task")
	}
}

// --- opt-in sandbox-clone path (docs/plans/git-gateway-cutover.md PR5) ---

func TestCloneMounts_NilWhenNoCloneDeclaration(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Visibility: orchestrator.Visibility{ProjectDir: "/home/user/project"},
	}
	if mounts := cloneMounts(spec, SandboxRuntimeInfo{}); mounts != nil {
		t.Fatalf("cloneMounts = %#v, want nil when Visibility.Clone is unset", mounts)
	}
}

func TestCloneMounts_IncludesSelfReferenceAndPeers(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/project",
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	rt := SandboxRuntimeInfo{
		WorkspacePeers: map[string]string{"peer-1": "/home/user/peer"},
	}
	mounts := cloneMounts(spec, rt)

	findTarget := func(target string) *sandbox.Mount {
		for i := range mounts {
			if mounts[i].Target == target {
				return &mounts[i]
			}
		}
		return nil
	}

	self := findTarget(sandboxCloneReferenceDir)
	if self == nil {
		t.Fatal("self project .git reference mount not found")
	}
	if self.Source != "/home/user/project/.git" {
		t.Errorf("self reference source = %q, want %q", self.Source, "/home/user/project/.git")
	}
	if !self.ReadOnly {
		t.Error("self reference mount must be read-only")
	}
	if self.Guard == "" {
		t.Error("self reference mount must have a Guard (graceful degradation when .git is missing)")
	}

	peerTarget := fmt.Sprintf(sandboxClonePeerReferenceDirFmt, "peer-1")
	peer := findTarget(peerTarget)
	if peer == nil {
		t.Fatal("workspace peer .git reference mount not found")
	}
	if peer.Source != "/home/user/peer/.git" {
		t.Errorf("peer reference source = %q, want %q", peer.Source, "/home/user/peer/.git")
	}
	if !peer.ReadOnly {
		t.Error("peer reference mount must be read-only")
	}

	// PR6 cutover removed the separate real-git-binary mount: the git shim
	// overlay it existed to route around (/usr/bin/git, /bin/git bound to
	// the boid binary) is itself retired, so the sandbox's own /usr/bin/git
	// (visible via the base rbind) is already the real binary. Assert its
	// absence explicitly so a future regression that re-introduces it here
	// is caught.
	if m := findTarget("/run/boid/real-git"); m != nil {
		t.Errorf("unexpected real-git-binary mount present post-cutover: %+v", m)
	}
}

// TestCloneMounts_IncludesWorkspaceBindWhenCloneWorkspaceDirSet is also the
// regression guard for the workspace 親化リファクタリング (nose 2026-07-13
// decision): the /workspace bind mount must land at the name-scoped
// subdirectory (sandboxCloneDir(spec.Visibility.ProjectName)), not the bare
// /workspace parent, so two different projects never collide on the exact
// same sandbox-internal path.
func TestCloneMounts_IncludesWorkspaceBindWhenCloneWorkspaceDirSet(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Visibility: orchestrator.Visibility{
			ProjectDir:  "/home/user/project",
			ProjectName: "bm-next",
			Clone:       &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	rt := SandboxRuntimeInfo{CloneWorkspaceDir: "/data/boid/runtimes/job-1/workspace"}
	mounts := cloneMounts(spec, rt)

	const wantTarget = "/workspace/bm-next"
	var workspace *sandbox.Mount
	for i := range mounts {
		if mounts[i].Target == wantTarget {
			workspace = &mounts[i]
		}
	}
	if workspace == nil {
		t.Fatalf("mount with Target %q not found among %#v", wantTarget, mounts)
	}
	if workspace.Source != rt.CloneWorkspaceDir {
		t.Errorf("workspace bind source = %q, want %q", workspace.Source, rt.CloneWorkspaceDir)
	}
	if workspace.ReadOnly {
		t.Error("workspace bind must be read-write (readonly is enforced by the gateway, not the local filesystem)")
	}
}

// TestCloneMounts_WorkspaceBindFallsBackToProjectDirBasenameWhenNameEmpty
// pins the fallback half of the workspace 親化リファクタリング decision: a
// project with no `name:` in project.yaml still gets a distinct, deterministic
// leaf directory — filepath.Base(ProjectDir) — instead of colliding on the
// bare /workspace parent.
func TestCloneMounts_WorkspaceBindFallsBackToProjectDirBasenameWhenNameEmpty(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/sumiron-project", // no ProjectName set
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	rt := SandboxRuntimeInfo{CloneWorkspaceDir: "/data/boid/runtimes/job-1/workspace"}
	mounts := cloneMounts(spec, rt)

	const wantTarget = "/workspace/sumiron-project"
	found := false
	for _, m := range mounts {
		if m.Target == wantTarget {
			found = true
		}
	}
	if !found {
		t.Fatalf("mount with Target %q not found among %#v", wantTarget, mounts)
	}
}

func TestCloneMounts_OmitsWorkspaceBindWhenCloneWorkspaceDirUnset(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Visibility: orchestrator.Visibility{
			ProjectDir:  "/home/user/project",
			ProjectName: "bm-next",
			Clone:       &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	mounts := cloneMounts(spec, SandboxRuntimeInfo{})
	for _, m := range mounts {
		if m.Target == "/workspace/bm-next" || m.Target == sandboxCloneTargetDir {
			t.Errorf("unexpected /workspace bind when rt.CloneWorkspaceDir is empty: %+v", m)
		}
	}
}

func TestBuildCloneSpec_NilWhenNoCloneDeclaration(t *testing.T) {
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}
	got := buildCloneSpec(spec, SandboxRuntimeInfo{GatewayCloneURL: "http://10.0.2.2:9/j/tok/github.com/o/r.git"})
	if got.Enabled {
		t.Fatalf("buildCloneSpec = %+v, want Enabled=false when Visibility.Clone is unset", got)
	}
}

func TestBuildCloneSpec_PopulatesFromDeclarationAndRuntimeInfo(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Visibility: orchestrator.Visibility{
			ProjectDir:  "/home/user/project",
			ProjectName: "bm-next",
			Clone: &orchestrator.CloneDeclaration{
				Branch:              "boid/abcd1234",
				BaseBranch:          "main",
				BaseBranchForkPoint: "origin/main",
			},
		},
	}
	rt := SandboxRuntimeInfo{GatewayCloneURL: "http://10.0.2.2:9/j/tok/github.com/o/r.git"}
	got := buildCloneSpec(spec, rt)

	// TargetDir is name-scoped under the /workspace parent dir (workspace 親化
	// リファクタリング, nose 2026-07-13 decision) — see cloneDirNameForVisibility.
	want := sandbox.CloneSpec{
		Enabled:             true,
		URL:                 rt.GatewayCloneURL,
		ReferenceDir:        sandboxCloneReferenceDir,
		TargetDir:           "/workspace/bm-next",
		Branch:              "boid/abcd1234",
		BaseBranch:          "main",
		BaseBranchForkPoint: "origin/main",
	}
	if got != want {
		t.Errorf("buildCloneSpec = %+v, want %+v", got, want)
	}
}

// TestResolveWorkDir_CloneEnabled_ReturnsCloneTargetDir pins both that the
// clone path takes priority over the plain-project-dir path, and (workspace
// 親化リファクタリング, nose 2026-07-13 decision) that the returned WorkDir
// is name-scoped — here via the ProjectDir-basename fallback, since no
// ProjectName is set.
func TestResolveWorkDir_CloneEnabled_ReturnsCloneTargetDir(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/project",
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main"},
		},
	}
	const want = "/workspace/project"
	if got := resolveWorkDir(spec); got != want {
		t.Errorf("resolveWorkDir = %q, want %q (clone path takes priority, name-scoped)", got, want)
	}
}

// TestResolveWorkDir_CloneEnabled_PrefersProjectNameOverBasename is the
// counterpart proving ProjectName — when set — wins over the ProjectDir
// basename fallback (workspace 親化リファクタリング, nose 2026-07-13
// decision: "project.Name (kebab-case)。空なら filepath.Base(host path) に
// フォールバック").
func TestResolveWorkDir_CloneEnabled_PrefersProjectNameOverBasename(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			ProjectDir:  "/home/user/some-other-basename",
			ProjectName: "bm-next",
			Clone:       &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main"},
		},
	}
	const want = "/workspace/bm-next"
	if got := resolveWorkDir(spec); got != want {
		t.Errorf("resolveWorkDir = %q, want %q (ProjectName should win over ProjectDir basename)", got, want)
	}
}

// TestBuildSandboxSpec_CloneNil_UnaffectedByPR5 is the regression guard for
// PR5's inertness claim: a JobSpec that never sets Visibility.Clone (i.e.
// every JobSpec dispatcher builds today) must produce byte-identical mounts
// and WorkDir to before PR5 — no clone mounts, no Clone.Enabled, no
// behavioural change at all.
func TestBuildSandboxSpec_CloneNil_UnaffectedByPR5(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"/bin/true"},
		Visibility: orchestrator.Visibility{ProjectDir: "/home/user/project", Writable: true},
	}
	rt := SandboxRuntimeInfo{JobID: "job-1"}
	out, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if out.Clone.Enabled {
		t.Errorf("Clone.Enabled = true, want false when Visibility.Clone is nil")
	}
	if out.WorkDir != "/home/user/project" {
		t.Errorf("WorkDir = %q, want project dir unchanged", out.WorkDir)
	}
	for _, m := range out.Mounts {
		if m.Target == sandboxCloneReferenceDir || m.Target == sandboxCloneTargetDir {
			t.Errorf("unexpected clone mount present when Visibility.Clone is nil: %+v", m)
		}
	}
}

// TestBuildSandboxSpec_CloneEnabled_SkipsProjectVisibilityMounts is the PR6
// cutover regression guard for the PR5 Opus review's double-mount concern:
// when Visibility.Clone is set, BuildSandboxSpec must not also emit
// projectVisibilityMounts' host ProjectDir bind (Source==Target==ProjectDir)
// — that mount layout belongs exclusively to the retired worktree/project
// path. A clone-mode job's only view of ProjectDir on the host is the
// read-only `.git` reference bind at sandboxCloneReferenceDir (for `git
// clone --reference`), never a live bind of the working tree itself.
func TestBuildSandboxSpec_CloneEnabled_SkipsProjectVisibilityMounts(t *testing.T) {
	const projectDir = "/home/user/project"
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"/bin/true"},
		Visibility: orchestrator.Visibility{
			ProjectDir: projectDir,
			Writable:   true,
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	rt := SandboxRuntimeInfo{JobID: "job-1", CloneWorkspaceDir: "/data/boid/runtimes/job-1/workspace"}
	out, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, m := range out.Mounts {
		if m.Source == projectDir && m.Target == projectDir {
			t.Errorf("unexpected host ProjectDir bind present when Visibility.Clone is set: %+v", m)
		}
	}
	// WorkDir is name-scoped (workspace 親化リファクタリング, nose 2026-07-13
	// decision) — here via the ProjectDir-basename fallback ("project"),
	// since spec.Visibility.ProjectName is unset above.
	const wantWorkDir = "/workspace/project"
	if out.WorkDir != wantWorkDir {
		t.Errorf("WorkDir = %q, want %q (clone target, name-scoped)", out.WorkDir, wantWorkDir)
	}
	if !out.Clone.Enabled {
		t.Error("Clone.Enabled = false, want true")
	}
}

// TestBuildSandboxSpec_ContainerBackendClone_WritesGatewayCAAndSetsEnv pins
// the PR9 e2e-container CI fix: a clone-visibility job dispatched against
// the container backend must get the git gateway's CA cert written into
// the sandbox (at containerGitGatewayCAPath) and GIT_SSL_CAINFO pointed at
// it — without this, the sandbox-internal `git clone` against the TLS-
// secured gateway (https://boid-gateway:<port>) cannot verify the
// gateway's server certificate ("server certificate verification failed.
// CAfile: none CRLfile: none", the real-docker e2e-container CI job's
// exact failure once the earlier client-cert-requirement bug was fixed).
func TestBuildSandboxSpec_ContainerBackendClone_WritesGatewayCAAndSetsEnv(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"/bin/true"},
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/project",
			Writable:   true,
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	rt := SandboxRuntimeInfo{
		JobID:                 "job-1",
		CloneWorkspaceDir:     "/data/boid/runtimes/job-1/workspace",
		UsingContainerBackend: true,
		GatewayCAPEM:          []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
	}
	out, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	var found bool
	for _, f := range out.Files {
		if f.Path == containerGitGatewayCAPath {
			found = true
			if f.Content != string(rt.GatewayCAPEM) {
				t.Errorf("gateway CA file content = %q, want %q", f.Content, string(rt.GatewayCAPEM))
			}
		}
	}
	if !found {
		t.Errorf("no spec.Files entry at %q, want the gateway CA cert written there", containerGitGatewayCAPath)
	}
	if got, want := out.Env["GIT_SSL_CAINFO"], containerGitGatewayCAPath; got != want {
		t.Errorf("Env[GIT_SSL_CAINFO] = %q, want %q", got, want)
	}
}

// TestBuildSandboxSpec_UsernsBackendClone_OmitsGatewayCA is the companion
// non-regression pin: the userns backend's gateway URL is plain HTTP (no
// TLS at all — see SandboxRuntimeInfo.GatewayCAPEM's own doc comment), so
// BuildSandboxSpec must not write the CA file or set GIT_SSL_CAINFO even
// when GatewayCAPEM happens to be non-empty (e.g. a daemon with TLSDir
// configured but sandbox.backend left at its userns default).
func TestBuildSandboxSpec_UsernsBackendClone_OmitsGatewayCA(t *testing.T) {
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"/bin/true"},
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/project",
			Writable:   true,
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	rt := SandboxRuntimeInfo{
		JobID:                 "job-1",
		CloneWorkspaceDir:     "/data/boid/runtimes/job-1/workspace",
		UsingContainerBackend: false,
		GatewayCAPEM:          []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
	}
	out, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, f := range out.Files {
		if f.Path == containerGitGatewayCAPath {
			t.Errorf("unexpected gateway CA file present for the userns backend: %+v", f)
		}
	}
	if _, ok := out.Env["GIT_SSL_CAINFO"]; ok {
		t.Errorf("Env[GIT_SSL_CAINFO] = %q, want unset for the userns backend", out.Env["GIT_SSL_CAINFO"])
	}
}

// --- workspace 親化リファクタリング helpers (nose 2026-07-13 decision) ---

func TestProjectDirName_PrefersExplicitName(t *testing.T) {
	if got := projectDirName("bm-next", "/home/user/some-other-dir"); got != "bm-next" {
		t.Errorf("projectDirName = %q, want %q", got, "bm-next")
	}
}

func TestProjectDirName_FallsBackToWorkDirBasenameWhenNameEmpty(t *testing.T) {
	if got := projectDirName("", "/home/user/sumiron-project"); got != "sumiron-project" {
		t.Errorf("projectDirName = %q, want %q", got, "sumiron-project")
	}
}

func TestSandboxCloneDir_NameScoped(t *testing.T) {
	if got := sandboxCloneDir("bm-next"); got != "/workspace/bm-next" {
		t.Errorf("sandboxCloneDir(%q) = %q, want %q", "bm-next", got, "/workspace/bm-next")
	}
}

// TestSandboxCloneDir_EmptyNameDegradesToParent pins the defensive fallback:
// an unresolved (empty) name must not produce a malformed path like
// "/workspace/" — it degrades to the bare parent dir, reproducing the
// pre-refactor flat layout.
func TestSandboxCloneDir_EmptyNameDegradesToParent(t *testing.T) {
	if got := sandboxCloneDir(""); got != sandboxCloneTargetDir {
		t.Errorf("sandboxCloneDir(\"\") = %q, want %q", got, sandboxCloneTargetDir)
	}
}

// TestSandboxCloneDir_RejectsUnsafeNames is the PR #737 review guard for the
// defense-in-depth filter on project.yaml's `meta.name`: an accidental
// path-escape or a stray `filepath.Base("")` result ("." — the shape
// projectDirName used to leak when workDir was empty) must never turn into a
// live subpath under /workspace. Each of these degrades to the bare parent
// dir instead.
func TestSandboxCloneDir_RejectsUnsafeNames(t *testing.T) {
	cases := []struct {
		name string
	}{
		{"."},           // filepath.Base("") — legacy "empty workDir" hole
		{".."},          // literal parent
		{"../etc"},      // ../ escape
		{"foo/bar"},     // path separator
		{"foo\x00"},     // NUL byte
		{"/etc/passwd"}, // absolute path
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxCloneDir(tc.name); got != sandboxCloneTargetDir {
				t.Errorf("sandboxCloneDir(%q) = %q, want %q (unsafe name should fall back to the bare parent dir)",
					tc.name, got, sandboxCloneTargetDir)
			}
		})
	}
}

// TestProjectDirName_EmptyWorkDirReturnsEmptyNotDot pins the fix for a
// legacy hole: filepath.Base("") returns ".", which would then flow into
// sandboxCloneDir and produce "/workspace/." — a live subdir, not the
// intended fallback to the bare parent. projectDirName must return the
// empty string instead so downstream defenses catch it.
func TestProjectDirName_EmptyWorkDirReturnsEmptyNotDot(t *testing.T) {
	if got := projectDirName("", ""); got != "" {
		t.Errorf("projectDirName(\"\", \"\") = %q, want \"\" (must not leak filepath.Base(\"\")==\".\")", got)
	}
}

func TestCloneDirNameForVisibility_PrefersProjectName(t *testing.T) {
	v := orchestrator.Visibility{ProjectDir: "/home/user/checkout-dir", ProjectName: "bm-next"}
	if got := cloneDirNameForVisibility(v); got != "bm-next" {
		t.Errorf("cloneDirNameForVisibility = %q, want %q", got, "bm-next")
	}
}

func TestCloneDirNameForVisibility_FallsBackToProjectDirBasename(t *testing.T) {
	v := orchestrator.Visibility{ProjectDir: "/home/user/sumiron-project"}
	if got := cloneDirNameForVisibility(v); got != "sumiron-project" {
		t.Errorf("cloneDirNameForVisibility = %q, want %q", got, "sumiron-project")
	}
}

// TestHostCommandSymlinks_BoidAndGitExcluded pins the 5a-3 cutover
// (docs/plans/phase5-shim-and-task-context.md 5a PR3) analog of the pre-5a-3
// TestHostCommandMounts_BoidAndGitExcluded assertion: `boid`, `git`,
// `fetch` are reserved names — ResolveHostCommands omits them entirely, so
// hostCommandSymlinks (fed from the same byName view) never materializes
// a `/run/boid/bin/boid` symlink that would collide with the dedicated
// boid binary bind mount at that exact path.
func TestHostCommandSymlinks_BoidAndGitExcluded(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"gh":  {},
		"git": {},
	}
	_, byName, err := ResolveHostCommands([]string{"boid", "git"}, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	links := hostCommandSymlinks(byName)
	for _, s := range links {
		if s.LinkPath == sandboxShimBinDir+"/boid" || s.LinkPath == sandboxShimBinDir+"/git" {
			t.Errorf("boid/git must not get a host command symlink, got link_path=%q", s.LinkPath)
		}
	}
	var hasGh bool
	for _, s := range links {
		if s.LinkPath == sandboxShimBinDir+"/gh" {
			hasGh = true
		}
	}
	if !hasGh {
		t.Error("gh must get a host command symlink")
	}
}

// ホストに存在しないコマンドは fail-fast でエラーになる（既存挙動）。
func TestHostCommandSymlinks_NotFound(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "", fmt.Errorf("not found")
	}
	hostCmds := map[string]orchestrator.CommandDef{"missing-cmd": {}}
	_, _, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err == nil {
		t.Error("expected error for missing host command, got nil")
	}
}

// TestHostCommandSymlinks_LinkPathIsShimBinDirSlashName pins the 5a-3
// invariant: every declared short name gets exactly one symlink at
// `/run/boid/bin/<name>` pointing at `boid` (relative). It replaces the
// pre-5a-3 "bind at host absolute path" scheme with a bind-mount basename
// that always equals the declared short name — no BOID_HOST_COMMAND_NAMES
// map lookup needed for a shim to identify itself to the broker.
func TestHostCommandSymlinks_LinkPathIsShimBinDirSlashName(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/local/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{"gh": {}}
	_, byName, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	links := hostCommandSymlinks(byName)
	if len(links) != 1 {
		t.Fatalf("expected 1 symlink, got %d: %+v", len(links), links)
	}
	s := links[0]
	if s.LinkPath != sandboxShimBinDir+"/gh" {
		t.Errorf("LinkPath = %q, want %q", s.LinkPath, sandboxShimBinDir+"/gh")
	}
	if s.LinkTarget != "boid" {
		t.Errorf("LinkTarget = %q, want %q (relative)", s.LinkTarget, "boid")
	}
}

// 同じコマンドが builtins と hostCommands の両方にある場合は重複しない。
func TestHostCommandSymlinks_Dedup(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{"gh": {}}
	_, byName, err := ResolveHostCommands([]string{"gh"}, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	links := hostCommandSymlinks(byName)
	if len(links) != 1 {
		t.Errorf("expected 1 symlink (dedup), got %d", len(links))
	}
}

// TestHostCommandSymlinks_AliasedPathUsesDeclaredName is the 5a-3 wiring
// that makes the whole cutover work end-to-end
// (docs/plans/phase5-shim-and-task-context.md). host_commands.<name>.path
// aliasing (e.g. `run-e2e: path: e2e/run.sh`) used to force
// BOID_HOST_COMMAND_NAMES + ResolveShimCommandName gymnastics so a shim
// bind-mounted at a file named "run.sh" could still identify itself as
// "run-e2e" to the broker. With the fixed-directory symlink scheme the
// bind-mount basename is always the declared name — proven here by pinning
// that an aliased entry lands at `/run/boid/bin/run-e2e` (not
// `/run/boid/bin/run.sh` or the source's absolute path).
func TestHostCommandSymlinks_AliasedPathUsesDeclaredName(t *testing.T) {
	dir := t.TempDir()
	scriptPath := dir + "/run-e2e.sh"
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"run-e2e": {Path: scriptPath},
	}
	_, byName, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	links := hostCommandSymlinks(byName)
	if len(links) != 1 {
		t.Fatalf("expected 1 symlink, got %d: %+v", len(links), links)
	}
	if links[0].LinkPath != sandboxShimBinDir+"/run-e2e" {
		t.Errorf("LinkPath = %q, want %q (declared name, never the source basename %q)",
			links[0].LinkPath, sandboxShimBinDir+"/run-e2e", "run.sh")
	}
	if links[0].LinkTarget != "boid" {
		t.Errorf("LinkTarget = %q, want %q", links[0].LinkTarget, "boid")
	}
}

// host_commands.<name>.path の相対パスは projectDir 基準で解決される。
func TestHostCommandSymlinks_RelativePathResolvedFromProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(projectDir+"/e2e", 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := projectDir + "/e2e/run.sh"
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"run-e2e": {Path: "e2e/run.sh"},
	}
	_, byName, err := ResolveHostCommands(nil, hostCmds, projectDir, func(string) (string, error) {
		return "", fmt.Errorf("lookPath should not be called")
	}, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	def, ok := byName["run-e2e"]
	if !ok {
		t.Fatalf("byName must contain 'run-e2e', got %v", byName)
	}
	if def.Path != scriptPath {
		t.Errorf("def.Path = %q, want %q", def.Path, scriptPath)
	}
}

// CommandDef.Path 指定だが対象ファイルが存在しない → "does not exist on host" エラー。
func TestHostCommandSymlinks_PathDoesNotExist_Error(t *testing.T) {
	dir := t.TempDir()
	missingPath := dir + "/nonexistent.sh"
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"run-e2e": {Path: missingPath},
	}
	_, _, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err == nil {
		t.Fatal("expected error for non-existent path, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist on host") {
		t.Errorf("error = %q, want it to contain 'does not exist on host'", err.Error())
	}
}

// TestHostCommandSymlinks_UnsafeNameDropped is the direct-observation
// guard for the shim-name validation added to hostCommandSymlinks (codex
// review P1 finding on the 5a-3 first draft): a host_commands map key that
// contains a path separator, NUL, "..", or resolves to empty/"." must NOT
// be concatenated into a LinkPath — the runner's symlink loop would
// otherwise create a symlink outside sandboxShimBinDir (e.g. on the
// persistent workspace home volume, which a later job could dereference or
// replace). project.yaml's map keys are user-authored so the trust boundary
// is loose; this is the last-line defense-in-depth guard before the
// symlink hits the runner. The valid sibling entry in the same map must
// still surface (safe names are unaffected).
func TestHostCommandSymlinks_UnsafeNameDropped(t *testing.T) {
	byName := map[string]orchestrator.CommandDef{
		"safe-cmd":              {Name: "safe-cmd", Path: "/usr/bin/safe-cmd"},
		"":                      {Name: "", Path: "/usr/bin/empty"},
		".":                     {Name: ".", Path: "/usr/bin/dot"},
		"..":                    {Name: "..", Path: "/usr/bin/dotdot"},
		"../etc/passwd":         {Name: "../etc/passwd", Path: "/etc/passwd"},
		"sub/dir":               {Name: "sub/dir", Path: "/usr/bin/x"},
		"has\x00null":           {Name: "has\x00null", Path: "/usr/bin/y"},
		"..hidden-but-starts":   {Name: "..hidden-but-starts", Path: "/usr/bin/z"},
	}
	links := hostCommandSymlinks(byName)
	if len(links) != 1 {
		t.Fatalf("expected exactly 1 symlink (only safe-cmd valid), got %d: %+v", len(links), links)
	}
	if links[0].LinkPath != sandboxShimBinDir+"/safe-cmd" {
		t.Errorf("LinkPath = %q, want %q", links[0].LinkPath, sandboxShimBinDir+"/safe-cmd")
	}
}

// TestIsSafeShimName_Cases table-drives the boundary conditions
// hostCommandSymlinks relies on. If this function ever changes its
// acceptance rules, this test must move in lockstep — a broadening (e.g.
// starting to accept "..foo") reopens the class of escape the P1 review
// closed.
func TestIsSafeShimName_Cases(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"..hidden", false}, // starts with ".."
		{"sub/dir", false},
		{"has\x00null", false},
		{"safe-cmd", true},
		{"gh", true},
		{"run-e2e", true},
		{"docker.compose", true},
		{".hidden", true}, // starts with "." but not ".."
	}
	for _, tc := range cases {
		if got := isSafeShimName(tc.name); got != tc.want {
			t.Errorf("isSafeShimName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// builtin と host command の複合ケース: host command 側のみ Path 指定。
// 両方とも /run/boid/bin/<name> に symlink される。
func TestHostCommandSymlinks_MixedBuiltinAndPathCommand(t *testing.T) {
	dir := t.TempDir()
	scriptPath := dir + "/run-e2e.sh"
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"run-e2e": {Path: scriptPath},
	}
	_, byName, err := ResolveHostCommands([]string{"jq"}, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	links := hostCommandSymlinks(byName)
	if len(links) != 2 {
		t.Fatalf("expected 2 symlinks, got %d", len(links))
	}
	paths := map[string]bool{}
	for _, s := range links {
		paths[s.LinkPath] = true
	}
	if !paths[sandboxShimBinDir+"/jq"] {
		t.Errorf("builtin jq must have a symlink at %s/jq", sandboxShimBinDir)
	}
	if !paths[sandboxShimBinDir+"/run-e2e"] {
		t.Errorf("host command run-e2e must have a symlink at %s/run-e2e", sandboxShimBinDir)
	}
}

// TestBuildPATH_ShimBinDirPrepended pins the 5a-3 PATH simplification
// (docs/plans/phase5-shim-and-task-context.md, "5a: shim 固定ディレクトリ化"
// PR3): the fixed sandboxShimBinDir replaces both the pre-5a-3
// per-boid-binary-parent entry and the per-host-command parent entries
// (retired hostCommandMounts). It always lands right after workspace home's
// ~/.local/bin, regardless of whether additional bindings are supplied.
func TestBuildPATH_ShimBinDirPrepended(t *testing.T) {
	want := hostHomeDir() + "/.local/bin:" + sandboxShimBinDir + ":"

	path := buildPATH(nil)
	if !strings.HasPrefix(path, want) {
		t.Errorf("buildPATH(nil) = %q, want prefix %q", path, want)
	}

	// Additional bindings inject after sandboxShimBinDir, not before it.
	bindings := []orchestrator.BindMount{{Source: "/opt/custom/sbin"}}
	path = buildPATH(bindings)
	if !strings.HasPrefix(path, want) {
		t.Errorf("buildPATH with bindings = %q, want prefix %q", path, want)
	}
}

// TestBuildPATH_HostCommandParentDirsAbsent is the negative half of the
// 5a-3 cutover: post-cutover, host command shims are all symlinks under
// sandboxShimBinDir (single PATH entry) — no host command's absolute
// host-side parent directory (e.g. /opt/custom/sbin) makes it onto PATH
// via the shim wiring any more. Only additional_bindings can still add
// their own bin/ directories to PATH, and only via their own explicit
// declaration.
func TestBuildPATH_HostCommandParentDirsAbsent(t *testing.T) {
	// No bindings, no adjacent-bin dirs — PATH should be exactly the
	// workspace home + sandboxShimBinDir + base PATH, nothing else.
	path := buildPATH(nil)
	want := hostHomeDir() + "/.local/bin:" + sandboxShimBinDir + ":/usr/local/bin:/usr/bin:/bin"
	if path != want {
		t.Errorf("buildPATH(nil) = %q, want %q", path, want)
	}
}

// TestBuildPATH_WorkspaceHomeLocalBinAlwaysLeads pins the Phase 4 PR3 PATH
// change (docs/plans/home-workspace-volume.md): with adapter-declared
// bindings retired, a harness CLI installed by the workspace's init.sh under
// $HOME/.local/bin must resolve by name without any binding or host-command
// wiring. It is unconditionally the first PATH entry — ahead of
// sandboxShimBinDir and every other source — regardless of whether
// bindings are supplied.
func TestBuildPATH_WorkspaceHomeLocalBinAlwaysLeads(t *testing.T) {
	want := hostHomeDir() + "/.local/bin"

	path := buildPATH(nil)
	if !strings.HasPrefix(path, want+":") {
		t.Errorf("buildPATH(nil) = %q, want prefix %q:", path, want)
	}

	path = buildPATH([]orchestrator.BindMount{{Source: "/opt/custom/sbin"}})
	if !strings.HasPrefix(path, want+":") {
		t.Errorf("buildPATH with bindings = %q, want prefix %q:", path, want)
	}
}

// additional_bindings の .../bin ディレクトリは従来どおり PATH に追加される
// (host_commands 経路が消えても additional_bindings 経路は残る)。 sandboxShimBinDir
// は先頭に来る。 buildPATH は Source が /bin で終わっていればそのまま、
// でなければ /bin を append する既存挙動を維持する。
func TestBuildPATH_AdditionalBindingsBinDirsAdded(t *testing.T) {
	bindings := []orchestrator.BindMount{
		{Source: "/opt/custom/bin"},
		{Source: "/opt/other"},
	}
	path := buildPATH(bindings)
	for _, dir := range []string{"/opt/custom/bin", "/opt/other/bin"} {
		if !strings.Contains(":"+path+":", ":"+dir+":") {
			t.Errorf("buildPATH = %q, want dir %q on PATH", path, dir)
		}
	}
	if !strings.HasSuffix(path, "/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("buildPATH = %q, want base PATH suffix", path)
	}
}

func TestAdditionalBindingMounts_Optional(t *testing.T) {
	bindings := []orchestrator.BindMount{
		{Source: "/opt/maybe-missing", Optional: true},
		{Source: "/opt/always-present"},
	}
	mounts := additionalBindingMounts(bindings)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	// optional=true → Guard が設定される
	if mounts[0].Guard == "" {
		t.Error("optional binding must have a Guard expression")
	}
	if !reflect.DeepEqual(mounts[0].Guard, dirGuardExpr("/opt/maybe-missing")) {
		t.Errorf("Guard = %q, want %q", mounts[0].Guard, dirGuardExpr("/opt/maybe-missing"))
	}

	// optional=false (デフォルト) → Guard は空
	if mounts[1].Guard != "" {
		t.Errorf("non-optional binding must have empty Guard, got %q", mounts[1].Guard)
	}
}

func TestAdditionalBindingMounts_OptionalWithRWMode(t *testing.T) {
	bindings := []orchestrator.BindMount{
		{Source: "/opt/rw-optional", Mode: "rw", Optional: true},
	}
	mounts := additionalBindingMounts(bindings)
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].ReadOnly {
		t.Error("rw optional binding must not be ReadOnly")
	}
	if mounts[0].Guard == "" {
		t.Error("rw optional binding must have a Guard expression")
	}
}

// Target を明示し、 展開後 Source と等値になった binding は self-mount を避ける
// ため skip される。 主用途: ${PROJECT_WORKDIR}/x → ${WORKTREE}/x が worktree=false
// task で同じパスに潰れるケース。
func TestAdditionalBindingMounts_SkipExplicitSelfMount(t *testing.T) {
	bindings := []orchestrator.BindMount{
		// 明示 target == source → skip
		{Source: "/proj/global.json", Target: "/proj/global.json", IsFile: true},
		// target 省略 (= source 同値だが explicit ではない) → 従来通り bind
		{Source: "/var/run/some.sock"},
		// 明示 target != source → bind
		{Source: "/proj/global.json", Target: "/wt/global.json", IsFile: true},
	}
	mounts := additionalBindingMounts(bindings)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts (self-mount skipped), got %d", len(mounts))
	}
	if mounts[0].Target != "/var/run/some.sock" {
		t.Errorf("mounts[0].Target = %q, want /var/run/some.sock", mounts[0].Target)
	}
	if mounts[1].Source != "/proj/global.json" || mounts[1].Target != "/wt/global.json" {
		t.Errorf("mounts[1] = %+v, want source=/proj/global.json target=/wt/global.json", mounts[1])
	}
}

// rw な Source==Target binding は self-mount skip の対象外。
// ProfileInit のように「ホスト root を ro-rbind した上でサブディレクトリを
// rw で上書きする」ユースケースで必要となる。
func TestAdditionalBindingMounts_RWExplicitSelfMount_NotSkipped(t *testing.T) {
	bindings := []orchestrator.BindMount{
		// Mode=="rw" な explicit self-mount は skip しない (ProfileInit kits dir)
		{Source: "/data/boid/kits", Target: "/data/boid/kits", Mode: "rw"},
		// Mode=="" (read-only) な explicit self-mount は従来通り skip
		{Source: "/data/boid/kits", Target: "/data/boid/kits"},
	}
	mounts := additionalBindingMounts(bindings)
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount (ro self-mount skipped, rw kept), got %d: %+v", len(mounts), mounts)
	}
	if mounts[0].Source != "/data/boid/kits" || mounts[0].ReadOnly {
		t.Errorf("expected rw mount for /data/boid/kits, got %+v", mounts[0])
	}
}

// expandWorktreeBindings は dispatch 時に ${WORKTREE} と ${PROJECT_WORKDIR} を
// per-job 値で展開する。
func TestExpandWorktreeBindings(t *testing.T) {
	const worktree = "/runtime/worktrees/proj/task1"
	const projectDir = "/host/proj"

	cases := []struct {
		name       string
		input      orchestrator.BindMount
		wantSource string
		wantTarget string
	}{
		{
			name:       "WORKTREE と PROJECT_WORKDIR を別 path に展開",
			input:      orchestrator.BindMount{Source: "${PROJECT_WORKDIR}/global.json", Target: "${WORKTREE}/global.json", IsFile: true},
			wantSource: "/host/proj/global.json",
			wantTarget: "/runtime/worktrees/proj/task1/global.json",
		},
		{
			name:       "token を含まない binding は据え置き",
			input:      orchestrator.BindMount{Source: "/var/run/sock"},
			wantSource: "/var/run/sock",
			wantTarget: "",
		},
		{
			name:       "未知の token は literal 維持 (debug 用)",
			input:      orchestrator.BindMount{Source: "${UNKNOWN}/x"},
			wantSource: "${UNKNOWN}/x",
			wantTarget: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandWorktreeBindings([]orchestrator.BindMount{tc.input}, worktree, projectDir)
			if len(got) != 1 {
				t.Fatalf("len(got) = %d, want 1", len(got))
			}
			if got[0].Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got[0].Source, tc.wantSource)
			}
			if got[0].Target != tc.wantTarget {
				t.Errorf("Target = %q, want %q", got[0].Target, tc.wantTarget)
			}
		})
	}
}

// worktree=true と worktree=false で同じ project.yaml 宣言が:
// - clone-mode (Visibility.Clone set): ${WORKTREE} resolves to the
//   sandbox-internal clone dir, distinct from ${PROJECT_WORKDIR} (the host
//   path) — src and tgt expand to different paths and the bind is kept
// - non-clone (plain project mount): ${WORKTREE} == ${PROJECT_WORKDIR} ==
//   the host project dir — src and tgt collapse to the same path and the
//   bind is skipped as a redundant self-mount
// という End-to-End 挙動を BuildSandboxSpec 越しに検証する。
func TestBuildSandboxSpec_WorktreeBindingExpansion(t *testing.T) {
	const projectDir = "/host/proj"
	binding := orchestrator.BindMount{
		Source: "${PROJECT_WORKDIR}/global.json",
		Target: "${WORKTREE}/global.json",
		IsFile: true,
	}

	// clone-mode: src と tgt が別 path (host vs sandbox-internal clone dir) に
	// 展開され bind される
	specClone := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			ProjectDir:         projectDir,
			AdditionalBindings: []orchestrator.BindMount{binding},
			Clone:              &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main"},
		},
	}
	resClone, err := BuildSandboxSpec(specClone, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec(clone-mode): %v", err)
	}
	var found bool
	for _, m := range resClone.Mounts {
		if m.Source == "/host/proj/global.json" && m.Target == "/workspace/proj/global.json" {
			found = true
			if !m.IsFile {
				t.Error("expected IsFile=true for global.json bind")
			}
			break
		}
	}
	if !found {
		t.Errorf("clone-mode: expected bind from %s to %s, got mounts:\n%+v",
			"/host/proj/global.json", "/workspace/proj/global.json", resClone.Mounts)
	}

	// 非 clone (project 直接 mount): src と tgt が同じ path に潰れ、
	// explicit-self-mount として skip される
	specPlain := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			ProjectDir:         projectDir,
			AdditionalBindings: []orchestrator.BindMount{binding},
		},
	}
	resPlain, err := BuildSandboxSpec(specPlain, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec(non-clone): %v", err)
	}
	for _, m := range resPlain.Mounts {
		if m.Source == "/host/proj/global.json" && m.IsFile {
			t.Errorf("non-clone: self-mount should be skipped, got mount %+v", m)
		}
	}
}

// The go-native runner replaces the former EXIT-trap `boid job done` script:
// BuildSandboxSpec carries Foreground (whether to post job-done). Phase 6 PR8
// (docs/plans/phase6-container-backend.md §決定 9) retired the sibling
// PayloadPatchPath field this test used to also assert on — agents / hook
// scripts now apply their payload patch immediately via the broker's
// `boid task update --payload-patch` RPC instead.
func TestBuildSandboxSpec_HookSetsForegroundFalse(t *testing.T) {
	spec := &orchestrator.JobSpec{Interactive: true}

	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: false})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if result.Foreground {
		t.Error("hook job must have Foreground=false so the runner posts boid job done")
	}
}

func TestBuildSandboxSpec_ForegroundExecSkipsJobDone(t *testing.T) {
	spec := &orchestrator.JobSpec{}

	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: true})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if !result.Foreground {
		t.Error("foreground exec job must have Foreground=true (no broker job done)")
	}
}

// TestBuildSandboxSpec_DockerProxy_EnvAndMount verifies that when
// ProxySocketPath is set (DockerEnabled), BuildSandboxSpec injects the docker
// env vars and bind-mounts the proxy socket at the fixed sandbox path.
func TestBuildSandboxSpec_DockerProxy_EnvAndMount(t *testing.T) {
	hostSocketPath := "/run/some/runtime/docker-proxy.sock"
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{DockerEnabled: true},
	}
	rt := SandboxRuntimeInfo{
		ProxySocketPath: hostSocketPath,
	}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	// Check environment variables.
	wantDockerHost := "unix://" + dockerProxySandboxSocket
	for _, kv := range []struct{ key, val string }{
		{"DOCKER_HOST", wantDockerHost},
		{"CONTAINER_HOST", wantDockerHost},
		{"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", dockerProxySandboxSocket},
		{"TESTCONTAINERS_RYUK_DISABLED", "true"},
	} {
		if got := result.Env[kv.key]; got != kv.val {
			t.Errorf("env %s = %q, want %q", kv.key, got, kv.val)
		}
	}

	// Check bind-mount of proxy socket.
	found := false
	for _, m := range result.Mounts {
		if m.Source == hostSocketPath && m.Target == dockerProxySandboxSocket {
			found = true
			if !m.IsFile {
				t.Errorf("docker proxy mount should have IsFile=true")
			}
			if m.ReadOnly {
				t.Errorf("docker proxy mount should not be ReadOnly")
			}
		}
	}
	if !found {
		t.Errorf("docker proxy socket not found in mounts (source=%s, target=%s)",
			hostSocketPath, dockerProxySandboxSocket)
	}
}

// TestBuildSandboxSpec_DockerProxy_DisabledWhenNoSocketPath verifies that
// without ProxySocketPath no docker env vars are injected and no proxy socket
// is mounted — even when Visibility.DockerEnabled is true (proxy failed to
// start upstream).
func TestBuildSandboxSpec_DockerProxy_DisabledWhenNoSocketPath(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{DockerEnabled: true},
	}
	rt := SandboxRuntimeInfo{ProxySocketPath: ""}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	for _, key := range []string{
		"DOCKER_HOST", "CONTAINER_HOST",
		"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "TESTCONTAINERS_RYUK_DISABLED",
	} {
		if v, ok := result.Env[key]; ok {
			t.Errorf("env %s = %q, want absent (no proxy socket path)", key, v)
		}
	}
	for _, m := range result.Mounts {
		if m.Target == dockerProxySandboxSocket {
			t.Errorf("unexpected docker proxy mount present when ProxySocketPath is empty")
		}
	}
}

// TestBuildSandboxSpec_NoDispatchTimeContextFiles is the Phase 5b PR6 cutover
// TDD guard (docs/plans/phase5-shim-and-task-context.md 「PR 分割案 > 5b」6):
// contextFiles/buildEnvironmentYAML (task.yaml/instructions.yaml/
// environment.yaml/payload.{json,yaml} under $HOME/.boid/context/) and the
// per-task attachments RO bind ($HOME/.boid/attachments) are retired
// entirely — task/instructions/environment/payload are pull-only via the
// Phase 5b PR1/PR2 broker RPCs (`boid task current/instructions/env/payload`,
// `boid task attachments list/get`) from this PR forward. A fully-populated
// JobSpec/SandboxRuntimeInfo (task, instruction, primary input all set —
// everything that used to trigger every one of contextFiles' FileWrite
// branches) must produce a sandbox.Spec with zero Files/Mounts touching
// either retired path.
func TestBuildSandboxSpec_NoDispatchTimeContextFiles(t *testing.T) {
	spec := &orchestrator.JobSpec{
		TaskID:       "task-1",
		PrimaryInput: []byte(`{"artifact":{"ok":true}}`),
		Task:         &orchestrator.TaskSnapshot{ID: "task-1", Title: "t", Status: "executing", Behavior: "executor"},
		Instruction:  &orchestrator.RoutedInstruction{Agent: "claude-code", Message: "go"},
	}
	rt := SandboxRuntimeInfo{
		AllowedDomains: []string{"github.com"},
	}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	for _, f := range result.Files {
		if strings.Contains(f.Path, "/.boid/context/") {
			t.Errorf("unexpected dispatch-time context file %q — file materialization must be fully retired", f.Path)
		}
	}
	for _, m := range result.Mounts {
		if strings.Contains(m.Target, "/.boid/attachments") {
			t.Errorf("unexpected attachments bind mount %+v — the RO bind must be fully retired (use `boid task attachments list/get` instead)", m)
		}
	}
}

// TestBuildSandboxSpec_ProfileInit_IsThreaded verifies that
// JobSpec.SandboxProfile == sandbox.ProfileInit is correctly threaded through
// BuildSandboxSpec into sandbox.Spec.Profile.
func TestBuildSandboxSpec_ProfileInit_IsThreaded(t *testing.T) {
	spec := &orchestrator.JobSpec{
		SandboxProfile: int(sandbox.ProfileInit),
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if result.Profile != sandbox.ProfileInit {
		t.Errorf("sandbox.Spec.Profile = %v, want ProfileInit (%v)", result.Profile, sandbox.ProfileInit)
	}
}

// TestBuildSandboxSpec_ProfileInit_DoesNotShadowHomeTmpfs guards against a
// regression where boid kit init / workspace configure could not detect host
// tools that live under HOME (volta, ~/.local/bin/go, nvm, ...). ProfileInit
// already rbinds the entire host root read-only; layering a tmpfs over the
// whole of HOME on top of that hides exactly the binaries the scan is supposed
// to find. The builder must instead tmpfs only `<HOME>/.boid` so context-file
// writes stay isolated while the rest of HOME remains visible through the
// host-root rbind.
func TestBuildSandboxSpec_ProfileInit_DoesNotShadowHomeTmpfs(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	spec := &orchestrator.JobSpec{
		SandboxProfile: int(sandbox.ProfileInit),
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, m := range result.Mounts {
		if m.Type == sandbox.MountTmpfs && m.Target == homeDir {
			t.Errorf("found tmpfs mount targeting whole HOME (%s); ProfileInit must not shadow HOME — that hides ~/.volta, ~/.local/bin, etc. which kit init needs to scan", homeDir)
		}
	}
	wantBoidTmpfs := homeDir + "/.boid"
	foundBoidTmpfs := false
	for _, m := range result.Mounts {
		if m.Type == sandbox.MountTmpfs && m.Target == wantBoidTmpfs {
			foundBoidTmpfs = true
			break
		}
	}
	if !foundBoidTmpfs {
		t.Errorf("expected tmpfs mount targeting %s so context/output writes have writable storage, got mounts=%+v", wantBoidTmpfs, result.Mounts)
	}
}

// TestBuildSandboxSpec_ProfileInit_ServerSocket_NoBind guards against the
// regression where ProfileInit (boid kit init / workspace configure) attaches
// a bind at /run/boid/server.sock and dies on `mkdir /run/boid: permission
// denied` because the host root is rbind'd read-only and /run/boid does not
// exist on the host (the daemon socket lives under /run/user/<uid>/). For
// ProfileInit we point BOID_SOCKET at the host socket path directly and skip
// the bind — the host root rbind already exposes the socket at that path.
func TestBuildSandboxSpec_ProfileInit_ServerSocket_NoBind(t *testing.T) {
	hostSock := "/run/user/1000/boid.sock"
	spec := &orchestrator.JobSpec{
		SandboxProfile: int(sandbox.ProfileInit),
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{ServerSocket: hostSock})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, m := range result.Mounts {
		if m.Target == "/run/boid/server.sock" {
			t.Errorf("ProfileInit: must NOT bind /run/boid/server.sock (mkdir /run/boid fails under host root ro-rbind); got mount %+v", m)
		}
	}
	if got := result.Env["BOID_SOCKET"]; got != hostSock {
		t.Errorf("ProfileInit: BOID_SOCKET = %q, want %q (host socket path)", got, hostSock)
	}
}

// TestBuildSandboxSpec_ProfileDefault_ServerSocket_Binds verifies the default
// profile keeps binding the daemon socket at /run/boid/server.sock so regular
// task/exec sandboxes — which do NOT rbind host root — still get a stable
// in-sandbox path for the socket.
func TestBuildSandboxSpec_ProfileDefault_ServerSocket_Binds(t *testing.T) {
	hostSock := "/run/user/1000/boid.sock"
	spec := &orchestrator.JobSpec{} // ProfileDefault
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{ServerSocket: hostSock})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	var found *sandbox.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Target == "/run/boid/server.sock" {
			found = &result.Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ProfileDefault: expected bind at /run/boid/server.sock, got mounts=%+v", result.Mounts)
	}
	if found.Source != hostSock {
		t.Errorf("ProfileDefault: server.sock bind Source = %q, want %q", found.Source, hostSock)
	}
	if !found.IsFile {
		t.Errorf("ProfileDefault: server.sock bind IsFile = false, want true")
	}
	if got := result.Env["BOID_SOCKET"]; got != "/run/boid/server.sock" {
		t.Errorf("ProfileDefault: BOID_SOCKET = %q, want /run/boid/server.sock", got)
	}
}

// TestBuildSandboxSpec_ProfileDefault_NoProject_KeepsHomeTmpfs guards the
// non-ProfileInit branch so that the ProfileInit fix above does not silently
// remove the HOME tmpfs for the default profile, which still wants HOME
// isolated when no project is bound in.
func TestBuildSandboxSpec_ProfileDefault_NoProject_KeepsHomeTmpfs(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	spec := &orchestrator.JobSpec{} // ProfileDefault, no project
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	found := false
	for _, m := range result.Mounts {
		if m.Type == sandbox.MountTmpfs && m.Target == homeDir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ProfileDefault with no project should still tmpfs HOME (%s); got mounts=%+v", homeDir, result.Mounts)
	}
}

// TestBuildSandboxSpec_ProfileDefault_ZeroValue verifies that the zero value of
// JobSpec.SandboxProfile maps to sandbox.ProfileDefault in the resulting spec,
// preserving backward compatibility for callers that do not set the field.
func TestBuildSandboxSpec_ProfileDefault_ZeroValue(t *testing.T) {
	spec := &orchestrator.JobSpec{} // SandboxProfile is zero (ProfileDefault)
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if result.Profile != sandbox.ProfileDefault {
		t.Errorf("sandbox.Spec.Profile = %v, want ProfileDefault (%v)", result.Profile, sandbox.ProfileDefault)
	}
}

// TestBuildSandboxSpec_ProfileInit_HarnessKeepsAdditionalBindings guards the
// kit-init / workspace-configure regression where, for harness in
// claude/codex/opencode, BuildSandboxSpec dropped Visibility.AdditionalBindings
// entirely in favour of the adapter-declared bindings. That made `boid kit
// init` unable to write `~/.local/share/boid/kits/<name>/kit.yaml` because the
// rw kits dir bind never landed in the sandbox; the agent only saw the host
// root ro-rbind layer and EROFS-ed on first write.
//
// For ProfileInit jobs the additional bindings carry the writable / extra-ro
// paths that the init skill *must* see, so they need to be appended alongside
// the harness bindings rather than replaced by them.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retired claude.Adapter's
// own bindings (it now returns nil), so this test no longer has a harness
// binding to assert alongside AdditionalBindings — it only pins that
// AdditionalBindings themselves still land regardless of HarnessType being
// set to a known adapter (the original PR #594-era regression this test
// guards).
func TestBuildSandboxSpec_ProfileInit_HarnessKeepsAdditionalBindings(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	kitsDir := homeDir + "/.local/share/boid/kits"
	spec := &orchestrator.JobSpec{
		SandboxProfile: int(sandbox.ProfileInit),
		HarnessType:    "claude",
		Visibility: orchestrator.Visibility{
			AdditionalBindings: []orchestrator.BindMount{
				{Source: kitsDir, Target: kitsDir, Mode: "rw"},
			},
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	foundKitsRW := false
	for _, m := range result.Mounts {
		if m.Target == kitsDir && m.Type == sandbox.MountBind && !m.ReadOnly {
			foundKitsRW = true
			break
		}
	}
	if !foundKitsRW {
		t.Errorf("ProfileInit + harness=claude must keep AdditionalBindings: expected rw bind at %s, got mounts=%+v", kitsDir, result.Mounts)
	}
}

// TestBuildSandboxSpec_ProfileDefault_HarnessKeepsAdditionalBindings ensures
// workspace kit-declared additional_bindings reach the sandbox even when the
// harness adapter (claude/codex/opencode) also declares its own bindings.
// The 2026-06-26 workspace+kit reorg made kits a per-user place to declare
// host-side tool bindings (~/.volta, ~/.nuget, /opt/google/chrome, ...); the
// original Phase 3-c kit-free dispatch path used to drop them on the claude/
// codex/opencode harness path on the assumption that kits only existed in
// boid-kits and supplied agent CLI plumbing — that assumption no longer
// holds, so they must apply on top of harness bindings rather than be
// replaced by them.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retired claude.Adapter's
// own bindings (it now returns nil), so this test no longer has a harness
// binding to assert alongside AdditionalBindings — see
// TestBuildSandboxSpec_ProfileInit_HarnessKeepsAdditionalBindings's doc
// comment for the same note.
func TestBuildSandboxSpec_ProfileDefault_HarnessKeepsAdditionalBindings(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	kitBind := "/srv/some-kit-binding"
	spec := &orchestrator.JobSpec{
		HarnessType: "claude",
		Visibility: orchestrator.Visibility{
			AdditionalBindings: []orchestrator.BindMount{
				{Source: kitBind, Target: kitBind, Mode: "rw"},
			},
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	foundKit := false
	for _, m := range result.Mounts {
		if m.Target == kitBind && m.Type == sandbox.MountBind && !m.ReadOnly {
			foundKit = true
			break
		}
	}
	if !foundKit {
		t.Errorf("ProfileDefault + harness=claude must keep workspace kit AdditionalBindings: expected rw bind at %s, got mounts=%+v", kitBind, result.Mounts)
	}
}

// TestBuildSandboxSpec_HostCommandRulesEnv_SetWhenRejectRulesPresent verifies
// that BOID_HOST_COMMAND_RULES is set to a JSON map keyed by command Name
// (rt.ResolvedHostCommandsByName, not the abs-path-keyed
// rt.ResolvedHostCommands) whenever at least one resolved host command
// declares reject rules.
func TestBuildSandboxSpec_HostCommandRulesEnv_SetWhenRejectRulesPresent(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{
		// buildHostCommandRulesEnv now reads the short-name-keyed view
		// (docs/plans/phase5-shim-and-task-context.md 5a PR1) — production
		// code populates both maps from a single ResolveHostCommands call, so
		// this test mirrors that by hand.
		ResolvedHostCommandsByName: map[string]orchestrator.CommandDef{
			"gh": {
				Name: "gh",
				RejectRules: []orchestrator.RejectRule{
					{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
				},
			},
			"git": {
				Name: "git",
			},
		},
	}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	got := result.Env[sandbox.HostCommandRulesEnv]
	if got == "" {
		t.Fatalf("expected %s to be set, got empty", sandbox.HostCommandRulesEnv)
	}
	want := `{"gh":[{"match":"*--body-file*","reason":"sandbox paths are not visible on the host"}]}`
	if got != want {
		t.Errorf("%s = %q, want %q", sandbox.HostCommandRulesEnv, got, want)
	}
}

// TestBuildSandboxSpec_HostCommandRulesEnv_AbsentWhenNoRejectRules verifies
// the env var is not set at all (not even as "{}") when no resolved host
// command declares reject rules.
func TestBuildSandboxSpec_HostCommandRulesEnv_AbsentWhenNoRejectRules(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{
		ResolvedHostCommandsByName: map[string]orchestrator.CommandDef{
			"git": {Name: "git"},
			"gh":  {Name: "gh"},
		},
	}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if _, ok := result.Env[sandbox.HostCommandRulesEnv]; ok {
		t.Errorf("expected %s to be absent, got %q", sandbox.HostCommandRulesEnv, result.Env[sandbox.HostCommandRulesEnv])
	}
}

// TestBuildSandboxSpec_HostCommandRulesEnv_AbsentWhenNoHostCommands verifies
// the env var is not set when the job declares no host commands at all.
func TestBuildSandboxSpec_HostCommandRulesEnv_AbsentWhenNoHostCommands(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if _, ok := result.Env[sandbox.HostCommandRulesEnv]; ok {
		t.Errorf("expected %s to be absent, got %q", sandbox.HostCommandRulesEnv, result.Env[sandbox.HostCommandRulesEnv])
	}
}

// --- Phase 5 5a-3: shim fixed-directory placement
// (docs/plans/phase5-shim-and-task-context.md) ---

// TestBuildSandboxSpec_HostCommandSymlinks_UnderShimBinDir pins the 5a-3
// end-to-end wiring: every declared host_commands entry — including an
// aliased entry whose source basename differs from the declared name (the
// alias-echo path documented in wiring-seams.md #14) — surfaces in the
// resulting spec's Symlinks as `<shimBinDir>/<name>` pointing at `boid`
// (relative). The absence of any legacy `BOID_HOST_COMMAND_NAMES` env
// injection is also structurally guaranteed here: the assertion below
// enumerates every Symlink emitted, and the ResolvedHostCommandsByName field
// is the sole shim wiring path — there is no per-shim env key that could
// silently drift out of sync with what the broker Commands map is keyed by.
func TestBuildSandboxSpec_HostCommandSymlinks_UnderShimBinDir(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{
		BoidBinary: "/usr/local/bin/boid",
		ResolvedHostCommandsByName: map[string]orchestrator.CommandDef{
			"gh":      {Name: "gh", Path: "/usr/bin/gh"},
			"run-e2e": {Name: "run-e2e", Path: "/home/user/proj/e2e/run.sh"},
		},
	}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	want := map[string]string{
		sandboxShimBinDir + "/gh":      "boid",
		sandboxShimBinDir + "/run-e2e": "boid",
	}
	got := map[string]string{}
	for _, s := range result.Symlinks {
		got[s.LinkPath] = s.LinkTarget
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Symlinks = %#v, want %#v", got, want)
	}
	// 5a-3 negative: the pre-5a-3 BOID_HOST_COMMAND_NAMES env key that used
	// to carry {absPath: declaredName} is retired — the shim now resolves
	// via argv[0]'s basename (which equals the declared name by
	// construction). Assert its absence directly so a future re-introduction
	// is caught here rather than by an obscure runtime mismatch.
	if _, ok := result.Env["BOID_HOST_COMMAND_NAMES"]; ok {
		t.Errorf("BOID_HOST_COMMAND_NAMES must not be set post-5a-3 cutover")
	}
}

// TestBuildSandboxSpec_ShimBinDirBoidMount pins the fixed-directory bind of
// the boid multi-call binary itself. The Target is invariant across every
// dispatch — the docs/plans/container-based-boid.md future backend swap
// (image bakes /run/boid/bin/) inherits the same contract without changes
// to any harness/skill code that assumes shims resolve by short name.
func TestBuildSandboxSpec_ShimBinDirBoidMount(t *testing.T) {
	const boidBin = "/usr/local/bin/boid"
	const wantTarget = sandboxShimBinDir + "/boid"
	spec := &orchestrator.JobSpec{}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{BoidBinary: boidBin})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	var found *sandbox.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Target == wantTarget {
			found = &result.Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("boid binary mount not found at %s; mounts=%+v", wantTarget, result.Mounts)
	}
	if found.Source != boidBin {
		t.Errorf("Source = %q, want %q", found.Source, boidBin)
	}
	if !found.IsFile {
		t.Error("IsFile = false, want true")
	}
	if !found.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
}

// TestBuildSandboxSpec_ShimBinDirBoidMountSkippedForProfileInit pins the
// carve-out for ProfileInit (docs/plans/phase5-shim-and-task-context.md 5a
// PR3): ProfileInit rbinds host / read-only, so a bind at /run/boid/bin/boid
// would either fail (target dir un-creatable) or duplicate a boid binary
// already visible via the host rbind. ProfileInit also declares no host
// commands, so no symlink is expected either.
func TestBuildSandboxSpec_ShimBinDirBoidMountSkippedForProfileInit(t *testing.T) {
	spec := &orchestrator.JobSpec{
		SandboxProfile: int(sandbox.ProfileInit),
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{BoidBinary: "/usr/local/bin/boid"})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, m := range result.Mounts {
		if m.Target == sandboxShimBinDir+"/boid" {
			t.Errorf("ProfileInit must not bind boid at %s (host / rbind ro), got %+v", sandboxShimBinDir+"/boid", m)
		}
	}
	if len(result.Symlinks) != 0 {
		t.Errorf("ProfileInit must not emit shim symlinks, got %+v", result.Symlinks)
	}
}

// TestBuildSandboxSpec_ShimBinDirBoidMountSkippedForContainerBackend pins
// the PR9 regression fix (docs/plans/phase6-cutover-followups.md's
// debugging trail): the container backend's shared image already bakes
// boid at sandboxShimBinDir ("/run/boid/bin/boid" — build/container/
// Dockerfile), so BuildSandboxSpec must NOT also emit a bind mount for it
// when SandboxRuntimeInfo.UsingContainerBackend is true — doing so tries
// to bind-mount rt.BoidBinary (the DAEMON's own in-image path, e.g.
// "/usr/local/bin/boid") as a docker-out-of-docker sibling mount SOURCE,
// which the host's real docker daemon rejects outright ("bind source path
// does not exist") since that path only exists inside the daemon's own
// container. Host-command shim symlinks must still be emitted for the
// container backend too (決定2: only baked at image-build time is the
// boid binary itself and the /run/boid/bin directory, not individual
// <name> shims — those the entrypoint generates fresh from spec.Symlinks
// on every container start, identically to the userns backend).
func TestBuildSandboxSpec_ShimBinDirBoidMountSkippedForContainerBackend(t *testing.T) {
	spec := &orchestrator.JobSpec{
		HostCommands: map[string]orchestrator.CommandDef{
			"gh": {Path: "/usr/bin/gh"},
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{
		BoidBinary:                 "/usr/local/bin/boid",
		UsingContainerBackend:      true,
		ResolvedHostCommandsByName: map[string]orchestrator.CommandDef{"gh": {Path: "/usr/bin/gh"}},
	})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, m := range result.Mounts {
		if m.Target == sandboxShimBinDir+"/boid" {
			t.Errorf("container backend must not bind rt.BoidBinary at %s (already baked into the shared image), got %+v",
				sandboxShimBinDir+"/boid", m)
		}
	}
	if len(result.Symlinks) == 0 {
		t.Error("container backend must still emit host-command shim symlinks (only the boid binary bind is userns-only), got none")
	}
}

// --- Phase 4 PR2: workspace home bind + $HOME/.boid job tmpfs overlay
// (docs/plans/home-workspace-volume.md) ---

// mountTargetIndex returns the index of the first mount in mounts whose
// Target matches target, or -1 if none match. Shared by the ordering
// assertions below so they read as "X comes before/right after Y" instead of
// each hand-rolling a scan loop.
func mountTargetIndex(mounts []sandbox.Mount, target string) int {
	for i, m := range mounts {
		if m.Target == target {
			return i
		}
	}
	return -1
}

// TestHomeMounts_WorkspaceHomeDirSet_ReturnsBindOnly pins the Phase 6 PR8
// state (docs/plans/phase6-container-backend.md §決定 9): homeMounts returns
// a single read-write bind of the workspace home, with no $HOME/.boid tmpfs
// overlay layered on top.
//
// That overlay existed from Phase 4 PR2 through Phase 6 PR7 to isolate the
// well-known $HOME/.boid/output/payload_patch.json file between concurrent
// jobs sharing the same workspace home (codex review on the Phase 5b PR6
// cutover found removing it prematurely exploitable — see git history for
// that finding). Phase 6 PR8 migrated the only remaining writer (claude
// adapter's session-id bookkeeping, internal/adapters/claude/run.go's
// sendTaskUpdatePayloadPatch) to the broker's `boid task update
// --payload-patch` RPC (JobID-scoped, applied under a per-task lock), which
// needs no shared file and therefore no filesystem isolation to guard.
func TestHomeMounts_WorkspaceHomeDirSet_ReturnsBindOnly(t *testing.T) {
	const homeDir = "/home/user"
	const wsHome = "/data/boid/homes/default"
	mounts := homeMounts(homeDir, wsHome)
	if len(mounts) != 1 {
		t.Fatalf("homeMounts returned %d mounts, want 1 (bind only, no .boid tmpfs overlay): %+v", len(mounts), mounts)
	}
	if mounts[0].Source != wsHome || mounts[0].Target != homeDir || mounts[0].Type != sandbox.MountBind {
		t.Errorf("mounts[0] = %+v, want bind %s -> %s", mounts[0], wsHome, homeDir)
	}
	if mounts[0].ReadOnly {
		t.Error("workspace home bind must be read-write")
	}
}

func TestHomeMounts_WorkspaceHomeDirEmpty_FallsBackToTmpfs(t *testing.T) {
	const homeDir = "/home/user"
	mounts := homeMounts(homeDir, "")
	if len(mounts) != 1 {
		t.Fatalf("homeMounts returned %d mounts, want 1 (fallback tmpfs): %+v", len(mounts), mounts)
	}
	if mounts[0].Target != homeDir || mounts[0].Type != sandbox.MountTmpfs || mounts[0].Source != "" {
		t.Errorf("mounts[0] = %+v, want plain tmpfs at %s", mounts[0], homeDir)
	}
}

// TestBuildSandboxSpec_CloneEnabled_WorkspaceHomeBind pins the Clone branch's
// mount: a plain workspace home bind, with no $HOME/.boid tmpfs overlay
// (retired by Phase 6 PR8 — see homeMounts' doc comment).
func TestBuildSandboxSpec_CloneEnabled_WorkspaceHomeBind(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"/bin/true"},
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/project",
			Writable:   true,
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main"},
		},
	}
	const wsHome = "/data/boid/homes/default"
	rt := SandboxRuntimeInfo{JobID: "job-1", CloneWorkspaceDir: "/data/boid/runtimes/job-1/workspace", WorkspaceHomeDir: wsHome}
	out, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	bindIdx := mountTargetIndex(out.Mounts, homeDir)
	if bindIdx == -1 {
		t.Fatalf("workspace home bind at %s not found: %+v", homeDir, out.Mounts)
	}
	if out.Mounts[bindIdx].Source != wsHome || out.Mounts[bindIdx].Type != sandbox.MountBind {
		t.Errorf("home mount = %+v, want bind from %s", out.Mounts[bindIdx], wsHome)
	}
	if idx := mountTargetIndex(out.Mounts, homeDir+"/.boid"); idx != -1 {
		t.Errorf("unexpected /.boid tmpfs overlay mount (retired by Phase 6 PR8): %+v", out.Mounts[idx])
	}
}

// TestBuildSandboxSpec_CloneEnabled_NoWorkspaceHome_FallsBackToTmpfs pins the
// test-wiring graceful degrade: an empty WorkspaceHomeDir (e.g. Runner not
// wired, or a minimal test SandboxRuntimeInfo{}) must still produce the
// pre-PR2 single tmpfs at HOME, not a bind of an empty path.
func TestBuildSandboxSpec_CloneEnabled_NoWorkspaceHome_FallsBackToTmpfs(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"/bin/true"},
		Visibility: orchestrator.Visibility{
			ProjectDir: "/home/user/project",
			Writable:   true,
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main"},
		},
	}
	rt := SandboxRuntimeInfo{JobID: "job-1", CloneWorkspaceDir: "/data/boid/runtimes/job-1/workspace"}
	out, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	var found *sandbox.Mount
	for i := range out.Mounts {
		if out.Mounts[i].Target == homeDir {
			found = &out.Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no mount targeting HOME (%s) found: %+v", homeDir, out.Mounts)
	}
	if found.Type != sandbox.MountTmpfs || found.Source != "" {
		t.Errorf("HOME mount = %+v, want plain tmpfs (no source) when WorkspaceHomeDir is empty", found)
	}
	for _, m := range out.Mounts {
		if m.Target == homeDir+"/.boid" {
			t.Errorf("unexpected /.boid mount when WorkspaceHomeDir is empty (fallback should be a single HOME tmpfs): %+v", m)
		}
	}
}

// TestProjectVisibilityMounts_WorkspaceHomeBind_Order pins the full mount
// order projectVisibilityMounts produces once a workspace home is resolved:
// [bind effectiveDir, bind homeDir<-workspaceHomeDir, re-bind effectiveDir,
// peers..., .boid bind, .git ro re-bind]. Phase 6 PR8 (docs/plans/
// phase6-container-backend.md §決定 9) removed the $HOME/.boid tmpfs overlay
// that used to sit between the home bind and the effectiveDir re-mount.
func TestProjectVisibilityMounts_WorkspaceHomeBind_Order(t *testing.T) {
	const effectiveDir = "/home/user/project"
	const homeDir = "/home/user"
	const wsHome = "/data/boid/homes/default"
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, homeDir, wsHome, true, map[string]string{"peer": "/home/user/peer"})

	effIdx := mountTargetIndex(mounts, effectiveDir)
	homeIdx := mountTargetIndex(mounts, homeDir)
	// Second occurrence of effectiveDir (the re-mount) — search after the
	// home bind since the first occurrence (index 0) would otherwise match.
	remountIdx := -1
	for i := homeIdx + 1; i < len(mounts); i++ {
		if mounts[i].Target == effectiveDir {
			remountIdx = i
			break
		}
	}
	peerIdx := mountTargetIndex(mounts, "/home/user/peer")
	gitIdx := mountTargetIndex(mounts, effectiveDir+"/.git")

	if effIdx != 0 {
		t.Errorf("effectiveDir bind index = %d, want 0", effIdx)
	}
	if homeIdx != 1 {
		t.Errorf("home bind index = %d, want 1", homeIdx)
	}
	if mounts[homeIdx].Source != wsHome || mounts[homeIdx].Type != sandbox.MountBind {
		t.Errorf("home mount = %+v, want bind from %s", mounts[homeIdx], wsHome)
	}
	if idx := mountTargetIndex(mounts, homeDir+"/.boid"); idx != -1 {
		t.Errorf("unexpected /.boid tmpfs overlay mount (retired by Phase 6 PR8): %+v", mounts[idx])
	}
	if remountIdx != 2 {
		t.Errorf("effectiveDir re-mount index = %d, want 2 (immediately after home bind)", remountIdx)
	}
	if peerIdx <= remountIdx {
		t.Errorf("peer bind index = %d, want after re-mount (%d)", peerIdx, remountIdx)
	}
	if gitIdx <= peerIdx {
		t.Errorf(".git re-bind index = %d, want after peer bind (%d)", gitIdx, peerIdx)
	}
}

// TestProjectVisibilityMounts_NoWorkspaceHome_FallsBackToTmpfs is the
// projectVisibility-branch counterpart of the Clone-branch fallback test.
func TestProjectVisibilityMounts_NoWorkspaceHome_FallsBackToTmpfs(t *testing.T) {
	const effectiveDir = "/home/user/project"
	const homeDir = "/home/user"
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, homeDir, "", true, nil)

	var found *sandbox.Mount
	for i := range mounts {
		if mounts[i].Target == homeDir {
			found = &mounts[i]
			break
		}
	}
	if found == nil || found.Type != sandbox.MountTmpfs || found.Source != "" {
		t.Errorf("HOME mount = %+v, want plain tmpfs when workspaceHomeDir is empty", found)
	}
	for _, m := range mounts {
		if m.Target == homeDir+"/.boid" {
			t.Errorf("unexpected /.boid mount when workspaceHomeDir is empty: %+v", m)
		}
	}
}

// TestBuildSandboxSpec_DefaultProfile_WorkspaceHomeBind pins the "no project
// visible" branch's mount, mirroring the Clone-branch test above: a plain
// workspace home bind, no $HOME/.boid tmpfs overlay (retired by Phase 6 PR8).
func TestBuildSandboxSpec_DefaultProfile_WorkspaceHomeBind(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	spec := &orchestrator.JobSpec{} // ProfileDefault, no project
	const wsHome = "/data/boid/homes/default"
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{WorkspaceHomeDir: wsHome})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	bindIdx := mountTargetIndex(result.Mounts, homeDir)
	if bindIdx == -1 {
		t.Fatalf("workspace home bind at %s not found: %+v", homeDir, result.Mounts)
	}
	if result.Mounts[bindIdx].Source != wsHome || result.Mounts[bindIdx].Type != sandbox.MountBind {
		t.Errorf("home mount = %+v, want bind from %s", result.Mounts[bindIdx], wsHome)
	}
	if idx := mountTargetIndex(result.Mounts, homeDir+"/.boid"); idx != -1 {
		t.Errorf("unexpected /.boid tmpfs overlay mount (retired by Phase 6 PR8): %+v", result.Mounts[idx])
	}
}

// TestBuildSandboxSpec_ProfileInit_IgnoresWorkspaceHomeDir is the regression
// pin for docs/plans/home-workspace-volume.md Phase 4 PR2's explicit
// decision to leave ProfileInit untouched: even when rt.WorkspaceHomeDir is
// set, ProfileInit must never bind it over HOME (that would shadow the host
// tools ProfileInit's host-root rbind exists to expose), and must keep the
// single $HOME/.boid tmpfs it already had.
func TestBuildSandboxSpec_ProfileInit_IgnoresWorkspaceHomeDir(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}
	spec := &orchestrator.JobSpec{
		SandboxProfile: int(sandbox.ProfileInit),
	}
	rt := SandboxRuntimeInfo{WorkspaceHomeDir: "/data/boid/homes/default"}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	for _, m := range result.Mounts {
		if m.Target == homeDir && m.Type == sandbox.MountBind {
			t.Errorf("ProfileInit must not bind WorkspaceHomeDir over HOME: %+v", m)
		}
	}
	boidTmpfsCount := 0
	for _, m := range result.Mounts {
		if m.Target == homeDir+"/.boid" {
			boidTmpfsCount++
			if m.Type != sandbox.MountTmpfs {
				t.Errorf(".boid mount = %+v, want tmpfs", m)
			}
		}
	}
	if boidTmpfsCount != 1 {
		t.Errorf("found %d mounts targeting %s/.boid, want exactly 1", boidTmpfsCount, homeDir)
	}
}
