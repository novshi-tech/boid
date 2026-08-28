package dispatcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file covers the WRAPPER SCRIPT half of PR5 of
// docs/plans/workspace-home-volume-persistence.md (論点 b-2 / 論点 c): the
// text boid assembles from the workspace's own init.sh plus a builtin prelude
// (bind-target skeleton), and feeds to `bash -s` on a throwaway container's
// stdin.
//
// PR6 removed the postlude these tests also covered. It stamped a home
// identity token into a file inside the home, which nothing could read once the
// home became a docker named volume; the identity moved onto the volume's own
// label instead (workspaceHomeMarker.HomeID), and the tests for it moved to
// workspace_home_volume_test.go.
//
// Every test here runs that text through a REAL bash. That is deliberate and
// not redundant with the fake-docker tests in
// container_backend_workspace_init_test.go, which pin what boid asks the
// engine for (name, labels, mounts, uid, userns, network) but would happily
// pass on a wrapper whose heredoc never terminates, whose exit code is
// swallowed, or whose prelude never creates the skeleton. Those are exactly
// the regressions the pre-PR5 tests caught by executing init.sh directly, and
// they have to keep being caught somewhere.
//
// What a real bash here canNOT stand in for is the container itself — there is
// no mount, so the "host side" and the "container side" of the workspace home
// are one and the same directory. Tests therefore build the request with
// HomeSource == HomeTarget; see runWorkspaceInitWrapper.

// runWorkspaceInitWrapper executes an assembled wrapper the way the init
// container does — `bash -s` with the script on stdin, no TTY, no cwd of its
// own (the wrapper cd's itself) — and returns the exit code plus the combined
// stdout/stderr.
//
// PATH is added on top of req.Env, which deliberately carries none: inside a
// container the image's own PATH applies (see buildWorkspaceInitEnv), and this
// harness has no image, so it lends the test process's own. Nothing else is
// inherited, matching the request's allowlist exactly.
func runWorkspaceInitWrapper(t *testing.T, req WorkspaceInitRequest) (exitCode int, output string) {
	t.Helper()
	cmd := exec.Command("/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(req.Script)
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	cmd.Env = append(env, "PATH="+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), out.String()
	}
	t.Fatalf("run wrapper: %v\n%s", err, out.String())
	return 0, ""
}

// newWorkspaceInitRequestForBash builds a request whose home is a real
// directory this test process owns, so the wrapper's own writes (the skeleton)
// and the user script's writes land somewhere the test can inspect.
//
// HomeSource is deliberately NOT that directory: in production it is a docker
// volume NAME, and the value never reaches the wrapper text — only HomeTarget
// does, through BOID_WORKSPACE_HOME. Passing a name here keeps the fixture
// shaped like production; passing the directory would hide a wrapper that
// started depending on the source.
func newWorkspaceInitRequestForBash(t *testing.T, script string, scriptExists bool) (WorkspaceInitRequest, string) {
	t.Helper()
	homeDir := t.TempDir()
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		Script:       []byte(script),
		ScriptExists: scriptExists,
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}
	return req, homeDir
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- 1. hash した bytes == 実行した bytes (PR #787 の契約を新経路で維持) ---

// TestBuildWorkspaceInitRequest_RunsTheHashedBytesVerbatim is the new-route
// statement of the contract PR #787 established for the host-exec route: the
// bytes recorded in the completion marker's sha256 are the bytes that actually
// ran. The old route made that true by writing scriptBytes to a temp file and
// exec'ing it; this one has to make it true THROUGH a quoted heredoc, which is
// where it can silently stop being true — a heredoc always terminates its last
// line, so a script with no trailing newline picks up a byte unless the
// wrapper trims it back.
//
// The script under test copies its own $0 into the home, so the comparison is
// against what bash was actually handed rather than against the wrapper text.
func TestBuildWorkspaceInitRequest_RunsTheHashedBytesVerbatim(t *testing.T) {
	const copySelf = `cp -- "$0" "$BOID_WORKSPACE_HOME/executed"`
	cases := []struct {
		name   string
		script string
	}{
		{"trailing newline", copySelf + "\n"},
		{"no trailing newline", copySelf},
		{"shebang and trailing newline", "#!/usr/bin/env bash\nset -eu\n" + copySelf + "\n"},
		{"two trailing newlines", copySelf + "\n\n"},
		{"blank first line", "\n" + copySelf + "\n"},
		{"CRLF line endings", "set -eu\r\n" + copySelf + "\n"},
		{"trailing comment without newline", copySelf + "\n# done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, homeDir := newWorkspaceInitRequestForBash(t, tc.script, true)
			code, out := runWorkspaceInitWrapper(t, req)
			if code != 0 {
				t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
			}
			got, err := os.ReadFile(filepath.Join(homeDir, "executed"))
			if err != nil {
				t.Fatalf("read the copy of $0 the script made: %v", err)
			}
			if string(got) != tc.script {
				t.Errorf("executed bytes differ from the hashed bytes\n got %q (sha %s)\nwant %q (sha %s)",
					got, sha256Hex(got), tc.script, sha256Hex([]byte(tc.script)))
			}
		})
	}
}

