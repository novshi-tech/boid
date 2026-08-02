package cmd

// cmd/check_test.go exercises the PR7 rewrite of `boid check`
// (docs/plans/release-onboarding.md 穴5) — the pure/fakeable pieces
// (socket-path resolution, the arch-mismatch comparison) directly, and the
// engine-dependent pieces (podmanSocketActive) via writeFakeExecutable
// (cmd/host_test.go), the same PATH-stubbing pattern host_test.go already
// uses since this sandbox has neither docker nor podman installed
// (host_test.go's own writeFakeExecutable doc comment).

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/image"
	dockerclient "github.com/moby/moby/client"
)

func TestResolveDockerSocketPath_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	if got := resolveDockerSocketPath(); got != dockerclient.DefaultDockerHost {
		t.Errorf("resolveDockerSocketPath() = %q, want default %q", got, dockerclient.DefaultDockerHost)
	}
}

func TestResolveDockerSocketPath_HonorsDockerHostEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/custom-docker.sock")
	if got := resolveDockerSocketPath(); got != "unix:///tmp/custom-docker.sock" {
		t.Errorf("resolveDockerSocketPath() = %q, want the DOCKER_HOST override", got)
	}
}

func TestResolvePodmanSocketPath_DefaultsToXDGRuntimeShape(t *testing.T) {
	t.Setenv("BOID_DOCKER_SOCK_SRC", "")
	want := "unix:///run/user/" + strconv.Itoa(os.Getuid()) + "/podman/podman.sock"
	if got := resolvePodmanSocketPath(); got != want {
		t.Errorf("resolvePodmanSocketPath() = %q, want %q (mirrors scripts/deploy-container.sh's BOID_DOCKER_SOCK_SRC default)", got, want)
	}
}

func TestResolvePodmanSocketPath_HonorsOverrideEnv(t *testing.T) {
	t.Setenv("BOID_DOCKER_SOCK_SRC", "unix:///custom/podman.sock")
	if got := resolvePodmanSocketPath(); got != "unix:///custom/podman.sock" {
		t.Errorf("resolvePodmanSocketPath() = %q, want the BOID_DOCKER_SOCK_SRC override", got)
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
	info       dockerclient.SystemInfoResult
	infoErr    error
	inspect    dockerclient.ImageInspectResult
	inspectErr error
}

func (f *fakeArchProbeAPI) Info(ctx context.Context, options dockerclient.InfoOptions) (dockerclient.SystemInfoResult, error) {
	return f.info, f.infoErr
}

func (f *fakeArchProbeAPI) ImageInspect(ctx context.Context, imageRef string, opts ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error) {
	return f.inspect, f.inspectErr
}

func newSystemInfoResult(arch string) dockerclient.SystemInfoResult {
	var r dockerclient.SystemInfoResult
	r.Info.Architecture = arch
	return r
}

func TestProbeArchMismatchWithAPI_MatchingArch_NoMismatch(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: "amd64"}},
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty (both sides resolved)", note)
	}
	if mismatch {
		t.Error("mismatch = true, want false: x86_64 (docker uname-style) normalizes to the same amd64 as the image reports")
	}
	if hostArch != "amd64" || imageArch != "amd64" {
		t.Errorf("hostArch=%q imageArch=%q, want both amd64", hostArch, imageArch)
	}
}

func TestProbeArchMismatchWithAPI_MismatchedArch_ReportsMismatch(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("aarch64"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: "amd64"}},
	}

	hostArch, imageArch, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if !mismatch {
		t.Error("mismatch = false, want true: host is arm64 (aarch64), image is amd64")
	}
	if hostArch != "arm64" {
		t.Errorf("hostArch = %q, want normalized arm64", hostArch)
	}
	if imageArch != "amd64" {
		t.Errorf("imageArch = %q, want amd64", imageArch)
	}
}

func TestProbeArchMismatchWithAPI_ImageNotPresentLocally_DegradesToNote(t *testing.T) {
	api := &fakeArchProbeAPI{
		info:       newSystemInfoResult("x86_64"),
		inspectErr: errors.New("no such image"),
	}

	_, _, mismatch, note := probeArchMismatchWithAPI(context.Background(), api, "ghcr.io/novshi-tech/boid-runner:v0.0.1")
	if mismatch {
		t.Error("mismatch = true, want false: an absent local image must never be reported as a positive mismatch")
	}
	if note == "" || !strings.Contains(note, "not present locally") {
		t.Errorf("note = %q, want a message explaining the image was not found locally", note)
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

func TestProbeArchMismatchWithAPI_UnknownImageArch_NoFalsePositive(t *testing.T) {
	// An image manifest that genuinely lacks platform metadata reports an
	// empty Architecture — must never be treated as "definitely a
	// mismatch" against a known host arch (probeArchMismatchWithAPI's own
	// doc comment: only fires when BOTH sides are positively known).
	api := &fakeArchProbeAPI{
		info:    newSystemInfoResult("x86_64"),
		inspect: dockerclient.ImageInspectResult{InspectResponse: image.InspectResponse{Architecture: ""}},
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
