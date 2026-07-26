package dockerres

import "testing"

// TestLabelKeys pins the exact label strings every package in the
// dispatcher → reap → dockerproxy chain now reads from this one place
// (docs/plans/workspace-home-volume-persistence.md 論点 a, PR1 §D2). A typo
// here silently breaks a reap filter or a containment rule, so the literals
// are asserted rather than merely referenced.
func TestLabelKeys(t *testing.T) {
	cases := []struct{ got, want string }{
		{LabelJobID, "boid.job_id"},
		{LabelWorkspace, "boid.workspace"},
		{LabelInstallID, "boid.install_id"},
		{LabelWorkspaceHome, "boid.workspace_home"},
		{LabelWorkspaceHomeInstallID, "boid.workspace_home_install_id"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("label = %q, want %q", c.got, c.want)
		}
	}
}

// TestNamePrefixes pins the two distinct prefixes and the nesting relation
// between them: every workspace HOME volume name is also a reserved name,
// but not every reserved name is a workspace HOME (the workspace *network*
// namespace shares ReservedVolumeNamePrefix — see the package doc).
func TestNamePrefixes(t *testing.T) {
	if ReservedVolumeNamePrefix != "boid-ws-" {
		t.Errorf("ReservedVolumeNamePrefix = %q, want %q", ReservedVolumeNamePrefix, "boid-ws-")
	}
	if WorkspaceHomeVolumePrefix != "boid-ws-home-" {
		t.Errorf("WorkspaceHomeVolumePrefix = %q, want %q", WorkspaceHomeVolumePrefix, "boid-ws-home-")
	}
	if len(WorkspaceHomeVolumePrefix) <= len(ReservedVolumeNamePrefix) ||
		WorkspaceHomeVolumePrefix[:len(ReservedVolumeNamePrefix)] != ReservedVolumeNamePrefix {
		t.Errorf("WorkspaceHomeVolumePrefix %q must extend ReservedVolumeNamePrefix %q",
			WorkspaceHomeVolumePrefix, ReservedVolumeNamePrefix)
	}
}

