package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/dockerres"
)

// stubWorkspaceHomeVolumeAPI is a WorkspaceHomeVolumeAPI that answers from
// canned data and records what it was asked, so the store's contracts can be
// pinned without an engine. Deliberately NOT the package's larger fake docker
// API: the point of WorkspaceHomeVolumeAPI being four methods wide is that a
// stub for it is four methods wide.
type stubWorkspaceHomeVolumeAPI struct {
	// volumes is the engine's whole volume set, keyed by name.
	volumes map[string]volume.Volume
	// sizes is what GET /system/df reports per volume name. A name absent
	// from this map models an engine that listed the volume but reported no
	// usage data for it.
	sizes map[string]int64

	inspectErr error
	listErr    error
	removeErr  error
	dfErr      error

	// dfHangs models the engine taking longer to answer GET /system/df than
	// the store is willing to wait: DiskUsage blocks until its context is
	// done and returns that context's error. This is the one failure mode the
	// canned-error fields above cannot express, and the store used to have a
	// real bug behind it — see
	// TestWorkspaceHomeVolumeStore_Remove_SlowDiskUsageDoesNotEatTheDeleteDeadline.
	dfHangs bool

	// slowBy, when non-zero, is how long EVERY engine call takes before it
	// answers — an engine that is merely slow rather than broken. It is what
	// lets a test spend wall-clock time inside one call and then observe what
	// budget the next one was given, which a canned error cannot show.
	slowBy time.Duration

	// Recorded calls.
	inspected    []string
	listFilters  []client.Filters
	removed      []string
	removeForced []bool
	dfOptions    []client.DiskUsageOptions
}

// Every method starts by honouring its context, which a stub built only from
// canned return values would not. Without it a store that handed an
// already-expired context to the engine would still look like it worked here,
// while failing against any real client — moby's does its deadline check in
// the HTTP round trip, so an expired context turns into an immediate error
// rather than a request.
func (s *stubWorkspaceHomeVolumeAPI) VolumeInspect(ctx context.Context, id string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	s.inspected = append(s.inspected, id)
	s.spendTime()
	if err := ctx.Err(); err != nil {
		return client.VolumeInspectResult{}, err
	}
	if s.inspectErr != nil {
		return client.VolumeInspectResult{}, s.inspectErr
	}
	v, ok := s.volumes[id]
	if !ok {
		return client.VolumeInspectResult{}, errdefs.ErrNotFound.WithMessage("no such volume: " + id)
	}
	return client.VolumeInspectResult{Volume: v}, nil
}

func (s *stubWorkspaceHomeVolumeAPI) VolumeList(ctx context.Context, opts client.VolumeListOptions) (client.VolumeListResult, error) {
	s.listFilters = append(s.listFilters, opts.Filters)
	if err := ctx.Err(); err != nil {
		return client.VolumeListResult{}, err
	}
	if s.listErr != nil {
		return client.VolumeListResult{}, s.listErr
	}
	var items []volume.Volume
	for _, v := range s.volumes {
		if matchesFilters(v, opts.Filters) {
			items = append(items, v)
		}
	}
	return client.VolumeListResult{Items: items}, nil
}

func (s *stubWorkspaceHomeVolumeAPI) VolumeRemove(ctx context.Context, id string, opts client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	s.removed = append(s.removed, id)
	s.removeForced = append(s.removeForced, opts.Force)
	if err := ctx.Err(); err != nil {
		return client.VolumeRemoveResult{}, err
	}
	if s.removeErr != nil {
		return client.VolumeRemoveResult{}, s.removeErr
	}
	delete(s.volumes, id)
	return client.VolumeRemoveResult{}, nil
}

