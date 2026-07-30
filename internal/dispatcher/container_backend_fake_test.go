package dispatcher

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// This file provides a fake dockerAPI implementation shared by
// container_backend_test.go. It is a "func field" fake — every dockerAPI
// method has a matching *Func field tests can override; when nil, a
// reasonable success default runs instead, so a test that only cares about
// one method doesn't have to stub the other fifteen. Every call is recorded
// into calls for assertions that only need "was X called, how many times,
// with what arguments" rather than full behavioral control.
type fakeDockerAPI struct {
	mu sync.Mutex

	ContainerCreateFunc  func(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStartFunc   func(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspectFunc func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerAttachFunc  func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerWaitFunc    func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerKillFunc    func(ctx context.Context, containerID string, options client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerStopFunc    func(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerResizeFunc  func(ctx context.Context, containerID string, options client.ContainerResizeOptions) (client.ContainerResizeResult, error)
	ContainerRemoveFunc  func(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerListFunc    func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerLogsFunc    func(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ImageInspectFunc     func(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePullFunc        func(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	NetworkListFunc      func(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemoveFunc    func(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	NetworkCreateFunc    func(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkConnectFunc   func(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkInspectFunc   func(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	VolumeCreateFunc     func(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeListFunc       func(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemoveFunc     func(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
	// ServerVersionFunc / InfoFunc back resolveUsernsMode's engine-identity
	// probe (container_backend_userns.go). Left nil by every test that does
	// not care, whose zero-valued results identify no particular engine —
	// which is exactly the "plain docker, leave UsernsMode unset" case those
	// tests already assert byte-for-byte.
	ServerVersionFunc func(ctx context.Context, options client.ServerVersionOptions) (client.ServerVersionResult, error)
	InfoFunc          func(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)

	nextID int
	// lastConn is the fakeAttachConn most recently handed out by an
	// ContainerAttachFunc that calls recordAttachConn — the workspace-init
	// wiring test reads back what was written to the container's stdin.
	lastConn *fakeAttachConn

	createCalls         []client.ContainerCreateOptions
	startIDs            []string
	attachCalls         []client.ContainerAttachOptions
	attachIDs           []string
	waitIDs             []string
	killCalls           []client.ContainerKillOptions
	killIDs             []string
	stopIDs             []string
	resizeCalls         []client.ContainerResizeOptions
	removeIDs           []string
	pullRefs            []string
	listFilters         []client.Filters
	logsIDs             []string
	inspectIDs          []string
	imageInspectRefs    []string
	volumeCreateCalls   []client.VolumeCreateOptions
	volumeRemoveIDs     []string
	volumeListCalls     int
	networkListCalls    int
	networkCreateCalls  []client.NetworkCreateOptions
	networkCreateNames  []string
	networkConnectCalls []client.NetworkConnectOptions
	networkConnectIDs   []string
	networkInspectIDs   []string

	// volumes is the modelled volume store: name -> the labels that volume
	// actually carries. It exists so VolumeCreate can answer with something
	// OTHER than what the caller asked for, which is the only shape in which
	// the engine's real behaviour can be observed — see VolumeCreate.
	// VolumeRemove takes entries back out again, so a name can go through more
	// than one incarnation within a single test — see VolumeRemove.
	volumes map[string]map[string]string
}

// seedVolume pre-creates a volume the fake engine already holds, carrying
// labels the caller did not ask for.
//
// This is the input every identity check needs and nothing else can produce.
// VolumeCreate against an EXISTING name returns that volume with its OWN
// labels and silently discards the request's (measured against podman 4.9.3,
// see dockerres.LabelWorkspaceHomeID), so "the volume boid ends up mounting is
// not the one it resolved" is expressible only by putting a different volume
// there first. Pass nil labels to model a volume created by somebody other than
// boid — one that carries no identity at all.
func (f *fakeDockerAPI) seedVolume(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumes == nil {
		f.volumes = map[string]map[string]string{}
	}
	stored := map[string]string{}
	for k, v := range labels {
		stored[k] = v
	}
	f.volumes[name] = stored
}

var _ dockerAPI = (*fakeDockerAPI)(nil)

func (f *fakeDockerAPI) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, options)
	f.mu.Unlock()
	if f.ContainerCreateFunc != nil {
		return f.ContainerCreateFunc(ctx, options)
	}
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("fake-container-%d", f.nextID)
	f.mu.Unlock()
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fakeDockerAPI) ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	f.startIDs = append(f.startIDs, containerID)
	f.mu.Unlock()
	if f.ContainerStartFunc != nil {
		return f.ContainerStartFunc(ctx, containerID, options)
	}
	return client.ContainerStartResult{}, nil
}

func (f *fakeDockerAPI) ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	f.inspectIDs = append(f.inspectIDs, containerID)
	f.mu.Unlock()
	if f.ContainerInspectFunc != nil {
		return f.ContainerInspectFunc(ctx, containerID, options)
	}
	return client.ContainerInspectResult{}, fmt.Errorf("fakeDockerAPI: no ContainerInspectFunc configured for %q", containerID)
}

func (f *fakeDockerAPI) ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.mu.Lock()
	f.attachCalls = append(f.attachCalls, options)
	f.attachIDs = append(f.attachIDs, containerID)
	f.mu.Unlock()
	if f.ContainerAttachFunc != nil {
		return f.ContainerAttachFunc(ctx, containerID, options)
	}
	conn := newFakeAttachConn()
	f.recordAttachConn(conn)
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
}

// recordAttachConn remembers the connection a ContainerAttach handed out, so a
// test can read back what the implementation wrote to the container's stdin.
func (f *fakeDockerAPI) recordAttachConn(conn *fakeAttachConn) {
	f.mu.Lock()
	f.lastConn = conn
	f.mu.Unlock()
}

func (f *fakeDockerAPI) ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
	f.mu.Lock()
	f.waitIDs = append(f.waitIDs, containerID)
	f.mu.Unlock()
	if f.ContainerWaitFunc != nil {
		return f.ContainerWaitFunc(ctx, containerID, options)
	}
	resCh := make(chan container.WaitResponse, 1)
	resCh <- container.WaitResponse{StatusCode: 0}
	return client.ContainerWaitResult{Result: resCh, Error: make(chan error, 1)}
}

func (f *fakeDockerAPI) ContainerKill(ctx context.Context, containerID string, options client.ContainerKillOptions) (client.ContainerKillResult, error) {
	f.mu.Lock()
	f.killCalls = append(f.killCalls, options)
	f.killIDs = append(f.killIDs, containerID)
	f.mu.Unlock()
	if f.ContainerKillFunc != nil {
		return f.ContainerKillFunc(ctx, containerID, options)
	}
	return client.ContainerKillResult{}, nil
}

func (f *fakeDockerAPI) ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	f.stopIDs = append(f.stopIDs, containerID)
	f.mu.Unlock()
	if f.ContainerStopFunc != nil {
		return f.ContainerStopFunc(ctx, containerID, options)
	}
	return client.ContainerStopResult{}, nil
}

func (f *fakeDockerAPI) ContainerResize(ctx context.Context, containerID string, options client.ContainerResizeOptions) (client.ContainerResizeResult, error) {
	f.mu.Lock()
	f.resizeCalls = append(f.resizeCalls, options)
	f.mu.Unlock()
	if f.ContainerResizeFunc != nil {
		return f.ContainerResizeFunc(ctx, containerID, options)
	}
	return client.ContainerResizeResult{}, nil
}

func (f *fakeDockerAPI) ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	f.removeIDs = append(f.removeIDs, containerID)
	f.mu.Unlock()
	if f.ContainerRemoveFunc != nil {
		return f.ContainerRemoveFunc(ctx, containerID, options)
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDockerAPI) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	f.listFilters = append(f.listFilters, options.Filters)
	f.mu.Unlock()
	if f.ContainerListFunc != nil {
		return f.ContainerListFunc(ctx, options)
	}
	return client.ContainerListResult{}, nil
}

func (f *fakeDockerAPI) ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.mu.Lock()
	f.logsIDs = append(f.logsIDs, containerID)
	f.mu.Unlock()
	if f.ContainerLogsFunc != nil {
		return f.ContainerLogsFunc(ctx, containerID, options)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDockerAPI) ImageInspect(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	f.imageInspectRefs = append(f.imageInspectRefs, imageRef)
	f.mu.Unlock()
	if f.ImageInspectFunc != nil {
		return f.ImageInspectFunc(ctx, imageRef, opts...)
	}
	// Default: the image is already present locally, with no
	// boid.runner_protocol label — a valid answer for every Launch-path
	// test that isn't specifically exercising image selection/override
	// validation (those tests supply their own ImageInspectFunc).
	return imageInspectResultWithLabel(""), nil
}

func (f *fakeDockerAPI) ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	f.pullRefs = append(f.pullRefs, ref)
	f.mu.Unlock()
	if f.ImagePullFunc != nil {
		return f.ImagePullFunc(ctx, ref, options)
	}
	return fakePullResponse{}, nil
}

func (f *fakeDockerAPI) NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error) {
	f.mu.Lock()
	f.networkListCalls++
	f.mu.Unlock()
	if f.NetworkListFunc != nil {
		return f.NetworkListFunc(ctx, options)
	}
	return client.NetworkListResult{}, nil
}

func (f *fakeDockerAPI) NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	if f.NetworkRemoveFunc != nil {
		return f.NetworkRemoveFunc(ctx, networkID, options)
	}
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeDockerAPI) NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.mu.Lock()
	f.networkCreateCalls = append(f.networkCreateCalls, options)
	f.networkCreateNames = append(f.networkCreateNames, name)
	f.mu.Unlock()
	if f.NetworkCreateFunc != nil {
		return f.NetworkCreateFunc(ctx, name, options)
	}
	return client.NetworkCreateResult{ID: "fake-network-" + name}, nil
}

// NetworkInspect's nil-Func default answers with a network carrying NO IPAM
// config at all — the "engine told us nothing about this network's subnets"
// case. That is deliberately the default rather than a plausible-looking
// 10.x/16: every pre-existing test in this package asserts NO_PROXY
// byte-for-byte, so a default that invented a subnet would silently append a
// CIDR to all of their expectations.
func (f *fakeDockerAPI) NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	f.mu.Lock()
	f.networkInspectIDs = append(f.networkInspectIDs, networkID)
	f.mu.Unlock()
	if f.NetworkInspectFunc != nil {
		return f.NetworkInspectFunc(ctx, networkID, options)
	}
	return client.NetworkInspectResult{}, nil
}

func (f *fakeDockerAPI) NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	f.mu.Lock()
	f.networkConnectCalls = append(f.networkConnectCalls, options)
	f.networkConnectIDs = append(f.networkConnectIDs, networkID)
	f.mu.Unlock()
	if f.NetworkConnectFunc != nil {
		return f.NetworkConnectFunc(ctx, networkID, options)
	}
	return client.NetworkConnectResult{}, nil
}

// VolumeCreate models the Engine API's idempotent create: a name that is not
// there yet is created with the requested labels, and a name that IS there
// comes back UNCHANGED, with its own labels, the request's discarded.
//
// The second half is the whole reason this is stateful rather than an echo.
// Every identity check boid makes rests on it — "this volume is still the one
// the completion marker describes" is only answerable because a surviving
// volume reports the label it was born with — and an echoing fake makes the
// request and the response the same value, so a caller that never compares them
// is indistinguishable from one that does. That is exactly the gap that let
// both PR6 use sites ship without verifying the returned identity (codex review
// Major 1): every assertion in this package looked at what was REQUESTED.
//
// Measured against podman 4.9.3 on 2026-07-27 (three creates of one name
// carrying different label sets all came back with the first call's labels);
// docker behaves the same way, and the API has no volume-label-update endpoint
// at all.
func (f *fakeDockerAPI) VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	f.mu.Lock()
	f.volumeCreateCalls = append(f.volumeCreateCalls, options)
	f.mu.Unlock()
	if f.VolumeCreateFunc != nil {
		return f.VolumeCreateFunc(ctx, options)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumes == nil {
		f.volumes = map[string]map[string]string{}
	}
	if existing, ok := f.volumes[options.Name]; ok {
		return client.VolumeCreateResult{Volume: volume.Volume{Name: options.Name, Labels: existing}}, nil
	}
	created := map[string]string{}
	for k, v := range options.Labels {
		created[k] = v
	}
	f.volumes[options.Name] = created
	return client.VolumeCreateResult{Volume: volume.Volume{Name: options.Name, Labels: created}}, nil
}

func (f *fakeDockerAPI) VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
	f.mu.Lock()
	f.volumeListCalls++
	f.mu.Unlock()
	if f.VolumeListFunc != nil {
		return f.VolumeListFunc(ctx, options)
	}
	return client.VolumeListResult{}, nil
}

// VolumeRemove models `docker volume rm`: the volume leaves the store, and the
// next VolumeCreate for that name therefore creates a NEW one carrying the
// labels of whoever asked for it.
//
// The store mutation is the whole point, and it is the half a call recorder
// cannot supply. VolumeCreate answering with an existing volume's own labels
// (see above) is only half the engine's contract; without the removal half, a
// name boid has ever seen keeps answering with its first incarnation's identity
// forever, so "the volume was removed and re-created underneath us" — the
// accident every identity check in this package exists to catch — is the one
// sequence the fake reports as nothing having happened (codex review of PR6,
// Minor 1).
func (f *fakeDockerAPI) VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.mu.Lock()
	f.volumeRemoveIDs = append(f.volumeRemoveIDs, volumeID)
	f.mu.Unlock()
	if f.VolumeRemoveFunc != nil {
		return f.VolumeRemoveFunc(ctx, volumeID, options)
	}
	f.mu.Lock()
	delete(f.volumes, volumeID)
	f.mu.Unlock()
	return client.VolumeRemoveResult{}, nil
}

