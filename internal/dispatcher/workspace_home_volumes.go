package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/dockerres"
)

// This file reports on, and deletes, the per-workspace HOME volumes (see
// docs/plans/workspace-home-volume-persistence.md 論点a-2).
//
// Everything policy-shaped — the reserved `default` slug is never deleted, a
// missing home is not an error, a sizing failure never blocks deletion, an
// orphan is a home with no workspace row — stays in internal/api, which is
// where the workspace rows are. What lives here is only the mechanism: which
// engine calls answer those questions and what their answers mean. The
// split keeps internal/api free of moby imports.

// workspaceHomeVolumeTimeout bounds ONE engine call — not one store
// operation. Every method here derives a fresh deadline from its caller's
// context immediately before each request (see engineCall).
//
// It exists because DiskUsage is not a cheap lookup: it is the only endpoint
// that reports a volume's size, and the engine answers it by walking every
// image, container and volume it has — this can take the better part of a
// second on a large host. That is fine for the three callers this store has
// (`boid workspace show`, `boid workspace remove`, `boid gc`, none on a
// dispatch path), but an engine wedged on a slow filesystem could otherwise
// hang an interactive CLI indefinitely; every failure mode here degrades
// gracefully (a size comes back unknown, rendered "?"), so a deadline costs
// nothing it cannot afford.
//
// Taken per call rather than once for the whole of Remove: Remove sizes the
// volume before deleting it, so a shared deadline that DiskUsage runs out
// would hand VolumeRemove an already-expired context — breaking this
// package's most important contract, that a sizing failure never blocks the
// deletion. Callers that need a tighter overall bound impose it the normal
// way, on the context they pass in: these deadlines only ever shorten it,
// never extend it.
var workspaceHomeVolumeTimeout = newAtomicDuration(30 * time.Second)