func (s *stubWorkspaceHomeVolumeAPI) DiskUsage(ctx context.Context, opts client.DiskUsageOptions) (client.DiskUsageResult, error) {
	s.dfOptions = append(s.dfOptions, opts)
	if s.dfHangs {
		<-ctx.Done()
		return client.DiskUsageResult{}, ctx.Err()
	}
	s.spendTime()
	if err := ctx.Err(); err != nil {
		return client.DiskUsageResult{}, err
	}
	if s.dfErr != nil {
		return client.DiskUsageResult{}, s.dfErr
	}
	var items []volume.Volume
	for name, v := range s.volumes {
		size, known := s.sizes[name]
		if known {
			v.UsageData = &volume.UsageData{Size: size, RefCount: 0}
		}
		items = append(items, v)
	}
	return client.DiskUsageResult{Volumes: client.VolumesDiskUsage{Items: items}}, nil
}

func (s *stubWorkspaceHomeVolumeAPI) spendTime() {
	if s.slowBy > 0 {
		time.Sleep(s.slowBy)
	}
}

// withShortWorkspaceHomeVolumeTimeout shrinks the per-engine-call deadline for
// one test, so a case about a deadline expiring does not have to wait 30
// seconds for it.
func withShortWorkspaceHomeVolumeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	restore := workspaceHomeVolumeTimeout
	workspaceHomeVolumeTimeout = d
	t.Cleanup(func() { workspaceHomeVolumeTimeout = restore })
}

// matchesFilters implements just enough of the engine's label filter for the
// store's single query shape: a bare key means "label present", key=value
// means an exact match (both verified against podman 4.9.3 while designing
// PR7 — see the store's own doc comment).
func matchesFilters(v volume.Volume, f client.Filters) bool {
	for term, values := range f {
		if term != "label" {
			continue
		}
		for expr := range values {
			key, want, hasValue := splitLabelExpr(expr)
			got, ok := v.Labels[key]
			if !ok {
				return false
			}
			if hasValue && got != want {
				return false
			}
		}
	}
	return true
}

func splitLabelExpr(expr string) (key, value string, hasValue bool) {
	for i := 0; i < len(expr); i++ {
		if expr[i] == '=' {
			return expr[:i], expr[i+1:], true
		}
	}
	return expr, "", false
}

func homeVolume(installID, slug string, labels map[string]string) volume.Volume {
	name := dockerres.WorkspaceHomeVolumeName(installID, slug)
	l := map[string]string{dockerres.LabelWorkspaceHome: slug}
	if installID != "" {
		l[dockerres.LabelWorkspaceHomeInstallID] = installID
	}
	for k, v := range labels {
		l[k] = v
	}
	return volume.Volume{Name: name, Labels: l}
}

// --- Get ---

// TestWorkspaceHomeVolumeStore_Get_NamesTheVolumeTheDispatcherMounts pins the
// single most load-bearing property of the whole rewiring: the name this
// store reports (and, in Remove, DELETES) is the same deterministic name
// resolveWorkspaceHome hands the job container.
func TestWorkspaceHomeVolumeStore_Get_NamesTheVolumeTheDispatcherMounts(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{volumes: map[string]volume.Volume{}}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "install-abcdefgh-1234"}

	got := store.Get(context.Background(), "team-a")

	want := dockerres.WorkspaceHomeVolumeName("install-abcdefgh-1234", "team-a")
	if got.Volume != want {
		t.Errorf("Volume = %q, want %q (the name dispatcher.resolveWorkspaceHome mounts)", got.Volume, want)
	}
	if got.Slug != "team-a" {
		t.Errorf("Slug = %q, want team-a", got.Slug)
	}
}

func TestWorkspaceHomeVolumeStore_Get_NotYetCreated_NotAnError(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{volumes: map[string]volume.Volume{}}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")

	if got.Exists {
		t.Error("Exists = true, want false for a workspace never dispatched into")
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", got.Bytes)
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty (a missing home is not an error)", got.SizeError)
	}
}

func TestWorkspaceHomeVolumeStore_Get_ExistingVolume_ReportsDiskUsageSize(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		sizes:   map[string]int64{name: 1234},
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")

	if !got.Exists {
		t.Fatal("Exists = false, want true")
	}
	if got.Bytes != 1234 {
		t.Errorf("Bytes = %d, want 1234", got.Bytes)
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty", got.SizeError)
	}
}