// TestBuildWorkspaceInitRequest_UserScriptCannotBreakOutOfTheHeredoc pins §D2.
// The user script is never concatenated into the wrapper — it is carried by a
// quoted heredoc whose delimiter is freshly random per run — so no amount of
// unbalanced quoting, command substitution or delimiter-lookalike text in it
// can change what the wrapper itself executes.
//
// Assertions are on both halves, because either alone is satisfiable by a
// broken implementation: the wrapper TEXT must contain the payload verbatim
// between the delimiter lines (a "sanitizing" implementation would fail here),
// and running it must deliver those exact bytes to a child bash without the
// wrapper shell ever evaluating them (an implementation that concatenated
// would fail here, usually with a syntax error rather than a wrong file).
func TestBuildWorkspaceInitRequest_UserScriptCannotBreakOutOfTheHeredoc(t *testing.T) {
	const copySelf = `cp -- "$0" "$BOID_WORKSPACE_HOME/executed"` + "\n"
	cases := []struct {
		name   string
		script string
	}{
		{"classic delimiters", "EOF\nEOT\nBOID_INIT_EOF\n" + copySelf},
		{"unbalanced single quote in a comment", "# it's fine\n" + copySelf},
		{"command substitution", "X=$(printf pwned)\nY=`printf pwned`\n" + copySelf},
		{"dollar and backslash", `A='\$HOME'` + "\nB=\"\\\\\"\n" + copySelf},
		{"heredoc of its own", "cat <<'INNER' >/dev/null\nanything\nINNER\n" + copySelf},
		{"line that only says exit", "false || true\n" + copySelf},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, homeDir := newWorkspaceInitRequestForBash(t, tc.script, true)

			body, ok := heredocBodyOf(t, req.Script)
			if !ok {
				t.Fatal("no quoted heredoc found in the assembled wrapper")
			}
			if body != tc.script {
				t.Errorf("heredoc body = %q, want the payload verbatim %q", body, tc.script)
			}

			code, out := runWorkspaceInitWrapper(t, req)
			if code != 0 {
				t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
			}
			got, err := os.ReadFile(filepath.Join(homeDir, "executed"))
			if err != nil {
				t.Fatalf("read the copy of $0 the script made: %v", err)
			}
			if string(got) != tc.script {
				t.Errorf("delivered bytes = %q, want %q", got, tc.script)
			}
		})
	}
}

// heredocBodyOf extracts the text between the wrapper's `<<'DELIM'` line and
// its terminating `DELIM` line. Deliberately a dumb textual scan rather than a
// call back into the generator: a test that re-derived the delimiter the same
// way the implementation does would agree with any implementation, including
// one that stopped using a heredoc at all.
func heredocBodyOf(t *testing.T, wrapper string) (string, bool) {
	t.Helper()
	const open = "<<'"
	i := strings.Index(wrapper, open)
	if i < 0 {
		return "", false
	}
	rest := wrapper[i+len(open):]
	j := strings.Index(rest, "'\n")
	if j < 0 {
		return "", false
	}
	delim := rest[:j]
	body := rest[j+2:]
	end := strings.Index(body, "\n"+delim+"\n")
	if end < 0 {
		// The body may be empty, in which case the delimiter line follows
		// immediately.
		if strings.HasPrefix(body, delim+"\n") {
			return "", true
		}
		return "", false
	}
	return body[:end+1], true
}

// TestBuildWorkspaceInitRequest_HeredocDelimiterIsFreshAndUnguessable pins the
// property that makes the containment above hold against a DELIBERATE attempt,
// not just an accidental collision: the delimiter is minted per run from
// crypto/rand, so a script author cannot embed a line equal to it. A constant
// delimiter (or one derived from the slug) passes every other test in this
// file and fails only this one.
func TestBuildWorkspaceInitRequest_HeredocDelimiterIsFreshAndUnguessable(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 8; i++ {
		req, _ := newWorkspaceInitRequestForBash(t, "true\n", true)
		body, ok := heredocBodyOf(t, req.Script)
		if !ok || body != "true\n" {
			t.Fatalf("heredoc body = %q ok=%v, want %q", body, ok, "true\n")
		}
		delim := delimiterOf(t, req.Script)
		if len(delim) < len(workspaceInitHeredocPrefix)+32 {
			t.Errorf("delimiter %q is only %d chars; want a %s prefix plus at least 32 random hex characters",
				delim, len(delim), workspaceInitHeredocPrefix)
		}
		if seen[delim] {
			t.Fatalf("delimiter %q repeated across runs — it must be minted per run, not derived from the request", delim)
		}
		seen[delim] = true
	}
}

