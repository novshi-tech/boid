package cmd

// cmd/check_test.go exercises the PR7 rewrite of `boid check`
// (docs/plans/release-onboarding.md 穴5) — the pure/fakeable pieces
// (compose bind-source resolution, the arch-mismatch comparison) directly,
// and the engine-dependent pieces (podmanSocketActive) via
// writeFakeExecutable (cmd/host_test.go), the same PATH-stubbing pattern
// host_test.go already uses since this sandbox has neither docker nor
// podman installed (host_test.go's own writeFakeExecutable doc comment).

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/image"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestResolveComposeBindSource_Docker_DefaultsToBareDockerSocket(t *testing.T) {
	t.Setenv("BOID_DOCKER_SOCK_SRC", "")
	if got := resolveComposeBindSource("docker"); got != "/var/run/docker.sock" {
		t.Errorf("resolveComposeBindSource(docker) = %q, want /var/run/docker.sock (compose.yml's own bare-docker default)", got)
	}
}

func TestResolveComposeBindSource_Podman_DefaultsToXDGRuntimeShape(t *testing.T) {
	t.Setenv("BOID_DOCKER_SOCK_SRC", "")
	want := "/run/user/" + strconv.Itoa(os.Getuid()) + "/podman/podman.sock"
	if got := resolveComposeBindSource("podman"); got != want {
		t.Errorf("resolveComposeBindSource(podman) = %q, want %q (mirrors scripts/deploy-container.sh's BOID_DOCKER_SOCK_SRC default)", got, want)
	}
}

func TestResolveComposeBindSource_HonorsOverrideEnvForEitherEngine(t *testing.T) {
	t.Setenv("BOID_DOCKER_SOCK_SRC", "/custom/engine.sock")
	if got := resolveComposeBindSource("docker"); got != "/custom/engine.sock" {
		t.Errorf("resolveComposeBindSource(docker) = %q, want the BOID_DOCKER_SOCK_SRC override", got)
	}
	if got := resolveComposeBindSource("podman"); got != "/custom/engine.sock" {
		t.Errorf("resolveComposeBindSource(podman) = %q, want the BOID_DOCKER_SOCK_SRC override", got)
	}
}

func TestCheckEngineSocket_MissingBindSource_ReportsFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOID_DOCKER_SOCK_SRC", dir+"/does-not-exist.sock")

	var buf strings.Builder
	if checkEngineSocket(context.Background(), &buf, "docker") {
		t.Error("checkEngineSocket() = true, want false when the bind source path does not exist")
	}
	if !strings.Contains(buf.String(), "MISSING") {
		t.Errorf("output = %q, want a MISSING line", buf.String())
	}
}

// TestCheckEngineSocket_NotAUnixSocket_ReportsFailure pins [codex round 2
// review, Blocker 2]: a bind source that exists but is a plain file (not a
// unix socket) must fail the check, not just print a WARNING — `boid
// start` would bind-mount it into the daemon container exactly the same as
// a real socket, and every docker-API call the daemon makes would then
// fail.
func TestCheckEngineSocket_NotAUnixSocket_ReportsFailure(t *testing.T) {
	dir := t.TempDir()
	plainFile := dir + "/not-a-socket"
	if err := os.WriteFile(plainFile, []byte("not a socket"), 0o644); err != nil {
		t.Fatalf("write plain file: %v", err)
	}
	t.Setenv("BOID_DOCKER_SOCK_SRC", plainFile)

	var buf strings.Builder
	if checkEngineSocket(context.Background(), &buf, "docker") {
		t.Error("checkEngineSocket() = true, want false when the bind source exists but is not a unix socket")
	}
	if !strings.Contains(buf.String(), "MISSING") {
		t.Errorf("output = %q, want a MISSING line (not a soft WARNING)", buf.String())
	}
}

func TestPodmanSocketActive_TrueWhenSystemctlExitsZero(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "systemctl", "exit 0")
	t.Setenv("PATH", dir)

	if !podmanSocketActive(context.Background()) {
		t.Error("expected podmanSocketActive() == true when `systemctl --user is-active podman.socket` exits 0")
	}
}

func TestPodmanSocketActive_FalseWhenSystemctlExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "systemctl", "exit 3")
	t.Setenv("PATH", dir)

	if podmanSocketActive(context.Background()) {
		t.Error("expected podmanSocketActive() == false when systemctl reports inactive (non-zero exit)")
	}
}

func TestPodmanSocketActive_FalseWhenSystemctlMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	if podmanSocketActive(context.Background()) {
		t.Error("expected podmanSocketActive() == false when systemctl is not on PATH at all")
	}
}

