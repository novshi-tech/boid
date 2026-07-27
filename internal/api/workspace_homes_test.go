package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// PR7 of docs/plans/workspace-home-volume-persistence.md (論点 a-2) replaced
// the mechanism under these cases — <runtimesRoot>/homes/<slug> directories
// became per-workspace docker named volumes — while keeping the CONTRACTS the
// PR6-era versions of these tests stated. Each of the four is still here,
// under a name that says which one it is:
//
//   - the reserved `default` slug is never deleted
//     (TestDeleteWorkspaceHome_DefaultWorkspace_NeverDeleted)
//   - a home that is not there is not an error
//     (TestComputeWorkspaceHomeSize_NotYetCreated,
//     TestDeleteWorkspaceHome_NotYetCreated_NoOp)
//   - a sizing failure does not block deletion
//     (TestDeleteWorkspaceHome_SizeErrorDoesNotBlockDeletion)
//   - an orphan is a home with no workspace row
//     (TestListWorkspaceHomeSizes_ListsAllAndFlagsOrphans)
//
// One PR6-era case is deliberately gone rather than translated:
// TestListWorkspaceHomeSizes_IgnoresLegacyMarkerAndLockFiles pinned an
// os.ReadDir IsDir() filter that kept leftover homes/<slug>.init.json and
// homes/<slug>.lock files from being mistaken for workspace homes. A volume
// listing has no such residue to confuse — the engine enumerates volumes,
// and the legacy files are ordinary files in a directory nothing here reads
// any more. The filter itself still exists, in dispatcher.WorkspaceHomesDir's
// world, for PR8's migration CLI to worry about.

// stubWorkspaceHomeStore is a WorkspaceHomeStore backed by a map, standing in
// for dispatcher.WorkspaceHomeVolumeStore's engine calls. The engine-side
// contracts (which endpoint reports a size, what force does to an in-use
// volume) are pinned in internal/dispatcher's own tests; what these tests are
// about is the policy this package layers on top.
type stubWorkspaceHomeStore struct {
	installID string
	// homes maps slug -> size in bytes for every volume that exists.
	homes map[string]int64
	// sizeErrs maps slug -> a sizing failure to report for it.
	sizeErrs map[string]string
	// removeErrs maps slug -> an engine failure to report from Remove.
	removeErrs map[string]error
	listErr    error

	removed []string
}

func (s *stubWorkspaceHomeStore) volumeName(slug string) string {
	return dockerres.WorkspaceHomeVolumeName(s.installID, slug)
}

func (s *stubWorkspaceHomeStore) get(slug string) dispatcher.WorkspaceHomeVolume {
	info := dispatcher.WorkspaceHomeVolume{Slug: slug, Volume: s.volumeName(slug)}
	if msg, ok := s.sizeErrs[slug]; ok {
		info.SizeError = msg
	}
	size, ok := s.homes[slug]
	if !ok {
		return info
	}
	info.Exists = true
	if info.SizeError == "" {
		info.Bytes = size
	}
	return info
}

func (s *stubWorkspaceHomeStore) Get(_ context.Context, slug string) dispatcher.WorkspaceHomeVolume {
	return s.get(slug)
}

func (s *stubWorkspaceHomeStore) List(_ context.Context) ([]dispatcher.WorkspaceHomeVolume, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	// Sorted by slug, as dispatcher.WorkspaceHomeVolumeStore.List promises.
	var slugs []string
	for slug := range s.homes {
		slugs = append(slugs, slug)
	}
	for i := 0; i < len(slugs); i++ {
		for j := i + 1; j < len(slugs); j++ {
			if slugs[j] < slugs[i] {
				slugs[i], slugs[j] = slugs[j], slugs[i]
			}
		}
	}
	out := make([]dispatcher.WorkspaceHomeVolume, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, s.get(slug))
	}
	return out, nil
}

func (s *stubWorkspaceHomeStore) Remove(_ context.Context, slug string) (dispatcher.WorkspaceHomeVolume, bool, error) {
	info := s.get(slug)
	s.removed = append(s.removed, slug)
	if err, ok := s.removeErrs[slug]; ok {
		return info, false, err
	}
	delete(s.homes, slug)
	return info, info.Exists, nil
}

func newStubHomeStore(homes map[string]int64) *stubWorkspaceHomeStore {
	return &stubWorkspaceHomeStore{installID: "inst1234", homes: homes}
}

