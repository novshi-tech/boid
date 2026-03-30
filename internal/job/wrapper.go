package job

import (
	"fmt"
	"os"
	"strings"

	"github.com/novshi-tech/boid/internal/model"
)

// WrapperConfig holds the parameters for sandbox script generation.
type WrapperConfig struct {
	JobID              string
	TaskID             string
	ProjectID          string
	ProjectDir         string            // host-side project directory
	HomeDir            string            // host-side user home directory (fallback to ProjectDir)
	HooksDir           string            // host-side hooks directory
	HookScript         string            // script filename, e.g. "run-build.sh"
	Command            string            // command to execute (non-interactive, non-hook mode)
	BoidBinary         string            // host-side path to boid binary
	ServerSocket       string            // host-side server socket path
	BrokerSocket       string            // host-side broker socket path
	BrokerToken        string            // broker authentication token
	Env                map[string]string // project environment variables
	HostCommands       []string          // command names to shim via symlinks
	AdditionalBindings []model.BindMount // extra host paths to bind-mount
	WorkspaceDirs      map[string]string // project-id -> host-dir (read-only mounts)
	ProxyPort          int               // host-side proxy port (0 = no proxy)
	StagingDir         string            // if set, staging dir to clean up after job
	TTY                bool              // if true, preserve TTY through pasta (for interactive commands)
}

// homeDir returns the effective home directory.
func (cfg WrapperConfig) homeDir() string {
	if cfg.HomeDir != "" {
		return cfg.HomeDir
	}
	return cfg.ProjectDir
}

// WriteSandboxScripts generates 3 sandbox scripts and writes them to /tmp.
// Returns the path to the outer script that should be executed in tmux.
func WriteSandboxScripts(cfg WrapperConfig) (string, error) {
	prefix := fmt.Sprintf("/tmp/boid-%s", cfg.JobID)

	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	outerPath := prefix + "-outer.sh"

	inner := generateInnerScript(cfg)
	setup := generateSetupScript(cfg, innerPath, setupPath, outerPath)
	outer := generateOuterScript(cfg, setupPath)

	for _, f := range []struct{ path, content string }{
		{innerPath, inner},
		{setupPath, setup},
		{outerPath, outer},
	} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o755); err != nil {
			return "", fmt.Errorf("write %s: %w", f.path, err)
		}
	}

	return outerPath, nil
}

func generateOuterScript(cfg WrapperConfig, setupPath string) string {
	if cfg.TTY {
		// Save original stderr to fd 3, suppress pasta's warnings,
		// then restore stderr in the child so the TTY is preserved.
		return fmt.Sprintf(`#!/bin/bash
set -e
exec 3>&2
exec pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    2>/dev/null \
    -- bash -c 'exec 2>&3 3>&-; exec unshare --mount -- bash %s'
`, setupPath)
	}
	return fmt.Sprintf(`#!/bin/bash
set -e
exec pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    2>/dev/null \
    -- unshare --mount -- bash %s
`, setupPath)
}

