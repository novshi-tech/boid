package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"gopkg.in/yaml.v3"
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

// /usr/bin/git と /bin/git が boid バイナリ bind で上書きされることを検証する。
// これにより絶対パスで実体 git を呼び出す迂回が防止される。
// boid バイナリ自身はホスト実パスのまま bind mount される（/opt/boid/bin/boid は廃止）。
// TestBuildSandboxSpec_BoidBinaryBindMountOnly is the PR6 cutover rewrite of
// the former "GitShimBindMounts" test: the git-shim PATH overlay
// (/usr/bin/git, /bin/git bound to the boid binary) is retired — sandbox git
// is now always the real binary visible via the base rbind of /usr/bin — so
// this asserts the boid binary itself is still bind-mounted (for host
// command shims) while /usr/bin/git and /bin/git are conspicuously absent
// regardless of whether BoidBinary is set.
func TestBuildSandboxSpec_BoidBinaryBindMountOnly(t *testing.T) {
	const boidBin = "/usr/local/bin/boid"
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
		case boidBin:
			boidMount = m
		case "/usr/bin/git", "/bin/git":
			t.Errorf("git-shim overlay mount must not exist post-cutover (docs/plans/git-gateway-cutover.md PR6): target=%q", m.Target)
		case "/opt/boid/bin/boid":
			t.Errorf("/opt/boid/bin/boid must not exist as mount target in new design")
		}
	}

	// boid バイナリはホスト実パスのまま bind mount される。
	if boidMount == nil {
		t.Fatalf("boid binary mount not found at target %q", boidBin)
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

// boid と git は ResolveHostCommands に含まれない（専用の bind mount が別途生成される）。
// その他の host commands はホスト実パスに bind mount される。
func TestHostCommandMounts_BoidAndGitExcluded(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"gh":  {},
		"git": {},
	}
	resolved, _, err := ResolveHostCommands([]string{"boid", "git"}, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	mounts := hostCommandMounts("/usr/local/bin/boid", resolved)
	for _, m := range mounts {
		if m.Target == "/usr/bin/boid" || m.Target == "/usr/bin/git" {
			t.Errorf("boid/git must not get host command mount, got target=%q", m.Target)
		}
	}
	var hasGh bool
	for _, m := range mounts {
		if m.Target == "/usr/bin/gh" {
			hasGh = true
		}
	}
	if !hasGh {
		t.Error("gh must get a host command mount")
	}
}

// ホストに存在しないコマンドは fail-fast でエラーになる。
func TestHostCommandMounts_NotFound(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "", fmt.Errorf("not found")
	}
	hostCmds := map[string]orchestrator.CommandDef{"missing-cmd": {}}
	_, _, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err == nil {
		t.Error("expected error for missing host command, got nil")
	}
}

// mount target はホスト実パス（/opt/boid/bin/<cmd> ではない）。
func TestHostCommandMounts_BindsAtHostPath(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/local/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{"gh": {}}
	resolved, _, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	mounts := hostCommandMounts("/usr/local/bin/boid", resolved)
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	m := mounts[0]
	if m.Target != "/usr/local/bin/gh" {
		t.Errorf("mount target = %q, want /usr/local/bin/gh", m.Target)
	}
	if m.Source != "/usr/local/bin/boid" {
		t.Errorf("mount source = %q, want /usr/local/bin/boid", m.Source)
	}
	if !m.ReadOnly {
		t.Error("host command mount must be ReadOnly")
	}
	if !m.IsFile {
		t.Error("host command mount must have IsFile=true")
	}
}

