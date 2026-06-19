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

// behavior は canonical 名 (executor/supervisor) で BOID_INVOKED_BEHAVIOR に渡す。
// run-agent.py はこれを見て /boid-executor / /boid-supervisor を直接起動プロンプトに
// 選ぶ。deprecated 別名 (dev/plan) は canonical 化してから渡すので、 agent ランナーは
// 別名を知らなくてよい。 旧 BOID_INVOKED_TYPE は instruction の phase 種別 (常に
// "execution") を運んでいて behavior と取り違えられていたため廃止する。
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

// KitRoots in Visibility are bound at their original host paths inside the sandbox.
func TestBuildSandboxSpec_KitRootsAreBound(t *testing.T) {
	const kitRoot = "/home/user/.local/share/boid/kits/git-auto-merge"
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			KitRoots: []string{kitRoot},
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	var found *sandbox.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Target == kitRoot {
			found = &result.Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("kit root mount not found: target=%q not in mounts", kitRoot)
	}
	if found.Source != kitRoot {
		t.Errorf("mount Source = %q, want %q", found.Source, kitRoot)
	}
	if !found.ReadOnly {
		t.Error("kit root mount must be ReadOnly")
	}
	if found.Type != sandbox.MountBind {
		t.Errorf("mount Type = %v, want MountBind", found.Type)
	}
}

// Regression: shell adapter (Bindings()=nil なのに HarnessType="shell" は
// 非空) でも KitRoots + AdditionalBindings が legacy 経路で mount される。
// 真因は sandbox_builder.go の分岐が `spec.HarnessType != ""` 単独だった
// こと ── shell adapter は HarnessType non-empty かつ Bindings=nil なので
// kit binding が「adapter 側で nil 上書き」 されて消え、 hook script が
// sandbox 内で見えなくなって exit 143 で死亡していた (PR #594 builtin-
// task-create 退行)。 修正後は `len(harnessBindings) > 0` を条件に切替。
func TestBuildSandboxSpec_ShellHarnessKeepsKitRoots(t *testing.T) {
	const kitRoot = "/home/user/.local/share/boid/kits/builtin-task-create"
	spec := &orchestrator.JobSpec{
		HarnessType: "shell",
		Visibility: orchestrator.Visibility{
			KitRoots: []string{kitRoot},
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	var found *sandbox.Mount
	for i := range result.Mounts {
		if result.Mounts[i].Target == kitRoot {
			found = &result.Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("shell harness must still mount KitRoots: target=%q not in mounts", kitRoot)
	}
	if found.Source != kitRoot {
		t.Errorf("mount Source = %q, want %q", found.Source, kitRoot)
	}
}

// Kit root parent directory must NOT appear as a mount target (security boundary).
func TestBuildSandboxSpec_KitRootParentNotBound(t *testing.T) {
	const kitRoot = "/home/user/.local/share/boid/kits/git-auto-merge"
	const kitParent = "/home/user/.local/share/boid/kits"
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			KitRoots: []string{kitRoot},
		},
	}
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	for _, m := range result.Mounts {
		if m.Target == kitParent {
			t.Errorf("kit root parent directory must not be mounted: found mount with target=%q", kitParent)
		}
	}
}

// /usr/bin/git と /bin/git が boid バイナリ bind で上書きされることを検証する。
// これにより絶対パスで実体 git を呼び出す迂回が防止される。
// boid バイナリ自身はホスト実パスのまま bind mount される（/opt/boid/bin/boid は廃止）。
func TestBuildSandboxSpec_GitShimBindMounts(t *testing.T) {
	const boidBin = "/usr/local/bin/boid"
	spec := &orchestrator.JobSpec{}
	rt := SandboxRuntimeInfo{BoidBinary: boidBin}
	result, err := BuildSandboxSpec(spec, rt)
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	var boidMount, usrBinGit, binGit *sandbox.Mount
	for i := range result.Mounts {
		m := &result.Mounts[i]
		switch m.Target {
		case boidBin:
			boidMount = m
		case "/usr/bin/git":
			usrBinGit = m
		case "/bin/git":
			binGit = m
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
	resolved, err := ResolveHostCommands([]string{"boid", "git"}, hostCmds, "", fakeLookPath)
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
	_, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath)
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
	resolved, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath)
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
	resolved, err := ResolveHostCommands([]string{"gh"}, hostCmds, "", fakeLookPath)
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
	resolved, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath)
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
	resolved, err := ResolveHostCommands(nil, hostCmds, projectDir, func(string) (string, error) {
		return "", fmt.Errorf("lookPath should not be called")
	})
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
	resolved, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath)
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
	_, err := ResolveHostCommands(nil, hostCmds, "", fakeLookPath)
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
	resolved, err := ResolveHostCommands([]string{"jq"}, hostCmds, "", fakeLookPath)
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
			wantPrefix: "/home/user/go/bin:",
		},
		{
			name:       "tmp e2e location",
			boidBinary: "/tmp/boid-e2e-test-ABCDEF/bin/boid",
			wantPrefix: "/tmp/boid-e2e-test-ABCDEF/bin:",
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
		want := "/usr/local/bin:/usr/bin:/bin"
		if path != want {
			t.Errorf("buildPATH(%q) = %q, want %q", boidBinary, path, want)
		}
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
	want := "/usr/local/bin:/usr/bin:/bin"
	if path != want {
		t.Errorf("buildPATH = %q, want %q", path, want)
	}
}

