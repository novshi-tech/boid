package sandbox

import (
	"strings"
	"testing"
)

// Most plan tests want the project to be visible in the sandbox. This helper
// builds a config that enables MountProjectDir with sensible defaults.
func projectMountCfg() WrapperConfig {
	return WrapperConfig{
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
	}
}

func TestBuildSandboxPlan_SystemDirs(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	sysDirs := []string{"/bin", "/sbin", "/lib", "/lib64", "/usr", "/etc"}
	for _, d := range sysDirs {
		found := false
		for _, m := range plan.Mounts {
			if m.Source == d && m.Target == d {
				found = true
				if m.Type != MountRBind {
					t.Errorf("system dir %s: want type rbind, got %s", d, m.Type)
				}
				if !m.Slave {
					t.Errorf("system dir %s: want Slave=true", d)
				}
				if m.Guard == "" {
					t.Errorf("system dir %s: want Guard set", d)
				}
				break
			}
		}
		if !found {
			t.Errorf("system dir %s not found in plan", d)
		}
	}
}

func TestBuildSandboxPlan_EssentialFS(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	want := []struct {
		source, target string
		typ            MountType
	}{
		{"/dev", "/dev", MountRBind},
		{"/proc", "/proc", MountRBind},
		{"", "/tmp", MountTmpfs},
	}

	for _, w := range want {
		found := false
		for _, m := range plan.Mounts {
			if m.Target == w.target && m.Type == w.typ && m.Source == w.source {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("essential FS %s (type=%s) not found", w.target, w.typ)
		}
	}
}

func TestBuildSandboxPlan_DNS(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	if len(plan.Files) == 0 {
		t.Fatal("no files in plan")
	}
	f := plan.Files[0]
	if f.Path != "/run/systemd/resolve/stub-resolv.conf" {
		t.Errorf("DNS file path: got %q", f.Path)
	}
	if f.Content != "nameserver 10.0.2.3" {
		t.Errorf("DNS file content: got %q", f.Content)
	}
}

func TestBuildSandboxPlan_ProjectDir_MountProjectDir(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	found := false
	for _, m := range plan.Mounts {
		if m.Source == "/home/user/proj" && m.Target == "/home/user/proj" && m.Type == MountBind && !m.ReadOnly {
			found = true
			break
		}
	}
	if !found {
		t.Error("project dir rw mount not found")
	}

	foundHome := false
	for _, m := range plan.Mounts {
		if m.Target == "/home/user" && m.Type == MountTmpfs {
			foundHome = true
			break
		}
	}
	if !foundHome {
		t.Error("HOME tmpfs not found")
	}
}

func TestBuildSandboxPlan_ProjectReadOnly(t *testing.T) {
	cfg := projectMountCfg()
	cfg.ProjectReadOnly = true
	plan := BuildSandboxPlan(cfg)

	for _, m := range plan.Mounts {
		if m.Source == "/home/user/proj" && m.Target == "/home/user/proj" && m.Type == MountBind {
			if !m.ReadOnly {
				t.Error("project mount should be ReadOnly when ProjectReadOnly=true")
			}
		}
	}
}

func TestBuildSandboxPlan_ProjectRemount(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	homeIdx := -1
	for i, m := range plan.Mounts {
		if m.Target == "/home/user" && m.Type == MountTmpfs {
			homeIdx = i
			break
		}
	}
	if homeIdx < 0 {
		t.Fatal("HOME tmpfs not found")
	}

	if homeIdx+1 >= len(plan.Mounts) {
		t.Fatal("no mount after HOME tmpfs")
	}
	remount := plan.Mounts[homeIdx+1]
	if remount.Source != "/home/user/proj" || remount.Target != "/home/user/proj" || remount.Type != MountBind {
		t.Errorf("expected project re-mount after HOME tmpfs, got %+v", remount)
	}
}

func TestBuildSandboxPlan_BoidDir_HookFiles(t *testing.T) {
	cfg := projectMountCfg()
	cfg.HookFiles = []HookFile{
		{Source: "/kits/claude-code/hooks/run.sh", TargetName: "claude-code--run.sh"},
		{Source: "/home/user/proj/.boid/hooks/local.sh", TargetName: "local.sh"},
	}
	plan := BuildSandboxPlan(cfg)

	boidDir := "/home/user/proj/.boid"
	hooksDir := "/home/user/proj/.boid/hooks"

	var boidMount *MountEntry
	var hooksTmpfs *MountEntry
	var fileMount1, fileMount2 *MountEntry
	for i := range plan.Mounts {
		m := &plan.Mounts[i]
		if m.Target == boidDir && m.Type == MountBind {
			boidMount = m
		}
		if m.Target == hooksDir && m.Type == MountTmpfs {
			hooksTmpfs = m
		}
		if m.Target == hooksDir+"/claude-code--run.sh" {
			fileMount1 = m
		}
		if m.Target == hooksDir+"/local.sh" {
			fileMount2 = m
		}
	}

	if boidMount == nil {
		t.Fatal(".boid mount not found")
	}
	if !boidMount.ReadOnly {
		t.Error(".boid mount should be ReadOnly")
	}
	if boidMount.Guard == "" {
		t.Error(".boid mount should have Guard")
	}
	if len(boidMount.NeedsDirs) != 1 || boidMount.NeedsDirs[0] != "hooks" {
		t.Errorf(".boid mount NeedsDirs: got %v, want [hooks]", boidMount.NeedsDirs)
	}

	if hooksTmpfs == nil {
		t.Fatal("hooks tmpfs mount not found")
	}
	if fileMount1 == nil || fileMount2 == nil {
		t.Fatal("hook file mounts not found")
	}
	if !fileMount1.IsFile || !fileMount1.ReadOnly {
		t.Error("hook file mount should be IsFile + ReadOnly")
	}
}

func TestBuildSandboxPlan_BoidDir_NoHookFiles(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	boidDir := "/home/user/proj/.boid"
	hooksDir := "/home/user/proj/.boid/hooks"

	var boidMount *MountEntry
	for i := range plan.Mounts {
		if plan.Mounts[i].Target == boidDir {
			boidMount = &plan.Mounts[i]
		}
		if plan.Mounts[i].Target == hooksDir {
			t.Error("hooks tmpfs mount should not exist when HookFiles is empty")
		}
	}

	if boidMount == nil {
		t.Fatal(".boid mount not found")
	}
	if len(boidMount.NeedsDirs) != 0 {
		t.Errorf("NeedsDirs should be empty when HookFiles is empty, got %v", boidMount.NeedsDirs)
	}
}

func TestBuildSandboxPlan_Proxy(t *testing.T) {
	cfg := projectMountCfg()
	cfg.ProxyPort = 8888
	plan := BuildSandboxPlan(cfg)

	if len(plan.NFTRules) == 0 {
		t.Fatal("NFTRules should be non-empty when ProxyPort > 0")
	}

	foundDrop := false
	for _, r := range plan.NFTRules {
		if strings.Contains(r, "policy drop") {
			foundDrop = true
		}
	}
	if !foundDrop {
		t.Error("NFTRules missing drop policy")
	}
}

func TestBuildSandboxPlan_NoProxy(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	if len(plan.NFTRules) != 0 {
		t.Error("NFTRules should be empty when ProxyPort is 0")
	}
}

func TestBuildSandboxPlan_WorkspaceDirs(t *testing.T) {
	cfg := projectMountCfg()
	cfg.WorkspaceDirs = map[string]string{"peer": "/home/user/peer"}
	plan := BuildSandboxPlan(cfg)

	found := false
	for _, m := range plan.Mounts {
		if m.Source == "/home/user/peer" && m.Target == "/home/user/peer" {
			found = true
			if !m.ReadOnly {
				t.Error("workspace mount should be ReadOnly")
			}
			if m.Type != MountBind {
				t.Errorf("workspace mount type: got %s, want bind", m.Type)
			}
		}
	}
	if !found {
		t.Error("workspace mount not found")
	}
}

func TestBuildSandboxPlan_AdditionalBindings(t *testing.T) {
	cfg := projectMountCfg()
	cfg.AdditionalBindings = []BindMount{
		{Source: "/home/user/.local/bin"},
		{Source: "/home/user/go", Mode: "rw"},
	}
	plan := BuildSandboxPlan(cfg)

	want := []struct {
		source string
		ro     bool
	}{
		{"/home/user/.local/bin", true},
		{"/home/user/go", false},
	}
	for _, w := range want {
		found := false
		for _, m := range plan.Mounts {
			if m.Source == w.source && m.Target == w.source {
				found = true
				if m.ReadOnly != w.ro {
					t.Errorf("binding %s: ReadOnly=%v, want %v", w.source, m.ReadOnly, w.ro)
				}
			}
		}
		if !found {
			t.Errorf("additional binding %s not found", w.source)
		}
	}
}

func TestBuildSandboxPlan_AdditionalBindings_WithTarget(t *testing.T) {
	cfg := projectMountCfg()
	cfg.AdditionalBindings = []BindMount{
		{Source: "/host/broker.sock", Target: "/run/boid/broker.sock", IsFile: true},
	}
	plan := BuildSandboxPlan(cfg)

	found := false
	for _, m := range plan.Mounts {
		if m.Source == "/host/broker.sock" && m.Target == "/run/boid/broker.sock" {
			found = true
			if !m.IsFile {
				t.Error("broker socket mount should be IsFile")
			}
			if m.DetectType {
				t.Error("IsFile mount should not use DetectType")
			}
		}
	}
	if !found {
		t.Error("additional binding with Target not applied")
	}
}

func TestBuildSandboxPlan_BoidBinary(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		HostCommands:    []string{"git", "gh"},
	}
	plan := BuildSandboxPlan(cfg)

	// boid binary is bind-mounted read-only at /opt/boid/bin/boid
	var boidMount *MountEntry
	for i := range plan.Mounts {
		if plan.Mounts[i].Target == "/opt/boid/bin/boid" {
			boidMount = &plan.Mounts[i]
			break
		}
	}
	if boidMount == nil {
		t.Fatal("boid binary mount not found")
	}
	if boidMount.Source != "/usr/local/bin/boid" {
		t.Errorf("boid mount source: got %q, want /usr/local/bin/boid", boidMount.Source)
	}
	if !boidMount.IsFile || !boidMount.ReadOnly || boidMount.Type != MountBind {
		t.Errorf("boid mount: got %+v, want bind+file+readonly", *boidMount)
	}

	if len(plan.Symlinks) != 2 {
		t.Fatalf("symlinks: got %d, want 2", len(plan.Symlinks))
	}
	symTargets := map[string]bool{}
	for _, s := range plan.Symlinks {
		symTargets[s.LinkPath] = true
		if s.LinkTarget != "boid" {
			t.Errorf("symlink target: got %q, want boid", s.LinkTarget)
		}
	}
	if !symTargets["/opt/boid/bin/git"] {
		t.Error("missing git shim symlink")
	}
	if !symTargets["/opt/boid/bin/gh"] {
		t.Error("missing gh shim symlink")
	}
}