// 同じコマンドが builtins と hostCommands の両方にある場合は重複しない。
func TestHostCommandMounts_Dedup(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{"gh": {}}
	resolved, _, err := ResolveHostCommands([]string{"gh"}, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	mounts := hostCommandMounts("/usr/local/bin/boid", resolved)
	if len(mounts) != 1 {
		t.Errorf("expected 1 mount (dedup), got %d", len(mounts))
	}
}

// CommandDef.Path 指定あり → lookPath は呼ばれず def.Path が Target になる。
// run-e2e のような別名キーが Path 指定されたファイル位置に bind mount されるケース。
func TestHostCommandMounts_PathSpecified_SkipsLookPath(t *testing.T) {
	dir := t.TempDir()
	scriptPath := dir + "/run-e2e.sh"
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lookPathCalled := false
	fakeLookPath := func(name string) (string, error) {
		lookPathCalled = true
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{
		"run-e2e": {Path: scriptPath},
	}
	resolved, _, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	mounts := hostCommandMounts("/usr/local/bin/boid", resolved)
	if lookPathCalled {
		t.Error("lookPath must not be called when CommandDef.Path is set")
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Target != scriptPath {
		t.Errorf("mount target = %q, want %q", mounts[0].Target, scriptPath)
	}
	// resolved map のキーも絶対パス、broker に渡る Path も同じ絶対パスである
	// ことを確認 (shim の os.Executable() lookup と一致する不変条件)。
	def, ok := resolved[scriptPath]
	if !ok {
		t.Fatalf("resolved map must be keyed by absolute path %q", scriptPath)
	}
	if def.Path != scriptPath {
		t.Errorf("resolved def.Path = %q, want %q", def.Path, scriptPath)
	}
	if def.Name != "run-e2e" {
		t.Errorf("resolved def.Name = %q, want run-e2e", def.Name)
	}
}

// host_commands.<name>.path の相対パスは projectDir 基準で解決される。
func TestHostCommandMounts_RelativePathResolvedFromProjectDir(t *testing.T) {
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
	resolved, _, err := ResolveHostCommands(nil, hostCmds, projectDir, func(string) (string, error) {
		return "", fmt.Errorf("lookPath should not be called")
	}, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	def, ok := resolved[scriptPath]
	if !ok {
		t.Fatalf("resolved must contain absolute key %q, got %v", scriptPath, resolved)
	}
	if def.Path != scriptPath {
		t.Errorf("def.Path = %q, want %q", def.Path, scriptPath)
	}
}

// CommandDef.Path 空 → lookPath 結果が Target になる（従来挙動の回帰防止）。
func TestHostCommandMounts_PathEmpty_UsesLookPath(t *testing.T) {
	fakeLookPath := func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	hostCmds := map[string]orchestrator.CommandDef{"gh": {}}
	resolved, _, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	mounts := hostCommandMounts("/usr/local/bin/boid", resolved)
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Target != "/usr/bin/gh" {
		t.Errorf("mount target = %q, want /usr/bin/gh", mounts[0].Target)
	}
}

// CommandDef.Path 指定だが対象ファイルが存在しない → "does not exist on host" エラー。
func TestHostCommandMounts_PathDoesNotExist_Error(t *testing.T) {
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

// builtin と host command の複合ケース: host command 側のみ Path 指定。
// builtin は lookPath、host command は def.Path を使い、順序が安定する。
func TestHostCommandMounts_MixedBuiltinAndPathCommand(t *testing.T) {
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
	resolved, _, err := ResolveHostCommands([]string{"jq"}, hostCmds, "", fakeLookPath, fakeGetOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	mounts := hostCommandMounts("/usr/local/bin/boid", resolved)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	targets := map[string]bool{}
	for _, m := range mounts {
		targets[m.Target] = true
	}
	if !targets["/usr/bin/jq"] {
		t.Error("builtin jq must be mounted at /usr/bin/jq")
	}
	if !targets[scriptPath] {
		t.Errorf("host command run-e2e must be mounted at %q", scriptPath)
	}
}

// boid バイナリが標準外パスにある場合 (~/go/bin, /tmp/.../bin 等)、
// そのディレクトリが PATH の先頭に追加されることを確認する。
// サンドボックス内スクリプトが `boid job done` / `boid task create` を
// フルパスなしで呼び出せることが目的。
func TestBuildPATH_BoidDirAddedWhenNonStandard(t *testing.T) {
	cases := []struct {
		name       string
		boidBinary string
		wantPrefix string
	}{
		{
			name:       "go/bin location",
			boidBinary: "/home/user/go/bin/boid",
			wantPrefix: hostHomeDir() + "/.local/bin:/home/user/go/bin:",
		},
		{
			name:       "tmp e2e location",
			boidBinary: "/tmp/boid-e2e-test-ABCDEF/bin/boid",
			wantPrefix: hostHomeDir() + "/.local/bin:/tmp/boid-e2e-test-ABCDEF/bin:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := buildPATH(nil, nil, tc.boidBinary)
			if !strings.HasPrefix(path, tc.wantPrefix) {
				t.Errorf("buildPATH = %q, want prefix %q", path, tc.wantPrefix)
			}
		})
	}
}

// boid が標準パス (/usr/local/bin 等) にある場合は重複しない。
func TestBuildPATH_BoidDirNotDuplicatedForStandardPaths(t *testing.T) {
	for _, boidBinary := range []string{
		"/usr/local/bin/boid",
		"/usr/bin/boid",
		"/bin/boid",
	} {
		path := buildPATH(nil, nil, boidBinary)
		want := hostHomeDir() + "/.local/bin:/usr/local/bin:/usr/bin:/bin"
		if path != want {
			t.Errorf("buildPATH(%q) = %q, want %q", boidBinary, path, want)
		}
	}
}

// TestBuildPATH_WorkspaceHomeLocalBinAlwaysLeads pins the Phase 4 PR3 PATH
// change (docs/plans/home-workspace-volume.md): with adapter-declared
// bindings retired, a harness CLI installed by the workspace's init.sh under
// $HOME/.local/bin must resolve by name without any binding or host-command
// wiring. It is unconditionally the first PATH entry — ahead of the boid
// binary dir and every other source — regardless of whether bindings/
// hostCommands/boidBinary are supplied at all.
func TestBuildPATH_WorkspaceHomeLocalBinAlwaysLeads(t *testing.T) {
	want := hostHomeDir() + "/.local/bin"

	path := buildPATH(nil, nil, "")
	if !strings.HasPrefix(path, want+":") {
		t.Errorf("buildPATH(nil, nil, \"\") = %q, want prefix %q:", path, want)
	}

	hostCommands := map[string]orchestrator.CommandDef{
		"/opt/custom/sbin/other": {Name: "other", Path: "/opt/custom/sbin/other"},
	}
	path = buildPATH(nil, hostCommands, "/home/user/go/bin/boid")
	if !strings.HasPrefix(path, want+":") {
		t.Errorf("buildPATH with hostCommands+boidBinary = %q, want prefix %q:", path, want)
	}
}

// host command の解決済み絶対パスのディレクトリが PATH に乗る。非標準ディレクトリ
// (~/.local/bin 等) に置かれた host command が、サンドボックス内で名前解決できる
// ようにするための配線。shim 自体は絶対パスに bind mount されるだけで PATH には
// 現れないため、ここで親ディレクトリを PATH に足さないと command not found になる。
func TestBuildPATH_HostCommandDirsAdded(t *testing.T) {
	hostCommands := map[string]orchestrator.CommandDef{
		"/home/user/.local/bin/mytool": {Name: "mytool", Path: "/home/user/.local/bin/mytool"},
		"/opt/custom/sbin/other":       {Name: "other", Path: "/opt/custom/sbin/other"},
	}
	path := buildPATH(nil, hostCommands, "/usr/local/bin/boid")
	for _, dir := range []string{"/home/user/.local/bin", "/opt/custom/sbin"} {
		if !strings.Contains(":"+path+":", ":"+dir+":") {
			t.Errorf("buildPATH = %q, want dir %q on PATH", path, dir)
		}
	}
	if !strings.HasSuffix(path, "/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("buildPATH = %q, want base PATH suffix", path)
	}
}

// 標準ディレクトリにある host command は base PATH でカバーされるので重複追加しない。
func TestBuildPATH_HostCommandStandardDirNotDuplicated(t *testing.T) {
	hostCommands := map[string]orchestrator.CommandDef{
		"/usr/bin/gh":       {Name: "gh", Path: "/usr/bin/gh"},
		"/usr/local/bin/jq": {Name: "jq", Path: "/usr/local/bin/jq"},
		"/bin/cat":          {Name: "cat", Path: "/bin/cat"},
	}
	path := buildPATH(nil, hostCommands, "/usr/local/bin/boid")
	want := hostHomeDir() + "/.local/bin:/usr/local/bin:/usr/bin:/bin"
	if path != want {
		t.Errorf("buildPATH = %q, want %q", path, want)
	}
}

// 同一ディレクトリに複数の host command があってもディレクトリは一度だけ追加。
// The fake host-command dir deliberately avoids "<hostHomeDir>/.local/bin"
// (a real value, not a literal) so this assertion stays deterministic
// regardless of the machine running the test.
func TestBuildPATH_HostCommandDirDeduplicated(t *testing.T) {
	hostCommands := map[string]orchestrator.CommandDef{
		"/srv/hostcmd/bin/a": {Name: "a", Path: "/srv/hostcmd/bin/a"},
		"/srv/hostcmd/bin/b": {Name: "b", Path: "/srv/hostcmd/bin/b"},
	}
	path := buildPATH(nil, hostCommands, "/usr/local/bin/boid")
	want := hostHomeDir() + "/.local/bin:/srv/hostcmd/bin:/usr/local/bin:/usr/bin:/bin"
	if path != want {
		t.Errorf("buildPATH = %q, want %q", path, want)
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

// contextFiles must materialize payload.yaml / payload.json for every hook
// that carries PrimaryInput.
func TestContextFiles_PayloadWrittenForNonInteractiveHook(t *testing.T) {
	inst := &orchestrator.RoutedInstruction{
		Role:    "rework",
		Agent:   "claude-code",
		Message: "verification findings に記載された問題を修正せよ。",
	}
	primary := []byte(`{"verification":{"findings":[{"status":"open","message":"failure"}]}}`)

	files := contextFiles(
		"/home/agent",
		nil,
		inst,
		primary,
		EnvironmentInput{},
	)

	var gotJSON, gotYAML bool
	for _, f := range files {
		switch f.Path {
		case "/home/agent/.boid/context/payload.json":
			gotJSON = true
			if f.Content != string(primary) {
				t.Errorf("payload.json content = %q, want %q", f.Content, string(primary))
			}
		case "/home/agent/.boid/context/payload.yaml":
			gotYAML = true
			if f.Content == "" {
				t.Error("payload.yaml content is empty")
			}
		}
	}
	if !gotJSON {
		t.Error("payload.json must be written for non-interactive hooks when PrimaryInput is present")
	}
	if !gotYAML {
		t.Error("payload.yaml must be written for non-interactive hooks when PrimaryInput is present")
	}
}

func TestContextFiles_PayloadWrittenForInteractiveHook(t *testing.T) {
	inst := &orchestrator.RoutedInstruction{
		Role:  "main",
		Agent: "claude-code",
	}
	primary := []byte(`{"artifact":null}`)

	files := contextFiles(
		"/home/agent",
		nil,
		inst,
		primary,
		EnvironmentInput{},
	)

	var gotJSON bool
	for _, f := range files {
		if f.Path == "/home/agent/.boid/context/payload.json" {
			gotJSON = true
			break
		}
	}
	if !gotJSON {
		t.Error("payload.json must be written for interactive hooks when PrimaryInput is present")
	}
}

// The go-native runner replaces the former EXIT-trap `boid job done` script:
// BuildSandboxSpec now carries Foreground (whether to post job-done) and the
// PayloadPatchPath the runner reads the result from.
func TestBuildSandboxSpec_HookSetsForegroundFalseAndPayloadPatchPath(t *testing.T) {
	spec := &orchestrator.JobSpec{Interactive: true}

	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{Foreground: false})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if result.Foreground {
		t.Error("hook job must have Foreground=false so the runner posts boid job done")
	}
	wantPatch := result.Env["HOME"] + "/.boid/output/payload_patch.json"
	if result.PayloadPatchPath != wantPatch {
		t.Errorf("PayloadPatchPath = %q, want %q", result.PayloadPatchPath, wantPatch)
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

func TestContextFiles_NoPayloadFilesWhenPrimaryInputEmpty(t *testing.T) {
	inst := &orchestrator.RoutedInstruction{
		Role:  "main",
		Agent: "claude-code",
	}

	files := contextFiles(
		"/home/agent",
		nil,
		inst,
		nil,
		EnvironmentInput{},
	)

	for _, f := range files {
		if f.Path == "/home/agent/.boid/context/payload.json" ||
			f.Path == "/home/agent/.boid/context/payload.yaml" {
			t.Errorf("unexpected payload file written with empty PrimaryInput: %s", f.Path)
		}
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

// parsedEnvDoc parses the YAML environment.yaml emitted by buildEnvironmentYAML
// into a generic map for assertion in the tests below. Keeping it a map rather
// than the typed WorkspaceEnvView keeps the tests robust to additive layout
// changes — they only assert on what they care about.
func parsedEnvDoc(t *testing.T, in EnvironmentInput) map[string]any {
	t.Helper()
	raw := buildEnvironmentYAML(in)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal env yaml: %v\n----\n%s", err, raw)
	}
	return doc
}

// TestBuildEnvironmentYAML_ReducedSchema pins the environment.yaml 縮退
// (docs/plans/phase5-shim-and-task-context.md 決定事項 4, Phase 5b PR5):
// buildEnvironmentYAML's output must contain only the two properties an
// in-sandbox agent cannot observe on its own (allowed_domains,
// host_commands). Every section the legacy environment.yaml additionally
// carried (readonly/worktree/tools/sandbox.*/filesystem.*/session.*/notes/
// workspace_projects, plus the `network:` wrapper allowed_domains used to
// live under) must be gone entirely — grep-based on the raw YAML text rather
// than typed-struct field checks, so a future accidental reintroduction of
// any retired key fails loudly regardless of where in the doc it reappears.
func TestBuildEnvironmentYAML_ReducedSchema(t *testing.T) {
	raw := buildEnvironmentYAML(EnvironmentInput{
		AllowedDomains: []string{"github.com"},
		HostCommands: map[string]orchestrator.CommandDef{
			"gh": {Name: "gh", AllowedSubcommands: []string{"pr"}},
		},
	})

	for _, want := range []string{"allowed_domains:", "host_commands:"} {
		if !strings.Contains(raw, want) {
			t.Errorf("environment.yaml missing %q:\n%s", want, raw)
		}
	}

	for _, retired := range []string{
		"sandbox:", "filesystem:", "session:", "notes:", "tools:",
		"workspace_projects:", "readonly:", "worktree:",
		"network.restricted:", "network.egress:", "network.proxy_url:",
		"network.webfetch:", "network:",
	} {
		if strings.Contains(raw, retired) {
			t.Errorf("environment.yaml still contains retired section %q:\n%s", retired, raw)
		}
	}
}

// TestBuildEnvironmentYAML_ReducedSchema_EmptyInput guards the degenerate
// all-zero-value EnvironmentInput case: the pre-縮退 schema unconditionally
// emitted sandbox:/filesystem:/network:/tools: sections regardless of input,
// so an all-empty EnvironmentInput is the sharpest regression trap for "a
// retired section crept back in as an always-present default".
func TestBuildEnvironmentYAML_ReducedSchema_EmptyInput(t *testing.T) {
	raw := buildEnvironmentYAML(EnvironmentInput{})
	for _, retired := range []string{
		"sandbox:", "filesystem:", "session:", "network:", "notes:",
		"readonly:", "worktree:", "tools:", "workspace_projects:",
	} {
		if strings.Contains(raw, retired) {
			t.Errorf("environment.yaml (empty input) still contains retired section %q:\n%s", retired, raw)
		}
	}
}

// TestBuildEnvironmentYAML_AllowedDomainsTopLevel pins allowed_domains' new
// top-level position (it lived under `network.allowed_domains` before the
// 縮退; the `network:` wrapper is gone entirely now that nothing else lives
// under it) — matching WorkspaceEnvView's YAML tag and
// references/data-model.md's documented `boid task env` schema.
func TestBuildEnvironmentYAML_AllowedDomainsTopLevel(t *testing.T) {
	doc := parsedEnvDoc(t, EnvironmentInput{AllowedDomains: []string{"pypi.org", "github.com"}})
	allowed, ok := doc["allowed_domains"].([]any)
	if !ok || len(allowed) != 2 {
		t.Fatalf("allowed_domains = %v, want 2 entries", doc["allowed_domains"])
	}
	if allowed[0] != "pypi.org" || allowed[1] != "github.com" {
		t.Errorf("allowed_domains order = %v, want [pypi.org, github.com]", allowed)
	}
	if _, has := doc["network"]; has {
		t.Errorf("network wrapper should be gone (allowed_domains is top-level now), got network=%v", doc["network"])
	}
}

// jsonThenYAML reproduces the actual `boid task env` CLI-side transform
// (internal/sandbox/boid_shim_task_context.go's jsonToYAMLForShim, fed by
// internal/server/boid_executor.go's marshalTaskContextResponse): the broker
// JSON-encodes a WorkspaceEnvView for wire transport, and the CLI decodes
// that JSON into a generic map[string]any before re-rendering it as YAML.
// Reimplemented here (rather than imported) because boid_shim_task_context.go
// lives in internal/sandbox, which internal/dispatcher may import but not the
// reverse — and jsonToYAMLForShim is unexported besides.
func jsonThenYAML(t *testing.T, v any) []byte {
	t.Helper()
	wireJSON, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var generic any
	if err := json.Unmarshal(wireJSON, &generic); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	out, err := yaml.Marshal(generic)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	return out
}

// TestBuildEnvironmentYAML_SemanticallyMatchesTaskEnvCLIPath is the drift
// guard wiring-seams.md #13 exists for: buildEnvironmentYAML (the
// environment.yaml file End A materializes into every sandbox) and what the
// real `boid task env` CLI prints (End B/C) must describe the exact same
// data for the exact same input, or the two paths could silently disagree
// about a job's sandbox constraints during the 5b-5→5b-6 parallel-path
// window.
//
// This is a **semantic**, not byte-identical, comparison — codex review
// caught that an earlier version of this test compared buildEnvironmentYAML's
// output against a bare yaml.Marshal(BuildWorkspaceEnvView(...)), which is
// not actually what `boid task env` prints. The real CLI path re-renders the
// broker's JSON reply by decoding into a generic map (jsonThenYAML above,
// mirroring jsonToYAMLForShim) — yaml.v3 sorts a map's keys alphabetically
// but preserves a struct's declared field order, so e.g. `host_commands[]`
// entries come out as `{name, allow, deny, reject}` (WorkspaceEnvHostCommand's
// field order) from buildEnvironmentYAML but `{allow, deny, name, reject}`
// (alphabetical) from the CLI path — different bytes, same data. Parsing
// both back into map[string]any and comparing with reflect.DeepEqual asserts
// the property that actually matters: no field is added, dropped, renamed,
// or reshaped in translation.
func TestBuildEnvironmentYAML_SemanticallyMatchesTaskEnvCLIPath(t *testing.T) {
	allowedDomains := []string{"github.com", "api.anthropic.com"}
	hostCommands := map[string]orchestrator.CommandDef{
		"gh": {
			Name:               "gh",
			AllowedSubcommands: []string{"pr", "issue"},
			DeniedPatterns:     []string{"auth"},
			RejectRules: []orchestrator.RejectRule{
				{Match: "*--body-file*", Reason: `use --body "$(cat <file>)"`},
			},
		},
		"aws": {Name: "aws"},
	}

	fileYAML := buildEnvironmentYAML(EnvironmentInput{
		AllowedDomains: allowedDomains,
		HostCommands:   hostCommands,
	})
	cliYAML := jsonThenYAML(t, BuildWorkspaceEnvView(allowedDomains, hostCommands))

	var fileDoc, cliDoc map[string]any
	if err := yaml.Unmarshal([]byte(fileYAML), &fileDoc); err != nil {
		t.Fatalf("unmarshal file YAML: %v\n%s", err, fileYAML)
	}
	if err := yaml.Unmarshal(cliYAML, &cliDoc); err != nil {
		t.Fatalf("unmarshal CLI YAML: %v\n%s", err, cliYAML)
	}
	if !reflect.DeepEqual(fileDoc, cliDoc) {
		t.Errorf("environment.yaml and the `boid task env` CLI output diverged semantically:\n--- file (parsed) ---\n%#v\n--- cli (parsed) ---\n%#v", fileDoc, cliDoc)
	}
}

// TestBuildEnvironmentYAML_SemanticallyMatchesTaskEnvCLIPath_EmptyInputs is
// the same drift guard for the degenerate empty-input case (no allowed
// domains, no host commands) — the reduced schema omits both keys entirely
// then (`omitempty` on both WorkspaceEnvView fields), worth pinning
// separately since an omitempty bug affecting only one side would not show
// up in the populated-input case above.
func TestBuildEnvironmentYAML_SemanticallyMatchesTaskEnvCLIPath_EmptyInputs(t *testing.T) {
	fileYAML := buildEnvironmentYAML(EnvironmentInput{})
	cliYAML := jsonThenYAML(t, BuildWorkspaceEnvView(nil, nil))

	var fileDoc, cliDoc map[string]any
	if err := yaml.Unmarshal([]byte(fileYAML), &fileDoc); err != nil {
		t.Fatalf("unmarshal file YAML: %v\n%s", err, fileYAML)
	}
	if err := yaml.Unmarshal(cliYAML, &cliDoc); err != nil {
		t.Fatalf("unmarshal CLI YAML: %v\n%s", err, cliYAML)
	}
	if !reflect.DeepEqual(fileDoc, cliDoc) {
		t.Errorf("environment.yaml and the `boid task env` CLI output diverged semantically for empty inputs:\n--- file (parsed) ---\n%#v\n--- cli (parsed) ---\n%#v", fileDoc, cliDoc)
	}
}

// 添付ファイル機能で、 AttachmentsRoot と spec.TaskID の両方が揃ったら
// `<root>/tasks/<id>/attachments` を read-only で bind する。 dir 不在時は
// 起動 script が Guard で skip するため、 attachments 0 件のタスクでも mount
// 行は出るが副作用は無い。 シェル / harness どちらの dispatch 経路でも同じ
// 結果になるのが要件。
func TestBuildSandboxSpec_AttachmentsBind(t *testing.T) {
	const taskID = "abc-123"
	root := t.TempDir()
	spec := &orchestrator.JobSpec{TaskID: taskID}
	rt := SandboxRuntimeInfo{AttachmentsRoot: root}

	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	var found *sandbox.Mount
	wantTarget := hostHomeDir() + "/.boid/attachments"
	wantSource := root + "/tasks/" + taskID + "/attachments"
	for i := range result.Mounts {
		if result.Mounts[i].Target == wantTarget {
			found = &result.Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("attachments bind target %q not present in mounts", wantTarget)
	}
	if found.Source != wantSource {
		t.Errorf("attachments bind Source = %q, want %q", found.Source, wantSource)
	}
	if !found.ReadOnly {
		t.Errorf("attachments bind must be ReadOnly")
	}
	if found.Guard == "" {
		t.Errorf("attachments bind must have a Guard so missing dir is skipped")
	}
	if !strings.Contains(found.Guard, "-d") {
		t.Errorf("attachments Guard = %q, want a -d dir test", found.Guard)
	}
}

// AttachmentsRoot 未設定 / TaskID 空のときは bind が出ない (regression guard:
// 既存テストの mount セットに余計な entry を足さない)。
func TestBuildSandboxSpec_AttachmentsBindAbsentWithoutRootOrTask(t *testing.T) {
	cases := []struct {
		name string
		spec *orchestrator.JobSpec
		rt   SandboxRuntimeInfo
	}{
		{"no root", &orchestrator.JobSpec{TaskID: "t1"}, SandboxRuntimeInfo{}},
		{"no task", &orchestrator.JobSpec{}, SandboxRuntimeInfo{AttachmentsRoot: "/tmp/dummy"}},
		{"neither", &orchestrator.JobSpec{}, SandboxRuntimeInfo{}},
	}
	wantTarget := hostHomeDir() + "/.boid/attachments"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := BuildSandboxSpec(tc.spec, tc.rt)
			if err != nil {
				t.Fatalf("BuildSandboxSpec: %v", err)
			}
			for _, m := range result.Mounts {
				if m.Target == wantTarget {
					t.Errorf("unexpected attachments bind mount: %+v", m)
				}
			}
		})
	}
}

// TestBuildSandboxSpec_AttachmentsBind_RejectsTraversalTaskID is the bind-side
// half of codex review's Blocker finding on PR #798 (Phase 5b PR2 attachments
// RPCs): CreateTaskRequest.ID is caller-supplied and saved as the literal DB
// primary key without validation, so a task can be dispatched with a literal
// ID like "alias/../<victim-id>". The attachments RO bind built the mount
// source with a bare filepath.Join (no equivalent to
// api.AttachmentsRootForTask's isCanonicalPathComponent guard), which
// silently collapsed such a TaskID down to the *victim* task's real
// attachments directory — the same class of cross-task leak fixed for the
// RPC read/write paths, but reachable here at dispatch time via the sandbox
// mount instead of a runtime RPC call. Fixed in the same PR (see
// wiring-seams.md #15): isCanonicalTaskIDComponent now gates the mount the
// same way, so a non-canonical TaskID gets no attachments bind at all
// (fail-closed, the same "just skip the mount" behavior the existing empty
// AttachmentsRoot/TaskID cases already use) rather than the wrong one.
func TestBuildSandboxSpec_AttachmentsBind_RejectsTraversalTaskID(t *testing.T) {
	root := t.TempDir()
	victimID := "550e8400-e29b-41d4-a716-446655440000"
	victimAttachSrc := root + "/tasks/" + victimID + "/attachments"
	wantTarget := hostHomeDir() + "/.boid/attachments"

	cases := []string{
		"alias/../" + victimID,
		"../other-task",
		"..",
		".",
		"foo/bar",
		"/abs/task",
	}
	for _, taskID := range cases {
		t.Run(taskID, func(t *testing.T) {
			spec := &orchestrator.JobSpec{TaskID: taskID}
			rt := SandboxRuntimeInfo{AttachmentsRoot: root}

			result, err := BuildSandboxSpec(spec, rt)
			if err != nil {
				t.Fatalf("BuildSandboxSpec: %v", err)
			}
			for _, m := range result.Mounts {
				if m.Source == victimAttachSrc {
					t.Fatalf("attachments bind escaped to the victim's directory: %+v", m)
				}
				if m.Target == wantTarget {
					t.Errorf("expected no attachments bind at all for non-canonical TaskID %q, got %+v", taskID, m)
				}
			}
		})
	}
}

func TestBuildEnvironmentYAML_HostCommandsSortedDeterministic(t *testing.T) {
	in := EnvironmentInput{
		HostCommands: map[string]orchestrator.CommandDef{
			"gh":  {Name: "gh", AllowedSubcommands: []string{"pr", "issue"}},
			"aws": {Name: "aws"},
		},
	}
	doc := parsedEnvDoc(t, in)
	hc, ok := doc["host_commands"].([]any)
	if !ok || len(hc) != 2 {
		t.Fatalf("host_commands = %v, want 2 entries", doc["host_commands"])
	}
	first := hc[0].(map[string]any)
	second := hc[1].(map[string]any)
	if first["name"] != "aws" || second["name"] != "gh" {
		t.Errorf("host_commands order = [%v, %v], want [aws, gh]", first["name"], second["name"])
	}
}

// TestBuildEnvironmentYAML_HostCommandsRejectSurfaced verifies that reject
// rules configured on a host command (match glob + reason) are surfaced in
// environment.yaml so the agent can read, per command, which arg shapes are
// rejected and what to do instead — without a --body-file trial-and-error
// round trip.
func TestBuildEnvironmentYAML_HostCommandsRejectSurfaced(t *testing.T) {
	in := EnvironmentInput{
		HostCommands: map[string]orchestrator.CommandDef{
			"gh": {
				Name:               "gh",
				AllowedSubcommands: []string{"pr", "issue"},
				RejectRules: []orchestrator.RejectRule{
					{Match: "*--body-file*", Reason: `sandbox paths are not visible on the host; use --body "$(cat <file>)"`},
				},
			},
			"aws": {Name: "aws"},
		},
	}
	doc := parsedEnvDoc(t, in)
	hc, ok := doc["host_commands"].([]any)
	if !ok || len(hc) != 2 {
		t.Fatalf("host_commands = %v, want 2 entries", doc["host_commands"])
	}
	// aws sorts first and has no reject rules configured.
	aws := hc[0].(map[string]any)
	if aws["name"] != "aws" {
		t.Fatalf("hc[0].name = %v, want aws", aws["name"])
	}
	if _, present := aws["reject"]; present {
		t.Errorf("aws host_command should omit reject when none configured, got %v", aws["reject"])
	}
	gh := hc[1].(map[string]any)
	if gh["name"] != "gh" {
		t.Fatalf("hc[1].name = %v, want gh", gh["name"])
	}
	reject, ok := gh["reject"].([]any)
	if !ok || len(reject) != 1 {
		t.Fatalf("gh.reject = %v, want 1 entry", gh["reject"])
	}
	rule := reject[0].(map[string]any)
	if rule["match"] != "*--body-file*" {
		t.Errorf("reject[0].match = %v, want *--body-file*", rule["match"])
	}
	if rule["reason"] == "" || rule["reason"] == nil {
		t.Errorf("reject[0].reason should be non-empty, got %v", rule["reason"])
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

// TestBuildSandboxSpec_HostCommandNamesEnv_MapsAliasedPathToDeclaredName is
// the codex review Minor fix 5a-2 closes
// (docs/plans/phase5-shim-and-task-context.md): BOID_HOST_COMMAND_NAMES must
// map the absolute bind-mount path (the same key hostCommandMounts binds the
// shim at — rt.ResolvedHostCommands, the byPath view) to the declared short
// name, even when host_commands.<name>.path aliases the shim to a file whose
// basename differs from name (run-e2e -> e2e/run.sh: the sandbox-visible
// file is "run.sh", not "run-e2e").
func TestBuildSandboxSpec_HostCommandNamesEnv_MapsAliasedPathToDeclaredName(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{
		ResolvedHostCommands: map[string]orchestrator.CommandDef{
			"/home/user/proj/e2e/run.sh": {Name: "run-e2e", Path: "/home/user/proj/e2e/run.sh"},
			"/usr/bin/gh":                {Name: "gh", Path: "/usr/bin/gh"},
		},
	}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	got := result.Env[sandbox.HostCommandNamesEnv]
	if got == "" {
		t.Fatalf("expected %s to be set, got empty", sandbox.HostCommandNamesEnv)
	}
	want := `{"/home/user/proj/e2e/run.sh":"run-e2e","/usr/bin/gh":"gh"}`
	if got != want {
		t.Errorf("%s = %q, want %q", sandbox.HostCommandNamesEnv, got, want)
	}
}

// TestBuildSandboxSpec_HostCommandNamesEnv_AbsentWhenNoHostCommands verifies
// the env var is not set when the job declares no host commands at all —
// mirrors TestBuildSandboxSpec_HostCommandRulesEnv_AbsentWhenNoHostCommands.
func TestBuildSandboxSpec_HostCommandNamesEnv_AbsentWhenNoHostCommands(t *testing.T) {
	spec := &orchestrator.JobSpec{}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}
	if _, ok := result.Env[sandbox.HostCommandNamesEnv]; ok {
		t.Errorf("expected %s to be absent, got %q", sandbox.HostCommandNamesEnv, result.Env[sandbox.HostCommandNamesEnv])
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

func TestHomeMounts_WorkspaceHomeDirSet_ReturnsBindPlusBoidTmpfs(t *testing.T) {
	const homeDir = "/home/user"
	const wsHome = "/data/boid/homes/default"
	mounts := homeMounts(homeDir, wsHome)
	if len(mounts) != 2 {
		t.Fatalf("homeMounts returned %d mounts, want 2: %+v", len(mounts), mounts)
	}
	if mounts[0].Source != wsHome || mounts[0].Target != homeDir || mounts[0].Type != sandbox.MountBind {
		t.Errorf("mounts[0] = %+v, want bind %s -> %s", mounts[0], wsHome, homeDir)
	}
	if mounts[0].ReadOnly {
		t.Error("workspace home bind must be read-write")
	}
	if mounts[1].Target != homeDir+"/.boid" || mounts[1].Type != sandbox.MountTmpfs {
		t.Errorf("mounts[1] = %+v, want tmpfs at %s/.boid", mounts[1], homeDir)
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
// PR2 mount order: workspace home bind immediately followed by the
// $HOME/.boid tmpfs overlay.
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
	tmpfsIdx := mountTargetIndex(out.Mounts, homeDir+"/.boid")
	if bindIdx == -1 {
		t.Fatalf("workspace home bind at %s not found: %+v", homeDir, out.Mounts)
	}
	if out.Mounts[bindIdx].Source != wsHome || out.Mounts[bindIdx].Type != sandbox.MountBind {
		t.Errorf("home mount = %+v, want bind from %s", out.Mounts[bindIdx], wsHome)
	}
	if tmpfsIdx != bindIdx+1 {
		t.Errorf(".boid tmpfs index = %d, want %d (immediately after home bind at %d); mounts=%+v", tmpfsIdx, bindIdx+1, bindIdx, out.Mounts)
	}
	if out.Mounts[tmpfsIdx].Type != sandbox.MountTmpfs {
		t.Errorf(".boid mount = %+v, want tmpfs", out.Mounts[tmpfsIdx])
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
// [bind effectiveDir, bind homeDir<-workspaceHomeDir, tmpfs homeDir/.boid,
// re-bind effectiveDir, peers..., .boid bind, .git ro re-bind].
func TestProjectVisibilityMounts_WorkspaceHomeBind_Order(t *testing.T) {
	const effectiveDir = "/home/user/project"
	const homeDir = "/home/user"
	const wsHome = "/data/boid/homes/default"
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, homeDir, wsHome, true, map[string]string{"peer": "/home/user/peer"})

	effIdx := mountTargetIndex(mounts, effectiveDir)
	homeIdx := mountTargetIndex(mounts, homeDir)
	boidTmpfsIdx := mountTargetIndex(mounts, homeDir+"/.boid")
	// Second occurrence of effectiveDir (the re-mount) — search after the
	// .boid tmpfs since the first occurrence (index 0) would otherwise match.
	remountIdx := -1
	for i := boidTmpfsIdx + 1; i < len(mounts); i++ {
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
	if boidTmpfsIdx != 2 {
		t.Errorf(".boid tmpfs index = %d, want 2 (immediately after home bind)", boidTmpfsIdx)
	}
	if remountIdx != 3 {
		t.Errorf("effectiveDir re-mount index = %d, want 3", remountIdx)
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
// visible" branch's PR2 mount order, mirroring the Clone-branch test above.
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
	tmpfsIdx := mountTargetIndex(result.Mounts, homeDir+"/.boid")
	if bindIdx == -1 {
		t.Fatalf("workspace home bind at %s not found: %+v", homeDir, result.Mounts)
	}
	if result.Mounts[bindIdx].Source != wsHome || result.Mounts[bindIdx].Type != sandbox.MountBind {
		t.Errorf("home mount = %+v, want bind from %s", result.Mounts[bindIdx], wsHome)
	}
	if tmpfsIdx != bindIdx+1 {
		t.Errorf(".boid tmpfs index = %d, want %d (immediately after home bind)", tmpfsIdx, bindIdx+1)
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

// TestBuildSandboxSpec_AttachmentsBind_OnTopOfWorkspaceHomeBind verifies the
// mount-chain concern flagged in docs/plans/home-workspace-volume.md PR2: the
// attachments bind (target $HOME/.boid/attachments) must still land correctly
// once $HOME/.boid is itself a job-scoped tmpfs stacked on a workspace home
// bind, not the old plain-tmpfs-HOME layout. applyMount (runner_linux.go)
// applies mounts in append order and mkdir's each target freshly, so a bind
// nested under an already-mounted tmpfs works the same way the pre-existing
// ProfileInit branch already relies on (tmpfs at $HOME/.boid, then the
// attachments bind on top) — this pins that the non-ProfileInit branches now
// get the same property.
func TestBuildSandboxSpec_AttachmentsBind_OnTopOfWorkspaceHomeBind(t *testing.T) {
	const taskID = "abc-123"
	root := t.TempDir()
	homeDir := hostHomeDir()
	spec := &orchestrator.JobSpec{TaskID: taskID}
	rt := SandboxRuntimeInfo{AttachmentsRoot: root, WorkspaceHomeDir: "/data/boid/homes/default"}

	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	boidTmpfsIdx := mountTargetIndex(result.Mounts, homeDir+"/.boid")
	attachIdx := mountTargetIndex(result.Mounts, homeDir+"/.boid/attachments")
	if boidTmpfsIdx == -1 {
		t.Fatalf(".boid tmpfs mount not found: %+v", result.Mounts)
	}
	if attachIdx == -1 {
		t.Fatalf("attachments bind not found: %+v", result.Mounts)
	}
	if attachIdx <= boidTmpfsIdx {
		t.Errorf("attachments bind index = %d, want after .boid tmpfs index %d", attachIdx, boidTmpfsIdx)
	}
	if result.Mounts[attachIdx].Source != root+"/tasks/"+taskID+"/attachments" {
		t.Errorf("attachments source = %q, want %q", result.Mounts[attachIdx].Source, root+"/tasks/"+taskID+"/attachments")
	}
}
