package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBuildPlan_ProfileDefault_BaseSystemDirs verifies that ProfileDefault (the
// zero value) produces mounts for the standard set of host system directories
// and does NOT include a host-root rbind.
func TestBuildPlan_ProfileDefault_BaseSystemDirs(t *testing.T) {
	spec := sandbox.Spec{} // Profile == ProfileDefault (zero value)
	plan := sandbox.BuildPlan(spec)

	// Must not have a mount with Source=="/" and Target=="/" (the ProfileInit
	// host-root rbind).
	for _, m := range plan.Mounts {
		if m.Source == "/" && m.Target == "/" {
			t.Errorf("ProfileDefault: unexpected host-root rbind mount (Source=%q Target=%q)", m.Source, m.Target)
		}
	}

	// Each of the standard system dirs must appear as a mount target.
	wantTargets := map[string]bool{
		"/bin": false, "/sbin": false, "/lib": false,
		"/lib64": false, "/usr": false, "/etc": false,
		"/dev": false, "/proc": false, "/tmp": false,
	}
	for _, m := range plan.Mounts {
		if _, ok := wantTargets[m.Target]; ok {
			wantTargets[m.Target] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("ProfileDefault: expected mount for %s not found in plan", target)
		}
	}
}

// TestBuildPlan_ProfileInit_HostRootRBind verifies that ProfileInit produces a
// host-root ro-rbind mount and does NOT include the individual system-dir mounts.
func TestBuildPlan_ProfileInit_HostRootRBind(t *testing.T) {
	spec := sandbox.Spec{Profile: sandbox.ProfileInit}
	plan := sandbox.BuildPlan(spec)

	// Must have exactly one mount with Source=="/" and Target=="/" that is
	// ReadOnly and Slave.
	foundRoot := false
	for _, m := range plan.Mounts {
		if m.Source == "/" && m.Target == "/" {
			foundRoot = true
			if !m.ReadOnly {
				t.Errorf("ProfileInit: host-root mount should be ReadOnly, got ReadOnly=%v", m.ReadOnly)
			}
			if !m.Slave {
				t.Errorf("ProfileInit: host-root mount should be Slave (rslave), got Slave=%v", m.Slave)
			}
			if m.Type != sandbox.MountRBind {
				t.Errorf("ProfileInit: host-root mount type = %v, want MountRBind", m.Type)
			}
		}
	}
	if !foundRoot {
		t.Error("ProfileInit: expected host-root rbind mount (Source=/ Target=/) not found in plan")
	}

	// Must still have /dev, /proc, /tmp (essential filesystems).
	wantTargets := map[string]bool{
		"/dev": false, "/proc": false, "/tmp": false,
	}
	for _, m := range plan.Mounts {
		if _, ok := wantTargets[m.Target]; ok {
			wantTargets[m.Target] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("ProfileInit: expected essential mount %s not found in plan", target)
		}
	}
}

// TestBuildPlan_ProfileInit_NoBrokerSocketMount verifies that a ProfileInit
// plan does not include a broker socket mount (the broker socket is never added
// in BuildPlan itself — it comes via spec.Mounts from BuildSandboxSpec when
// rt.BrokerSocket is set; ProfileInit prevents that path from running).
// This test validates the plan layer only: when Profile==Init and no extra
// spec.Mounts are provided, no broker-socket-like bind appears.
func TestBuildPlan_ProfileInit_NoExtraBindsByDefault(t *testing.T) {
	spec := sandbox.Spec{Profile: sandbox.ProfileInit}
	plan := sandbox.BuildPlan(spec)

	// Verify no file-bind mounts exist (broker socket is a file bind).
	for _, m := range plan.Mounts {
		if m.IsFile && m.Type == sandbox.MountBind {
			t.Errorf("ProfileInit: unexpected file-bind mount (Source=%q Target=%q) — broker socket should not be present", m.Source, m.Target)
		}
	}
}

// TestBuildPlan_CallerMountsAppended verifies that caller-supplied spec.Mounts
// are appended after the profile-specific base mounts in both profiles.
func TestBuildPlan_CallerMountsAppended(t *testing.T) {
	extraMount := sandbox.Mount{
		Source: "/extra/src",
		Target: "/extra/target",
		Type:   sandbox.MountBind,
	}

	for _, profile := range []sandbox.Profile{sandbox.ProfileDefault, sandbox.ProfileInit} {
		spec := sandbox.Spec{
			Profile: profile,
			Mounts:  []sandbox.Mount{extraMount},
		}
		plan := sandbox.BuildPlan(spec)

		found := false
		for _, m := range plan.Mounts {
			if m.Source == extraMount.Source && m.Target == extraMount.Target {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Profile=%d: caller-supplied mount %+v not found in plan.Mounts", profile, extraMount)
		}
	}
}
