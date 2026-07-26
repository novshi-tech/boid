package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// TestServer_New_RunnerReservedVolumeNamesComeFromSelfInspect pins the
// server-side half of this PR's one new wiring edge: the startup self-inspect
// result has to reach dispatcher.WireConfig.ReservedVolumeNames, or the whole
// containment is inert in production while every dispatcher- and
// dockerproxy-side test stays green.
//
// The dispatcher-side half — that name actually producing a 403 on a live
// proxy socket — is pinned by TestWire_DaemonStateVolume_ReachesDockerProxyPolicy
// (internal/dispatcher/daemon_state_volume_wire_test.go). Same split, for the
// same reason, as TestServer_New_RunnerDataHomeDirIsPersistentDataRoot.
//
// Same Server.New() -> buildRuntime() approach as that test, including the
// deliberately split DBPath/SocketPath parents.
func TestServer_New_RunnerReservedVolumeNamesComeFromSelfInspect(t *testing.T) {
	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })

	var gotDataHome string
	detectDaemonStateVolumesFn = func(_ context.Context, _ dispatcher.SelfContainerInspector, dataHome string) (dispatcher.DaemonStateVolumes, error) {
		gotDataHome = dataHome
		return dispatcher.DaemonStateVolumes{
			StateVolumeExpected: true,
			ContainerID:         "b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f",
			Names:               []string{"boid_boid_state"},
			DataHomeCovered:     true,
		}, nil
	}

	runner, cfg := newServerRunnerForTest(t)

	if want := dataHomeFor(cfg); gotDataHome != want {
		t.Errorf("self-inspect was asked about dataHome %q, want %q (the root boid.db lives in — the thing that must end up covered)", gotDataHome, want)
	}
	if len(runner.ReservedVolumeNames) != 1 || runner.ReservedVolumeNames[0] != "boid_boid_state" {
		t.Errorf("Runner.ReservedVolumeNames = %v, want [boid_boid_state]", runner.ReservedVolumeNames)
	}
}

// TestServer_New_ReservedVolumeNamesEmptyOutsideContainer pins the bare
// `boid start` contract: no state volume exists, so nothing is reserved and
// startup must not fail.
func TestServer_New_ReservedVolumeNamesEmptyOutsideContainer(t *testing.T) {
	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })

	detectDaemonStateVolumesFn = func(context.Context, dispatcher.SelfContainerInspector, string) (dispatcher.DaemonStateVolumes, error) {
		return dispatcher.DaemonStateVolumes{StateVolumeExpected: false}, nil
	}

	runner, _ := newServerRunnerForTest(t)
	if len(runner.ReservedVolumeNames) != 0 {
		t.Errorf("Runner.ReservedVolumeNames = %v, want none outside a container", runner.ReservedVolumeNames)
	}
}

// TestServer_New_DetectionFailureDoesNotBlockStartup pins the fail-open
// posture: a daemon that cannot identify its own container still boots (the
// alternative — refusing to start — costs far more than the exposure it
// would close), just with nothing reserved.
func TestServer_New_DetectionFailureDoesNotBlockStartup(t *testing.T) {
	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })

	detectDaemonStateVolumesFn = func(context.Context, dispatcher.SelfContainerInspector, string) (dispatcher.DaemonStateVolumes, error) {
		return dispatcher.DaemonStateVolumes{StateVolumeExpected: true}, errors.New("engine unreachable")
	}

	runner, _ := newServerRunnerForTest(t)
	if len(runner.ReservedVolumeNames) != 0 {
		t.Errorf("Runner.ReservedVolumeNames = %v, want none after a detection failure", runner.ReservedVolumeNames)
	}
}

