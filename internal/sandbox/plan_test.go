package sandbox

import (
	"testing"
)

func TestBuildSandboxPlan_SystemDirs(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
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
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
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
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
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

func TestBuildSandboxPlan_ProjectDir(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		HomeDir:      "/home/user",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
	plan := BuildSandboxPlan(cfg)

	// Project dir should appear as rw bind mount
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

	// HOME should appear as tmpfs
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

func TestBuildSandboxPlan_ProjectRemount(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		HomeDir:      "/home/user",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
	plan := BuildSandboxPlan(cfg)

	// After HOME tmpfs, project dir must be re-mounted.
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

func TestBuildSandboxPlan_BoidDir_HookMode(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir: "/home/user/proj",
		HookFiles: []HookFile{
			{Source: "/kits/claude-code/hooks/run.sh", TargetName: "claude-code--run.sh"},
			{Source: "/home/user/proj/.boid/hooks/local.sh", TargetName: "local.sh"},
		},
		HookScript:   "run.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
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
	if hooksTmpfs.Guard == "" {
		t.Error("hooks tmpfs mount should have Guard")
	}

	if fileMount1 == nil {
		t.Fatal("claude-code--run.sh file mount not found")
	}
	if fileMount1.Source != "/kits/claude-code/hooks/run.sh" {
		t.Errorf("file mount source: got %q", fileMount1.Source)
	}
	if !fileMount1.ReadOnly {
		t.Error("file mount should be ReadOnly")
	}
	if !fileMount1.IsFile {
		t.Error("file mount should be IsFile")
	}
	if fileMount1.Guard == "" {
		t.Error("file mount should have Guard")
	}

	if fileMount2 == nil {
		t.Fatal("local.sh file mount not found")
	}
	if fileMount2.Source != "/home/user/proj/.boid/hooks/local.sh" {
		t.Errorf("file mount source: got %q", fileMount2.Source)
	}
}

func TestBuildSandboxPlan_BoidDir_CommandMode(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		Argv:         []string{"/bin/bash"},
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
	plan := BuildSandboxPlan(cfg)

	boidDir := "/home/user/proj/.boid"
	hooksDir := "/home/user/proj/.boid/hooks"

	var boidMount *MountEntry
	for i := range plan.Mounts {
		if plan.Mounts[i].Target == boidDir {
			boidMount = &plan.Mounts[i]
		}
		if plan.Mounts[i].Target == hooksDir {
			t.Error("hooks mount should not exist in command mode")
		}
	}

	if boidMount == nil {
		t.Fatal(".boid mount not found in command mode")
	}
	if len(boidMount.NeedsDirs) != 0 {
		t.Errorf("NeedsDirs should be empty in command mode, got %v", boidMount.NeedsDirs)
	}
}

func TestBuildSandboxPlan_Proxy(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		ProxyPort:    8888,
	}
	plan := BuildSandboxPlan(cfg)

	if len(plan.NFTRules) == 0 {
		t.Fatal("NFTRules should be non-empty when ProxyPort > 0")
	}

	foundDrop := false
	for _, r := range plan.NFTRules {
		if contains(r, "policy drop") {
			foundDrop = true
		}
	}
	if !foundDrop {
		t.Error("NFTRules missing drop policy")
	}
}

func TestBuildSandboxPlan_NoProxy(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		ProxyPort:    0,
	}
	plan := BuildSandboxPlan(cfg)

	if len(plan.NFTRules) != 0 {
		t.Error("NFTRules should be empty when ProxyPort is 0")
	}
}

func TestBuildSandboxPlan_WorkspaceDirs(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		WorkspaceDirs: map[string]string{
			"peer": "/home/user/peer",
		},
	}
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
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		AdditionalBindings: []BindMount{
			{Source: "/home/user/.local/bin"},
			{Source: "/home/user/go", Mode: "rw"},
		},
	}
	plan := BuildSandboxPlan(cfg)

	for _, want := range []struct {
		source   string
		readOnly bool
		detect   bool
	}{
		{"/home/user/.local/bin", true, true},
		{"/home/user/go", false, true},
	} {
		found := false
		for _, m := range plan.Mounts {
			if m.Source == want.source && m.Target == want.source {
				found = true
				if m.ReadOnly != want.readOnly {
					t.Errorf("binding %s: ReadOnly got %v, want %v", want.source, m.ReadOnly, want.readOnly)
				}
				if m.DetectType != want.detect {
					t.Errorf("binding %s: DetectType got %v, want %v", want.source, m.DetectType, want.detect)
				}
			}
		}
		if !found {
			t.Errorf("additional binding %s not found", want.source)
		}
	}
}

func TestBuildSandboxPlan_BoidBinary(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		HostCommands: []string{"git", "gh"},
	}
	plan := BuildSandboxPlan(cfg)

	if len(plan.Copies) != 1 {
		t.Fatalf("copies: got %d, want 1", len(plan.Copies))
	}
	c := plan.Copies[0]
	if c.Source != "/usr/local/bin/boid" || c.Target != "/opt/boid/bin/boid" || !c.Executable {
		t.Errorf("boid copy: got %+v", c)
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

func TestBuildSandboxPlan_BuiltinCommands(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:      "/home/user/proj",
		BoidBinary:      "/usr/local/bin/boid",
		BuiltinCommands: []string{"boid", "git"},
	}
	plan := BuildSandboxPlan(cfg)

	if len(plan.Symlinks) != 1 {
		t.Fatalf("symlinks: got %d, want 1", len(plan.Symlinks))
	}
	symlink := plan.Symlinks[0]
	if symlink.LinkPath != "/opt/boid/bin/git" || symlink.LinkTarget != "boid" {
		t.Fatalf("unexpected symlink: %+v", symlink)
	}
}

