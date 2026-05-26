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
//
// Cleanup is deliberately not handled here. Bind mounts created below live in
// the caller-provided `unshare --mount` namespace and are reclaimed by the
// kernel when the namespace is torn down (i.e. when this script's bash exits).
// Any leftover scaffolding under ROOT, the script files, and CleanupPaths are
// removed from outer.sh after pasta returns, where the namespace is already
// gone and rm cannot accidentally traverse a still-active bind mount.
func renderSetupScript(plan *sandboxPlan, rootDir, innerPath, setupPath, outerPath string) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")
	if rootDir != "" {
		fmt.Fprintf(&b, "ROOT=%s\n\n", shellQuote(rootDir))
	} else {
		b.WriteString("ROOT=$(mktemp -d /tmp/boid-root-XXXXXX)\n\n")
	}

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

func renderMount(b *strings.Builder, m Mount) {
	indent := ""

	if m.Guard != "" {
		fmt.Fprintf(b, "if [ %s ]; then\n", m.Guard)
		indent = "    "
	}

	// Create target
	if m.DetectType {
		// `-e` (exists) rather than `-f` (regular file) so sockets, FIFOs and
		// device nodes land in the file-like branch. The prior `-f` form
		// silently skipped mountpoint creation for sockets (e.g. cetusguard's
		// docker.sock), causing the subsequent `mount --bind` to fail.
		fmt.Fprintf(b, "%sif [ -d %s ]; then\n", indent, shellQuote(m.Source))
		fmt.Fprintf(b, "%s    mkdir -p \"$ROOT%s\"\n", indent, m.Target)
		fmt.Fprintf(b, "%selif [ -e %s ]; then\n", indent, shellQuote(m.Source))
		fmt.Fprintf(b, "%s    mkdir -p \"$(dirname \"$ROOT%s\")\"\n", indent, m.Target)
		fmt.Fprintf(b, "%s    [ -e \"$ROOT%s\" ] || touch \"$ROOT%s\"\n", indent, m.Target, m.Target)
		fmt.Fprintf(b, "%sfi\n", indent)
	} else if m.IsFile {
		// `touch` unconditionally fails with EACCES when the target already
		// exists via a base rbind and is owned by root (e.g. /usr/bin/git
		// surfaced through the /usr rbind). Skip the touch when the path is
		// already present — `mount --bind` only needs the target to exist.
		dir := filepath.Dir(m.Target)
		fmt.Fprintf(b, "%smkdir -p \"$ROOT%s\"\n", indent, dir)
		fmt.Fprintf(b, "%s[ -e \"$ROOT%s\" ] || touch \"$ROOT%s\"\n", indent, m.Target, m.Target)
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
