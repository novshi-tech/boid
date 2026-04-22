package dispatcher

import (
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestStageArgv0_BareCommandLeftUntouched(t *testing.T) {
	target, mount, ok := stageArgv0("claude", "")
	if ok {
		t.Errorf("bare command should not be staged, got target=%q mount=%v", target, mount)
	}
}

func TestStageArgv0_UnderProjectRootLeftUntouched(t *testing.T) {
	target, mount, ok := stageArgv0("/host/proj/bin/run.sh", "/host/proj")
	if ok {
		t.Errorf("project-local argv[0] should not be staged, target=%q mount=%v", target, mount)
	}
}

func TestStageArgv0_ExternalAbsolutePath_BindsParentDirectory(t *testing.T) {
	const entry = "/tmp/boid-hooks-abc/claude-code--run-agent.py"

	target, mount, ok := stageArgv0(entry, "/host/proj")
	if !ok {
		t.Fatal("expected ok=true for external absolute argv[0]")
	}
	if target != "/opt/boid/entry/claude-code--run-agent.py" {
		t.Errorf("target = %q, want /opt/boid/entry/claude-code--run-agent.py", target)
	}
	if mount == nil {
		t.Fatal("expected a mount for external argv[0]")
	}
	want := sandbox.Mount{
		Source:   "/tmp/boid-hooks-abc",
		Target:   "/opt/boid/entry",
		Type:     sandbox.MountBind,
		ReadOnly: true,
	}
	if !reflect.DeepEqual(*mount, want) {
		t.Errorf("mount = %+v, want %+v", *mount, want)
	}
}

// /usr/bin/git と /bin/git が boid バイナリ bind で上書きされることを検証する。
// これにより絶対パスで実体 git を呼び出す迂回が防止される。
func TestBuildSandboxSpec_GitShimBindMounts(t *testing.T) {
	const boidBin = "/usr/local/bin/boid"
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{BoidBinary: boidBin}
	result := BuildSandboxSpec(spec, rt)

	var usrBinGit, binGit *sandbox.Mount
	for i := range result.Mounts {
		m := &result.Mounts[i]
		switch m.Target {
		case "/usr/bin/git":
			usrBinGit = m
		case "/bin/git":
			binGit = m
		}
	}

	if usrBinGit == nil {
		t.Fatal("/usr/bin/git mount not found in Spec.Mounts")
	}
	if usrBinGit.Source != boidBin {
		t.Errorf("/usr/bin/git source = %q, want %q", usrBinGit.Source, boidBin)
	}
	if !usrBinGit.ReadOnly {
		t.Error("/usr/bin/git mount should be ReadOnly")
	}
	if !usrBinGit.IsFile {
		t.Error("/usr/bin/git mount should have IsFile=true")
	}

	if binGit == nil {
		t.Fatal("/bin/git mount not found in Spec.Mounts")
	}
	if binGit.Source != boidBin {
		t.Errorf("/bin/git source = %q, want %q", binGit.Source, boidBin)
	}
	if binGit.Guard == "" {
		t.Error("/bin/git mount must have a Guard (conditional on host /bin/git existence)")
	}

	// BoidBinary 未設定時はオーバーライド mount が存在しないことを確認。
	noGit := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	for _, m := range noGit.Mounts {
		if m.Target == "/usr/bin/git" || m.Target == "/bin/git" {
			t.Errorf("unexpected git override mount when BoidBinary is empty: target=%q", m.Target)
		}
	}
}

// writable worktree では .git が ro 再 bind されることを確認する。
// これにより sandbox 内プロセスが .git/config 等を直接書き換えられない。
func TestProjectVisibilityMounts_GitROBind_Writable(t *testing.T) {
	const effectiveDir = "/home/user/project"
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, "/home/user", true, nil, false)

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
	mounts := projectVisibilityMounts(effectiveDir, effectiveDir, "/home/user", false, nil, false)

	for _, m := range mounts {
		if m.Target == effectiveDir+"/.git" && m.ReadOnly && m.DetectType {
			t.Error(".git ro re-bind should not be added for read-only project")
		}
	}
}