func TestBuildSandboxPlan_BoidDoesNotDuplicateShim(t *testing.T) {
	cfg := projectMountCfg()
	cfg.BuiltinCommands = []string{"boid", "git"}
	cfg.HostCommands = []string{"boid", "git"}
	plan := BuildSandboxPlan(cfg)

	if len(plan.Symlinks) != 1 {
		t.Fatalf("symlinks: got %d, want 1", len(plan.Symlinks))
	}
	symlink := plan.Symlinks[0]
	if symlink.LinkPath != "/opt/boid/bin/git" || symlink.LinkTarget != "boid" {
		t.Fatalf("unexpected symlink: %+v", symlink)
	}
}

func TestBuildSandboxPlan_CleanupPaths(t *testing.T) {
	cfg := projectMountCfg()
	cfg.StagingDir = "/tmp/staging-123"
	plan := BuildSandboxPlan(cfg)

	if len(plan.CleanupPaths) != 1 || plan.CleanupPaths[0] != "/tmp/staging-123" {
		t.Errorf("CleanupPaths: got %v", plan.CleanupPaths)
	}
}

func TestBuildSandboxPlan_Worktree(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir: "/home/user/proj",
		HomeDir:    "/home/user",
		HookFiles: []HookFile{
			{Source: "/kits/claude-code/hooks/run.sh", TargetName: "claude-code--run.sh"},
		},
		BoidBinary:      "/usr/local/bin/boid",
		WorktreeDir:     "/home/user/.local/share/boid/worktrees/proj/task-abc",
		MountProjectDir: true,
	}
	plan := BuildSandboxPlan(cfg)

	wt := "/home/user/.local/share/boid/worktrees/proj/task-abc"
	origProj := "/home/user/proj"

	foundWT := false
	for _, m := range plan.Mounts {
		if m.Source == wt && m.Target == wt && m.Type == MountBind && !m.ReadOnly {
			foundWT = true
		}
	}
	if !foundWT {
		t.Error("worktree dir rw mount not found")
	}

	for _, m := range plan.Mounts {
		if m.Source == origProj && m.Target == origProj {
			t.Error("original project dir should not be directly mounted in worktree mode")
		}
	}

	gitDir := origProj + "/.git"
	foundGit := false
	for _, m := range plan.Mounts {
		if m.Source == gitDir && m.Target == gitDir && m.Type == MountBind && !m.ReadOnly {
			foundGit = true
		}
	}
	if !foundGit {
		t.Error(".git rw mount not found")
	}

	boidSource := origProj + "/.boid"
	boidTarget := wt + "/.boid"
	foundBoid := false
	for _, m := range plan.Mounts {
		if m.Source == boidSource && m.Target == boidTarget && m.ReadOnly {
			foundBoid = true
		}
	}
	if !foundBoid {
		t.Errorf(".boid mount not found: want source=%s target=%s ro", boidSource, boidTarget)
	}

	hooksTarget := wt + "/.boid/hooks"
	foundHooksTmpfs := false
	foundHookFile := false
	for _, m := range plan.Mounts {
		if m.Target == hooksTarget && m.Type == MountTmpfs {
			foundHooksTmpfs = true
		}
		if m.Source == "/kits/claude-code/hooks/run.sh" && m.Target == hooksTarget+"/claude-code--run.sh" && m.IsFile && m.ReadOnly {
			foundHookFile = true
		}
	}
	if !foundHooksTmpfs {
		t.Errorf("hooks tmpfs not found at %s", hooksTarget)
	}
	if !foundHookFile {
		t.Errorf("hook file mount not found at %s/claude-code--run.sh", hooksTarget)
	}
}

func TestBuildSandboxPlan_NoProjectMount_WorkDirTmpfs(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/tmp",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: false,
	}
	plan := BuildSandboxPlan(cfg)

	// Project dir must NOT be bind-mounted when MountProjectDir=false
	for _, m := range plan.Mounts {
		if m.Source == "/home/user/proj" {
			t.Errorf("project dir should not be bind-mounted when MountProjectDir=false, got %+v", m)
		}
	}

	// Empty tmpfs at WorkDir so `cd` succeeds
	found := false
	for _, m := range plan.Mounts {
		if m.Target == "/home/user/proj" && m.Type == MountTmpfs && m.Source == "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tmpfs mount at workDir not found when MountProjectDir=false")
	}
}

func TestBuildSandboxPlan_NoWorktree_NoGitMount(t *testing.T) {
	cfg := projectMountCfg()
	plan := BuildSandboxPlan(cfg)

	for _, m := range plan.Mounts {
		if m.Target == "/home/user/proj/.git" {
			t.Error(".git should not be explicitly mounted in non-worktree mode")
		}
	}
}
