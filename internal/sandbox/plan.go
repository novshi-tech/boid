package sandbox

// MountType represents the type of filesystem mount.
type MountType string

const (
	MountBind  MountType = "bind"
	MountRBind MountType = "rbind"
	MountTmpfs MountType = "tmpfs"
)

// MountEntry describes a single mount operation inside the sandbox.
type MountEntry struct {
	Source     string // host path (empty for tmpfs)
	Target     string // absolute path inside sandbox
	Type       MountType
	ReadOnly   bool
	Slave      bool     // mount --make-rslave after mounting
	IsFile     bool     // target is a file, not a directory
	DetectType bool     // detect file vs dir at runtime (if/elif)
	Guard      string   // shell test expression; if non-empty, wrap in if [ $Guard ]; then
	NeedsDirs  []string // subdirs to create under Target before ro remount
}

// FileEntry describes a file to write inside the sandbox.
type FileEntry struct {
	Path    string // absolute path inside sandbox
	Content string
}

// SymlinkEntry describes a symlink to create inside the sandbox.
type SymlinkEntry struct {
	LinkTarget string // what the symlink points to (e.g. "boid")
	LinkPath   string // absolute path inside sandbox
}

// HookFile describes a single hook file to bind-mount into the sandbox.
type HookFile struct {
	Source     string // host-side absolute path
	TargetName string // filename inside sandbox .boid/hooks/
}

// SandboxPlan is a declarative description of the sandbox environment.
// BuildSandboxPlan decides what to mount; RenderSetupScript generates shell.
type SandboxPlan struct {
	Mounts       []MountEntry
	Files        []FileEntry
	Symlinks     []SymlinkEntry
	NFTRules     []string
	CleanupPaths []string // extra paths to remove on exit (staging dirs, etc.)
}

