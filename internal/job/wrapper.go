package job

import (
	"fmt"
	"os"
	"strings"
)

// WrapperConfig holds the parameters for sandbox script generation.
type WrapperConfig struct {
	JobID        string
	ProjectID    string
	ProjectDir   string            // host-side project directory
	HooksDir     string            // host-side hooks directory
	HookScript   string            // script filename, e.g. "run-build.sh"
	BoidBinary   string            // host-side path to boid binary
	ServerSocket string            // host-side server socket path
	Env          map[string]string // project environment variables
	HostCommands []string          // commands to shim via symlinks
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
	outer := generateOuterScript(setupPath)

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

func generateOuterScript(setupPath string) string {
	return fmt.Sprintf(`#!/bin/bash
set -e
exec pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 --no-map-gw \
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
    # Unmount all bind mounts under $ROOT
    umount -R "$ROOT" 2>/dev/null || true
    # Safety: only rm if no mounts remain (prevent deleting host files via stale bind mounts)
    if ! findmnt --submounts --noheadings --output TARGET "$ROOT" | grep -q .; then
        rm -rf "$ROOT"
    else
        echo "WARNING: mounts still active under $ROOT, skipping rm" >&2
    fi
    rm -f %s %s %s
}
trap cleanup EXIT
`, outerPath, setupPath, innerPath)
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

	// Workspace (tmpfs)
	b.WriteString("# Workspace\n")
	b.WriteString("mkdir -p \"$ROOT/workspace\"\n")
	b.WriteString("mount -t tmpfs tmpfs \"$ROOT/workspace\"\n")

	// Project directory (rw)
	fmt.Fprintf(&b, "mkdir -p \"$ROOT/workspace/%s\"\n", cfg.ProjectID)
	fmt.Fprintf(&b, "mount --bind %s \"$ROOT/workspace/%s\"\n", cfg.ProjectDir, cfg.ProjectID)

	// Hooks directory (ro)
	b.WriteString("mkdir -p \"$ROOT/workspace/.boid/hooks\"\n")
	fmt.Fprintf(&b, "mount --bind %s \"$ROOT/workspace/.boid/hooks\"\n", cfg.HooksDir)
	b.WriteString("mount -o remount,bind,ro \"$ROOT/workspace/.boid/hooks\"\n")

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

	// Copy inner script into sandbox
	b.WriteString("\n# Copy inner script\n")
	fmt.Fprintf(&b, "cp %s \"$ROOT/tmp/inner.sh\"\n", innerPath)
	b.WriteString("chmod +x \"$ROOT/tmp/inner.sh\"\n")

	// Enter sandbox
	b.WriteString("\n# Enter sandbox\n")
	b.WriteString("exec chroot \"$ROOT\" /bin/bash /tmp/inner.sh\n")

	return b.String()
}

func generateInnerScript(cfg WrapperConfig) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")
	b.WriteString("export HOME=/workspace\n")
	b.WriteString("export BOID_SOCKET=/run/boid/server.sock\n")
	b.WriteString("export PATH=/opt/boid/bin:/usr/local/bin:/usr/bin:/bin\n")

	for k, v := range cfg.Env {
		fmt.Fprintf(&b, "export %s=%q\n", k, v)
	}

	fmt.Fprintf(&b, "\ncd /workspace/%s\n\n", cfg.ProjectID)
	fmt.Fprintf(&b, "trap 'boid job done %s --exit-code $?' EXIT\n", cfg.JobID)
	fmt.Fprintf(&b, "exec /workspace/.boid/hooks/%s\n", cfg.HookScript)

	return b.String()
}
