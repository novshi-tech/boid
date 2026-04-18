package sandbox

import (
	"strings"
	"testing"
)

func TestBuildPlan_SystemDirs(t *testing.T) {
	plan := buildPlan(Spec{})

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

func TestBuildPlan_EssentialFS(t *testing.T) {
	plan := buildPlan(Spec{})

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

func TestBuildPlan_DNS(t *testing.T) {
	plan := buildPlan(Spec{})

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

func TestBuildPlan_Proxy(t *testing.T) {
	plan := buildPlan(Spec{ProxyPort: 8888})

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

func TestBuildPlan_NoProxy(t *testing.T) {
	plan := buildPlan(Spec{})

	if len(plan.NFTRules) != 0 {
		t.Error("NFTRules should be empty when ProxyPort is 0")
	}
}

func TestBuildPlan_UserMountsAreAppended(t *testing.T) {
	userMount := Mount{
		Source: "/home/user/proj",
		Target: "/home/user/proj",
		Type:   MountBind,
	}
	plan := buildPlan(Spec{Mounts: []Mount{userMount}})

	// User mount must appear after the base system/essential mounts.
	lastIdx := -1
	for i, m := range plan.Mounts {
		if m.Source == userMount.Source && m.Target == userMount.Target {
			lastIdx = i
		}
	}
	if lastIdx < 0 {
		t.Fatal("user mount not found in plan")
	}

	// Base mounts should appear before.
	for i := 0; i < lastIdx; i++ {
		m := plan.Mounts[i]
		if m.Source == userMount.Source && m.Target == userMount.Target {
			t.Errorf("user mount should be at the tail, but found copy at index %d", i)
		}
	}
}

func TestBuildPlan_UserFilesAreNotInPlan(t *testing.T) {
	// Spec.Files are rendered by the inner script, not baked into the setup
	// plan, because they live inside the sandbox's HOME (which is tmpfs) and
	// need to be recreated on every invocation.
	plan := buildPlan(Spec{
		Files: []FileWrite{{Path: "/home/user/.boid/context/task.yaml", Content: "id: t"}},
	})
	for _, f := range plan.Files {
		if f.Path == "/home/user/.boid/context/task.yaml" {
			t.Error("spec.Files should not leak into the sandbox plan (setup script)")
		}
	}
}

func TestBuildPlan_Symlinks(t *testing.T) {
	plan := buildPlan(Spec{
		Symlinks: []Symlink{{LinkPath: "/opt/boid/bin/gh", LinkTarget: "boid"}},
	})
	if len(plan.Symlinks) != 1 {
		t.Fatalf("symlinks: got %d, want 1", len(plan.Symlinks))
	}
	if plan.Symlinks[0].LinkPath != "/opt/boid/bin/gh" {
		t.Errorf("symlink path: got %q", plan.Symlinks[0].LinkPath)
	}
}

func TestBuildPlan_CleanupPaths(t *testing.T) {
	plan := buildPlan(Spec{CleanupPaths: []string{"/tmp/staging-1"}})
	if len(plan.CleanupPaths) != 1 || plan.CleanupPaths[0] != "/tmp/staging-1" {
		t.Errorf("CleanupPaths: got %v", plan.CleanupPaths)
	}
}
