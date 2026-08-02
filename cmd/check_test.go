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

	"github.com/novshi-tech/boid/internal/version"
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

func TestResolveCheckImage_DefaultsToVersionDefaultContainerImage(t *testing.T) {
	t.Setenv("BOID_IMAGE", "")
	if got := resolveCheckImage(); got != version.DefaultContainerImage() {
		t.Errorf("resolveCheckImage() = %q, want version.DefaultContainerImage() = %q", got, version.DefaultContainerImage())
	}
}

func TestResolveCheckImage_HonorsBoidImageEnv(t *testing.T) {
	// [codex round 3 review, Major]: build/container/compose.yml's own
	// image line is `${BOID_IMAGE:-...}` — BOID_IMAGE always wins when
	// set, so `boid check` must validate the SAME image `boid start`
	// would actually run under that override.
	t.Setenv("BOID_IMAGE", "ghcr.io/example/custom-runner:v9.9.9")
	if got := resolveCheckImage(); got != "ghcr.io/example/custom-runner:v9.9.9" {
		t.Errorf("resolveCheckImage() = %q, want the BOID_IMAGE override", got)
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

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
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
		info:       newSystemInfoResult("aarch64"),
		dist:       newDistributionInspectResult("amd64"),
		inspectErr: errors.New("no such image (never pulled)"),
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
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
		info:       newSystemInfoResult("aarch64"),
		dist:       newDistributionInspectResult("amd64", "arm64"),
		inspectErr: errors.New("no such image (never pulled)"),
	}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
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

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "boid-runner:latest")
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

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if mismatch {
		t.Error("mismatch = true, want false: neither source resolving must never be reported as a positive mismatch")
	}
	if note == "" {
		t.Error("note = \"\", want a message explaining neither the manifest nor a local copy could be found")
	}
}

func TestProbeArchMismatchWithAPI_InfoProbeFails_DegradesToNote(t *testing.T) {
	api := &fakeArchProbeAPI{infoErr: errors.New("connection refused")}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if mismatch {
		t.Error("mismatch = true, want false: a failed engine probe must never be reported as a positive mismatch")
	}
	if note == "" {
		t.Error("note = \"\", want a message explaining the engine info probe failed")
	}
}

func TestProbeArchMismatchWithAPI_UnknownLocalImageArch_ReportsInconclusive(t *testing.T) {
	// A local image whose manifest genuinely lacks platform metadata
	// reports an empty Architecture — must never be treated as
	// "definitely matches" a known host arch either ([codex round 3
	// review, Blocker 1]: an unresolved verdict is not a pass).
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"),
		distErr: errors.New("no registry"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: ""}},
	}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "some-custom:tag")
	if mismatch {
		t.Error("mismatch = true, want false when the image reports no architecture at all — an unresolved verdict is inconclusive, not a positive mismatch")
	}
	if note == "" {
		t.Error("note = \"\", want a message explaining the local image's architecture could not be determined")
	}
}

// TestProbeArchMismatchWithAPI_LocalImageCheckedBeforeRegistry pins [codex
// round 3 review, Blocker 2]: a locally cached image under the SAME tag
// must be checked BEFORE the registry manifest, not after. Compose's
// default pull_policy ("missing") never re-pulls a tag that already
// resolves to something cached locally, however stale or wrong-arch — so
// what's cached locally is what `boid start` will actually run, even when
// the registry currently publishes a different (matching) architecture.
func TestProbeArchMismatchWithAPI_LocalImageCheckedBeforeRegistry(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"), // host is amd64
		dist:    newDistributionInspectResult("amd64"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: "arm64"}}, // stale local cache
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if !mismatch {
		t.Error("mismatch = false, want true: the LOCALLY CACHED image is arm64 even though the registry itself is amd64 — compose runs the cached one")
	}
	if hostArch != "amd64" || imageArch != "arm64" {
		t.Errorf("hostArch=%q imageArch=%q, want amd64/arm64 (from the local cache, not the registry)", hostArch, imageArch)
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

// TestProbeArchMismatchWithAPI_Podman_UsesManifestInspectNotDistributionAPI
// pins [codex round 4 review, Blocker 1]: Podman's API server does not
// implement docker's `/distribution/{name}/json` endpoint at all (the one
// api.DistributionInspect calls) — using it unconditionally would make
// `boid check` refuse EVERY fresh Podman install that has never pulled the
// image yet, even though `boid start` itself can pull that same public
// image just fine. On engine=="podman", the not-yet-pulled path must go
// through podmanManifestArches (`podman manifest inspect`) instead, never
// api.DistributionInspect.
func TestProbeArchMismatchWithAPI_Podman_UsesManifestInspectNotDistributionAPI(t *testing.T) {
	orig := podmanManifestArches
	defer func() { podmanManifestArches = orig }()

	var calledWithImage string
	podmanManifestArches = func(ctx context.Context, image string) ([]string, error) {
		calledWithImage = image
		return []string{"amd64"}, nil
	}

	api := &fakeArchProbeAPI{
		info:       newSystemInfoResult("x86_64"),
		inspectErr: errors.New("no such image (never pulled)"),
		// DistributionInspect would "succeed" here too, deliberately, to
		// prove the podman branch does NOT consult it at all — only
		// podmanManifestArches's stubbed result should determine the
		// verdict.
		dist: newDistributionInspectResult("arm64"),
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "podman", "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false: podmanManifestArches reported amd64, matching the amd64 host")
	}
	if hostArch != "amd64" || imageArch != "amd64" {
		t.Errorf("hostArch=%q imageArch=%q, want both amd64 (from podmanManifestArches, not the DistributionInspect stub)", hostArch, imageArch)
	}
	if calledWithImage != "ghcr.io/novshi-tech/boid-runner:v0.0.1" {
		t.Errorf("podmanManifestArches called with image=%q, want the probed image", calledWithImage)
	}
}