// --- [Minor 1] the WARN / no-WARN contract itself ---------------------------
//
// Everything above checks WHAT gets reserved. Nothing checked whether the
// operator is TOLD when nothing does — which is how [Major 1] shipped: a
// containerd/k8s daemon classified as "not in a container", reserved nothing,
// and said nothing, and every wiring test above stayed green. The three
// states below are the actual contract, and silence is only ever correct for
// the first of them.
//
// These run against reservedDaemonStateVolumes rather than Server.New()
// because it is the function that logs, and because a full New() drowns the
// three lines under construction noise it would then have to filter out.
func TestReservedDaemonStateVolumes_warnContract(t *testing.T) {
	const (
		selfID    = "b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f"
		dataHome  = "/home/boid/.local/share/boid"
		stateVol  = "boid_boid_state"
		parentVol = "some_other_project_state"
	)

	cases := []struct {
		name      string
		res       dispatcher.DaemonStateVolumes
		err       error
		dataHome  string
		wantNames []string
		wantWarn  bool
		// wantWarnContains is checked only when wantWarn: the message has to
		// tell an operator WHAT is unprotected, not merely that something is.
		wantWarnContains string
	}{
		{
			// State 1. Bare `boid start`: the data root is an ordinary
			// directory on the root mount, so no volume can be holding it.
			// An empty reservation is the complete and correct answer and a
			// warning here would be pure noise on a healthy deployment.
			name:     "no state volume can exist",
			res:      dispatcher.DaemonStateVolumes{StateVolumeExpected: false},
			dataHome: "/home/nosen/.local/share/boid",
			wantWarn: false,
		},
		{
			// State 2. The live compose deploy.
			name: "identified",
			res: dispatcher.DaemonStateVolumes{
				StateVolumeExpected: true, ContainerID: selfID,
				Names: []string{stateVol}, DataHomeCovered: true,
			},
			dataHome:  dataHome,
			wantNames: []string{stateVol},
			wantWarn:  false,
		},
		{
			// State 3a. The k8s/containerd shape of [Major 1].
			name:             "identification failed",
			res:              dispatcher.DaemonStateVolumes{StateVolumeExpected: true},
			err:              errors.New("no container id found"),
			dataHome:         dataHome,
			wantWarn:         true,
			wantWarnContains: "INACTIVE",
		},
		{
			// State 3b. Docker-in-docker, [Major 3]: two ids, no way to tell
			// which is ours. Reserving the parent's volume would be worse
			// than reserving nothing, because it would look like success.
			name:             "identification ambiguous",
			res:              dispatcher.DaemonStateVolumes{StateVolumeExpected: true},
			err:              errors.New("ambiguous container id"),
			dataHome:         dataHome,
			wantWarn:         true,
			wantWarnContains: "INACTIVE",
		},
		{
			// State 3c.
			name:             "inspect failed",
			res:              dispatcher.DaemonStateVolumes{StateVolumeExpected: true, ContainerID: selfID},
			err:              errors.New("engine unreachable"),
			dataHome:         dataHome,
			wantWarn:         true,
			wantWarnContains: "INACTIVE",
		},
		{
			// State 3d, [Major 2]. The daemon got this far only because the
			// self-inspect has a deadline; without one there would be no
			// startup to log from.
			name:             "inspect timed out",
			res:              dispatcher.DaemonStateVolumes{StateVolumeExpected: true, ContainerID: selfID},
			err:              context.DeadlineExceeded,
			dataHome:         dataHome,
			wantWarn:         true,
			wantWarnContains: "INACTIVE",
		},
		{
			// Every step succeeded and the answer is still "nothing to
			// reserve" — a shape that must not read as success.
			name:             "identified but no named volumes",
			res:              dispatcher.DaemonStateVolumes{StateVolumeExpected: true, ContainerID: selfID},
			dataHome:         dataHome,
			wantWarn:         true,
			wantWarnContains: "reserved nothing",
		},
		{
			// DataHomeCovered=false: volumes were reserved, but not the one
			// the secrets are in. Reserved names are still returned (they are
			// correct as far as they go); the warning is the point.
			name: "data root outside every reserved volume",
			res: dispatcher.DaemonStateVolumes{
				StateVolumeExpected: true, ContainerID: selfID,
				Names: []string{parentVol}, DataHomeCovered: false,
			},
			dataHome:         dataHome,
			wantNames:        []string{parentVol},
			wantWarn:         true,
			wantWarnContains: "not inside any of the reserved volumes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := detectDaemonStateVolumesFn
			t.Cleanup(func() { detectDaemonStateVolumesFn = restore })
			detectDaemonStateVolumesFn = func(context.Context, dispatcher.SelfContainerInspector, string) (dispatcher.DaemonStateVolumes, error) {
				return tc.res, tc.err
			}
			logs := captureSlog(t)

			got := reservedDaemonStateVolumes(context.Background(), nil, tc.dataHome)

			if len(got) != len(tc.wantNames) {
				t.Errorf("reserved %v, want %v", got, tc.wantNames)
			} else {
				for i := range tc.wantNames {
					if got[i] != tc.wantNames[i] {
						t.Errorf("reserved %v, want %v", got, tc.wantNames)
						break
					}
				}
			}

			warned := strings.Contains(logs.String(), "level=WARN")
			if warned != tc.wantWarn {
				if tc.wantWarn {
					t.Errorf("no WARN logged; an operator is never told containment is off.\nlogs:\n%s", logs.String())
				} else {
					t.Errorf("WARN logged on a deployment with nothing to protect.\nlogs:\n%s", logs.String())
				}
			}
			if tc.wantWarn && warned && !strings.Contains(logs.String(), tc.wantWarnContains) {
				t.Errorf("WARN does not mention %q.\nlogs:\n%s", tc.wantWarnContains, logs.String())
			}
		})
	}
}

