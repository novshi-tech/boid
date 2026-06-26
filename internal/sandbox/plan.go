package sandbox

// NFTRule is a single nftables command expressed as an argv. The go-native
// sandbox runner applies it with exec.Command("nft", Args...). Replaces the
// former []string of pre-rendered shell lines so the runner can exec nft
// directly without a shell.
type NFTRule struct {
	Args []string
}

// Plan is the declarative description of the sandbox layout that the go-native
// runner materializes via syscalls. It starts with base mounts (system dirs,
// /dev, /proc, /tmp, DNS) plus optional nft rules and then appends everything
// the caller supplied in Spec.
type Plan struct {
	Mounts       []Mount
	Files        []FileWrite
	Symlinks     []Symlink
	NFTRules     []NFTRule
	CleanupPaths []string
}

// BuildPlan constructs the Plan from a Spec. The base mounts and nft rules are
// sandbox-package knowledge; the runner package consumes the result.
func BuildPlan(spec Spec) *Plan {
	plan := &Plan{}

	if spec.Profile == ProfileInit {
		// ProfileInit: mount the entire host root read-only so the init script
		// can discover installed tools without an explicit allowlist. /dev, /proc,
		// and /tmp still need individual mounts because they are special
		// filesystems that must be layered on top of the rbind.
		plan.Mounts = append(plan.Mounts, Mount{
			Source:   "/",
			Target:   "/",
			Type:     MountRBind,
			Slave:    true,
			ReadOnly: true,
		})
		plan.Mounts = append(plan.Mounts,
			Mount{Source: "/dev", Target: "/dev", Type: MountRBind},
			Mount{Source: "/proc", Target: "/proc", Type: MountRBind},
			Mount{Target: "/tmp", Type: MountTmpfs},
		)
	} else {
		// ProfileDefault: the standard small set of host system directories.
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
	}

	// DNS: inject a stub resolv.conf pointing at the pasta gateway DNS.
	// ProfileInit mounts the entire host root read-only, so /run/systemd/resolve/
	// is inside the rbind and the file write would fail with EPERM. Skip the
	// write for ProfileInit — the host DNS configuration is already visible via
	// the rbind, and the proxy port is always 0 for init jobs (no network
	// filtering) so pasta routes DNS through the host resolver without needing a
	// stub file.
	if spec.Profile != ProfileInit {
		plan.Files = append(plan.Files, FileWrite{
			Path:    "/run/systemd/resolve/stub-resolv.conf",
			Content: "nameserver 10.0.2.3",
		})
	}

	// Network filtering (nftables) — drops everything except the proxy hosts.
	if spec.ProxyPort > 0 {
		plan.NFTRules = []NFTRule{
			{Args: []string{"add", "table", "inet", "filter"}},
			{Args: []string{"add", "chain", "inet", "filter", "output", "{ type filter hook output priority 0 ; policy drop ; }"}},
			{Args: []string{"add", "rule", "inet", "filter", "output", "oifname", "lo", "accept"}},
			{Args: []string{"add", "rule", "inet", "filter", "output", "ip", "daddr", "10.0.2.2", "accept"}},
			{Args: []string{"add", "rule", "inet", "filter", "output", "ip", "daddr", "10.0.2.3", "accept"}},
		}
	}

	// Caller-supplied mounts/symlinks.
	plan.Mounts = append(plan.Mounts, spec.Mounts...)
	// (Note: spec.Files are written by the runner inside the sandbox after
	// pivot_root, not as plan files, because they live under the tmpfs HOME and
	// must not be shadowed by the HOME tmpfs mount.)
	plan.Symlinks = append(plan.Symlinks, spec.Symlinks...)

	plan.CleanupPaths = append(plan.CleanupPaths, spec.CleanupPaths...)

	return plan
}