func delimiterOf(t *testing.T, wrapper string) string {
	t.Helper()
	const open = "<<'"
	i := strings.Index(wrapper, open)
	if i < 0 {
		t.Fatal("no quoted heredoc in the wrapper")
	}
	rest := wrapper[i+len(open):]
	j := strings.Index(rest, "'\n")
	if j < 0 {
		t.Fatal("unterminated heredoc opener in the wrapper")
	}
	return rest[:j]
}

// TestBuildWorkspaceInitRequest_RejectsAScriptItCannotCarryVerbatim pins the
// two fail-closed refusals. A NUL byte cannot survive delivery through a shell
// heredoc on a text stream, and a script containing the (random) delimiter as
// a line of its own would silently truncate the payload. Both are practically
// unreachable — the second has a 2^-128 chance and the first means the
// workspace's init.sh is not a shell script at all — which is exactly why they
// must be errors rather than assumptions: an unnoticed truncation here runs a
// PREFIX of the script the marker's hash vouches for.
func TestBuildWorkspaceInitRequest_RejectsAScriptItCannotCarryVerbatim(t *testing.T) {
	homeDir := t.TempDir()
	base := workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		ScriptExists: true,
		HomeID:       "abc",
	}

	base.Script = []byte("echo hi\x00\n")
	if _, err := buildWorkspaceInitRequest(base); err == nil {
		t.Error("buildWorkspaceInitRequest accepted a script containing a NUL byte, want an error")
	}

	// The delimiter check, exercised by forcing the generator to reuse a
	// delimiter the script already contains.
	base.Script = []byte("line\n" + workspaceInitHeredocPrefix + "deadbeef\nmore\n")
	base.heredocDelimiter = workspaceInitHeredocPrefix + "deadbeef"
	if _, err := buildWorkspaceInitRequest(base); err == nil {
		t.Error("buildWorkspaceInitRequest accepted a script containing the heredoc delimiter as a line, want an error")
	}
}

// --- 2. prelude (論点 b-2 の skeleton mkdir) ---

// TestBuildWorkspaceInitRequest_PreludeCreatesTheBindTargetSkeleton pins the
// half of PR5 that has nothing to do with init.sh: the directories a job
// container's per-skill bind mounts need must exist BEFORE that container
// starts, or the engine creates the whole missing path as uid 0 and the uid
// 1000 harness can never write ~/.claude/.credentials.json again
// (論点 b-2's measurement on podman 4.9.3).
func TestBuildWorkspaceInitRequest_PreludeCreatesTheBindTargetSkeleton(t *testing.T) {
	req, homeDir := newWorkspaceInitRequestForBash(t, "", false)
	code, out := runWorkspaceInitWrapper(t, req)
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}
	for _, rel := range workspaceHomeSkeletonDirs() {
		info, err := os.Stat(filepath.Join(homeDir, rel))
		if err != nil {
			t.Errorf("skeleton dir %q not created: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("skeleton entry %q is %s, want a directory", rel, info.Mode())
		}
	}
}

// TestBuildWorkspaceInitRequest_PreludeRunsBeforeTheUserScript pins the
// ordering. A prelude that ran after init.sh would still leave the right
// directories behind, so a "do they exist at the end" check cannot see the
// difference — the user script has to observe them itself.
func TestBuildWorkspaceInitRequest_PreludeRunsBeforeTheUserScript(t *testing.T) {
	script := `[ -d "$BOID_WORKSPACE_HOME/.claude/skills" ] || exit 17` + "\n"
	req, _ := newWorkspaceInitRequestForBash(t, script, true)
	code, out := runWorkspaceInitWrapper(t, req)
	if code == 17 {
		t.Fatalf("init.sh observed no skeleton — the prelude must run first\n%s", out)
	}
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}
}

