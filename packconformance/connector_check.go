package packconformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/integrationpack"
)

// connectorViolation is one finding from findConnectorExecutableViolations.
type connectorViolation struct {
	connector string
	reason    string
}

// findConnectorExecutableViolations is the pure detection half of the
// connector-executable check: every declared connectors[].executable must
// exist on disk, be a regular file, and have at least one executable bit
// set. integrationpack.ParseManifest has already checked the STRING shape
// of Executable (non-empty, relative, no ".." escape); this is the
// filesystem-level follow-up that only makes sense once there is an actual
// Pack directory to look inside.
//
// Separated from checkConnectorsExecutable's *testing.T reporting for the
// same reason as findSkillDocViolations (see its own doc comment): a
// *testing.T subtest failure always propagates to every ancestor test, so
// this package's own tests assert on this function's return value directly
// instead of running the real check against a negative fixture.
func findConnectorExecutableViolations(dir string, m *integrationpack.Manifest) []connectorViolation {
	var violations []connectorViolation
	for _, c := range m.Connectors {
		path := filepath.Join(dir, c.Executable)
		info, err := os.Stat(path)
		if err != nil {
			violations = append(violations, connectorViolation{c.Name, fmt.Sprintf("executable %q: %v", c.Executable, err)})
			continue
		}
		if !info.Mode().IsRegular() {
			violations = append(violations, connectorViolation{c.Name, fmt.Sprintf("executable %q is not a regular file (mode %v)", c.Executable, info.Mode())})
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			violations = append(violations, connectorViolation{c.Name, fmt.Sprintf("executable %q has no executable bit set (mode %v) — chmod +x it", c.Executable, info.Mode().Perm())})
		}
	}
	return violations
}

// checkConnectorsExecutable is findConnectorExecutableViolations'
// *testing.T reporter.
func checkConnectorsExecutable(t *testing.T, dir string, m *integrationpack.Manifest) {
	t.Helper()
	for _, v := range findConnectorExecutableViolations(dir, m) {
		t.Errorf("connector %q: %s", v.connector, v.reason)
	}
}

// connectorLaunchEnv builds the environment evaluateConnectorLaunch runs a
// connector under — the subset of the connector contract env a connector
// can observe before making its first external call, plus PATH (needed for
// the kernel to resolve a "#!/usr/bin/env python3"-style shebang
// interpreter — the real sandbox's own PATH is not this package's concern,
// so this borrows the test process's).
//
// BOID_API_BASE is deliberately set to an address that fails a connection
// attempt IMMEDIATELY and never leaves the host (127.0.0.1, a port nothing
// listens on) rather than left unset or pointed at a real hostname:
//   - unset risks a connector's own code path differing (e.g. skipping the
//     HTTP call entirely) in a way that would tell us nothing about
//     whether it can actually run
//   - a real hostname risks a slow DNS lookup, or worse, an actual
//     external request — explicitly out of bounds for this check
//
// A well-behaved connector reacts to this exactly like a real, temporarily
// unreachable API: it fails fast with a non-zero exit. evaluateConnectorLaunch
// treats that as a PASS — see its own doc comment for the full pass/fail
// boundary this check draws.
func connectorLaunchEnv(execPath, connectorName string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"BOID_SIGNAL_SERVICE=conformance-test",
		"BOID_SIGNAL_CONNECTOR=" + connectorName,
		"BOID_SIGNAL_CONFIG={}",
		"BOID_CONNECTOR_EXEC=" + execPath,
		"BOID_API_BASE=https://127.0.0.1:1/api/conformance-test/conformance-test",
	}
}

// connectorLaunchOutcome is evaluateConnectorLaunch's pure result.
type connectorLaunchOutcome struct {
	// skipped is true when the check couldn't run at all (e.g. the
	// executable is missing) — not itself a violation, since
	// findConnectorExecutableViolations already covers that case.
	skipped    bool
	skipReason string
	// failed is true when the launch itself is the violation (crashed,
	// hung, or never even started).
	failed     bool
	failReason string
}