// WorkspaceHomeVolumeAPI is the engine surface workspace HOME reporting and
// deletion needs, declared as its own narrow interface for the same two
// reasons SelfContainerInspector is: internal/server can hand in the bare
// *client.Client it already built without constructing a container backend,
// and a test stub does not have to implement two dozen unrelated methods.
//
// DiskUsage is on it, rather than VolumeInspect alone, because of a fact that
// is easy to get wrong and expensive to get wrong silently: Volume.UsageData
// is populated ONLY by GET /system/df — GET /volumes/<name> and GET /volumes
// both return the volume with no UsageData field at all, exactly as the
// moby type's own comment says ("This information is used by the
// `GET /system/df` endpoint, and omitted in other endpoints"). A sizing
// implementation built on VolumeInspect would compile, run, and report every
// workspace as 0 B.
type WorkspaceHomeVolumeAPI interface {
	VolumeInspect(ctx context.Context, volumeID string, options client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
	DiskUsage(ctx context.Context, options client.DiskUsageOptions) (client.DiskUsageResult, error)
}

// WorkspaceHomeVolume is one workspace HOME volume as the engine describes
// it. Plain data on purpose: internal/api renders it into the wire types
// (api.WorkspaceHomeSize, api.WorkspaceRemoveResponse) and this package never
// learns about those.
type WorkspaceHomeVolume struct {
	// Slug is the workspace this volume belongs to. For Get/Remove it is the
	// slug that was asked about; for List it is read off the volume's
	// dockerres.LabelWorkspaceHome LABEL — see List's doc comment for why the
	// name cannot supply it.
	Slug string

	// Volume is the engine-side volume name,
	// dockerres.WorkspaceHomeVolumeName(installID, slug) — the same name
	// resolveWorkspaceHome hands the job container as its $HOME mount.
	// Always set, whether or not the volume currently exists.
	Volume string

	// Exists reports whether the engine currently has this volume. false
	// means the workspace has never been dispatched into — the
	// init-on-first-dispatch contract, not an error.
	Exists bool

	// Bytes is the volume's size as GET /system/df reports it, meaningful
	// only when Exists is true and SizeError is empty.
	Bytes int64

	// SizeError is non-empty when the size could not be determined. Bytes is
	// 0 and must not be trusted when it is set; callers render "?" instead.
	// It never implies the volume is gone or undeletable — Remove treats
	// sizing as best-effort and attempts the deletion regardless.
	SizeError string
}

// WorkspaceHomeVolumeStore answers three questions about workspace HOME
// volumes: how big is this one, what ones exist, and delete this one.
//
// Sizing goes through GET /system/df rather than VolumeInspect/VolumeList,
// because only /system/df populates UsageData{Size,RefCount} for a volume;
// its Size uses `du --apparent-size` semantics (a hardlink to another file
// in the same volume is counted once, not duplicated). Running `du` inside
// a throwaway container per volume was considered and rejected: it needs a
// container start and an image per volume, against one df call for the
// whole listing, for a number in the same units df already reports.
type WorkspaceHomeVolumeStore struct {
	// API is the engine handle, the very *client.Client server/wire.go built
	// for the container backend. nil means no engine was available, which
	// every method degrades gracefully on rather than panicking.
	API WorkspaceHomeVolumeAPI

	// InstallID scopes the volume names this store generates and recognizes,
	// matching Runner.InstallID / ContainerBackendOptions.InstallID.
	InstallID string
}

// Get reports on slug's workspace HOME volume.
//
// It never returns an error: every failure mode (no engine handle, an inspect
// that failed for a reason other than "no such volume", an unavailable size)
// is reflected in the returned value's Exists/SizeError fields instead. That
// is the same "never block the rest of the response on a size lookup failure"
// contract the host-path computeWorkspaceHomeSize had, carried across the
// mechanism change.
func (s *WorkspaceHomeVolumeStore) Get(ctx context.Context, slug string) WorkspaceHomeVolume {
	info := WorkspaceHomeVolume{
		Slug:   slug,
		Volume: dockerres.WorkspaceHomeVolumeName(s.InstallID, slug),
	}
	if s.API == nil {
		info.SizeError = errNoWorkspaceHomeEngine.Error()
		return info
	}

	exists, existsErr := s.exists(ctx, info.Volume)
	info.Exists = exists
	if existsErr != nil {
		// Reported through SizeError rather than swallowed: an inspect that
		// failed for a reason other than "no such volume" means Exists=false
		// is a guess, and the caller must be able to tell that apart from a
		// confident "never dispatched into". SizeError is the field that
		// already means "do not trust the numbers here" and that Remove
		// already refuses to be gated by.
		info.SizeError = existsErr.Error()
		return info
	}
	if !exists {
		return info // Exists false, no SizeError: not an error.
	}

	size, sizeErr := s.size(ctx, info.Volume)
	if sizeErr != nil {
		info.SizeError = sizeErr.Error()
		return info
	}
	info.Bytes = size
	return info
}

// List enumerates every workspace HOME volume belonging to this install.
//
// The slug comes off dockerres.LabelWorkspaceHome, not the volume name:
// dockerres.WorkspaceHomeVolumeName runs the slug through SanitizeNamePart,
// which is not invertible ("team/a" and "team-a" produce the same volume
// name), so the label is the only way back to the workspace a volume
// belongs to.
//
// The engine query filters on the mere presence of that label rather than
// also filtering on dockerres.LabelWorkspaceHomeInstallID, because that
// label is only attached when the install has an id — an install-id-scoped
// query would list nothing on an install without one. Install scoping is
// instead done by reconstructing the name
// (dockerres.WorkspaceHomeVolumeName(s.InstallID, labelSlug) == v.Name),
// which works whether or not the install-id label is present, so two boid
// installs sharing one engine never list each other's homes. A volume this
// install created before it had an install id degrades out of the listing
// (consistent with dispatch, which no longer mounts it either) but stays
// visible to `docker volume ls` for manual cleanup.
//
// A sizing failure does not fail the call: the listing is what drives orphan
// detection, and losing it because df was busy would be a much worse trade
// than an entry rendering "?" for its size.
func (s *WorkspaceHomeVolumeStore) List(ctx context.Context) ([]WorkspaceHomeVolume, error) {
	if s.API == nil {
		return nil, errNoWorkspaceHomeEngine
	}

	filters := client.Filters{}.Add("label", dockerres.LabelWorkspaceHome)
	listCtx, cancelList := engineCall(ctx)
	listRes, err := s.API.VolumeList(listCtx, client.VolumeListOptions{Filters: filters})
	cancelList()
	if err != nil {
		return nil, fmt.Errorf("list workspace home volumes: %w", err)
	}

	sizes, sizeErr := s.sizes(ctx)

	result := make([]WorkspaceHomeVolume, 0, len(listRes.Items))
	for _, v := range listRes.Items {
		slug := v.Labels[dockerres.LabelWorkspaceHome]
		if dockerres.WorkspaceHomeVolumeName(s.InstallID, slug) != v.Name {
			continue
		}
		entry := WorkspaceHomeVolume{Slug: slug, Volume: v.Name, Exists: true}
		switch {
		case sizeErr != nil:
			entry.SizeError = sizeErr.Error()
		default:
			size, known := sizes[v.Name]
			if !known {
				entry.SizeError = noSizeReportedError(v.Name).Error()
			} else {
				entry.Bytes = size
			}
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Slug < result[j].Slug })
	return result, nil
}

// Remove deletes slug's workspace HOME volume, reporting what was there
// beforehand.
//
// deleted is true only when the volume existed AND the removal succeeded.
//
// Force is set, but only one of its two meanings is being relied on: a
// running container holding the volume makes DELETE /volumes/<name> return
// 409 with or without force, so force buys tolerance of a volume that
// vanished between the inspect and the remove, and nothing at all against a
// job that is currently running in that workspace. That 409 is returned to
// the caller rather than swallowed: `workspace remove` reports it as a
// part-completed remove (the row is gone, the volume is not) — silently
// reporting success would tell an operator their credentials were destroyed
// when they are still sitting on the engine.
//
// Sizing stays best-effort and never gates the delete attempt: neither an
// unavailable size nor an inspect that could not establish existence skips
// the VolumeRemove call.
func (s *WorkspaceHomeVolumeStore) Remove(ctx context.Context, slug string) (WorkspaceHomeVolume, bool, error) {
	info := WorkspaceHomeVolume{
		Slug:   slug,
		Volume: dockerres.WorkspaceHomeVolumeName(s.InstallID, slug),
	}
	if s.API == nil {
		info.SizeError = errNoWorkspaceHomeEngine.Error()
		return info, false, errNoWorkspaceHomeEngine
	}
	if !dockerres.IsValidVolumeName(info.Volume) {
		// Unreachable for any slug that passed orchestrator.ValidWorkspaceSlug,
		// but the guard is the same one ensureNamedVolumes has: never hand the
		// engine a name that is not a name. This is the analogue of the
		// host-path version's "info.Path == "" so there is nothing to
		// RemoveAll on" bail-out.
		err := fmt.Errorf("workspace home volume %q is not a valid docker volume name", info.Volume)
		info.SizeError = err.Error()
		return info, false, err
	}

	exists, existsErr := s.exists(ctx, info.Volume)
	info.Exists = exists
	switch {
	case existsErr != nil:
		info.SizeError = existsErr.Error()
	case exists:
		if size, sizeErr := s.size(ctx, info.Volume); sizeErr != nil {
			info.SizeError = sizeErr.Error()
		} else {
			info.Bytes = size
		}
	}

	// A deadline of its own, taken here rather than at the top of the
	// function: the sizing calls above are best-effort and must not be able
	// to spend this one's budget (see workspaceHomeVolumeTimeout's own doc
	// comment for the failure this prevents).
	removeCtx, cancelRemove := engineCall(ctx)
	defer cancelRemove()
	if _, err := s.API.VolumeRemove(removeCtx, info.Volume, client.VolumeRemoveOptions{Force: true}); err != nil {
		return info, false, fmt.Errorf("remove workspace home volume %q: %w", info.Volume, err)
	}
	return info, info.Exists, nil
}

// engineCall derives the deadline for exactly one engine request from ctx.
// Every call site takes its own, so no request can be starved by time an
// earlier one spent — see workspaceHomeVolumeTimeout's doc comment. The
// returned cancel must be called before the next engine call rather than
// merely deferred to the end of the enclosing function, so a long operation
// does not accumulate live timers.
func engineCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, workspaceHomeVolumeTimeout.Get())
}

