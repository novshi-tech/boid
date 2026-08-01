// Package selfuser implements the runtime half of "決定 1 の実装形" in
// docs/plans/release-onboarding.md: the arbitrary-uid image
// (build/container/Dockerfile) no longer bakes a fixed `useradd --uid
// ${BOID_UID}` entry, so a container started as an arbitrary `--user
// <uid>:0` (the OpenShift-style gid-0 convention) has no matching
// /etc/passwd row. Tooling that does an os/user-style lookup for the
// running uid (ssh, some git credential helpers, `id`, coreutils' `whoami`)
// breaks on that unknown-uid the moment it is asked, so both Go entrypoints
// that can be that "first thing to run in the container" — `boid start`
// (cmd/start.go, the compose daemon service) and `boid runner-container`
// (cmd/runner_container.go, every job container's own entrypoint) — call
// EnsureRuntimeUserRegistered() once at startup to close that gap.
//
// This mirrors the plan doc's shell-level sketch:
//
//	if ! getent passwd "$(id -u)" >/dev/null; then
//	  echo "boid:x:$(id -u):$(id -g)::/home/boid:/bin/bash" >> /etc/passwd
//	fi
//
// done in Go per the doc's explicit recommendation (§決定 1 の実装形 1 —
// "Go 側推奨"), plus PasswdSelfRegisterShellSnippet below for the one
// consumer Go-side registration cannot reach: the workspace init.sh
// wrapper, whose container overrides the image ENTRYPOINT with a bare
// `/bin/bash -s` and so never runs boid's own Go code at all (docs/plans/
// release-onboarding.md's "届かない第 3 の consumer").
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
// container overrides the image ENTRYPOINT with `/bin/bash -s`
// (internal/dispatcher/container_backend_workspace_init.go). init.sh
// commonly touches toolchain installers that do an `id`/ssh/git passwd
// lookup (go/volta/claude/codex install scripts), so the same registration
// this package does in Go has to happen here too, ahead of the user's own
// script.
//
// Deliberately silent and non-fatal on every branch: a getent hit is a
// no-op (the common case — most non-container callers of this wrapper,
// e.g. a developer running init.sh by hand outside a container, already
// have a real passwd entry for their uid), and a failed append (permission
// denied, read-only /etc/passwd, ...) must not abort the wrapper — the
// workspace init flow has no `set -e` and every other stage checks its own
// exit status explicitly (buildWorkspaceInitScript's own doc comment); this
// is not one of those stages and should never be able to fail the run on
// its own.
const PasswdSelfRegisterShellSnippet = "" +
	"# arbitrary-uid self-registration (docs/plans/release-onboarding.md\n" +
	"# 決定1/PR2, internal/selfuser package): this container's runtime uid\n" +
	"# may have no /etc/passwd entry (the arbitrary-uid image bakes none),\n" +
	"# which breaks id/ssh/git passwd lookups the toolchain installers below\n" +
	"# may do. Best-effort and silent — see this file's own header comment.\n" +
	"getent passwd \"$(id -u)\" >/dev/null 2>&1 || " +
	"echo \"boid:x:$(id -u):$(id -g)::${HOME:-/home/boid}:/bin/bash\" >> /etc/passwd 2>/dev/null || true\n"

// EnsureRuntimeUserRegistered is the Go-side self-registration call
// (docs/plans/release-onboarding.md §決定1 実装形1's "Go 側推奨"). It is
// best-effort and never fails the caller's startup: on any error (most
// commonly a bare-host `boid start` where the process's real uid already
// has a passwd entry and nothing needs doing, or one where /etc/passwd
// isn't writable by this process at all) it logs at Debug and returns,
// exactly the "log-and-continue" contract EnsurePasswdEntry documents.
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

// ApplyGroupWritableUmask sets the process umask to 002 (docs/plans/
// release-onboarding.md §決定1 実装形2's recommended fix for
// runtime-generated files): `chmod g=u` at image-build time only reaches
// directories that exist at build time, but boid.db/its WAL files/the
// job-container TLS material under BOID_RUNTIME_DIR are all created at
// RUNTIME, under whatever umask the process that creates them is running
// with. The default umask (022) clears the group-write bit, so a file one
// uid creates comes out unreadable-for-write by any OTHER uid running with
// the same supplementary group 0 — silently reintroducing the single-fixed-
// uid assumption gid-0 arbitrary-uid was supposed to remove. 002 keeps
// owner permissions exactly as before (it only ever CLEARS bits, never
// sets them) and additionally clears the group-write-deny bit, so a
// default-mode (0644/0755) caller picks up group-write.
//
// This does NOT cover everything, and callers should not read it as
// "any uid change is now safe": secret.key and the internal mTLS CA
// key/cert (internal/dispatcher/secret_keyfile.go, internal/mtls/ca.go)
// are written 0600 — OWNER-ONLY, with no group bits requested in the
// first place — so umask 002 has nothing to preserve there; a umask can
// only CLEAR bits a request already has, never add ones it doesn't
// (build/container/.env.example's BOID_UID comment documents this
// explicitly as the "fixed per install" contract's one real exception).
//
// For everything else, this makes "uid is fixed per install" (docs/plans/
// release-onboarding.md's 未決6) a real contract rather than an
// accidental one: a SECOND uid, still in group 0, can still write files
// the first uid created, but only because of this umask — a process that
// skips this call and later runs as a different uid will find those files
// group-read-only and fail exactly the way the plan doc's 論点 3 warns
// about.
func ApplyGroupWritableUmask() {
	syscall.Umask(0o002)
}