// fakeArchProbeAPI implements archProbeAPI for probeArchMismatchWithAPI
// tests, without needing a real docker/podman engine.
type fakeArchProbeAPI struct {
	info    dockerclient.SystemInfoResult
	infoErr error

	dist    dockerclient.DistributionInspectResult
	distErr error

	inspect    dockerclient.ImageInspectResult
	inspectErr error
}

func (f *fakeArchProbeAPI) Info(ctx context.Context, options dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error) {
	return f.info, f.infoErr
}

func (f *fakeArchProbeAPI) DistributionInspect(ctx context.Context, imageRef string, options dockerclient.DistributionInspectOptions) (dockerclient.DistributionInspectResult, error) {
	return f.dist, f.distErr
}

func (f *fakeArchProbeAPI) ImageInspect(ctx context.Context, imageRef string, opts ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error) {
	return f.inspect, f.inspectErr
}

func newSystemInfoResult(arch string) dockerclient.SystemInfoResult {
	var r dockerclient.SystemInfoResult
	r.Info.Architecture = arch
	return r
}

func newDistributionInspectResult(archs ...string) dockerclient.DistributionInspectResult {
	var r dockerclient.DistributionInspectResult
	for _, a := range archs {
		r.Platforms = append(r.Platforms, ocispec.Platform{Architecture: a})
	}
	return r
}

func TestProbeArchMismatchWithAPI_RemoteManifestMatchingArch_NoMismatch(t *testing.T) {
	// The primary path (codex round-1 review, Blocker 1): the image has
	// never been pulled locally (ImageInspect would fail), but the
	// registry's own manifest — reachable WITHOUT pulling — already
	// answers the question.
	api := &fakeArchProbeAPI{
		info:       newSystemInfoResult("x86_64"),
		dist:       newDistributionInspectResult("amd64"),
		inspectErr: errors.New("no such image (never pulled)"),
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty (the remote manifest resolved this without ever touching ImageInspect)", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false: x86_64 (uname-style) normalizes to the same amd64 the manifest reports")
	}
	if hostArch != "amd64" || imageArch != "amd64" {
		t.Errorf("hostArch=%q imageArch=%q, want both amd64", hostArch, imageArch)
	}
}

func TestProbeArchMismatchWithAPI_RemoteManifestMismatchedArch_ReportsMismatch(t *testing.T) {
	api := &fakeArchProbeAPI{
		info: newSystemInfoResult("aarch64"),
		dist: newDistributionInspectResult("amd64"),
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if !mismatch {
		t.Error("mismatch = false, want true: host is arm64 (aarch64), the published manifest is amd64-only")
	}
	if hostArch != "arm64" {
		t.Errorf("hostArch = %q, want normalized arm64", hostArch)
	}
	if imageArch != "amd64" {
		t.Errorf("imageArch = %q, want amd64", imageArch)
	}
}

func TestProbeArchMismatchWithAPI_RemoteManifestList_MatchesOneOfMultiplePlatforms(t *testing.T) {
	api := &fakeArchProbeAPI{
		info: newSystemInfoResult("aarch64"),
		dist: newDistributionInspectResult("amd64", "arm64"),
	}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false: a multi-platform manifest list that includes the host's arm64 must not be flagged")
	}
}

func TestProbeArchMismatchWithAPI_DistributionInspectFails_FallsBackToLocalImageInspect(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"),
		distErr: errors.New("no registry configured for this local tag"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: "amd64"}},
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "boid-runner:latest")
	if note != "" {
		t.Fatalf("note = %q, want empty (local ImageInspect fallback resolved it)", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false")
	}
	if hostArch != "amd64" || imageArch != "amd64" {
		t.Errorf("hostArch=%q imageArch=%q, want both amd64", hostArch, imageArch)
	}
}

func TestProbeArchMismatchWithAPI_NeitherManifestNorLocalImageResolve_DegradesToNote(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:       newSystemInfoResult("x86_64"),
		distErr:    errors.New("registry unreachable"),
		inspectErr: errors.New("no such image"),
	}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if mismatch {
		t.Error("mismatch = true, want false: neither source resolving must never be reported as a positive mismatch")
	}
	if note == "" {
		t.Error("note = \"\", want a message explaining neither the manifest nor a local copy could be found")
	}
}

func TestProbeArchMismatchWithAPI_InfoProbeFails_DegradesToNote(t *testing.T) {
	api := &fakeArchProbeAPI{infoErr: errors.New("connection refused")}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if mismatch {
		t.Error("mismatch = true, want false: a failed engine probe must never be reported as a positive mismatch")
	}
	if note == "" {
		t.Error("note = \"\", want a message explaining the engine info probe failed")
	}
}