// TestReservedDaemonStateVolumes_unresolvedDataHomeWarnsWithoutClaimingInactive
// is the caller-side half of [round 3 Minor 1, codex review].
//
// The table above has no row for it because the shape does not fit the
// three-state axis it walks: this one is BOTH — containment is active (the
// volumes were identified and are returned) AND something the operator has to
// be told (data_home_covered on the INFO line one row up was computed against a
// data root the daemon could not normalize, so that field is not evidence of
// anything). Before the fix the detection dropped it and this produced a
// startup log with no warning in it at all.
//
// The two assertions that matter are opposite in sign, and the negative one is
// the load-bearing half: reusing the INACTIVE wording here would tell an
// operator to go fix a containment that is working, and would make
// `grep INACTIVE` — the only thing anyone actually does with a startup log —
// report a breach on a healthy deploy. Same reason the "not inside any of the
// reserved volumes" warning must stay quiet: that one is a claim about coverage
// this run is explicitly unable to make.
// TestReservedDaemonStateVolumes_unresolvedDataHomeSuppressesTheCoverageClaim
// pins the gate that keeps the two coverage warnings from contradicting each
// other.
//
// The case only arises when BOTH are true: the data root failed to normalize
// AND the (therefore meaningless) coverage comparison came out false. The
// sibling test above cannot reach it — its fixture has DataHomeCovered=true,
// so the second warning is skipped for an unrelated reason and the gate is
// never exercised. Ungated, the log carries "coverage may be wrong"
// immediately followed by "your secrets are NOT protected" stated as fact,
// which is worse than either line alone: an operator cannot act on a
// contradiction, and the actionable half is the one they will believe.
func TestReservedDaemonStateVolumes_unresolvedDataHomeSuppressesTheCoverageClaim(t *testing.T) {
	const (
		selfID   = "b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f"
		dataHome = "/home/boid/.local/share/boid"
		stateVol = "boid_boid_state"
	)

	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })
	detectDaemonStateVolumesFn = func(context.Context, dispatcher.SelfContainerInspector, string) (dispatcher.DaemonStateVolumes, error) {
		return dispatcher.DaemonStateVolumes{
			StateVolumeExpected: true,
			ContainerID:         selfID,
			Names:               []string{stateVol},
			// The combination the gate exists for: unresolved, and the
			// comparison it could not do honestly came out false.
			DataHomeCovered:    false,
			DataHomeUnresolved: errors.New("resolve symlinks in the daemon's data root " + dataHome + ": no such file or directory"),
		}, nil
	}
	logs := captureSlog(t)

	got := reservedDaemonStateVolumes(context.Background(), nil, dataHome)

	if len(got) != 1 || got[0] != stateVol {
		t.Errorf("reserved %v, want [%s]", got, stateVol)
	}
	out := logs.String()
	if !strings.Contains(out, "could not be normalized") {
		t.Errorf("the coverage-unknown WARN is missing, so suppressing the other one would leave the operator with nothing at all.\nlogs:\n%s", out)
	}
	if strings.Contains(out, "not inside any of the reserved volumes") {
		t.Errorf("both coverage warnings fired: one says the coverage result may be wrong, the next states that same result as fact. "+
			"DataHomeCovered=false is not evidence when the path it compares was never resolved.\nlogs:\n%s", out)
	}
}