// TestBuildWorkspaceInitRequest_PreludeFailureIsDistinguishable pins §D3's
// requirement that the operator can tell WHICH stage failed. The home is made
// unreachable so the prelude's own cd/mkdir fails; nothing of the user script
// may run, and the failure must not be reported as init.sh's.
func TestBuildWorkspaceInitRequest_PreludeFailureIsDistinguishable(t *testing.T) {
	req, homeDir := newWorkspaceInitRequestForBash(t, `touch "$BOID_WORKSPACE_HOME/ran"`+"\n", true)
	if err := os.RemoveAll(homeDir); err != nil {
		t.Fatalf("remove home: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != workspaceInitPreludeExitCode {
		t.Errorf("exit = %d, want the prelude code %d\n%s", code, workspaceInitPreludeExitCode, out)
	}
	if got := workspaceInitStageOf(out, code); got != workspaceInitStagePrelude {
		t.Errorf("classified stage = %q, want %q\n%s", got, workspaceInitStagePrelude, out)
	}
	if _, err := os.Stat(filepath.Join(homeDir, "ran")); err == nil {
		t.Error("the user script ran even though the prelude failed")
	}
}

// --- 3. postlude が無いこと (PR6) ---

// TestBuildWorkspaceInitRequest_NoPostludeWritesIntoTheHome pins PR6's removal
// of the postlude, which is a deletion worth a test rather than one that just
// happens.
//
// PR5's postlude stamped a home identity token into
// <home>/.boid-workspace-home-id after a successful init.sh. PR6 moved that
// identity onto the home VOLUME's own label, because the daemon cannot read
// anything inside a volume without starting a container and doing that on every
// dispatch would destroy the fast path the completion marker exists to provide.
// Keeping the write anyway would leave a value that looks like a check and is
// not one: written by every init, compared by nothing, and — since it lives in
// storage the job owns read-write — freely forgeable by the thing it would
// appear to be defending against.
//
// So the wrapper must leave the home holding exactly what the prelude and the
// user script put there.
func TestBuildWorkspaceInitRequest_NoPostludeWritesIntoTheHome(t *testing.T) {
	for _, tc := range []struct {
		name         string
		script       string
		scriptExists bool
	}{
		{"with init.sh", "true\n", true},
		{"pass-through workspace", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, homeDir := newWorkspaceInitRequestForBash(t, tc.script, tc.scriptExists)
			code, out := runWorkspaceInitWrapper(t, req)
			if code != 0 {
				t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
			}
			if _, err := os.Lstat(filepath.Join(homeDir, ".boid-workspace-home-id")); !os.IsNotExist(err) {
				t.Errorf("the wrapper wrote an identity file into the home (lstat err=%v); PR6 moved the identity onto the volume label", err)
			}
			// The prelude, which PR6 keeps, still ran.
			for _, rel := range req.SkeletonDirs {
				if info, err := os.Stat(filepath.Join(homeDir, rel)); err != nil || !info.IsDir() {
					t.Errorf("skeleton dir %q missing after the run: %v", rel, err)
				}
			}
		})
	}
}

// --- 4. exit code / stage 分類 (§D3) ---

// TestBuildWorkspaceInitRequest_UserScriptExitCodePropagates pins that the
// wrapper is transparent about the user script's own status. Collapsing every
// failure onto a single code would leave an operator unable to tell "your
// installer returned 4" from "boid's own prep broke".
func TestBuildWorkspaceInitRequest_UserScriptExitCodePropagates(t *testing.T) {
	for _, want := range []int{1, 3, 42, 127} {
		req, _ := newWorkspaceInitRequestForBash(t, fmt.Sprintf("exit %d\n", want), true)
		code, out := runWorkspaceInitWrapper(t, req)
		if code != want {
			t.Errorf("exit = %d, want %d (the user script's own code)\n%s", code, want, out)
		}
		if got := workspaceInitStageOf(out, code); got != workspaceInitStageUserScript {
			t.Errorf("classified stage = %q, want %q\n%s", got, workspaceInitStageUserScript, out)
		}
	}
}

// TestWorkspaceInitStageOf_PrefersTheMarkerOverTheExitCode pins the
// classifier's precedence. The boid-side stage codes are ordinary exit codes,
// so a user script is free to return one; the emitted marker line is what
// disambiguates, and it must win. Falling back to the code table only when no
// marker is present is what keeps an engine-side failure (no output at all)
// from being reported as a stage that never ran.
func TestWorkspaceInitStageOf_PrefersTheMarkerOverTheExitCode(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		exitCode int
		want     string
	}{
		{"marker wins over a colliding user exit code",
			"boid-init: stage=init.sh exit=91\n", workspaceInitPreludeExitCode, workspaceInitStageUserScript},
		{"last marker wins",
			"boid-init: stage=prelude exit=1\nboid-init: stage=init.sh exit=2\n", 2, workspaceInitStageUserScript},
		{"a marker forged mid-output does not shadow the real one",
			"boid-init: stage=script-setup exit=0\nboid-init: stage=prelude exit=5\n", workspaceInitPreludeExitCode, workspaceInitStagePrelude},
		{"no marker falls back to the code table",
			"nothing useful\n", workspaceInitScriptSetupExitCode, workspaceInitStageScriptSetup},
		{"a stage name PR6 retired is not recognized, even as a marker",
			"boid-init: stage=postlude exit=0\n", 0, workspaceInitStageUnknown},
		{"no marker and an unknown code",
			"", 137, workspaceInitStageUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workspaceInitStageOf(tc.output, tc.exitCode); got != tc.want {
				t.Errorf("workspaceInitStageOf(%q, %d) = %q, want %q", tc.output, tc.exitCode, got, tc.want)
			}
		})
	}
}

