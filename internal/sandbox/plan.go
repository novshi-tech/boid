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

	// Working directory: worktree or project dir
	// Gates have no filesystem access, so skip project/workspace mounts.
	workDir := cfg.workDir()

	if cfg.Role != "gate" {
		// Project/worktree directory (rw, or ro if Readonly)
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:   workDir,
			Target:   workDir,
			Type:     MountBind,
			ReadOnly: cfg.Readonly,
		})
	}

	// HOME as tmpfs
	homeDir := cfg.homeDir()
	if cfg.Role == "gate" {
		homeDir = "/tmp" // gates use /tmp as home
	}
	plan.Mounts = append(plan.Mounts, MountEntry{
		Target: homeDir,
		Type:   MountTmpfs,
	})
	if cfg.Role == "gate" {
		// Mount empty tmpfs at workDir AFTER the homeDir (/tmp) mount.
		// workDir is a path under /tmp (e.g. /tmp/boid-e2e-.../workspace/app),
		// so it must be mounted after /tmp to avoid being hidden by the homeDir remount.
		// The broker runs host commands with the Cwd received from the shim,
		// so this allows gh and similar tools to resolve the repo from the
		// host-side .git/config without exposing any project files inside the sandbox.
		plan.Mounts = append(plan.Mounts, MountEntry{
			Target: workDir,
			Type:   MountTmpfs,
		})
	}

	if cfg.Role != "gate" {
		// Re-mount working directory on top of HOME tmpfs
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:   workDir,
			Target:   workDir,
			Type:     MountBind,
			ReadOnly: cfg.Readonly,
		})

		// Workspace projects (ro) — after HOME tmpfs so paths under HOME remain accessible
		for _, dir := range cfg.WorkspaceDirs {
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source:   dir,
				Target:   dir,
				Type:     MountBind,
				ReadOnly: true,
			})
		}
	}

	if cfg.Role != "gate" {
		// .boid directory (ro) with optional hooks overlay
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
		if len(cfg.Argv) == 0 && len(cfg.HookFiles) > 0 {
			boidMount.NeedsDirs = []string{"hooks"}
		}
		plan.Mounts = append(plan.Mounts, boidMount)

		if len(cfg.Argv) == 0 && len(cfg.HookFiles) > 0 {
			hooksTarget := workDir + "/.boid/hooks"
			// Mount tmpfs at .boid/hooks to allow individual file bind-mounts
			plan.Mounts = append(plan.Mounts, MountEntry{
				Target: hooksTarget,
				Type:   MountTmpfs,
				Guard:  dirGuard(boidSource),
			})
			// Bind-mount each hook file individually (read-only)
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

		// Worktree mode: re-mount .git inside sandbox for git worktree reference
		if cfg.WorktreeDir != "" {
			gitDir := cfg.ProjectDir + "/.git"
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source: gitDir,
				Target: gitDir,
				Type:   MountBind,
				Guard:  dirGuard(gitDir),
			})
		}

		// Additional bindings
		for _, bm := range cfg.AdditionalBindings {
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source:     bm.Source,
				Target:     bm.Source,
				Type:       MountBind,
				ReadOnly:   bm.Mode != "rw",
				DetectType: true,
			})
		}
	}

	// Boid binary — bind-mounted read-only at /opt/boid/bin/boid.
	// Source's executable bit is preserved across bind-mount.
	plan.Mounts = append(plan.Mounts, MountEntry{
		Source:   cfg.BoidBinary,
		Target:   "/opt/boid/bin/boid",
		Type:     MountBind,
		IsFile:   true,
		ReadOnly: true,
	})
	if cfg.Role == "gate" && cfg.HookScript != "" {
		gatesDir := cfg.GatesDir
		if gatesDir == "" {
			gatesDir = cfg.ProjectDir + "/.boid/gates"
		}
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:   gatesDir + "/" + cfg.HookScript,
			Target:   "/opt/boid/gates/" + cfg.HookScript,
			Type:     MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
	}

	// Command shims
	for _, cmd := range shimCommands(cfg.BuiltinCommands, cfg.HostCommands) {
		plan.Symlinks = append(plan.Symlinks, SymlinkEntry{
			LinkTarget: "boid",
			LinkPath:   "/opt/boid/bin/" + cmd,
		})
	}

	// Server socket — only in legacy/command mode (hooks and gates use broker only)
	if cfg.Role == "" && cfg.ServerSocket != "" {
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source: cfg.ServerSocket,
			Target: "/run/boid/server.sock",
			Type:   MountBind,
			IsFile: true,
		})
	}

	// Broker socket
	if cfg.BrokerSocket != "" {
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source: cfg.BrokerSocket,
			Target: "/run/boid/broker.sock",
			Type:   MountBind,
			IsFile: true,
		})
	}

	// Cleanup paths
	if cfg.StagingDir != "" && len(cfg.Argv) == 0 {
		plan.CleanupPaths = append(plan.CleanupPaths, cfg.StagingDir)
	}

	return plan
}

func shimCommands(builtins, hostCommands []string) []string {
	seen := make(map[string]struct{}, len(builtins)+len(hostCommands))
	var out []string
	for _, name := range builtins {
		if name == "boid" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range hostCommands {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