func TestReservedDaemonStateVolumes_unresolvedDataHomeWarnsWithoutClaimingInactive(t *testing.T) {
	const (
		selfID   = "b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f"
		dataHome = "/home/boid/.local/share/boid"
		stateVol = "boid_boid_state"
	)

	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })
	detectDaemonStateVolumesFn = func(context.Context, dispatcher.SelfContainerInspector, string) (dispatcher.DaemonStateVolumes, error) {
		// Exactly what DetectDaemonStateVolumes returns for a data root that
		// is a dangling symlink in a container it could identify — pinned on
		// the producing side by dispatcher's
		// TestDetectDaemonStateVolumes_unresolvableDataHomeStillReportsItWhenIdentified.
		return dispatcher.DaemonStateVolumes{
			StateVolumeExpected: true,
			ContainerID:         selfID,
			Names:               []string{stateVol},
			DataHomeCovered:     true,
			DataHomeUnresolved:  errors.New("resolve symlinks in the daemon's data root " + dataHome + ": no such file or directory"),
		}, nil
	}
	logs := captureSlog(t)

	got := reservedDaemonStateVolumes(context.Background(), nil, dataHome)

	if len(got) != 1 || got[0] != stateVol {
		t.Errorf("reserved %v, want [%s]: a data root that would not resolve says nothing about the volumes, which were named and must still be reserved", got, stateVol)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("no WARN logged: data_home_covered was computed against a path the daemon could not resolve and the operator is never told.\nlogs:\n%s", out)
	}
	if !strings.Contains(out, "could not be normalized") {
		t.Errorf("the WARN does not say the data root failed to normalize, so an operator cannot tell WHICH field is untrustworthy.\nlogs:\n%s", out)
	}
	if !strings.Contains(out, "no such file or directory") {
		t.Errorf("the WARN does not carry the resolution's own error, so an operator cannot tell what to go fix.\nlogs:\n%s", out)
	}
	if strings.Contains(out, "INACTIVE") {
		t.Errorf("the WARN claims containment is INACTIVE, but the volumes above ARE reserved — this is the coverage CHECK being untrustworthy, "+
			"not the containment being off, and conflating the two sends an operator after a working deployment.\nlogs:\n%s", out)
	}
	if strings.Contains(out, "not inside any of the reserved volumes") {
		t.Errorf("the WARN asserts the data root is outside every reserved volume, which is a claim this run cannot make: the path it would "+
			"have been compared against never resolved.\nlogs:\n%s", out)
	}
	if !strings.Contains(out, "containment active") {
		t.Errorf("the INFO line naming the reserved volumes is missing; the WARN reads as a total failure without it.\nlogs:\n%s", out)
	}
}