func TestProbeArchMismatchWithAPI_UnknownLocalImageArch_NoFalsePositive(t *testing.T) {
	// A local image whose manifest genuinely lacks platform metadata
	// reports an empty Architecture — must never be treated as
	// "definitely a mismatch" against a known host arch.
	api := &fakeArchProbeAPI{
		info:       newSystemInfoResult("x86_64"),
		distErr:    errors.New("no registry"),
		inspect:    dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: ""}},
		inspectErr: nil,
	}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "some-custom:tag")
	if mismatch {
		t.Error("mismatch = true, want false when the image reports no architecture at all")
	}
	if note != "" {
		t.Errorf("note = %q, want empty (this is a legitimate, non-error, no-verdict case)", note)
	}
}

// TestRunCheck_NoEngineAvailable_DoesNotPanicAndReportsError pins that
// `boid check` degrades gracefully — never panics or hangs — on a host with
// neither docker nor podman on PATH and no boid daemon running, which is
// exactly this sandbox's own state (see writeFakeExecutable's doc comment
// in cmd/host_test.go). This is the "doesn't break with no docker/podman
// environment" contract the PR7 task description calls out explicitly.
func TestRunCheck_NoEngineAvailable_DoesNotPanicAndReportsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("BOID_NO_AUTOSTART", "1")

	cmd := checkCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	err := runCheck(cmd, nil)
	if err == nil {
		t.Fatal("expected an error when no engine is usable")
	}
	out := buf.String()
	if !strings.Contains(out, "Container engine") {
		t.Errorf("output = %q, want a Container engine section", out)
	}
	if !strings.Contains(out, "ERROR: no usable engine+compose combination found") {
		t.Errorf("output = %q, want an explicit no-engine error", out)
	}
}

// TestRunCheck_RootBoidUID_ReportsErrorEvenWithNoEngine pins the uid
// section's reuse of cmd/start.go's refuseRootUID/effectiveBoidUID
// (codex round-1 review, Major 1): `boid check` must fail on
// `BOID_UID=0 boid check` the same way scripts/deploy-container.sh's own
// preflight refuses `BOID_UID=0 boid start`, independent of engine
// availability.
func TestRunCheck_RootBoidUID_ReportsError(t *testing.T) {
	t.Setenv("BOID_UID", "0")

	cmd := checkCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	err := runCheck(cmd, nil)
	if err == nil {
		t.Fatal("expected an error when BOID_UID=0")
	}
	if !strings.Contains(buf.String(), "refusing to run as uid 0") {
		t.Errorf("output = %q, want the root-uid refusal message", buf.String())
	}
}

// TestRunCheck_MalformedBoidUIDEnv_ReportsError pins [codex round 2
// review, Major]: effectiveBoidUID() alone silently falls back to
// os.Getuid() on a malformed BOID_UID (its own doc comment says this is
// deliberate — it leaves validation to whoever consumes it), but
// scripts/deploy-container.sh — the actual `boid start` enforcement point
// — rejects a malformed raw value outright. `boid check` must predict
// that rejection, not just check the parsed/fallback uid.
func TestRunCheck_MalformedBoidUIDEnv_ReportsError(t *testing.T) {
	t.Setenv("BOID_UID", "01")

	cmd := checkCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	err := runCheck(cmd, nil)
	if err == nil {
		t.Fatal("expected an error when BOID_UID is malformed (\"01\", a leading-zero non-canonical decimal)")
	}
	if !strings.Contains(buf.String(), "BOID_UID must be a plain non-negative decimal integer") {
		t.Errorf("output = %q, want the raw-BOID_UID validation message", buf.String())
	}
}

// TestRunCheck_ArchProbeInconclusive_ReportsError pins [codex round 2
// review, Blocker 1]: when a usable engine is found but the arch probe
// cannot reach it at the resolved compose bind source (e.g. no engine
// actually listening there), `boid check` must fail rather than silently
// pass — an inconclusive host/image arch verdict is not evidence of
// compatibility.
func TestRunCheck_ArchProbeInconclusive_ReportsError(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "if [ \"$1\" = compose ]; then exit 0; fi\nexit 0")
	t.Setenv("PATH", dir)
	t.Setenv("BOID_DOCKER_SOCK_SRC", dir+"/no-engine-here.sock")
	t.Setenv("BOID_NO_AUTOSTART", "1")

	cmd := checkCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	err := runCheck(cmd, nil)
	if err == nil {
		t.Fatal("expected an error when the arch probe cannot reach the resolved engine socket")
	}
	if !strings.Contains(buf.String(), "Host / image architecture") {
		t.Errorf("output = %q, want the architecture section", buf.String())
	}
	if !strings.Contains(buf.String(), "ERROR:") {
		t.Errorf("output = %q, want an ERROR line for the inconclusive arch probe", buf.String())
	}
}