// TestRunCheck_BoidComposeRoot_SkipsArchProbe pins [codex round 4 review,
// Major]: BOID_COMPOSE_ROOT selects deployFromCheckout's `--build` path
// (cmd/host.go), which overrides BOID_IMAGE to a locally-BUILT
// "boid-runner:latest" rather than pulling anything — a plain `docker
// build`/`podman build` with no --platform flag always builds for the
// host's own architecture, so this scenario cannot produce the mismatch
// 決定5 exists to catch. Probing anyway would report a false-negative
// failure before that local image has ever been built, even though `boid
// start` would go on to build and run it successfully.
func TestRunCheck_BoidComposeRoot_SkipsArchProbe(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "if [ \"$1\" = compose ]; then exit 0; fi\nexit 0")
	t.Setenv("PATH", dir)
	t.Setenv("BOID_COMPOSE_ROOT", "/some/checkout")
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "")
	t.Setenv("BOID_NO_AUTOSTART", "1")

	cmd := checkCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	_ = runCheck(cmd, nil)
	out := buf.String()
	if !strings.Contains(out, "skipped: BOID_COMPOSE_ROOT is set") {
		t.Errorf("output = %q, want the arch probe to report itself skipped for BOID_COMPOSE_ROOT", out)
	}
}

// TestRunCheck_BoidComposeRoot_DockerDefaultPlatformMismatch_ReportsError
// pins [codex round 5 review, Major]: DOCKER_DEFAULT_PLATFORM is the one
// way a plain `docker build` (no --platform flag, exactly what
// scripts/deploy-container.sh's --build path invokes) can still target a
// foreign architecture — the checkout-skip must not blindly assume
// host-native in that case.
func TestRunCheck_BoidComposeRoot_DockerDefaultPlatformMismatch_ReportsError(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "if [ \"$1\" = compose ]; then exit 0; fi\nexit 0")
	t.Setenv("PATH", dir)
	t.Setenv("BOID_COMPOSE_ROOT", "/some/checkout")
	// The bind source has no real engine behind it, so the Info() probe
	// inside checkoutBuildPlatformWarning will fail — which this test
	// asserts is ALSO reported as an error (isErr=true branch), not a
	// silent skip, since "cannot verify" must never be conflated with
	// "verified safe".
	t.Setenv("BOID_DOCKER_SOCK_SRC", dir+"/no-engine-here.sock")
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "linux/arm64")
	t.Setenv("BOID_NO_AUTOSTART", "1")

	cmd := checkCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	err := runCheck(cmd, nil)
	if err == nil {
		t.Fatal("expected an error when DOCKER_DEFAULT_PLATFORM is set but this host's architecture cannot be verified")
	}
	out := buf.String()
	if !strings.Contains(out, "DOCKER_DEFAULT_PLATFORM") {
		t.Errorf("output = %q, want it to mention DOCKER_DEFAULT_PLATFORM", out)
	}
	if strings.Contains(out, "skipped: BOID_COMPOSE_ROOT is set") {
		t.Errorf("output = %q, must NOT silently skip when DOCKER_DEFAULT_PLATFORM is set", out)
	}
}

func TestCheckoutBuildPlatformWarning_Unset_Skips(t *testing.T) {
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "")

	msg, isErr := checkoutBuildPlatformWarning(context.Background(), "docker")
	if msg != "" || isErr {
		t.Errorf("checkoutBuildPlatformWarning() = (%q, %v), want (\"\", false) when DOCKER_DEFAULT_PLATFORM is unset", msg, isErr)
	}
}

func TestCheckoutBuildPlatformWarning_MalformedValue_ReportsError(t *testing.T) {
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "not-a-platform")

	msg, isErr := checkoutBuildPlatformWarning(context.Background(), "docker")
	if !isErr || msg == "" {
		t.Errorf("checkoutBuildPlatformWarning() = (%q, %v), want an error for a malformed (no os/arch slash) value", msg, isErr)
	}
}

