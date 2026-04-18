package sandbox

// MountType represents the type of filesystem mount.
type MountType string

const (
	MountBind  MountType = "bind"
	MountRBind MountType = "rbind"
	MountTmpfs MountType = "tmpfs"
)

// HookFile describes a single hook file to bind-mount into the sandbox.
// Retained as a helper shape; dispatcher composes these into Spec.Mounts.
type HookFile struct {
	Source     string // host-side absolute path
	TargetName string // filename inside sandbox .boid/hooks/
}

// sandboxPlan is the internal declarative description of the sandbox layout
// that the setup script renders. Only the sandbox package manipulates it.
type sandboxPlan struct {
	Mounts       []Mount
	Files        []FileWrite
	Symlinks     []Symlink
	NFTRules     []string
	CleanupPaths []string
}

// buildPlan constructs the internal plan from a Spec. The plan starts with
// base mounts (system dirs, /dev, /proc, /tmp, DNS, optional nft rules) and
// then appends everything the caller provided in Spec.
func buildPlan(spec Spec) *sandboxPlan {
	plan := &sandboxPlan{}

	// Host system directories (ro, rbind, rslave)
	for _, d := range []string{"/bin", "/sbin", "/lib", "/lib64", "/usr", "/etc"} {
		plan.Mounts = append(plan.Mounts, Mount{
			Source: d,
			Target: d,
			Type:   MountRBind,
			Slave:  true,
			Guard:  dirGuard(d),
		})
	}

	// Essential filesystems
	plan.Mounts = append(plan.Mounts,
		Mount{Source: "/dev", Target: "/dev", Type: MountRBind},
		Mount{Source: "/proc", Target: "/proc", Type: MountRBind},
		Mount{Target: "/tmp", Type: MountTmpfs},
	)

	// DNS
	plan.Files = append(plan.Files, FileWrite{
		Path:    "/run/systemd/resolve/stub-resolv.conf",
		Content: "nameserver 10.0.2.3",
	})

	// Network filtering (nftables) — drops everything except the proxy hosts.
	if spec.ProxyPort > 0 {
		plan.NFTRules = []string{
			"nft add table inet filter",
			`nft 'add chain inet filter output { type filter hook output priority 0 ; policy drop ; }'`,
			`nft add rule inet filter output oifname "lo" accept`,
			"nft add rule inet filter output ip daddr 10.0.2.2 accept",
			"nft add rule inet filter output ip daddr 10.0.2.3 accept",
		}
	}

	// Caller-supplied mounts/files/symlinks.
	plan.Mounts = append(plan.Mounts, spec.Mounts...)
	// (Note: spec.Files are written by generateInnerScript, not as plan files,
	// because they live inside the sandbox and we want them to go through the
	// normal HOME/env setup of the inner script.)
	plan.Symlinks = append(plan.Symlinks, spec.Symlinks...)

	plan.CleanupPaths = append(plan.CleanupPaths, spec.CleanupPaths...)

	return plan
}