// TestWorkspaceInitStageOf_OnlyReadsAMarkerAtTheStartOfALine pins that the
// classifier reads the WRAPPER's marker and not a lookalike inside the user
// script's own output.
//
// The two streams are merged (§決定8: 「TTY/非 TTY とも単一結合」), so "it came
// from stderr" is not available as a discriminator, and the user script can
// print any bytes it likes. The case that matters is the one where the wrapper
// printed NO marker of its own — an OOM kill or a SIGKILL takes the container
// down between the user script and `__boid_stage`, leaving exit 137 and
// whatever the script last wrote. If a line the script printed is allowed to
// match anywhere within itself, that run gets classified as a clean `prelude`
// (or any other stage the script happened to mention) instead of `unknown`,
// which is the answer §D3 wants for "the wrapper never got to report".
func TestWorkspaceInitStageOf_OnlyReadsAMarkerAtTheStartOfALine(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		exitCode int
		want     string
	}{
		{
			// The SIGKILL case: no wrapper marker, only the script's own echo
			// of one, and it is not at the start of a line.
			name:     "a marker embedded mid-line is not the wrapper's",
			output:   "downloading toolchain: boid-init: stage=prelude exit=0\n",
			exitCode: 137,
			want:     workspaceInitStageUnknown,
		},
		{
			name:     "the wrapper's marker still wins over an embedded one printed later",
			output:   "boid-init: stage=init.sh exit=1\nlog: boid-init: stage=prelude exit=0\n",
			exitCode: 1,
			want:     workspaceInitStageUserScript,
		},
		{
			name:     "a marker at the very start of the output is at the start of a line",
			output:   "boid-init: stage=prelude exit=1\n",
			exitCode: 1,
			want:     workspaceInitStagePrelude,
		},
		{
			// The retention bound discards from the FRONT, so the first line in
			// the retained window is routinely a fragment of a longer one.
			name:     "a truncated leading fragment is not promoted to a line start",
			output:   "progress boid-init: stage=prelude exit=0\nboid-init: stage=init.sh exit=9\n",
			exitCode: 9,
			want:     workspaceInitStageUserScript,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workspaceInitStageOf(tc.output, tc.exitCode); got != tc.want {
				t.Errorf("workspaceInitStageOf(%q, %d) = %q, want %q", tc.output, tc.exitCode, got, tc.want)
			}
		})
	}
}

// TestBuildWorkspaceInitRequest_StageMarkerStartsALineAfterUnterminatedOutput
// is the emit-side half of the line anchoring, and the reason the anchoring
// alone is not the whole fix.
//
// A script whose last write has no trailing newline (`printf 'installing…'`,
// a progress bar, any `echo -n`) leaves the merged stream mid-line, so a marker
// printed straight after it would be glued to the script's own text and the
// anchored classifier would — correctly, by its own rule — refuse to read it.
// That would turn a precise `init.sh` diagnosis into `unknown` for a very
// ordinary script, so the wrapper has to open its own line first.
func TestBuildWorkspaceInitRequest_StageMarkerStartsALineAfterUnterminatedOutput(t *testing.T) {
	req, _ := newWorkspaceInitRequestForBash(t, "printf 'installing toolchain...'\nexit 4\n", true)
	code, out := runWorkspaceInitWrapper(t, req)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (the user script's own code)\n%s", code, out)
	}
	if got := workspaceInitStageOf(out, code); got != workspaceInitStageUserScript {
		t.Errorf("classified stage = %q, want %q — the wrapper's marker has to start its own line even when the script left one open\n%q",
			got, workspaceInitStageUserScript, out)
	}
}