func TestIsReservedVolumeName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"boid-ws-home-abc12345-default", true},
		{"boid-ws-abc12345-default", true}, // workspace network name shape
		{"boid-ws-", true},
		{"boid-workspace", false},
		{"my-volume", false},
		{"", false},
		{"BOID-WS-home-x", false}, // docker names are case-sensitive
	}
	for _, c := range cases {
		if got := IsReservedVolumeName(c.name); got != c.want {
			t.Errorf("IsReservedVolumeName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsWorkspaceHomeVolumeName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"boid-ws-home-abc12345-default", true},
		{"boid-ws-home-", true},
		{"boid-ws-abc12345-default", false}, // workspace network, reap may destroy it
		{"boid-ws-homely", false},
		{"my-volume", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsWorkspaceHomeVolumeName(c.name); got != c.want {
			t.Errorf("IsWorkspaceHomeVolumeName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSanitizeNamePart(t *testing.T) {
	cases := []struct{ in, want string }{
		{"default", "default"},
		{"a_b.c-d", "a_b.c-d"},
		{"my workspace", "my-workspace"},
		{"a/b", "a-b"},
		{"日本語", "---"},
		{"", "x"},
	}
	for _, c := range cases {
		if got := SanitizeNamePart(c.in); got != c.want {
			t.Errorf("SanitizeNamePart(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWorkspaceNetworkName pins the behavior moved verbatim out of
// internal/dispatcher.containerWorkspaceNetworkName: install_id truncated to
// 8 characters, "noinst" when unset, every part sanitized.
func TestWorkspaceNetworkName(t *testing.T) {
	cases := []struct {
		installID, workspace, want string
	}{
		{"abcdef0123456789", "default", "boid-ws-abcdef01-default"},
		{"short", "default", "boid-ws-short-default"},
		{"", "default", "boid-ws-noinst-default"},
		{"abcdef0123456789", "my workspace", "boid-ws-abcdef01-my-workspace"},
		{"", "", "boid-ws-noinst-x"},
	}
	for _, c := range cases {
		if got := WorkspaceNetworkName(c.installID, c.workspace); got != c.want {
			t.Errorf("WorkspaceNetworkName(%q, %q) = %q, want %q", c.installID, c.workspace, got, c.want)
		}
	}
}

// TestWorkspaceHomeVolumeName pins the name PR6 will mount as /home/boid.
// Nothing calls it in PR1; it lives here so the reserved prefix the
// containment rules key off and the name they must match can never drift
// apart (PR1 §D2).
func TestWorkspaceHomeVolumeName(t *testing.T) {
	cases := []struct {
		installID, workspace, want string
	}{
		{"abcdef0123456789", "default", "boid-ws-home-abcdef01-default"},
		{"", "default", "boid-ws-home-noinst-default"},
		{"abcdef0123456789", "bm next", "boid-ws-home-abcdef01-bm-next"},
	}
	for _, c := range cases {
		got := WorkspaceHomeVolumeName(c.installID, c.workspace)
		if got != c.want {
			t.Errorf("WorkspaceHomeVolumeName(%q, %q) = %q, want %q", c.installID, c.workspace, got, c.want)
		}
	}
}

// TestWorkspaceHomeVolumeName_IsRecognizedAndValid is the drift guard the
// two predicates exist for: whatever WorkspaceHomeVolumeName produces must
// be classified as both reserved (so dockerproxy denies the sandbox
// touching it) and as a workspace HOME (so both reapers skip it), and must
// be a legal docker volume name.
func TestWorkspaceHomeVolumeName_IsRecognizedAndValid(t *testing.T) {
	for _, ws := range []string{"default", "boid", "bm-next", "with space", "日本語", ""} {
		name := WorkspaceHomeVolumeName("abcdef0123456789", ws)
		if !IsReservedVolumeName(name) {
			t.Errorf("IsReservedVolumeName(%q) = false, want true", name)
		}
		if !IsWorkspaceHomeVolumeName(name) {
			t.Errorf("IsWorkspaceHomeVolumeName(%q) = false, want true", name)
		}
		if !IsValidVolumeName(name) {
			t.Errorf("IsValidVolumeName(%q) = false, want true", name)
		}
	}
}

// TestIsValidVolumeName covers the guard 論点 e's option (i) requires: the
// "Source が / で始まらなければ named volume" convention means a stray
// relative path could reach VolumeCreate, so the container backend rejects
// anything docker itself would not accept as a volume name.
func TestIsValidVolumeName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"boid-ws-home-abc12345-default", true},
		{"my-volume", true},
		{"Vol_1.2-3", true},
		{"0abc", true},
		{"", false},
		{"-leading-dash", false},
		{".leading-dot", false},
		{"_leading-underscore", false},
		{"relative/path", false}, // the accidental-relative-path case
		{"../escape", false},     // ditto
		{"has space", false},
		{"日本語", false},
	}
	for _, c := range cases {
		if got := IsValidVolumeName(c.name); got != c.want {
			t.Errorf("IsValidVolumeName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- workspace HOME init container (PR5, docs/plans/
// workspace-home-volume-persistence.md 論点 c §D5/§D6) ---

// TestWorkspaceInitLabelKeys pins the init container's own two label keys and,
// more importantly, that they are NOT any of the keys an existing sweep
// enumerates on. This is the whole reason they exist as separate keys: a
// boid.job_id on a container that may be 20 minutes into a 1.5GB toolchain
// download would put it in ReapOrphans' force-remove list, and its job id into
// ReapReport.ReapedJobIDs/FailedJobIDs — which drive MarkStale and gate every
// task's auto-reopen for that boot.
func TestWorkspaceInitLabelKeys(t *testing.T) {
	if LabelWorkspaceInit != "boid.workspace_init" {
		t.Errorf("LabelWorkspaceInit = %q, want %q", LabelWorkspaceInit, "boid.workspace_init")
	}
	if LabelWorkspaceInitInstallID != "boid.workspace_init_install_id" {
		t.Errorf("LabelWorkspaceInitInstallID = %q, want %q", LabelWorkspaceInitInstallID, "boid.workspace_init_install_id")
	}
	for _, enumerated := range []string{LabelJobID, LabelWorkspace, LabelInstallID} {
		if LabelWorkspaceInit == enumerated || LabelWorkspaceInitInstallID == enumerated {
			t.Errorf("workspace-init label collides with the enumeration filter %q", enumerated)
		}
	}
}

// TestWorkspaceInitContainerName pins the deterministic name PR5 §D6 uses as
// a cross-process mutual exclusion: two daemons (or a daemon and the leftover
// container of its own previous incarnation) computing this for the same
// (installID, slug) must collide on the engine, because that collision is the
// ONLY thing that survives the process death a flock does not.
func TestWorkspaceInitContainerName(t *testing.T) {
	cases := []struct {
		installID, workspace, want string
	}{
		{"0123456789abcdef", "default", "boid-ws-init-01234567-default"},
		{"", "default", "boid-ws-init-noinst-default"},
		{"0123456789abcdef", "has space", "boid-ws-init-01234567-has-space"},
		{"0123456789abcdef", "", "boid-ws-init-01234567-x"},
	}
	for _, c := range cases {
		if got := WorkspaceInitContainerName(c.installID, c.workspace); got != c.want {
			t.Errorf("WorkspaceInitContainerName(%q, %q) = %q, want %q", c.installID, c.workspace, got, c.want)
		}
	}
}

// TestWorkspaceInitContainerName_SharesTheInstallScopingOfTheOtherNames keeps
// the three generators from drifting apart in how they scope by install: a
// name that stopped being install-scoped would make two installs sharing one
// engine mutually exclude each other's workspace inits.
func TestWorkspaceInitContainerName_SharesTheInstallScopingOfTheOtherNames(t *testing.T) {
	const workspace = "default"
	a := WorkspaceInitContainerName("aaaaaaaaaaaa", workspace)
	b := WorkspaceInitContainerName("bbbbbbbbbbbb", workspace)
	if a == b {
		t.Errorf("two installs produced the same init container name %q", a)
	}
	if got := WorkspaceInitContainerName("aaaaaaaaaaaa", workspace); got != a {
		t.Errorf("WorkspaceInitContainerName is not deterministic: %q then %q", a, got)
	}
}
