package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// renderSetupScript generates the setup shell script from a sandboxPlan.
// innerPath, setupPath, outerPath are the generated script paths (for cleanup).
// rootDir, if non-empty, is used as the sandbox ROOT (pre-created by caller) so
// Go-side cleanup can delete it after the sandbox exits; if empty, the script
// falls back to creating ROOT with mktemp (legacy behavior, leaks on success).
func renderSetupScript(plan *sandboxPlan, rootDir, innerPath, setupPath, outerPath string) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")
	if rootDir != "" {
		fmt.Fprintf(&b, "ROOT=%s\n\n", shellQuote(rootDir))
	} else {
		b.WriteString("ROOT=$(mktemp -d /tmp/boid-root-XXXXXX)\n\n")
	}

	renderCleanup(&b, plan.CleanupPaths, innerPath, setupPath, outerPath)

	for _, m := range plan.Mounts {
		renderMount(&b, m)
	}

	for _, f := range plan.Files {
		renderFile(&b, f)
	}

	if len(plan.NFTRules) > 0 {
		for _, rule := range plan.NFTRules {
			b.WriteString(rule)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	for _, s := range plan.Symlinks {
		renderSymlink(&b, s)
	}

	// Copy inner script into sandbox
	fmt.Fprintf(&b, "\ncp %s \"$ROOT/tmp/inner.sh\"\n", shellQuote(innerPath))
	b.WriteString("chmod +x \"$ROOT/tmp/inner.sh\"\n")

	// Enter sandbox
	b.WriteString("\nexec unshare --user --map-user=1000 --map-group=1000 --root=\"$ROOT\" -- /bin/bash /tmp/inner.sh\n")

	return b.String()
}

func renderCleanup(b *strings.Builder, cleanupPaths []string, innerPath, setupPath, outerPath string) {
	fmt.Fprintf(b, `cleanup() {
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
`, shellQuote(outerPath), shellQuote(setupPath), shellQuote(innerPath))
	for _, p := range cleanupPaths {
		fmt.Fprintf(b, "    rm -rf %s\n", shellQuote(p))
	}
	b.WriteString("}\ntrap cleanup EXIT\n")
}

func renderMount(b *strings.Builder, m Mount) {
	indent := ""

	if m.Guard != "" {
		fmt.Fprintf(b, "if [ %s ]; then\n", m.Guard)
		indent = "    "
	}

	// Create target
	if m.DetectType {
		fmt.Fprintf(b, "%sif [ -d %s ]; then\n", indent, shellQuote(m.Source))
		fmt.Fprintf(b, "%s    mkdir -p \"$ROOT%s\"\n", indent, m.Target)
		fmt.Fprintf(b, "%selif [ -f %s ]; then\n", indent, shellQuote(m.Source))
		fmt.Fprintf(b, "%s    mkdir -p \"$(dirname \"$ROOT%s\")\"\n", indent, m.Target)
		fmt.Fprintf(b, "%s    touch \"$ROOT%s\"\n", indent, m.Target)
		fmt.Fprintf(b, "%sfi\n", indent)
	} else if m.IsFile {
		dir := filepath.Dir(m.Target)
		fmt.Fprintf(b, "%smkdir -p \"$ROOT%s\"\n", indent, dir)
		fmt.Fprintf(b, "%stouch \"$ROOT%s\"\n", indent, m.Target)
	} else {
		fmt.Fprintf(b, "%smkdir -p \"$ROOT%s\"\n", indent, m.Target)
	}

	// Mount command
	switch m.Type {
	case MountBind:
		fmt.Fprintf(b, "%smount --bind %s \"$ROOT%s\"\n", indent, shellQuote(m.Source), m.Target)
	case MountRBind:
		fmt.Fprintf(b, "%smount --rbind %s \"$ROOT%s\"\n", indent, shellQuote(m.Source), m.Target)
	case MountTmpfs:
		fmt.Fprintf(b, "%smount -t tmpfs tmpfs \"$ROOT%s\"\n", indent, m.Target)
	}

	// Post-mount operations
	if m.Slave {
		fmt.Fprintf(b, "%smount --make-rslave \"$ROOT%s\"\n", indent, m.Target)
	}
	for _, d := range m.NeedsDirs {
		fmt.Fprintf(b, "%smkdir -p \"$ROOT%s/%s\"\n", indent, m.Target, d)
	}
	if m.ReadOnly {
		fmt.Fprintf(b, "%smount -o remount,bind,ro \"$ROOT%s\"\n", indent, m.Target)
	}

	if m.Guard != "" {
		b.WriteString("fi\n")
	}
}

func renderFile(b *strings.Builder, f FileWrite) {
	dir := filepath.Dir(f.Path)
	fmt.Fprintf(b, "mkdir -p \"$ROOT%s\"\n", dir)
	fmt.Fprintf(b, "printf '%%s' %s > \"$ROOT%s\"\n", shellQuote(f.Content), f.Path)
}

func renderSymlink(b *strings.Builder, s Symlink) {
	fmt.Fprintf(b, "ln -sf %s \"$ROOT%s\"\n", shellQuote(s.LinkTarget), s.LinkPath)
}