func generateSetupScript(cfg WrapperConfig, innerPath, setupPath, outerPath string) string {
	var b strings.Builder

	b.WriteString(`#!/bin/bash
set -e

ROOT=$(mktemp -d /tmp/boid-root-XXXXXX)

`)
	fmt.Fprintf(&b, `cleanup() {
    # Safety: refuse to rm if ROOT is not our tmpdir prefix
    case "$ROOT" in
        /tmp/boid-root-*) ;;
        *) echo "FATAL: ROOT=$ROOT is not a boid tmpdir, refusing cleanup" >&2; return 1 ;;
    esac
    # Unmount all bind mounts under $ROOT
    umount -R "$ROOT" 2>/dev/null || true
    # Safety: only rm if no mounts remain (prevent deleting host files via stale bind mounts)
    if ! findmnt --submounts --noheadings --output TARGET "$ROOT" | grep -q .; then
        rm -rf "$ROOT"
    else
        echo "WARNING: mounts still active under $ROOT, skipping rm" >&2
    fi
    rm -f %s %s %s
`, outerPath, setupPath, innerPath)
	if cfg.StagingDir != "" && cfg.Command == "" {
		fmt.Fprintf(&b, "    rm -rf %s\n", cfg.StagingDir)
	}
	b.WriteString(`}
trap cleanup EXIT
`)
	b.WriteString(`
# Host system directories (read-only)
for d in bin sbin lib lib64 usr etc; do
    [ -d "/$d" ] || continue
    mkdir -p "$ROOT/$d"
    mount --rbind "/$d" "$ROOT/$d"
    mount --make-rslave "$ROOT/$d"
done

# Essential filesystems
mkdir -p "$ROOT/dev" "$ROOT/proc" "$ROOT/tmp"
mount --rbind /dev "$ROOT/dev"
mount --rbind /proc "$ROOT/proc"
mount -t tmpfs tmpfs "$ROOT/tmp"

`)

	// DNS: pasta dns-forward (10.0.2.3) via resolv.conf symlink target
	b.WriteString("# DNS\n")
	b.WriteString("mkdir -p \"$ROOT/run/systemd/resolve\"\n")
	b.WriteString("echo \"nameserver 10.0.2.3\" > \"$ROOT/run/systemd/resolve/stub-resolv.conf\"\n\n")

	// Network filtering (nftables)
	if cfg.ProxyPort > 0 {
		b.WriteString("# Network filtering\n")
		b.WriteString("nft add table inet filter\n")
		b.WriteString("nft 'add chain inet filter output { type filter hook output priority 0 ; policy drop ; }'\n")
		b.WriteString("nft add rule inet filter output oifname \"lo\" accept\n")
		b.WriteString("nft add rule inet filter output ip daddr 10.0.2.2 accept\n")
		b.WriteString("nft add rule inet filter output ip daddr 10.0.2.3 accept\n\n")
	}

	// Project directory (rw) -- mounted at same path as host
	fmt.Fprintf(&b, "mkdir -p \"$ROOT%s\"\n", cfg.ProjectDir)
	fmt.Fprintf(&b, "mount --bind %s \"$ROOT%s\"\n", cfg.ProjectDir, cfg.ProjectDir)

	// Workspace projects (ro) -- same workspace, mounted at host paths
	if len(cfg.WorkspaceDirs) > 0 {
		b.WriteString("\n# Workspace projects (ro)\n")
		for _, dir := range cfg.WorkspaceDirs {
			fmt.Fprintf(&b, "mkdir -p \"$ROOT%s\"\n", dir)
			fmt.Fprintf(&b, "mount --bind %s \"$ROOT%s\"\n", dir, dir)
			fmt.Fprintf(&b, "mount -o remount,bind,ro \"$ROOT%s\"\n", dir)
		}
	}

	// Hooks directory (ro) -- only needed in hook mode
	if cfg.Command == "" {
		fmt.Fprintf(&b, "mkdir -p \"$ROOT%s/.boid/hooks\"\n", cfg.ProjectDir)
		fmt.Fprintf(&b, "mount --bind %s \"$ROOT%s/.boid/hooks\"\n", cfg.HooksDir, cfg.ProjectDir)
		fmt.Fprintf(&b, "mount -o remount,bind,ro \"$ROOT%s/.boid/hooks\"\n", cfg.ProjectDir)
	}

	// HOME as tmpfs with project re-mount on top
	homeDir := cfg.homeDir()
	b.WriteString("\n# HOME tmpfs\n")
	fmt.Fprintf(&b, "mkdir -p \"$ROOT%s\"\n", homeDir)
	fmt.Fprintf(&b, "mount -t tmpfs tmpfs \"$ROOT%s\"\n", homeDir)
	// Re-mount project directory on top of HOME tmpfs
	fmt.Fprintf(&b, "mkdir -p \"$ROOT%s\"\n", cfg.ProjectDir)
	fmt.Fprintf(&b, "mount --bind %s \"$ROOT%s\"\n", cfg.ProjectDir, cfg.ProjectDir)

	// Additional bindings
	if len(cfg.AdditionalBindings) > 0 {
		b.WriteString("\n# Additional bindings\n")
		for _, bm := range cfg.AdditionalBindings {
			src := bm.Source
			fmt.Fprintf(&b, "if [ -d %s ]; then\n", src)
			fmt.Fprintf(&b, "    mkdir -p \"$ROOT%s\"\n", src)
			fmt.Fprintf(&b, "elif [ -f %s ]; then\n", src)
			fmt.Fprintf(&b, "    mkdir -p \"$(dirname \"$ROOT%s\")\"\n", src)
			fmt.Fprintf(&b, "    touch \"$ROOT%s\"\n", src)
			fmt.Fprintf(&b, "fi\n")
			fmt.Fprintf(&b, "mount --bind %s \"$ROOT%s\"\n", src, src)
			if bm.Mode != "rw" {
				fmt.Fprintf(&b, "mount -o remount,bind,ro \"$ROOT%s\"\n", src)
			}
		}
	}

	// Boid binary + command shims
	b.WriteString("\n# Boid binary + command shims\n")
	b.WriteString("mkdir -p \"$ROOT/opt/boid/bin\"\n")
	fmt.Fprintf(&b, "cp %s \"$ROOT/opt/boid/bin/boid\"\n", cfg.BoidBinary)
	b.WriteString("chmod +x \"$ROOT/opt/boid/bin/boid\"\n")
	for _, cmd := range cfg.HostCommands {
		fmt.Fprintf(&b, "ln -sf boid \"$ROOT/opt/boid/bin/%s\"\n", cmd)
	}

	// Server socket
	b.WriteString("\n# Server socket\n")
	b.WriteString("mkdir -p \"$ROOT/run/boid\"\n")
	b.WriteString("touch \"$ROOT/run/boid/server.sock\"\n")
	fmt.Fprintf(&b, "mount --bind %s \"$ROOT/run/boid/server.sock\"\n", cfg.ServerSocket)

	// Broker socket
	if cfg.BrokerSocket != "" {
		b.WriteString("\n# Broker socket\n")
		b.WriteString("touch \"$ROOT/run/boid/broker.sock\"\n")
		fmt.Fprintf(&b, "mount --bind %s \"$ROOT/run/boid/broker.sock\"\n", cfg.BrokerSocket)
	}

	// Copy inner script into sandbox
	b.WriteString("\n# Copy inner script\n")
	fmt.Fprintf(&b, "cp %s \"$ROOT/tmp/inner.sh\"\n", innerPath)
	b.WriteString("chmod +x \"$ROOT/tmp/inner.sh\"\n")

	// Enter sandbox
	b.WriteString("\n# Enter sandbox\n")
	b.WriteString("exec unshare --user --map-user=1000 --map-group=1000 --root=\"$ROOT\" -- /bin/bash /tmp/inner.sh\n")

	return b.String()
}