// TestWorkspaceHomeVolumeStore_Get_AsksDiskUsageForVolumesVerbosely pins the
// two DiskUsageOptions fields that are not cosmetic: without Verbose the moby
// client drops Items on its way out on any engine speaking API >= 1.52, so
// every size would silently come back unknown against a modern docker.
func TestWorkspaceHomeVolumeStore_Get_AsksDiskUsageForVolumesVerbosely(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		sizes:   map[string]int64{name: 1},
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	store.Get(context.Background(), "team-a")

	if len(api.dfOptions) != 1 {
		t.Fatalf("DiskUsage called %d times, want 1", len(api.dfOptions))
	}
	if !api.dfOptions[0].Volumes {
		t.Error("DiskUsageOptions.Volumes = false, want true")
	}
	if !api.dfOptions[0].Verbose {
		t.Error("DiskUsageOptions.Verbose = false, want true (Items is dropped without it on API >= 1.52)")
	}
}

func TestWorkspaceHomeVolumeStore_Get_DiskUsageFailure_ReportsSizeErrorNotExistence(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		dfErr:   errors.New("engine busy"),
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")

	if !got.Exists {
		t.Error("Exists = false, want true (the volume is there; only sizing failed)")
	}
	if got.SizeError == "" {
		t.Error("SizeError = empty, want the DiskUsage failure")
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0 alongside a SizeError", got.Bytes)
	}
}

// TestWorkspaceHomeVolumeStore_Get_EngineReportsNoUsageData_IsASizeError pins
// that "the engine listed it but gave no number" is reported as an unknown
// size rather than as a confident 0 B.
func TestWorkspaceHomeVolumeStore_Get_EngineReportsNoUsageData_IsASizeError(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		sizes:   map[string]int64{}, // listed by df, but with no UsageData
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")

	if !got.Exists {
		t.Fatal("Exists = false, want true")
	}
	if got.SizeError == "" {
		t.Error("SizeError = empty, want an 'engine reported no size' message rather than a confident 0 B")
	}
}

func TestWorkspaceHomeVolumeStore_Get_InspectFailure_ReportsSizeError(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{
		volumes:    map[string]volume.Volume{},
		inspectErr: errors.New("permission denied"),
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")

	if got.Exists {
		t.Error("Exists = true, want false (existence could not be established)")
	}
	if got.SizeError == "" {
		t.Error("SizeError = empty, want the inspect failure reported somewhere")
	}
}

// --- List ---

// TestWorkspaceHomeVolumeStore_List_EnumeratesOnTheLabelNotTheName pins 論点
// a-2's D7: dockerres.SanitizeNamePart is not invertible, so the slug has to
// come off the boid.workspace_home LABEL. A volume carrying the label is
// listed under the label's value even when the sanitized name would not spell
// that slug back.
func TestWorkspaceHomeVolumeStore_List_EnumeratesOnTheLabelNotTheName(t *testing.T) {
	// A slug containing a character SanitizeNamePart rewrites: the name says
	// "team-a" but the workspace really is "team/a".
	slug := "team/a"
	name := dockerres.WorkspaceHomeVolumeName("inst", slug)
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", slug, nil)},
		sizes:   map[string]int64{name: 99},
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	if got[0].Slug != slug {
		t.Errorf("Slug = %q, want %q (read off the label, not parsed back out of the name)", got[0].Slug, slug)
	}
	if got[0].Bytes != 99 {
		t.Errorf("Bytes = %d, want 99", got[0].Bytes)
	}
	if !got[0].Exists {
		t.Error("Exists = false, want true (it was enumerated, so it exists)")
	}
}

// TestWorkspaceHomeVolumeStore_List_FiltersOnLabelPresence pins that the
// engine query enumerates on the mere PRESENCE of boid.workspace_home rather
// than on boid.workspace_home_install_id: PR6/PR1 only attach the install-id
// label when the install HAS an id, so an install-id-scoped query silently
// lists nothing on an install without one.
func TestWorkspaceHomeVolumeStore_List_FiltersOnLabelPresence(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{volumes: map[string]volume.Volume{}}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	if _, err := store.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(api.listFilters) != 1 {
		t.Fatalf("VolumeList called %d times, want 1", len(api.listFilters))
	}
	labels := api.listFilters[0]["label"]
	if !labels[dockerres.LabelWorkspaceHome] {
		t.Errorf("filters = %v, want a bare %q presence term", api.listFilters[0], dockerres.LabelWorkspaceHome)
	}
	for term := range labels {
		if term != dockerres.LabelWorkspaceHome {
			t.Errorf("unexpected extra label filter term %q — an install-id-scoped term lists nothing on an install with no id", term)
		}
	}
}

// TestWorkspaceHomeVolumeStore_List_SkipsOtherInstallsVolumes pins the
// install scoping: two boid installs sharing one engine must not report each
// other's homes (which would then be flagged orphan and invite a
// `docker volume rm` of somebody else's credentials). Scoping is by NAME
// reconstruction — dockerres.WorkspaceHomeVolumeName(myInstallID, labelSlug)
// == v.Name — rather than by the install-id label, so it works identically
// whether or not that optional label was attached.
func TestWorkspaceHomeVolumeStore_List_SkipsOtherInstallsVolumes(t *testing.T) {
	mine := dockerres.WorkspaceHomeVolumeName("inst-a", "team-a")
	theirs := dockerres.WorkspaceHomeVolumeName("inst-b", "team-b")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{
			mine:   homeVolume("inst-a", "team-a", nil),
			theirs: homeVolume("inst-b", "team-b", nil),
		},
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst-a"}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (only this install's homes): %+v", len(got), got)
	}
	if got[0].Volume != mine {
		t.Errorf("Volume = %q, want %q", got[0].Volume, mine)
	}
}

