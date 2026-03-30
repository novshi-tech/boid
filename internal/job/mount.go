package job

import "fmt"

// MountType represents the type of filesystem mount.
type MountType string

const (
	MountBind  MountType = "bind"
	MountRBind MountType = "rbind"
	MountTmpfs MountType = "tmpfs"
)

// MountEntry describes a single mount operation inside the sandbox.
type MountEntry struct {
	Source     string    // host path (empty for tmpfs)
	Target     string    // absolute path inside sandbox
	Type       MountType
	ReadOnly   bool
	Slave      bool      // mount --make-rslave after mounting
	IsFile     bool      // target is a file, not a directory
	DetectType bool      // detect file vs dir at runtime (if/elif)
	Guard      string    // shell test expression; if non-empty, wrap in if [ $Guard ]; then
	NeedsDirs  []string  // subdirs to create under Target before ro remount
}

// FileEntry describes a file to write inside the sandbox.
type FileEntry struct {
	Path    string // absolute path inside sandbox
	Content string
}

// CopyEntry describes a file to copy from host into the sandbox.
type CopyEntry struct {
	Source     string // host path
	Target     string // absolute path inside sandbox
	Executable bool
}

// SymlinkEntry describes a symlink to create inside the sandbox.
type SymlinkEntry struct {
	LinkTarget string // what the symlink points to (e.g. "boid")
	LinkPath   string // absolute path inside sandbox
}

// SandboxPlan is a declarative description of the sandbox environment.
// BuildSandboxPlan decides what to mount; RenderSetupScript generates shell.
type SandboxPlan struct {
	Mounts       []MountEntry
	Files        []FileEntry
	Copies       []CopyEntry
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
			Guard:  fmt.Sprintf("-d %s", d),
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

		// Workspace projects (ro)
		for _, dir := range cfg.WorkspaceDirs {
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source:   dir,
				Target:   dir,
				Type:     MountBind,
				ReadOnly: true,
			})
		}
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

	if cfg.Role != "gate" {
		// Re-mount working directory on top of HOME tmpfs
		plan.Mounts = append(plan.Mounts, MountEntry{
			Source:   workDir,
			Target:   workDir,
			Type:     MountBind,
			ReadOnly: cfg.Readonly,
		})
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
			Guard:    fmt.Sprintf("-d %s", boidSource),
		}
		if cfg.Command == "" && cfg.HooksDir != "" {
			boidMount.NeedsDirs = []string{"hooks"}
		}
		plan.Mounts = append(plan.Mounts, boidMount)

		if cfg.Command == "" && cfg.HooksDir != "" {
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source:   cfg.HooksDir,
				Target:   workDir + "/.boid/hooks",
				Type:     MountBind,
				ReadOnly: true,
				Guard:    fmt.Sprintf("-d %s", boidSource),
			})
		}

		// Worktree mode: re-mount .git inside sandbox for git worktree reference
		if cfg.WorktreeDir != "" {
			gitDir := cfg.ProjectDir + "/.git"
			plan.Mounts = append(plan.Mounts, MountEntry{
				Source: gitDir,
				Target: gitDir,
				Type:   MountBind,
				Guard:  fmt.Sprintf("-d %s", gitDir),
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

	// Boid binary
	plan.Copies = append(plan.Copies, CopyEntry{
		Source:     cfg.BoidBinary,
		Target:     "/opt/boid/bin/boid",
		Executable: true,
	})

	// Command shims
	for _, cmd := range cfg.HostCommands {
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
	if cfg.StagingDir != "" && cfg.Command == "" {
		plan.CleanupPaths = append(plan.CleanupPaths, cfg.StagingDir)
	}

	return plan
}