// evaluateConnectorLaunch is the best-effort half of the connector check:
// actually running the connector and confirming it can launch in this
// environment at all, without requiring it to succeed against a real
// service (impossible without real network access, which this check
// deliberately never attempts) and without requiring any particular exit
// code — a connector correctly detecting an unreachable API base and
// exiting non-zero is doing exactly the right thing, not failing this
// check.
//
// What DOES fail this check (outcome.failed):
//   - exec.Start() itself failing (ENOENT, permission denied, exec format
//     error, missing shebang interpreter, ...) — the connector cannot even
//     be launched in this environment, squarely "起動自体ができない"
//   - the process being killed by a fault signal (SIGSEGV etc.) rather
//     than exiting normally — a real crash, not an ordinary non-zero exit
//   - the process still running after launchTimeout — a well-behaved
//     connector reacting to an immediately-refused connection has no
//     legitimate reason to hang; see connectorLaunchEnv's doc comment for
//     why BOID_API_BASE is built to fail fast rather than slow
//
// stdin is closed — a connector talks to `boid signal ingest` as a
// SEPARATE subprocess this check does not spawn, and never reads its own
// stdin.
//
// If the executable is missing entirely, this returns outcome.skipped
// rather than failed — checkConnectorsExecutable/
// findConnectorExecutableViolations already reports that, and exec.Start()
// failing with ENOENT here would just be a confusing duplicate of the same
// finding.
//
// Separated from checkConnectorLaunchable's *testing.T reporting for the
// same reason as findSkillDocViolations (see its own doc comment).
func evaluateConnectorLaunch(dir string, c integrationpack.Connector, launchTimeout time.Duration) connectorLaunchOutcome {
	execPath, err := filepath.Abs(filepath.Join(dir, c.Executable))
	if err != nil {
		return connectorLaunchOutcome{failed: true, failReason: fmt.Sprintf("resolve executable path: %v", err)}
	}
	if _, err := os.Stat(execPath); err != nil {
		return connectorLaunchOutcome{skipped: true, skipReason: fmt.Sprintf("executable not available (already reported by connector_executable): %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, execPath)
	cmd.Env = connectorLaunchEnv(execPath, c.Name)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// A connector is free to shell out (a `#!/bin/sh` script that forks a
	// grandchild via a plain `sleep`/`curl`/... invocation, not `exec ...`)
	// — exec.CommandContext's DEFAULT Cancel behavior only kills the
	// direct child it started, which does nothing about a grandchild that
	// has already been forked off and reparented. That grandchild then
	// keeps running past this function's return (an orphaned "sleep 999"
	// reproduced this exact hang while writing this check's own tests) and
	// can also keep stdout/stderr's pipe fds open, which makes cmd.Wait()
	// itself hang past the timeout waiting for EOF on them. Setpgid+a
	// custom Cancel that kills the whole process group, plus a bounded
	// WaitDelay, is the fix os/exec's own docs recommend for exactly this
	// failure mode.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return connectorLaunchOutcome{failed: true, failReason: fmt.Sprintf(
			"did not exit within %s (BOID_API_BASE points at an unreachable local port, so a well-behaved connector should fail fast, not hang)\nstdout:\n%s\nstderr:\n%s",
			launchTimeout, stdout.String(), stderr.String())}
	}
	if runErr == nil {
		return connectorLaunchOutcome{} // exited 0 — fine, though not required
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return connectorLaunchOutcome{failed: true, failReason: fmt.Sprintf(
				"was killed by signal %v (looks like a crash, not an ordinary non-zero exit)\nstdout:\n%s\nstderr:\n%s",
				status.Signal(), stdout.String(), stderr.String())}
		}
		// An ordinary non-zero exit — acceptable, see doc comment above.
		return connectorLaunchOutcome{}
	}
	return connectorLaunchOutcome{failed: true, failReason: fmt.Sprintf(
		"failed to launch: %v\nstdout:\n%s\nstderr:\n%s", runErr, stdout.String(), stderr.String())}
}

// checkConnectorLaunchable is evaluateConnectorLaunch's *testing.T
// reporter.
func checkConnectorLaunchable(t *testing.T, dir string, c integrationpack.Connector, launchTimeout time.Duration) {
	t.Helper()
	outcome := evaluateConnectorLaunch(dir, c, launchTimeout)
	if outcome.skipped {
		t.Skip(outcome.skipReason)
	}
	if outcome.failed {
		t.Errorf("connector %q %s", c.Name, outcome.failReason)
	}
}