func TestBuildSandboxPlan_Sockets(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
	}
	plan := BuildSandboxPlan(cfg)

	foundServer := false
	foundBroker := false
	for _, m := range plan.Mounts {
		if m.Target == "/run/boid/server.sock" {
			foundServer = true
			if !m.IsFile {
				t.Error("server socket should be IsFile")
			}
		}
		if m.Target == "/run/boid/broker.sock" {
			foundBroker = true
			if !m.IsFile {
				t.Error("broker socket should be IsFile")
			}
		}
	}
	if !foundServer {
		t.Error("server socket mount not found")
	}
	if !foundBroker {
		t.Error("broker socket mount not found")
	}
}

func TestBuildSandboxPlan_NoBroker(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
	plan := BuildSandboxPlan(cfg)

	for _, m := range plan.Mounts {
		if m.Target == "/run/boid/broker.sock" {
			t.Error("broker socket mount should not exist when BrokerSocket is empty")
		}
	}
}

func TestBuildSandboxPlan_CleanupPaths(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		HookScript:   "run.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		StagingDir:   "/tmp/staging-123",
	}
	plan := BuildSandboxPlan(cfg)

	if len(plan.CleanupPaths) != 1 || plan.CleanupPaths[0] != "/tmp/staging-123" {
		t.Errorf("CleanupPaths: got %v", plan.CleanupPaths)
	}
}

func TestBuildSandboxPlan_CleanupPaths_CommandMode(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		Argv:         []string{"/bin/bash"},
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		StagingDir:   "/tmp/staging-123",
	}
	plan := BuildSandboxPlan(cfg)

	if len(plan.CleanupPaths) != 0 {
		t.Errorf("CleanupPaths should be empty in command mode, got %v", plan.CleanupPaths)
	}
}

func TestBuildSandboxPlan_Worktree(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir: "/home/user/proj",
		HomeDir:    "/home/user",
		HookFiles: []HookFile{
			{Source: "/kits/claude-code/hooks/run.sh", TargetName: "claude-code--run.sh"},
		},
		HookScript:   "run.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		WorktreeDir:  "/home/user/.local/share/boid/worktrees/proj/task-abc",
	}
	plan := BuildSandboxPlan(cfg)

	wt := "/home/user/.local/share/boid/worktrees/proj/task-abc"
	origProj := "/home/user/proj"

	// Worktree directory should be mounted rw
	foundWT := false
	for _, m := range plan.Mounts {
		if m.Source == wt && m.Target == wt && m.Type == MountBind && !m.ReadOnly {
			foundWT = true
		}
	}
	if !foundWT {
		t.Error("worktree dir rw mount not found")
	}

	// Original project dir should NOT be mounted (only .git)
	for _, m := range plan.Mounts {
		if m.Source == origProj && m.Target == origProj {
			t.Error("original project dir should not be directly mounted in worktree mode")
		}
	}

	// .git should be mounted rw at original path
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

	// .boid should come from original project dir, mounted at worktree path
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

	// Hooks tmpfs should be at worktree/.boid/hooks
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

	// .git mount should come AFTER HOME tmpfs
	homeIdx := -1
	gitIdx := -1
	for i, m := range plan.Mounts {
		if m.Target == "/home/user" && m.Type == MountTmpfs {
			homeIdx = i
		}
		if m.Source == gitDir && m.Target == gitDir {
			gitIdx = i
		}
	}
	if homeIdx >= 0 && gitIdx >= 0 && gitIdx <= homeIdx {
		t.Error(".git mount should come after HOME tmpfs")
	}
}

func TestBuildSandboxPlan_Gate_WorkDirTmpfs(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir: "/home/user/proj",
		BoidBinary: "/usr/local/bin/boid",
		Role:       "gate",
	}
	plan := BuildSandboxPlan(cfg)

	// gate must NOT bind-mount the project dir
	for _, m := range plan.Mounts {
		if m.Source == "/home/user/proj" {
			t.Errorf("gate must not bind-mount project dir, but found: %+v", m)
		}
	}

	// gate must mount an empty tmpfs at workDir
	found := false
	for _, m := range plan.Mounts {
		if m.Target == "/home/user/proj" && m.Type == MountTmpfs && m.Source == "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("gate: tmpfs mount at workDir not found")
	}
}

func TestBuildSandboxPlan_Worktree_NoGitInNonWorktreeMode(t *testing.T) {
	cfg := WrapperConfig{
		ProjectDir:   "/home/user/proj",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}
	plan := BuildSandboxPlan(cfg)

	for _, m := range plan.Mounts {
		if m.Target == "/home/user/proj/.git" {
			t.Error(".git should not be explicitly mounted in non-worktree mode")
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