// --- [round 2 Major 2] the caller must read the error, not the flag ---------

// TestReservedDaemonStateVolumes_errorIsReadBeforeTheFlag is the regression
// test for the second half of [Major 2, codex round 2].
//
// reservedDaemonStateVolumes used to branch on res.StateVolumeExpected FIRST
// and return nil silently when it was false — which meant any failure that
// could not know whether a state volume exists (the docker client failing to
// construct, so detection never ran at all) came back as a zero
// DaemonStateVolumes alongside a perfectly real error, and the error was
// dropped unread. A daemon with an unreadable DOCKER_CERT_PATH or a malformed
// DOCKER_HOST therefore started with containment off and said nothing.
//
// DetectDaemonStateVolumes now guarantees the complementary half — every one
// of its error returns carries StateVolumeExpected=true, pinned by
// TestDetectDaemonStateVolumes_everyErrorCarriesStateVolumeExpected — so this
// shape should no longer be reachable from production. It is asserted anyway
// because "no caller may treat an error as silence" is a property of the
// CALLER, and a future detection path that forgets the invariant must not be
// able to re-open the silent hole from the other side.
func TestReservedDaemonStateVolumes_errorIsReadBeforeTheFlag(t *testing.T) {
	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })
	detectDaemonStateVolumesFn = func(context.Context, dispatcher.SelfContainerInspector, string) (dispatcher.DaemonStateVolumes, error) {
		// Exactly what a failed docker-client construction returned: a zero
		// value (so StateVolumeExpected is false) plus a real error.
		return dispatcher.DaemonStateVolumes{}, errors.New("connect to docker: unable to resolve docker endpoint")
	}
	logs := captureSlog(t)

	got := reservedDaemonStateVolumes(context.Background(), nil, "/home/boid/.local/share/boid")

	if len(got) != 0 {
		t.Errorf("reserved %v, want none after a failure", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no WARN logged for a detection error that arrived with StateVolumeExpected=false; "+
			"the error was discarded unread and the operator is never told containment is off.\nlogs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "unable to resolve docker endpoint") {
		t.Errorf("WARN does not carry the underlying error, so an operator cannot tell WHICH failure it was.\nlogs:\n%s", logs.String())
	}
}