// 同一ディレクトリに複数の host command があってもディレクトリは一度だけ追加。
func TestBuildPATH_HostCommandDirDeduplicated(t *testing.T) {
	hostCommands := map[string]orchestrator.CommandDef{
		"/home/user/.local/bin/a": {Name: "a", Path: "/home/user/.local/bin/a"},
		"/home/user/.local/bin/b": {Name: "b", Path: "/home/user/.local/bin/b"},
	}
	path := buildPATH(nil, hostCommands, "/usr/local/bin/boid")
	want := "/home/user/.local/bin:/usr/local/bin:/usr/bin:/bin"
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
// - worktree=true: worktree path に bind される
// - worktree=false: skip される (self-mount 回避)
// という End-to-End 挙動を BuildSandboxSpec 越しに検証する。
func TestBuildSandboxSpec_WorktreeBindingExpansion(t *testing.T) {
	const projectDir = "/host/proj"
	const worktreeDir = "/runtime/worktrees/proj/task1"
	binding := orchestrator.BindMount{
		Source: "${PROJECT_WORKDIR}/global.json",
		Target: "${WORKTREE}/global.json",
		IsFile: true,
	}

	// worktree=true: src と tgt が別 path に展開され bind される
	specWT := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			ProjectDir:         projectDir,
			UseWorktree:        true,
			AdditionalBindings: []orchestrator.BindMount{binding},
		},
	}
	rtWT := SandboxRuntimeInfo{WorktreeDir: worktreeDir}
	resWT, err := BuildSandboxSpec(specWT, rtWT)
	if err != nil {
		t.Fatalf("BuildSandboxSpec(worktree=true): %v", err)
	}
	var found bool
	for _, m := range resWT.Mounts {
		if m.Source == "/host/proj/global.json" && m.Target == "/runtime/worktrees/proj/task1/global.json" {
			found = true
			if !m.IsFile {
				t.Error("expected IsFile=true for global.json bind")
			}
			break
		}
	}
	if !found {
		t.Errorf("worktree=true: expected bind from %s to %s, got mounts:\n%+v",
			"/host/proj/global.json", "/runtime/worktrees/proj/task1/global.json", resWT.Mounts)
	}

	// worktree=false: src と tgt が同じ path に潰れ、 explicit-self-mount として skip される
	specNoWT := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{
			ProjectDir:         projectDir,
			UseWorktree:        false,
			AdditionalBindings: []orchestrator.BindMount{binding},
		},
	}
	resNoWT, err := BuildSandboxSpec(specNoWT, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec(worktree=false): %v", err)
	}
	for _, m := range resNoWT.Mounts {
		if m.Source == "/host/proj/global.json" && m.IsFile {
			t.Errorf("worktree=false: self-mount should be skipped, got mount %+v", m)
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
		Role:  "main",
		Agent: "claude-code",
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

// TestSandboxRuntimeInfo_DockerEnabled verifies that SandboxRuntimeInfo carries
// a DockerEnabled field that can be set from Visibility.DockerEnabled, and that
// BuildSandboxSpec accepts it without error. Phase 2 PR will wire this to
// actually mount the docker proxy socket.
func TestSandboxRuntimeInfo_DockerEnabled(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{DockerEnabled: true},
	}
	rtInfo := SandboxRuntimeInfo{DockerEnabled: true}
	if _, err := BuildSandboxSpec(spec, rtInfo); err != nil {
		t.Fatalf("BuildSandboxSpec with DockerEnabled=true: %v", err)
	}
	rtInfoOff := SandboxRuntimeInfo{DockerEnabled: false}
	if _, err := BuildSandboxSpec(spec, rtInfoOff); err != nil {
		t.Fatalf("BuildSandboxSpec with DockerEnabled=false: %v", err)
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
		DockerEnabled:   true,
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
// is mounted — even when DockerEnabled is true (proxy failed to start upstream).
func TestBuildSandboxSpec_DockerProxy_DisabledWhenNoSocketPath(t *testing.T) {
	spec := &orchestrator.JobSpec{
		Visibility: orchestrator.Visibility{DockerEnabled: true},
	}
	rt := SandboxRuntimeInfo{DockerEnabled: true, ProxySocketPath: ""}
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

