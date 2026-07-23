package dispatcher

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"net"
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
	ImageInspectFunc     func(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePullFunc        func(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	NetworkListFunc      func(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemoveFunc    func(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	VolumeCreateFunc     func(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeListFunc       func(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemoveFunc     func(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)

	nextID int

	createCalls       []client.ContainerCreateOptions
	startIDs          []string
	attachCalls       []client.ContainerAttachOptions
	attachIDs         []string
	waitIDs           []string
	killCalls         []client.ContainerKillOptions
	killIDs           []string
	stopIDs           []string
	resizeCalls       []client.ContainerResizeOptions
	removeIDs         []string
	pullRefs          []string
	listFilters       []client.Filters
	inspectIDs        []string
	imageInspectRefs  []string
	volumeCreateCalls []client.VolumeCreateOptions
	volumeListCalls   int
	networkListCalls  int
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
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
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

func (f *fakeDockerAPI) VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	f.mu.Lock()
	f.volumeCreateCalls = append(f.volumeCreateCalls, options)
	f.mu.Unlock()
	if f.VolumeCreateFunc != nil {
		return f.VolumeCreateFunc(ctx, options)
	}
	// Default: a fresh volume, carrying whatever labels the caller asked
	// for — the "newly created" case. Tests exercising the "already exists
	// without labels" case supply their own VolumeCreateFunc.
	return client.VolumeCreateResult{Volume: volume.Volume{Name: options.Name, Labels: options.Labels}}, nil
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

func (f *fakeDockerAPI) VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	if f.VolumeRemoveFunc != nil {
		return f.VolumeRemoveFunc(ctx, volumeID, options)
	}
	return client.VolumeRemoveResult{}, nil
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