// additionalPATH builds PATH entries from additional bindings.
// Paths ending in /bin are added directly; others get /bin appended.
func additionalPATH(bindings []model.BindMount) string {
	var parts []string
	for _, bm := range bindings {
		if strings.HasSuffix(bm.Source, "/bin") {
			parts = append(parts, bm.Source)
		} else {
			parts = append(parts, bm.Source+"/bin")
		}
	}
	return strings.Join(parts, ":")
}

func generateInnerScript(cfg WrapperConfig) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")

	homeDir := cfg.homeDir()
	fmt.Fprintf(&b, "export HOME=%s\n", homeDir)

	if cfg.TaskID != "" {
		fmt.Fprintf(&b, "export BOID_TASK_ID=%s\n", cfg.TaskID)
	}
	fmt.Fprintf(&b, "export BOID_JOB_ID=%s\n", cfg.JobID)

	b.WriteString("export BOID_SOCKET=/run/boid/server.sock\n")
	if cfg.BrokerSocket != "" {
		b.WriteString("export BOID_BROKER_SOCKET=/run/boid/broker.sock\n")
	}
	if cfg.BrokerToken != "" {
		fmt.Fprintf(&b, "export BOID_BROKER_TOKEN=%s\n", cfg.BrokerToken)
	}

	pathPrefix := additionalPATH(cfg.AdditionalBindings)
	basePath := "/opt/boid/bin:/usr/local/bin:/usr/bin:/bin"
	if pathPrefix != "" {
		fmt.Fprintf(&b, "export PATH=%s:%s\n", pathPrefix, basePath)
	} else {
		fmt.Fprintf(&b, "export PATH=%s\n", basePath)
	}

	// Proxy environment variables
	if cfg.ProxyPort > 0 {
		proxyURL := fmt.Sprintf("http://10.0.2.2:%d", cfg.ProxyPort)
		fmt.Fprintf(&b, "export http_proxy=%s\n", proxyURL)
		fmt.Fprintf(&b, "export https_proxy=%s\n", proxyURL)
		fmt.Fprintf(&b, "export HTTP_PROXY=%s\n", proxyURL)
		fmt.Fprintf(&b, "export HTTPS_PROXY=%s\n", proxyURL)
		b.WriteString("export no_proxy=10.0.2.2,10.0.2.3,localhost,127.0.0.1\n")
		b.WriteString("export NO_PROXY=10.0.2.2,10.0.2.3,localhost,127.0.0.1\n")
	}

	for k, v := range cfg.Env {
		fmt.Fprintf(&b, "export %s=%q\n", k, v)
	}

	fmt.Fprintf(&b, "\ncd %s\n\n", cfg.ProjectDir)

	if cfg.Command != "" {
		fmt.Fprintf(&b, "exec %s\n", cfg.Command)
	} else {
		fmt.Fprintf(&b, "trap 'boid job done %s --exit-code $?' EXIT\n", cfg.JobID)
		fmt.Fprintf(&b, "%s/.boid/hooks/%s\n", cfg.ProjectDir, cfg.HookScript)
	}

	return b.String()
}