// errNoWorkspaceHomeEngine is what every method degrades to with a nil API.
// server/wire.go never constructs a store without one — it leaves the handler
// field nil instead, which turns the whole feature off — so this is a
// belt-and-braces answer for a typed-nil or a hand-built store, not a
// production path.
var errNoWorkspaceHomeEngine = errors.New("no docker client available to inspect workspace home volumes")

func noSizeReportedError(name string) error {
	return fmt.Errorf("engine reported no usage data for volume %q", name)
}

// exists reports whether the engine currently has this volume. A non-nil
// error means the question could not be answered — NOT that the answer is no.
func (s *WorkspaceHomeVolumeStore) exists(ctx context.Context, name string) (bool, error) {
	callCtx, cancel := engineCall(ctx)
	defer cancel()
	if _, err := s.API.VolumeInspect(callCtx, name, client.VolumeInspectOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect workspace home volume %q: %w", name, err)
	}
	return true, nil
}

func (s *WorkspaceHomeVolumeStore) size(ctx context.Context, name string) (int64, error) {
	sizes, err := s.sizes(ctx)
	if err != nil {
		return 0, err
	}
	size, ok := sizes[name]
	if !ok {
		return 0, noSizeReportedError(name)
	}
	return size, nil
}

// sizes reads every volume's size out of one GET /system/df.
//
// Volumes and Verbose are both set, and Verbose is not cosmetic: the moby
// client only copies VolumeUsage.Items into its result when Verbose is set
// (client.DiskUsage, the API >= 1.52 branch), so without it every size would
// come back unknown against a modern docker engine. Against podman 4.9.3 the
// client takes its legacy-response branch instead — podman's Ping negotiates
// API 1.41 — where Items is populated unconditionally, which is why the
// omission would not have shown up in dogfooding here.
//
// A volume with no UsageData, or one the driver reports as -1 ("not
// available" per the moby type), is left OUT of the map rather than recorded
// as 0, so callers can tell "unknown" from "empty".
func (s *WorkspaceHomeVolumeStore) sizes(ctx context.Context) (map[string]int64, error) {
	callCtx, cancel := engineCall(ctx)
	defer cancel()
	du, err := s.API.DiskUsage(callCtx, client.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return nil, fmt.Errorf("read engine disk usage: %w", err)
	}
	sizes := make(map[string]int64, len(du.Volumes.Items))
	for _, v := range du.Volumes.Items {
		if v.UsageData == nil || v.UsageData.Size < 0 {
			continue
		}
		sizes[v.Name] = v.UsageData.Size
	}
	return sizes, nil
}