// --- computeWorkspaceHomeSize ---

func TestComputeWorkspaceHomeSize_NotYetCreated(t *testing.T) {
	store := newStubHomeStore(map[string]int64{})

	got := computeWorkspaceHomeSize(context.Background(), store, "myws")

	if got.Exists {
		t.Errorf("Exists = true, want false for a workspace never dispatched into")
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", got.Bytes)
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty (not-yet-created is not an error)", got.SizeError)
	}
	if want := store.volumeName("myws"); got.Volume != want {
		t.Errorf("Volume = %q, want %q (reported even when the volume does not exist yet)", got.Volume, want)
	}
}

func TestComputeWorkspaceHomeSize_ExistingContent(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"myws": 200})

	got := computeWorkspaceHomeSize(context.Background(), store, "myws")

	if !got.Exists {
		t.Fatal("Exists = false, want true")
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty", got.SizeError)
	}
	if got.Bytes != 200 {
		t.Errorf("Bytes = %d, want 200", got.Bytes)
	}
}

func TestComputeWorkspaceHomeSize_SizingFailure_ReportsSizeError(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"myws": 200})
	store.sizeErrs = map[string]string{"myws": "engine busy"}

	got := computeWorkspaceHomeSize(context.Background(), store, "myws")

	if !got.Exists {
		t.Error("Exists = false, want true (the volume is there; only sizing failed)")
	}
	if got.SizeError == "" {
		t.Error("SizeError = empty, want the store's sizing failure")
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0 when SizeError is set", got.Bytes)
	}
}

// --- ListWorkspaceHomeSizes ---

type stubWorkspaceSlugLister struct {
	summaries []*orchestrator.WorkspaceSummary
	err       error
}

func (s *stubWorkspaceSlugLister) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return s.summaries, s.err
}

func TestListWorkspaceHomeSizes_NoVolumesYet(t *testing.T) {
	store := newStubHomeStore(map[string]int64{})

	got, listErr, err := ListWorkspaceHomeSizes(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes: %v", err)
	}
	if listErr != "" {
		t.Errorf("listErr = %q, want empty", listErr)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestListWorkspaceHomeSizes_ListsAllAndFlagsOrphans(t *testing.T) {
	store := newStubHomeStore(map[string]int64{
		"default":   100,
		"known-ws":  200,
		"orphan-ws": 50,
	})
	lister := &stubWorkspaceSlugLister{summaries: []*orchestrator.WorkspaceSummary{
		{ID: "default"},
		{ID: "known-ws"},
	}}

	got, listErr, err := ListWorkspaceHomeSizes(context.Background(), store, lister)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes: %v", err)
	}
	if listErr != "" {
		t.Errorf("listErr = %q, want empty (lister succeeded)", listErr)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}

	byslug := map[string]WorkspaceHomeSize{}
	var total int64
	for _, e := range got {
		byslug[e.Slug] = e
		total += e.Bytes
	}
	if total != 350 {
		t.Errorf("total bytes = %d, want 350", total)
	}
	if byslug["default"].Orphan {
		t.Error("default: Orphan = true, want false")
	}
	if byslug["known-ws"].Orphan {
		t.Error("known-ws: Orphan = true, want false")
	}
	if !byslug["orphan-ws"].Orphan {
		t.Error("orphan-ws: Orphan = false, want true (no matching workspace row)")
	}
	if want := store.volumeName("known-ws"); byslug["known-ws"].Volume != want {
		t.Errorf("known-ws: Volume = %q, want %q", byslug["known-ws"].Volume, want)
	}
}

// TestListWorkspaceHomeSizes_ListerError_OmitsListingWithListError carries
// PR #791 Should-fix #3 across the mechanism change: a lister failure used to
// leave the listing intact with every entry mismarked Orphan=true
// (indistinguishable from "every workspace really was deleted"). Selection A
// omits the listing outright instead and reports the reason.
func TestListWorkspaceHomeSizes_ListerError_OmitsListingWithListError(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"myws": 10})
	lister := &stubWorkspaceSlugLister{err: errors.New("db unavailable")}

	got, listErr, err := ListWorkspaceHomeSizes(context.Background(), store, lister)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes should not fail on a lister error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0 (listing omitted when orphan status can't be trusted): %+v", len(got), got)
	}
	if listErr == "" {
		t.Error("listErr = empty, want the lister's error message")
	}
	if !strings.Contains(listErr, "db unavailable") {
		t.Errorf("listErr = %q, want it to mention the lister's underlying error", listErr)
	}
}