func TestIsLatestTag(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"ghcr.io/novshi-tech/boid-runner:latest", true},
		{"boid-runner:latest", true},
		{"boid-runner", true},                     // no tag at all defaults to latest
		{"ghcr.io/novshi-tech/boid-runner", true}, // no tag, registry host present
		{"ghcr.io/novshi-tech/boid-runner:v0.0.1", false},
		{"localhost:5000/boid-runner:v1", false}, // port in host, not a tag
		{"localhost:5000/boid-runner", true},     // port in host, no tag -> latest
		{"localhost:5000/boid-runner:latest", true},
	}
	for _, c := range cases {
		if got := isLatestTag(c.ref); got != c.want {
			t.Errorf("isLatestTag(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

// TestProbeArchMismatchWithAPI_LatestTag_PrefersRemoteOverStaleLocalCache
// pins [codex round 5 review, Major]: compose's pull_policy "missing"
// exception for `:latest` means a stale local cache under that tag is NOT
// what `boid start` will actually run — the registry/remote manifest is
// authoritative for `:latest`, unlike every other tag (where local-first
// is correct, per TestProbeArchMismatchWithAPI_LocalImageCheckedBeforeRegistry).
func TestProbeArchMismatchWithAPI_LatestTag_PrefersRemoteOverStaleLocalCache(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"), // host is amd64
		dist:    newDistributionInspectResult("amd64"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: "arm64"}}, // stale local :latest
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "ghcr.io/novshi-tech/boid-runner:latest")
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false: compose ALWAYS re-pulls :latest, so the registry's amd64 (not the stale local arm64 cache) is what actually runs")
	}
	if hostArch != "amd64" || imageArch != "amd64" {
		t.Errorf("hostArch=%q imageArch=%q, want both amd64 (from the registry, not the stale local cache)", hostArch, imageArch)
	}
}

// TestProbeArchMismatchWithAPI_LatestTag_FallsBackToLocalWhenRemoteFails
// pins the degrade path: if the registry cannot be reached at all for a
// `:latest` image, a local cache (even though compose would still try to
// re-pull) is still better evidence than an outright "cannot verify"
// failure.
func TestProbeArchMismatchWithAPI_LatestTag_FallsBackToLocalWhenRemoteFails(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"),
		distErr: errors.New("registry unreachable"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: "amd64"}},
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "docker", "boid-runner:latest")
	if note != "" {
		t.Fatalf("note = %q, want empty (local fallback resolved it)", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false")
	}
	if hostArch != "amd64" || imageArch != "amd64" {
		t.Errorf("hostArch=%q imageArch=%q, want both amd64", hostArch, imageArch)
	}
}

// TestPodmanManifestArches_FallsBackFromSkopeoToManifestInspect pins
// [codex round 5 review, Blocker]: for boid's own published shape (a
// single-platform, non-manifest-list image), `podman manifest inspect`
// alone reports no architecture at all (no top-level field, no per-entry
// platform list) — skopeo (tried first) is what actually resolves it via
// the image's config blob. This test fakes both CLIs on PATH to pin the
// preference order and the JSON shapes each is expected to emit.
func TestPodmanManifestArches_PrefersSkopeoOverManifestInspect(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "skopeo", `echo '{"Architecture":"amd64"}'`)
	writeFakeExecutable(t, dir, "podman", `echo 'this should never run'; exit 1`)
	t.Setenv("PATH", dir)

	archs, err := podmanManifestArches(context.Background(), "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if err != nil {
		t.Fatalf("podmanManifestArches: %v", err)
	}
	if len(archs) != 1 || archs[0] != "amd64" {
		t.Errorf("archs = %v, want [amd64] from skopeo, not podman manifest inspect", archs)
	}
}

func TestPodmanManifestArches_FallsBackToManifestListWhenSkopeoUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "podman", `echo '{"manifests":[{"platform":{"architecture":"amd64"}},{"platform":{"architecture":"arm64"}}]}'`)
	t.Setenv("PATH", dir)

	archs, err := podmanManifestArches(context.Background(), "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if err != nil {
		t.Fatalf("podmanManifestArches: %v", err)
	}
	want := map[string]bool{"amd64": true, "arm64": true}
	if len(archs) != 2 || !want[archs[0]] || !want[archs[1]] {
		t.Errorf("archs = %v, want [amd64 arm64] (order-independent) from the manifest-list fallback", archs)
	}
}

func TestPodmanManifestArches_SinglePlatformManifest_NeitherSourceResolves(t *testing.T) {
	// The realistic failure mode round 5 caught: neither skopeo (absent)
	// nor `podman manifest inspect` (a single-platform manifest has no
	// top-level architecture field) can resolve anything.
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "podman", `echo '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc"},"layers":[]}'`)
	t.Setenv("PATH", dir)

	if _, err := podmanManifestArches(context.Background(), "ghcr.io/novshi-tech/boid-runner:v0.0.1"); err == nil {
		t.Error("podmanManifestArches() = nil error, want an error when a single-platform manifest has no architecture info to extract")
	}
}