// volumeRemoveIDsSnapshot returns the volume names VolumeRemove has been
// called with so far — the symmetric counterpart of removeIDs for
// containers, added so the workspace-HOME containment tests
// (container_backend_workspace_home_test.go) can assert on which volumes a
// reap sweep did NOT touch. A copy under f.mu, since ReapOrphans's internal
// reap.Run pass shares this fake.
func (f *fakeDockerAPI) volumeRemoveIDsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.volumeRemoveIDs...)
}

func (f *fakeDockerAPI) ServerVersion(ctx context.Context, options client.ServerVersionOptions) (client.ServerVersionResult, error) {
	if f.ServerVersionFunc != nil {
		return f.ServerVersionFunc(ctx, options)
	}
	return client.ServerVersionResult{}, nil
}

func (f *fakeDockerAPI) Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error) {
	if f.InfoFunc != nil {
		return f.InfoFunc(ctx, options)
	}
	return client.SystemInfoResult{}, nil
}

// waitCallCount returns how many times ContainerWait has been invoked so
// far — used by TestContainerSession_Wait_SingleOwnerFanOut to pin the
// single-owner contract.
func (f *fakeDockerAPI) waitCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waitIDs)
}

// removeCallCount returns how many times ContainerRemove has been invoked
// so far — used by TestContainerSession_TranscriptSpool_SurvivesContainerRemove
// to poll (race-free) for waitLoop's asynchronous remove call without
// reading f.removeIDs directly (which races against the append under
// f.mu in ContainerRemove above).
func (f *fakeDockerAPI) removeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removeIDs)
}

