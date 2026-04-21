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
