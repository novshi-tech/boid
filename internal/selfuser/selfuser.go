// Package selfuser registers a synthetic /etc/passwd entry for the
// container's runtime uid: the arbitrary-uid image no longer bakes a fixed
// `useradd` entry, so a container started as an arbitrary `--user <uid>:0`
// (the OpenShift-style gid-0 convention) has no matching /etc/passwd row,
// which breaks any tooling that does an os/user-style lookup (ssh, some git
// credential helpers, `id`, coreutils' `whoami`). Both Go entrypoints that
// can be the first thing to run in the container — `boid start` and `boid
// runner-container` — call EnsureRuntimeUserRegistered() at startup to
// close that gap; PasswdSelfRegisterShellSnippet covers the one consumer
// Go-side registration cannot reach (see its own doc comment).
package selfuser

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// DefaultPasswdPath is the system passwd file EnsureRuntimeUserRegistered
// targets. A package var (not a literal) purely so tests can point
// EnsurePasswdEntry at a scratch file instead of the real /etc/passwd.
const DefaultPasswdPath = "/etc/passwd"

// DefaultShell is the login shell recorded for a self-registered entry —
// matches the plan doc's sketch and the image's own baked entry it
// replaces (build/container/Dockerfile no longer bakes one; this is the
// runtime equivalent).
const DefaultShell = "/bin/bash"

// PasswdSelfRegisterShellSnippet is the shell-level equivalent of
// EnsureRuntimeUserRegistered, for the one consumer that never runs boid's
// own Go code: the workspace init.sh wrapper
// (internal/dispatcher/workspace_init.go's buildWorkspaceInitScript), whose
// container overrides the image ENTRYPOINT with `/bin/bash -s`. init.sh
// commonly touches toolchain installers that do an `id`/ssh/git passwd
// lookup, so the same registration this package does in Go has to happen
// here too, ahead of the user's own script.
//
// Deliberately silent and non-fatal on every branch: a getent hit is a
// no-op, and a failed append (permission denied, read-only /etc/passwd,
// ...) must not abort the wrapper — the workspace init flow has no `set -e`
// and this is not one of the stages that checks its own exit status.
const PasswdSelfRegisterShellSnippet = "" +
	"# arbitrary-uid self-registration (docs/plans/release-onboarding.md\n" +
	"# 決定1/PR2, internal/selfuser package): this container's runtime uid\n" +
	"# may have no /etc/passwd entry (the arbitrary-uid image bakes none),\n" +
	"# which breaks id/ssh/git passwd lookups the toolchain installers below\n" +
	"# may do. Best-effort and silent — see this file's own header comment.\n" +
	"getent passwd \"$(id -u)\" >/dev/null 2>&1 || " +
	"echo \"boid:x:$(id -u):$(id -g)::${HOME:-/home/boid}:/bin/bash\" >> /etc/passwd 2>/dev/null || true\n"

// EnsureRuntimeUserRegistered is the Go-side self-registration call. It is
// best-effort and never fails the caller's startup: on any error (most
// commonly a bare-host `boid start` where the process's real uid already
// has a passwd entry and nothing needs doing, or one where /etc/passwd
// isn't writable by this process at all) it logs at Debug and returns.
func EnsureRuntimeUserRegistered() {
	uid, gid := os.Getuid(), os.Getgid()
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/boid"
	}
	registered, err := EnsurePasswdEntry(DefaultPasswdPath, uid, gid, home, DefaultShell)
	switch {
	case err != nil:
		slog.Debug("selfuser: passwd self-registration skipped", "uid", uid, "gid", gid, "error", err)
	case registered:
		slog.Info("selfuser: registered /etc/passwd entry for runtime uid", "uid", uid, "gid", gid, "home", home)
	}
}

// EnsurePasswdEntry ensures passwdPath has an entry whose uid field (the
// third colon-separated field) equals uid. If one is already present, this
// is a no-op (false, nil). If none is present, it appends a synthesized
// "boid:x:<uid>:<gid>::<home>:<shell>" line and returns (true, nil) on
// success.
//
// Any I/O error — the file doesn't exist, isn't readable, or (the common
// real-world case: a bare-host `boid start` running as an unprivileged
// user against the SYSTEM /etc/passwd) isn't writable by this process —
// is returned rather than swallowed, so a caller can decide how loudly to
// report it; EnsureRuntimeUserRegistered's own policy is "log at Debug and
// move on", not "fail startup".
func EnsurePasswdEntry(passwdPath string, uid, gid int, home, shell string) (registered bool, err error) {
	has, err := passwdHasUID(passwdPath, uid)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}

	line := fmt.Sprintf("boid:x:%d:%d::%s:%s\n", uid, gid, home, shell)
	f, err := os.OpenFile(passwdPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return false, fmt.Errorf("selfuser: open %s for append: %w", passwdPath, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return false, fmt.Errorf("selfuser: append entry to %s: %w", passwdPath, err)
	}
	return true, nil
}

// passwdHasUID reports whether passwdPath contains a line whose uid field
// (colon-separated field index 2, 0-based) equals uid. Blank lines and `#`
// comments (glibc tolerates both in /etc/passwd) are skipped rather than
// mis-parsed as a malformed entry.
func passwdHasUID(passwdPath string, uid int) (bool, error) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return false, fmt.Errorf("selfuser: open %s: %w", passwdPath, err)
	}
	defer f.Close()

	target := strconv.Itoa(uid)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		if fields[2] == target {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("selfuser: scan %s: %w", passwdPath, err)
	}
	return false, nil
}

// ApplyGroupWritableUmask sets the process umask to 002 so runtime-created
// files (boid.db and its WAL files, job-container TLS material under
// BOID_RUNTIME_DIR) stay group-writable: the default umask (022) clears the
// group-write bit, so a file one uid creates would otherwise be
// unreadable-for-write by any OTHER uid running with the same
// supplementary group 0 — reintroducing the single-fixed-uid assumption
// gid-0 arbitrary-uid was meant to remove. 002 only ever clears bits, never
// sets them, so owner permissions are unaffected.
//
// This does not cover files written 0600 (owner-only) in the first
// place, such as secret.key and the internal mTLS CA key/cert — a umask
// can only clear bits a request already has, never add ones it doesn't.
func ApplyGroupWritableUmask() {
	syscall.Umask(0o002)
}