// worktree モードでは origProjectDir/.git も ro で bind される。
func TestProjectVisibilityMounts_WorktreeMode_OrigGitReadOnly(t *testing.T) {
	const origProject = "/home/user/project"
	const worktreeDir = "/home/user/worktrees/task1"
	mounts := projectVisibilityMounts(origProject, worktreeDir, "/home/user", true, nil, true)

	var origGitMount *sandbox.Mount
	for i := range mounts {
		if mounts[i].Target == origProject+"/.git" {
			origGitMount = &mounts[i]
			break
		}
	}
	if origGitMount == nil {
		t.Fatal("origProjectDir/.git mount not found in worktree mounts")
	}
	if !origGitMount.ReadOnly {
		t.Error("origProjectDir/.git mount must be ReadOnly in worktree mode")
	}
}

// Mounting the parent directory (rather than the single entry file) is what
// lets hook runners like claude-code/run-agent.py find their sibling helper
// scripts (e.g. format-stream.py) inside the sandbox.
func TestStageArgv0_SiblingHelpersAreReachable(t *testing.T) {
	target, mount, ok := stageArgv0("/tmp/boid-hooks-abc/claude-code--run-agent.py", "")
	if !ok || mount == nil {
		t.Fatal("expected external argv[0] to be staged with a mount")
	}
	if mount.IsFile {
		t.Error("parent-directory bind must not set IsFile=true")
	}
	if mount.Source != "/tmp/boid-hooks-abc" || mount.Target != "/opt/boid/entry" {
		t.Errorf("mount = %+v, want parent dir bound at /opt/boid/entry", *mount)
	}
	if target != "/opt/boid/entry/claude-code--run-agent.py" {
		t.Errorf("target = %q, want /opt/boid/entry/claude-code--run-agent.py", target)
	}
}

// BuiltinPolicies に git が含まれていても /opt/boid/bin/git symlink は生成されない。
// /usr/bin/git と /bin/git は boid バイナリの bind mount で上書き済みなので不要。
func TestShimSymlinks_GitExcluded(t *testing.T) {
	builtins := []string{"boid", "git"}
	hostCmds := []string{"gh", "git"}
	symlinks := shimSymlinks(builtins, hostCmds)

	for _, sl := range symlinks {
		if sl.LinkPath == "/opt/boid/bin/git" {
			t.Errorf("git symlink must not be generated, got %+v", sl)
		}
	}
	// gh は生成される。
	var hasGh bool
	for _, sl := range symlinks {
		if sl.LinkPath == "/opt/boid/bin/gh" {
			hasGh = true
		}
	}
	if !hasGh {
		t.Error("gh symlink must be generated")
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

// contextFiles must materialize payload.yaml / payload.json for every hook
// that carries PrimaryInput, regardless of the Instruction.Interactive flag.
// Regression: once the condition was `inst.Interactive && len(PrimaryInput)>0`
// which silently stripped payload from non-interactive agents such as the
// rework hook — leaving agents blind to verification findings. See task
// 2219755f post-mortem.
func TestContextFiles_PayloadWrittenForNonInteractiveHook(t *testing.T) {
	inst := &orchestrator.RoutedInstruction{
		Role:        "rework",
		Type:        "rework",
		Consumer:    "claude-code",
		Message:     "verification findings に記載された問題を修正せよ。",
		Interactive: false,
	}
	primary := []byte(`{"verification":{"findings":[{"status":"open","message":"failure"}]}}`)

	files := contextFiles(
		"/home/agent",
		nil,
		inst,
		primary,
		orchestrator.Visibility{},
		nil,
		nil,
		false,
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
		Role:        "main",
		Type:        "execution",
		Consumer:    "claude-code",
		Interactive: true,
	}
	primary := []byte(`{"artifact":null}`)

	files := contextFiles(
		"/home/agent",
		nil,
		inst,
		primary,
		orchestrator.Visibility{},
		nil,
		nil,
		false,
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

func TestContextFiles_NoPayloadFilesWhenPrimaryInputEmpty(t *testing.T) {
	inst := &orchestrator.RoutedInstruction{
		Role:        "main",
		Type:        "execution",
		Consumer:    "claude-code",
		Interactive: true,
	}

	files := contextFiles(
		"/home/agent",
		nil,
		inst,
		nil,
		orchestrator.Visibility{},
		nil,
		nil,
		false,
	)

	for _, f := range files {
		if f.Path == "/home/agent/.boid/context/payload.json" ||
			f.Path == "/home/agent/.boid/context/payload.yaml" {
			t.Errorf("unexpected payload file written with empty PrimaryInput: %s", f.Path)
		}
	}
}