// fakePullResponse is a no-op client.ImagePullResponse: an already-drained,
// already-complete pull. Sufficient for every test here since none needs to
// inspect pull progress.
type fakePullResponse struct{}

func (fakePullResponse) Read([]byte) (int, error)   { return 0, io.EOF }
func (fakePullResponse) Close() error               { return nil }
func (fakePullResponse) Wait(context.Context) error { return nil }

// JSONMessages satisfies client.ImagePullResponse's iterator method with an
// empty sequence — nothing in this test suite consumes pull progress
// messages.
func (fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {}
}

// fakeAttachConn is a controllable net.Conn (+ client.CloseWriter) standing
// in for a real docker-attach hijacked connection. Reads deliver whatever
// the test feeds via feed/feedFrame; writes are only recorded (there is no
// real "other side" consuming stdin in these tests).
type fakeAttachConn struct {
	mu          sync.Mutex
	outR        *io.PipeReader
	outW        *io.PipeWriter
	writes      [][]byte
	closeWrites int32
	closed      bool
}

var (
	_ net.Conn           = (*fakeAttachConn)(nil)
	_ client.CloseWriter = (*fakeAttachConn)(nil)
)

func newFakeAttachConn() *fakeAttachConn {
	r, w := io.Pipe()
	return &fakeAttachConn{outR: r, outW: w}
}