func TestWorkspaceHomeVolumeStore_List_SortedBySlug(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{volumes: map[string]volume.Volume{}}
	for _, slug := range []string{"zeta", "alpha", "mid"} {
		api.volumes[dockerres.WorkspaceHomeVolumeName("inst", slug)] = homeVolume("inst", slug, nil)
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var slugs []string
	for _, e := range got {
		slugs = append(slugs, e.Slug)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if i >= len(slugs) || slugs[i] != want[i] {
			t.Fatalf("slugs = %v, want %v", slugs, want)
		}
	}
}

func TestWorkspaceHomeVolumeStore_List_VolumeListFailure_IsAnError(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{volumes: map[string]volume.Volume{}, listErr: errors.New("engine down")}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("List returned nil error, want the enumeration failure")
	}
}

// TestWorkspaceHomeVolumeStore_List_DiskUsageFailure_StillListsWithSizeErrors
// pins that an unavailable size does not cost the caller the listing itself —
// which is the half that drives orphan detection.
func TestWorkspaceHomeVolumeStore_List_DiskUsageFailure_StillListsWithSizeErrors(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		dfErr:   errors.New("engine busy"),
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v, want the listing to survive a sizing failure", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].SizeError == "" {
		t.Error("SizeError = empty, want the DiskUsage failure")
	}
}

// --- Remove ---