// TestServer_New_SelfInspectGetsTheBackendsDockerClient pins the first half of
// [Major 2, codex round 2]: the self-inspection must be handed the SAME docker
// client the container backend uses, rather than constructing a second one of
// its own.
//
// Two things ride on it. The narrow one is that a daemon which can talk to an
// engine at all can also inspect itself — a detection that builds its own
// client can fail on its own, with its own error, on a deployment where the
// backend is perfectly healthy. The wider one is DOCKER_CERT_PATH: FromEnv
// loads ca.pem/cert.pem/key.pem SYNCHRONOUSLY (moby client_options.go's
// WithTLSClientConfigFromEnv), so a second construction is a second
// uninterruptible read of a filesystem that may be wedged — a startup hang
// that no context deadline can cut, introduced by this feature alone. With one
// construction the read is the container backend's own precondition, which
// every release since the volume-only cutover has already had.
//
// The assertion is on IDENTITY — the backend's own handle and the
// self-inspection's handle being one object — because everything weaker passes
// on the regression [round 3 Minor 2, codex review]. Checking only that the
// self-inspection received some non-nil *client.Client is true of a
// buildRuntime that constructs a second client for it, which is the entire
// thing this test's name claims does not happen: both costs above are costs of
// the SECOND construction, not of a nil one.
//
// The concrete-type assertion is kept alongside it for a different failure:
// handing a typed-nil *client.Client through a SelfContainerInspector parameter
// produces a non-nil interface that panics on first use, which is the exact
// trap buildRuntime's own wsLookup guard exists for — and which pointer
// identity alone would not catch, since two typed-nils are equal.
func TestServer_New_SelfInspectGetsTheBackendsDockerClient(t *testing.T) {
	restore := detectDaemonStateVolumesFn
	t.Cleanup(func() { detectDaemonStateVolumesFn = restore })

	var gotAPI dispatcher.SelfContainerInspector
	var called bool
	detectDaemonStateVolumesFn = func(_ context.Context, api dispatcher.SelfContainerInspector, _ string) (dispatcher.DaemonStateVolumes, error) {
		gotAPI, called = api, true
		return dispatcher.DaemonStateVolumes{StateVolumeExpected: false}, nil
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	// Backend deliberately unset: this is the production path, the only one
	// that builds a real docker client at all.
	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if !called {
		t.Fatal("the startup self-inspection never ran")
	}
	cli, ok := gotAPI.(*client.Client)
	if !ok {
		t.Fatalf("self-inspect was handed a %T, want the *client.Client the container backend was built with", gotAPI)
	}
	if cli == nil {
		t.Fatal("self-inspect was handed a typed-nil *client.Client: non-nil as an interface, panics on first use")
	}

	// The identity itself. dispatcher.ContainerBackendUsesDockerAPI exists for
	// this assertion — internal/server cannot name the unexported
	// *containerBackend, let alone read its engine handle, so "these two are
	// the same object" is not otherwise sayable from here.
	sandboxBackend := runnerFromServer(t, srv).Backend
	if !dispatcher.IsContainerBackend(sandboxBackend) {
		t.Fatalf("Runner.Backend is %T, want the containerBackend sandboxBackendForConfig builds on the production path", sandboxBackend)
	}
	if !dispatcher.ContainerBackendUsesDockerAPI(sandboxBackend, cli) {
		t.Errorf("the container backend holds a DIFFERENT docker client than the one handed to the self-inspection (%p): "+
			"buildRuntime built two, so DOCKER_CERT_PATH's ca/cert/key are read twice — synchronously, on the startup path, "+
			"outside any deadline — and a self-inspect can now fail on its own on a deployment whose backend is healthy", cli)
	}
}

// captureSlog redirects the default logger into a buffer for one test. Same
// shape as internal/orchestrator's own helper.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// newServerRunnerForTest boots a Server the same way
// TestServer_New_RunnerDataHomeDirIsPersistentDataRoot does (with a fake
// backend, so no engine is involved) and returns the *dispatcher.Runner
// buildRuntime constructed.
func newServerRunnerForTest(t *testing.T) (*dispatcher.Runner, Config) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	runtimeDir := filepath.Join(tmpDir, "runtime")
	for _, d := range []string{dataDir, runtimeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	cfg := Config{
		DBPath:     filepath.Join(dataDir, "boid.db"),
		SocketPath: filepath.Join(runtimeDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    &fakeSandboxBackend{},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	return runnerFromServer(t, srv), cfg
}

// runnerFromServer reaches through a booted Server to the *dispatcher.Runner
// buildRuntime constructed (the boid builtin executor's jobContexts field is
// the only post-construction handle on it). Split out of
// newServerRunnerForTest so a test that has to build its Server ITSELF — the
// production path, with Config.Backend deliberately unset so a real docker
// client and a real containerBackend are constructed — can still get at it.
func runnerFromServer(t *testing.T, srv *Server) *dispatcher.Runner {
	t.Helper()
	if srv.broker == nil || srv.broker.BoidExecutor == nil {
		t.Fatal("srv.broker.BoidExecutor is nil, want the wired boidBuiltinExecutor")
	}
	exec, ok := srv.broker.BoidExecutor.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("srv.broker.BoidExecutor is %T, want *boidBuiltinExecutor", srv.broker.BoidExecutor)
	}
	runner, ok := exec.jobContexts.(*dispatcher.Runner)
	if !ok {
		t.Fatalf("boidBuiltinExecutor.jobContexts is %T, want *dispatcher.Runner", exec.jobContexts)
	}
	return runner
}