// TestWorkspaceInitFailure_NamesTheStageAndCarriesTheTail pins the operator
// facing half of §D3: the error has to say which stage broke and show what was
// printed, since neither the container nor its output survives the call.
//
// The output goes in through workspaceInitOutput rather than as a ready-made
// string, because that is the only way this test can see the property it is
// about. The first version handed workspaceInitFailure a finished string, which
// left the capture — the part that decides WHICH bytes survive — entirely
// untested, and it was keeping the head.
func TestWorkspaceInitFailure_NamesTheStageAndCarriesTheTail(t *testing.T) {
	out := newWorkspaceInitOutput()
	mustWriteInitOutput(t, out, strings.Repeat("x", workspaceInitOutputTail*2)+"LAST-LINE\n")
	mustWriteInitOutput(t, out, "boid-init: stage=init.sh exit=42\n")

	err := workspaceInitFailure("myws", 42, out)
	msg := err.Error()
	for _, want := range []string{`"myws"`, workspaceInitStageUserScript, "42", "LAST-LINE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not mention %q:\n%s", want, msg)
		}
	}
	if len(msg) > workspaceInitOutputTail*2 {
		t.Errorf("error message is %d bytes; the output must be tailed, not embedded whole", len(msg))
	}
	if !strings.Contains(msg, workspaceInitTruncationPrefix) {
		t.Errorf("the error dropped output without saying so; %q must appear:\n%s", workspaceInitTruncationPrefix, msg)
	}
}

// TestWorkspaceInitOutput_RetainsTheTailNotTheHead pins the capture bound
// itself.
//
// "Bounded" and "tail" are separate promises and only the first one is
// obvious. An init.sh downloading a toolchain prints megabytes; what diagnoses
// its failure — the installer's last words, and the wrapper's `boid-init:
// stage=` marker, which is printed AFTER the user script exits — is entirely
// at the end. A bound that keeps the first N bytes therefore keeps exactly the
// part nobody needs, and drops the marker that stops workspaceInitStageOf from
// falling back to the exit-code table and naming the wrong stage.
func TestWorkspaceInitOutput_RetainsTheTailNotTheHead(t *testing.T) {
	out := newWorkspaceInitOutput()
	// Written in many small pieces, the way frames arrive off a real attach:
	// the bound has to hold incrementally, not just on one oversized write.
	const head = "FIRST-LINE\n"
	mustWriteInitOutput(t, out, head)
	for written := 0; written < workspaceInitOutputLimit*2; written += 4096 {
		mustWriteInitOutput(t, out, strings.Repeat("n", 4096))
	}
	const tail = "\nLAST-LINE\n"
	mustWriteInitOutput(t, out, tail)

	got := out.String()
	if len(got) > workspaceInitOutputLimit {
		t.Errorf("retained %d bytes, want at most %d", len(got), workspaceInitOutputLimit)
	}
	if !strings.HasSuffix(got, tail) {
		t.Errorf("the retained window does not end with the last thing written (%q); it is not a tail", tail)
	}
	if strings.Contains(got, head) {
		t.Errorf("the retained window still contains the FIRST line (%q) after %d bytes of overflow; the bound is dropping the wrong end",
			head, workspaceInitOutputLimit*2)
	}

	// Tail(n) narrows further, and one notice covers BOTH drops: the operator
	// is told how much never made it into the error, not how much each of two
	// internal stages happened to discard.
	rendered := out.Tail(workspaceInitOutputTail)
	if !strings.HasSuffix(rendered, tail) {
		t.Errorf("Tail() does not end with %q:\n%s", tail, rendered)
	}
	if !strings.HasPrefix(rendered, workspaceInitTruncationPrefix) {
		t.Errorf("Tail() dropped output without announcing it; want a leading %q:\n%s", workspaceInitTruncationPrefix, rendered[:min(len(rendered), 200)])
	}
	if n := strings.Count(rendered, workspaceInitTruncationPrefix); n != 1 {
		t.Errorf("Tail() announced truncation %d times, want exactly 1", n)
	}
}

// TestWorkspaceInitOutput_SaysNothingWhenNothingWasDropped keeps the notice
// honest in the ordinary case: an init that printed a few lines must not have
// a "bytes were omitted" banner stapled to its output.
func TestWorkspaceInitOutput_SaysNothingWhenNothingWasDropped(t *testing.T) {
	out := newWorkspaceInitOutput()
	mustWriteInitOutput(t, out, "all of it\n")
	if got := out.String(); got != "all of it\n" {
		t.Errorf("String() = %q, want the bytes verbatim", got)
	}
	if got := out.Tail(workspaceInitOutputTail); got != "all of it\n" {
		t.Errorf("Tail() = %q, want the bytes verbatim with no truncation notice", got)
	}
}

func mustWriteInitOutput(t *testing.T, out *workspaceInitOutput, s string) {
	t.Helper()
	if n, err := out.Write([]byte(s)); err != nil || n != len(s) {
		t.Fatalf("workspaceInitOutput.Write(%d bytes) = %d, %v", len(s), n, err)
	}
}