func TestWorkspaceHomeVolumeStore_Remove_RemovesTheVolumeAndReportsPriorSize(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		sizes:   map[string]int64{name: 4242},
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	info, deleted, err := store.Remove(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !deleted {
		t.Error("deleted = false, want true")
	}
	if info.Bytes != 4242 {
		t.Errorf("info.Bytes = %d, want 4242 (size as observed before deletion)", info.Bytes)
	}
	if len(api.removed) != 1 || api.removed[0] != name {
		t.Errorf("removed = %v, want [%s]", api.removed, name)
	}
	if _, still := api.volumes[name]; still {
		t.Error("volume still present after Remove")
	}
}

func TestWorkspaceHomeVolumeStore_Remove_NoVolume_NoOpNotAnError(t *testing.T) {
	api := &stubWorkspaceHomeVolumeAPI{volumes: map[string]volume.Volume{}}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	info, deleted, err := store.Remove(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Remove: %v, want nil (nothing to delete is not an error)", err)
	}
	if deleted {
		t.Error("deleted = true, want false (nothing was there)")
	}
	if info.Exists {
		t.Error("info.Exists = true, want false")
	}
}

// TestWorkspaceHomeVolumeStore_Remove_InUse_SurfacesTheConflict pins 論点 a-2's
// D6. Measured against podman 4.9.3 (2026-07-27): DELETE /volumes/<name> and
// DELETE /volumes/<name>?force=1 BOTH return 409 "volume is being used by the
// following container(s): <id>" while a container holds the volume — the
// engine's force flag suppresses 404, it does not tear a volume away from a
// running container. Reporting that as a success would tell an operator their
// credentials were destroyed when they were not.
func TestWorkspaceHomeVolumeStore_Remove_InUse_SurfacesTheConflict(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes:   map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		removeErr: errors.New("volume is being used by the following container(s): abc123"),
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	_, deleted, err := store.Remove(context.Background(), "team-a")
	if err == nil {
		t.Fatal("Remove returned nil error, want the engine's in-use conflict")
	}
	if deleted {
		t.Error("deleted = true, want false (the volume is still there)")
	}
}

// TestWorkspaceHomeVolumeStore_Remove_SizeErrorDoesNotBlockDeletion carries
// over PR #791 Should-fix #2 across the mechanism change: sizing is
// best-effort and must never gate the delete attempt.
func TestWorkspaceHomeVolumeStore_Remove_SizeErrorDoesNotBlockDeletion(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		dfErr:   errors.New("engine busy"),
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	info, deleted, err := store.Remove(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Remove: %v, want nil (a sizing failure is not a deletion error)", err)
	}
	if !deleted {
		t.Error("deleted = false, want true (a sizing failure must not block deletion)")
	}
	if info.SizeError == "" {
		t.Error("info.SizeError = empty, want the injected sizing failure")
	}
	if _, still := api.volumes[name]; still {
		t.Error("volume still present after Remove")
	}
}

// TestWorkspaceHomeVolumeStore_Remove_SlowDiskUsageDoesNotEatTheDeleteDeadline
// pins the same "sizing never blocks the delete" contract against the failure
// mode a canned error cannot express: an engine that does not answer at all
// (PR7 round-2 codex review, Major 1).
//
// The bug it exists for: the store used to take ONE deadline for the whole
// Remove and hand the same context to DiskUsage and then to VolumeRemove. A
// DiskUsage that ran out that deadline therefore left VolumeRemove with an
// already-expired context, so the delete failed instantly without a request
// ever leaving the process — and, because WorkspaceHandler.Remove deletes the
// DB row FIRST, the outcome was the worst available one: the workspace is
// gone from boid and a volume full of harness credentials stays on the engine.
// A canned dfErr could not catch it, because returning an error promptly is
// exactly the case where the shared deadline still had budget left.
func TestWorkspaceHomeVolumeStore_Remove_SlowDiskUsageDoesNotEatTheDeleteDeadline(t *testing.T) {
	withShortWorkspaceHomeVolumeTimeout(t, 50*time.Millisecond)

	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		dfHangs: true,
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	info, deleted, err := store.Remove(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Remove: %v, want nil — a sizing call that timed out must not spend the deletion's deadline", err)
	}
	if !deleted {
		t.Error("deleted = false, want true")
	}
	if info.SizeError == "" {
		t.Error("info.SizeError = empty, want the sizing timeout reported")
	}
	if _, still := api.volumes[name]; still {
		t.Error("volume still present after Remove: the deletion ran on an already-expired context")
	}
}

// TestWorkspaceHomeVolumeStore_Get_SlowInspectDoesNotEatTheSizingDeadline is
// the same property on Get's two-call chain, and the case a merely-slow engine
// makes visible where a broken one would not: each call is comfortably inside
// the per-call budget (250ms of 400ms) while their SUM is not, so the sizing
// half succeeds only if it was given a deadline of its own rather than the
// remainder of a shared one.
//
// Get degrades to a SizeError either way, so what this pins is that the
// degradation reflects what the SIZE lookup did rather than what the call
// before it already spent.
func TestWorkspaceHomeVolumeStore_Get_SlowInspectDoesNotEatTheSizingDeadline(t *testing.T) {
	withShortWorkspaceHomeVolumeTimeout(t, 400*time.Millisecond)

	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		sizes:   map[string]int64{name: 4242},
		slowBy:  250 * time.Millisecond,
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")

	if !got.Exists {
		t.Fatal("Exists = false, want true")
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty — the size lookup had its own deadline and the engine answered well inside it", got.SizeError)
	}
	if got.Bytes != 4242 {
		t.Errorf("Bytes = %d, want 4242", got.Bytes)
	}
}

// TestWorkspaceHomeVolumeStore_Remove_InspectFailureStillAttemptsDeletion is
// the same contract one layer up: not being able to establish that something
// is there must not turn into "so do not try", which is exactly the bug
// PR #791 Should-fix #2 fixed on the host-path implementation.
func TestWorkspaceHomeVolumeStore_Remove_InspectFailureStillAttemptsDeletion(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes:    map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
		inspectErr: errors.New("engine hiccup"),
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	if _, _, err := store.Remove(context.Background(), "team-a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(api.removed) != 1 || api.removed[0] != name {
		t.Errorf("removed = %v, want [%s] — an inspect failure must not skip the delete attempt", api.removed, name)
	}
}

// TestWorkspaceHomeVolumeStore_Remove_UsesForce documents which of force's two
// meanings is being relied on: it makes a volume that vanished between the
// inspect and the remove a non-error (204 rather than 404, measured against
// podman 4.9.3), matching os.RemoveAll's tolerance that the host-path
// implementation had. It emphatically does NOT remove an in-use volume — see
// TestWorkspaceHomeVolumeStore_Remove_InUse_SurfacesTheConflict.
func TestWorkspaceHomeVolumeStore_Remove_UsesForce(t *testing.T) {
	name := dockerres.WorkspaceHomeVolumeName("inst", "team-a")
	api := &stubWorkspaceHomeVolumeAPI{
		volumes: map[string]volume.Volume{name: homeVolume("inst", "team-a", nil)},
	}
	store := &WorkspaceHomeVolumeStore{API: api, InstallID: "inst"}

	if _, _, err := store.Remove(context.Background(), "team-a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(api.removeForced) != 1 || !api.removeForced[0] {
		t.Errorf("VolumeRemoveOptions.Force = %v, want [true]", api.removeForced)
	}
}

// TestWorkspaceHomeVolumeStore_NilAPI_DegradesGracefully pins that a store
// with no engine handle behaves like the feature is off rather than
// panicking. server/wire.go never builds one, but the zero value has to be
// safe: a typed-nil *client.Client handed through an interface is a non-nil
// interface (the trap DetectDaemonStateVolumes' wiring is already guarded
// for).
func TestWorkspaceHomeVolumeStore_NilAPI_DegradesGracefully(t *testing.T) {
	store := &WorkspaceHomeVolumeStore{InstallID: "inst"}

	got := store.Get(context.Background(), "team-a")
	if got.SizeError == "" {
		t.Error("Get: SizeError = empty, want an explanation that no engine is available")
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Error("List returned nil error, want a no-engine failure")
	}
	if _, deleted, err := store.Remove(context.Background(), "team-a"); err == nil || deleted {
		t.Errorf("Remove = (deleted=%v, err=%v), want (false, non-nil)", deleted, err)
	}
}

// The production wiring (server/wire.go) hands this store the very
// *client.Client the container backend was built with, so pin that the real
// type satisfies the narrow interface — the same compile-time assertion
// SelfContainerInspector carries for the same reason.
var _ WorkspaceHomeVolumeAPI = (*client.Client)(nil)