// TestListWorkspaceHomeSizes_StoreError_IsAGenuineFailure keeps the third
// return value's meaning distinct from listErr's: an engine enumeration
// failure means the whole call failed, not just orphan detection.
func TestListWorkspaceHomeSizes_StoreError_IsAGenuineFailure(t *testing.T) {
	store := newStubHomeStore(map[string]int64{})
	store.listErr = errors.New("engine down")

	if _, _, err := ListWorkspaceHomeSizes(context.Background(), store, nil); err == nil {
		t.Fatal("err = nil, want the engine enumeration failure")
	}
}

// --- deleteWorkspaceHome ---

func TestDeleteWorkspaceHome_DefaultWorkspace_NeverDeleted(t *testing.T) {
	store := newStubHomeStore(map[string]int64{orchestrator.DefaultWorkspaceSlug: 10})

	info, deleted, err := deleteWorkspaceHome(context.Background(), store, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v", err)
	}
	if deleted {
		t.Error("deleted = true, want false (default workspace home must never be deleted)")
	}
	if len(store.removed) != 0 {
		t.Errorf("store.Remove was called %v, want no call at all for the reserved default slug", store.removed)
	}
	if _, still := store.homes[orchestrator.DefaultWorkspaceSlug]; !still {
		t.Error("default workspace home volume was removed")
	}
	if want := store.volumeName(orchestrator.DefaultWorkspaceSlug); info.Volume != want {
		t.Errorf("info.Volume = %q, want %q", info.Volume, want)
	}
}

func TestDeleteWorkspaceHome_ExistingHome_RemovesVolume(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"myws": 42})

	info, deleted, err := deleteWorkspaceHome(context.Background(), store, "myws")
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v", err)
	}
	if !deleted {
		t.Error("deleted = false, want true")
	}
	if info.Bytes != 42 {
		t.Errorf("info.Bytes = %d, want 42 (size as observed before deletion)", info.Bytes)
	}
	if _, still := store.homes["myws"]; still {
		t.Error("home volume still present after delete")
	}
}

// TestDeleteWorkspaceHome_SizeErrorDoesNotBlockDeletion carries PR #791
// Should-fix #2 across the mechanism change: a sizing failure must not skip
// the removal attempt, which used to leave an undeletable-looking orphan
// behind after the workspace row was already gone.
func TestDeleteWorkspaceHome_SizeErrorDoesNotBlockDeletion(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"myws": 10})
	store.sizeErrs = map[string]string{"myws": "simulated sizing failure"}

	info, deleted, err := deleteWorkspaceHome(context.Background(), store, "myws")
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v, want nil (a sizing failure must not surface as a deletion error)", err)
	}
	if !deleted {
		t.Error("deleted = false, want true (a sizing failure must not block deletion)")
	}
	if info.SizeError == "" {
		t.Error("info.SizeError = empty, want the injected sizing failure")
	}
	if _, still := store.homes["myws"]; still {
		t.Error("home volume still present after delete")
	}
}

func TestDeleteWorkspaceHome_NotYetCreated_NoOp(t *testing.T) {
	store := newStubHomeStore(map[string]int64{})

	info, deleted, err := deleteWorkspaceHome(context.Background(), store, "myws")
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v", err)
	}
	if deleted {
		t.Error("deleted = true, want false (nothing to delete)")
	}
	if info.Exists {
		t.Error("info.Exists = true, want false")
	}
}

// TestDeleteWorkspaceHome_InUse_ReturnsErrorNotDeleted pins 論点 a-2's D6 at
// the policy layer: removing a workspace while a job is still running in it
// leaves the volume behind, and that has to reach the operator as a
// part-completed remove rather than as a success.
func TestDeleteWorkspaceHome_InUse_ReturnsErrorNotDeleted(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"myws": 10})
	store.removeErrs = map[string]error{
		"myws": errors.New("volume is being used by the following container(s): abc123"),
	}

	_, deleted, err := deleteWorkspaceHome(context.Background(), store, "myws")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if deleted {
		t.Error("deleted = true, want false")
	}
	if _, still := store.homes["myws"]; !still {
		t.Error("home volume was removed after the engine reported a conflict")
	}
}
