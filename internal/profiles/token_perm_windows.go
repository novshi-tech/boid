//go:build windows

package profiles

import "io/fs"

// warnIfTokenPermsLoose is deliberately a no-op on Windows.
//
// Go's os package does not map file modes to Windows ACLs: it sets only
// the read-only attribute, and os.Stat reports every writable file as
// 0666. So the POSIX check would fire on EVERY token file, on EVERY
// command invocation, and the remediation it names (`chmod 600`) does not
// exist on the platform. That was the real, observed behaviour before this
// no-op — a permanent warning banner above every command's output, telling
// the user to run an impossible command about a mode bit that carries no
// information:
//
//	WARN token file has looser permissions than required; run chmod 600
//	     path=C:\Users\...\boid\tokens\boid.json mode=-rw-rw-rw- want=-rw-------
//
// A warning that cannot be acted on and cannot be silenced is worse than
// no warning: it trains the reader to skip warnings, including the ones
// that matter.
//
// Silence here is NOT a claim that the token is protected. It is not — see
// TokenProtectionNote below, which says so at the one moment the user can
// do something about it. Actually restricting the file needs an explicit
// DACL granting only the current user's SID at create time; that is real
// work and is tracked in docs/plans/windows-client-build.md's 残件.
func warnIfTokenPermsLoose(string, fs.FileMode) {}

// TokenProtectionNote returns the Windows caveat, shown once after a
// successful `boid login` rather than on every command: the daemon Bearer
// token this just wrote is not ACL-restricted, so on a shared machine
// another local account can read it.
//
// Once, at login, is the right moment — that is when the token is created,
// when the user is already thinking about credentials, and when "use a
// machine you don't share" is still an option they can take.
func TokenProtectionNote() string {
	return "note: on Windows this token file is not ACL-restricted — Go sets only the read-only attribute, " +
		"so another local account on this machine can read it. Prefer a machine you do not share."
}