func (c *fakeAttachConn) Read(p []byte) (int, error) { return c.outR.Read(p) }

func (c *fakeAttachConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}

func (c *fakeAttachConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.outW.CloseWithError(io.EOF)
}

func (c *fakeAttachConn) CloseWrite() error {
	atomic.AddInt32(&c.closeWrites, 1)
	return nil
}

// isClosed reports whether Close has been called on c — a race-safe
// (under c.mu, the same lock Close itself uses) accessor for tests that
// want to observe whether something explicitly closed this connection,
// as opposed to merely causing its Read side to error out (e.g. a raw
// c.outW.CloseWithError from a test simulating a remote hangup, which
// does NOT set c.closed — see TestContainerBackend_Adopt_
// ReattachClosesThePreviousGenerationsConnection's own doc comment for
// why that distinction is exactly the point).
func (c *fakeAttachConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// feedFrame writes one docker multiplexed-stream frame (non-TTY mode):
// 8-byte header (stream type + big-endian uint32 length) followed by the
// payload — the same shape demuxDockerFrame parses.
func (c *fakeAttachConn) feedFrame(streamType byte, p []byte) {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	_, _ = c.outW.Write(header)
	_, _ = c.outW.Write(p)
}

func (*fakeAttachConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (*fakeAttachConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (*fakeAttachConn) SetDeadline(time.Time) error      { return nil }
func (*fakeAttachConn) SetReadDeadline(time.Time) error  { return nil }
func (*fakeAttachConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// imageInspectResultWithLabel builds a client.ImageInspectResult carrying
// the given boid.runner_protocol label value (used by the image-override
// tests). An empty labelValue omits the label entirely.
func imageInspectResultWithLabel(labelValue string) client.ImageInspectResult {
	cfg := &dockerspec.DockerOCIImageConfig{}
	if labelValue != "" {
		cfg.ImageConfig = ocispec.ImageConfig{Labels: map[string]string{boidRunnerProtocolLabel: labelValue}}
	}
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{Config: cfg}}
}