// --- 5. stdin (§D2 の「wrapper は stdin から流す」の副作用) ---

// TestBuildWorkspaceInitRequest_UserScriptStdinIsNotTheWrapperStream is the
// regression guard for the trap that comes free with feeding the wrapper to
// `bash -s`: the wrapper IS bash's stdin, so a user script that reads stdin
// would consume the rest of the wrapper — including its own exit-status check,
// so a failing init.sh would be reported as a success. The old host-exec route
// had cmd.Stdin nil (i.e. /dev/null), so this is a property the new route has
// to restore rather than one it may quietly change.
func TestBuildWorkspaceInitRequest_UserScriptStdinIsNotTheWrapperStream(t *testing.T) {
	script := `cat > "$BOID_WORKSPACE_HOME/slurped"` + "\n"
	req, homeDir := newWorkspaceInitRequestForBash(t, script, true)

	code, out := runWorkspaceInitWrapper(t, req)
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}
	slurped, err := os.ReadFile(filepath.Join(homeDir, "slurped"))
	if err != nil {
		t.Fatalf("read what the script slurped off stdin: %v", err)
	}
	if len(slurped) != 0 {
		t.Errorf("init.sh read %d bytes off stdin, want 0 — it must not be able to eat the wrapper:\n%q", len(slurped), slurped)
	}
	// ...and the wrapper still reached its own end: a script that had eaten
	// the rest of the stream would leave bash with nothing after `bash
	// "$__boid_script"`, so the status check and the final `exit 0` would
	// never run and the wrapper would exit on the LAST command's status
	// instead. Re-running with a failing script is what makes that visible.
	failing, _ := newWorkspaceInitRequestForBash(t, `cat > /dev/null; exit 7`+"\n", true)
	if code, out := runWorkspaceInitWrapper(t, failing); code != 7 {
		t.Errorf("wrapper exited %d for a script that slurped stdin and failed, want 7 — the wrapper's own tail was eaten\n%s", code, out)
	}
}

// --- 6. env 契約 (§D9) ---

// TestBuildWorkspaceInitEnv_IsAContainerEnvironmentNotTheDaemonsOwn pins §D9.
// The pre-PR5 allowlist forwarded PATH/USER/LOGNAME/LANG/LC_ALL/TERM from the
// DAEMON PROCESS, which was meaningful while init.sh ran on the host and is
// actively wrong once it runs in a container: the host PATH names directories
// the image does not have, USER/LOGNAME name the daemon's own account rather
// than the uid the container runs as, and a TERM with no TTY behind it makes
// curses-style installers try to draw.
//
// PATH is the one that must be ABSENT rather than replaced with a boid-owned
// constant: leaving the key unset is what lets the engine apply the image's own
// PATH, so the value cannot drift from build/container/Dockerfile.
func TestBuildWorkspaceInitEnv_IsAContainerEnvironmentNotTheDaemonsOwn(t *testing.T) {
	for _, key := range []string{"PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "TERM"} {
		t.Setenv(key, "host-value-for-"+key)
	}
	const containerHome = "/home/boid"
	env := buildWorkspaceInitEnv("myws", containerHome)

	want := map[string]string{
		"HOME":                containerHome,
		"BOID_WORKSPACE_SLUG": "myws",
		"BOID_WORKSPACE_HOME": containerHome,
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
	for k := range env {
		if _, ok := want[k]; !ok {
			t.Errorf("env carries unexpected key %q = %q; the init container's environment is exactly the three contractual vars", k, env[k])
		}
	}
}

// --- 7. skeleton の単一出典 (daemon 側 bind target と prep が食い違わないこと) ---

// TestWorkspaceHomeSkeletonDirs_CoversEveryPerSkillBindTarget pins that the
// set the prep container creates is the set homeMounts actually binds into. A
// missing entry is not a cosmetic drift: the engine creates the absent bind
// target itself, as uid 0, and neither the harness nor the daemon can chown it
// back (論点 b-2).
// --- 4. Integration Pack skill symlinks (shadow-b フォローアップ) ---
//
// Embedded skills reach .claude/skills/<name> via a host-visible bind mount
// (skills_overlay.go) because their content is regenerated per installation.
// Integration Pack skills are baked into the very image the init container
// runs from (build/container/Dockerfile's `cp -r ... /opt/boid/integrations/`)
// — daemon and job share one image, so there is no host-visible directory to
// bind FROM. A symlink inside the init container is therefore the whole
// mechanism: no bind mount, no daemon-side materialization step.

// TestBuildWorkspaceInitRequest_PreludeCreatesPackSkillSymlinks pins that a
// PackSkillLink becomes a working symlink at .claude/skills/<Name>, readable
// through to the real content.
func TestBuildWorkspaceInitRequest_PreludeCreatesPackSkillSymlinks(t *testing.T) {
	homeDir := t.TempDir()
	// Stands in for /opt/boid/integrations/jira-cloud/1.0.0/skills/jira-api —
	// an image-baked path, never a bind-mount source.
	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "jira-cloud", "1.0.0", "skills", "jira-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   []SkillLink{{Name: "jira-api", Target: skillDir}},
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}

	linkPath := filepath.Join(homeDir, ".claude", "skills", "jira-api")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stat symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".claude/skills/jira-api is not a symlink (mode %s)", info.Mode())
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != skillDir {
		t.Errorf("symlink target = %q, want %q", target, skillDir)
	}
	data, err := os.ReadFile(filepath.Join(linkPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content through symlink = %q, want %q", data, "hello")
	}
}