// BuildSandboxPlan constructs a SandboxPlan from WrapperConfig.
// This function is pure logic with no side effects.
func BuildSandboxPlan(cfg WrapperConfig) *SandboxPlan {
	plan := &SandboxPlan{}

	// Host system directories (ro, rbind, rslave)
	for _, d := range []string{"/bin", "/sbin", "/lib", "/lib64", "/usr", "/etc"} {
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source: d,
			Target: d,
			Type:   MountRBind,
			Slave:  true,
			Guard:  dirGuard(d),
		})
	}

	// Essential filesystems
	plan.Mounts = append(plan.Mounts,
		MountEntry{Source: "/dev", Target: "/dev", Type: MountRBind},
		MountEntry{Source: "/proc", Target: "/proc", Type: MountRBind},
		MountEntry{Target: "/tmp", Type: MountTmpfs},
	)

	// DNS
	plan.Files = append(plan.Files, FileEntry{
		Path:    "/run/systemd/resolve/stub-resolv.conf",
		Content: "nameserver 10.0.2.3",
	})

	// Network filtering (nftables)
	if cfg.ProxyPort > 0 {
		plan.NFTRules = []string{
			"nft add table inet filter",
			`nft 'add chain inet filter output { type filter hook output priority 0 ; policy drop ; }'`,
			`nft add rule inet filter output oifname "lo" accept`,
			"nft add rule inet filter output ip daddr 10.0.2.2 accept",
			"nft add rule inet filter output ip daddr 10.0.2.3 accept",
		}
	}

	workDir := cfg.workDir()
	homeDir := cfg.homeDir()

	// Project directory bind-mount (before HOME tmpfs).
	// When MountProjectDir=false, WorkDir is left for the tmpfs below to create.
	if cfg.MountProjectDir {
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:   workDir,
			Target:   workDir,
			Type:     MountBind,
			ReadOnly: cfg.ProjectReadOnly,
		})
	}

	// HOME tmpfs — always at cfg.HomeDir, independent of other settings.
	plan.Mounts = append(plan.Mounts, MountEntry{
		Target: homeDir,
		Type:   MountTmpfs,
	})

	if !cfg.MountProjectDir {
		// Without a project bind, WorkDir may not exist inside the sandbox
		// (e.g. it's shadowed by the HOME tmpfs). Mount an empty tmpfs so
		// `cd WorkDir` succeeds and host-side tools can resolve the path.
		plan.Mounts = append(plan.Mounts, MountEntry{
			Target: workDir,
			Type:   MountTmpfs,
		})
	} else {
		// Re-mount working directory on top of HOME tmpfs so its contents
		// stay visible when WorkDir is a descendant of HomeDir.
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:   workDir,
			Target:   workDir,
			Type:     MountBind,
			ReadOnly: cfg.ProjectReadOnly,
		})

		// Workspace peers are read-only mirrors of other projects.
		for _, dir := range cfg.WorkspaceDirs {
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source:   dir,
				Target:   dir,
				Type:     MountBind,
				ReadOnly: true,
			})
		}

		// .boid directory (ro) with optional hooks overlay.
		// In worktree mode, .boid comes from the original project dir
		// but is mounted at the worktree path.
		boidSource := cfg.ProjectDir + "/.boid"
		boidTarget := workDir + "/.boid"
		boidMount := MountEntry{
			Source:   boidSource,
			Target:   boidTarget,
			Type:     MountBind,
			ReadOnly: true,
			Guard:    dirGuard(boidSource),
		}
		if len(cfg.HookFiles) > 0 {
			boidMount.NeedsDirs = []string{"hooks"}
		}
		plan.Mounts = append(plan.Mounts, boidMount)

		if len(cfg.HookFiles) > 0 {
			hooksTarget := workDir + "/.boid/hooks"
			plan.Mounts = append(plan.Mounts, MountEntry{
				Target: hooksTarget,
				Type:   MountTmpfs,
				Guard:  dirGuard(boidSource),
			})
			for _, hf := range cfg.HookFiles {
				plan.Mounts = append(plan.Mounts, MountEntry{
					Source:   hf.Source,
					Target:   hooksTarget + "/" + hf.TargetName,
					Type:     MountBind,
					ReadOnly: true,
					IsFile:   true,
					Guard:    dirGuard(boidSource),
				})
			}
		}

		// Worktree mode: re-mount .git inside sandbox for git worktree reference.
		if cfg.WorktreeDir != "" {
			gitDir := cfg.ProjectDir + "/.git"
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source: gitDir,
				Target: gitDir,
				Type:   MountBind,
				Guard:  dirGuard(gitDir),
			})
		}
	}

	// Additional bindings — applied regardless of MountProjectDir so callers can
	// expose sockets (broker, server) and gate scripts under /opt/boid/... etc.
	for _, bm := range cfg.AdditionalBindings {
		target := bm.Target
		if target == "" {
			target = bm.Source
		}
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:     bm.Source,
			Target:     target,
			Type:       MountBind,
			ReadOnly:   bm.Mode != "rw",
			IsFile:     bm.IsFile,
			DetectType: !bm.IsFile,
		})
	}

	// Boid binary — bind-mounted read-only at /opt/boid/bin/boid.
	plan.Mounts = append(plan.Mounts, MountEntry{
		Source:   cfg.BoidBinary,
		Target:   "/opt/boid/bin/boid",
		Type:     MountBind,
		IsFile:   true,
		ReadOnly: true,
	})

	// Command shims
	for _, cmd := range shimCommands(cfg.BuiltinCommands, cfg.HostCommands) {
		plan.Symlinks = append(plan.Symlinks, SymlinkEntry{
			LinkTarget: "boid",
			LinkPath:   "/opt/boid/bin/" + cmd,
		})
	}

	// Cleanup paths — retained so orchestrator-staged gate dirs get removed.
	if cfg.StagingDir != "" {
		plan.CleanupPaths = append(plan.CleanupPaths, cfg.StagingDir)
	}

	return plan
}

func shimCommands(builtins, hostCommands []string) []string {
	seen := make(map[string]struct{}, len(builtins)+len(hostCommands))
	var out []string
	add := func(name string) {
		if name == "boid" {
			return // boid binary is bind-mounted directly, not shimmed
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range builtins {
		add(name)
	}
	for _, name := range hostCommands {
		add(name)
	}
	return out
}