// TestBuildWorkspaceInitRequest_PackSkillSymlinkReplacesStaleDirectory pins
// the recovery path for exactly the state found in production: a skill name
// that was, before this wiring existed, manually copied into
// .claude/skills/<name> as a real directory (e.g. jira-api/bitbucket-api,
// hand-placed from a since-superseded checkout). The prelude must replace it
// with the symlink rather than fail or nest inside it.
func TestBuildWorkspaceInitRequest_PackSkillSymlinkReplacesStaleDirectory(t *testing.T) {
	homeDir := t.TempDir()
	staleDir := filepath.Join(homeDir, ".claude", "skills", "jira-api")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "jira-cloud", "1.0.0", "skills", "jira-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}

	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   []SkillLink{{Name: "jira-api", Target: skillDir}},
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}

	linkPath := filepath.Join(homeDir, ".claude", "skills", "jira-api")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("stale directory was not replaced by a symlink (mode %s)", info.Mode())
	}
	data, err := os.ReadFile(filepath.Join(linkPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "fresh" {
		t.Errorf("content = %q, want %q (stale directory should have been replaced)", data, "fresh")
	}
}

// TestBuildWorkspaceInitRequest_PackSkillSymlinksRunBeforeUserScript mirrors
// TestBuildWorkspaceInitRequest_PreludeRunsBeforeTheUserScript: the symlinks
// are prelude, not user-script-visible-only-at-the-end state.
func TestBuildWorkspaceInitRequest_PackSkillSymlinksRunBeforeUserScript(t *testing.T) {
	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "slack", "1.0.0", "skills", "slack-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}

	homeDir := t.TempDir()
	script := `[ -L "$BOID_WORKSPACE_HOME/.claude/skills/slack-api" ] || exit 17` + "\n"
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   []SkillLink{{Name: "slack-api", Target: skillDir}},
		Script:       []byte(script),
		ScriptExists: true,
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code == 17 {
		t.Fatalf("init.sh observed no pack skill symlink — the prelude must run first\n%s", out)
	}
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}
}

// TestBuildWorkspaceInitRequest_PackSkillSymlinkRerunDoesNotDeleteTarget pins
// the highest-blast-radius risk in this mechanism: "rm -rf -- <dest>"
// removes a SYMLINK (dest, inside the workspace home) without following it
// into the directory it points at (POSIX rm never recurses through a
// symlink argument itself — it unlinks the link). Running the prelude twice
// in a row — the ordinary "already initialized, dispatch again" case — must
// leave the Pack's own image-baked content untouched on the second run.
func TestBuildWorkspaceInitRequest_PackSkillSymlinkRerunDoesNotDeleteTarget(t *testing.T) {
	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "jira-cloud", "1.0.0", "skills", "jira-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("reference"), 0o644); err != nil {
		t.Fatal(err)
	}

	homeDir := t.TempDir()
	links := []SkillLink{{Name: "jira-api", Target: skillDir}}
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   links,
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	for i := 0; i < 2; i++ {
		code, out := runWorkspaceInitWrapper(t, req)
		if code != 0 {
			t.Fatalf("run %d: wrapper exited %d, want 0\n%s", i, code, out)
		}
	}

	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatalf("Pack's own content was removed by a re-run of the symlink prelude: %v", err)
	}
	linkPath := filepath.Join(homeDir, ".claude", "skills", "jira-api")
	data, err := os.ReadFile(filepath.Join(linkPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("read through symlink after re-run: %v", err)
	}
	if string(data) != "reference" {
		t.Errorf("content through symlink after re-run = %q, want %q", data, "reference")
	}
}
