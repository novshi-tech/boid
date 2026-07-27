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

// This file is the engine-side half of 論点 a-2 of
// docs/plans/workspace-home-volume-persistence.md (PR7): reporting on, and
// deleting, the per-workspace HOME volumes PR6 made the workspace home.
//
// Everything policy-shaped — the reserved `default` slug is never deleted, a
// missing home is not an error, a sizing failure never blocks deletion, an
// orphan is a home with no workspace row — stays in internal/api, which is
// where it was and where the workspace rows are. What lives here is only the
// mechanism: which engine calls answer those questions and what their answers
// mean. The split is what keeps internal/api free of moby imports (the
// package currently has none, and 論点 a-2's D2 asks that it stay that way).

// workspaceHomeVolumeTimeout bounds ONE engine call — not one store
// operation. Every method here derives a fresh deadline from its caller's
// context immediately before each request (see engineCall).
//
// It exists because DiskUsage is not a cheap lookup: it is the ONLY endpoint
// that reports a volume's size (see WorkspaceHomeVolumeStore's doc comment)
// and the engine answers it by walking every image, container and volume it
// has. Measured against rootless podman 4.9.3 on a host with 469 images / 157
// volumes / 10 containers (2026-07-27): 0.47–0.72s warm. That is fine for the
// three callers this store has — `boid workspace show`, `boid workspace
// remove` and `boid gc`, none of them on a dispatch path — but an engine
// wedged on a slow filesystem could otherwise hang an interactive CLI
// indefinitely, and every failure mode here degrades gracefully (a size comes
// back unknown, rendered "?"), so a deadline costs nothing it cannot afford.
//
// Generous rather than tight for the same reason: on a host with a genuinely
// large image store the walk is the expected cost, not a hang, and reporting
// "?" for a size the engine would have produced in 20 seconds is a worse
// answer than waiting.
//
// # Per call, not per operation (PR7 round-2 codex review, Major 1)
//
// One deadline for the whole of Remove looks tidier and quietly breaks this
// package's most important contract — "a sizing failure never blocks the
// deletion". Remove sizes the volume before deleting it, so a DiskUsage that
// runs the shared deadline out hands VolumeRemove a context that is ALREADY
// expired: the delete then fails without a request ever leaving the process,
// and since WorkspaceHandler.Remove removes the DB row first, the operator is
// left with no workspace and a live volume full of harness credentials. The
// only way sizing can be genuinely best-effort is for its budget to be its
// own. The upper bound on a whole operation grows accordingly (Remove can now
// spend up to 3× this) — the right trade, since the alternative bounds the
// operation by silently sacrificing its point. Callers that need a tighter
// overall bound impose it the normal way, on the context they pass in: these
// deadlines only ever shorten it, never extend it.
var workspaceHomeVolumeTimeout = 30 * time.Second

// WorkspaceHomeVolumeAPI is the engine surface workspace HOME reporting and
// deletion needs, declared as its own narrow interface for the same two
// reasons SelfContainerInspector is: internal/server can hand in the bare
// *client.Client it already built without constructing a container backend,
// and a test stub does not have to implement two dozen unrelated methods.
//
// DiskUsage is on it, rather than VolumeInspect alone, because of a fact that
// is easy to get wrong and expensive to get wrong silently: Volume.UsageData
// is populated ONLY by GET /system/df. Measured against rootless podman 4.9.3
// (2026-07-27), GET /volumes/<name> and GET /volumes both return the volume
// with no UsageData field at all, exactly as the moby type's own comment says
// ("This information is used by the `GET /system/df` endpoint, and omitted in
// other endpoints"). A sizing implementation built on VolumeInspect would
// compile, run, and report every workspace as 0 B.
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

// WorkspaceHomeVolumeStore answers the three questions 論点 a-2 needs about
// workspace HOME volumes: how big is this one, what ones exist, and delete
// this one.
//
// # Sizing goes through GET /system/df, and why that was the choice
//
// Measured against rootless podman 4.9.3 (2026-07-27), read-only:
//
//   - GET /volumes/<name> (VolumeInspect) and GET /volumes (VolumeList),
//     with and without a label filter, return NO UsageData at all. Sizing
//     cannot be built on them.
//   - GET /system/df returns UsageData{Size,RefCount} for every volume. Its
//     Size is du --apparent-size semantics: a probe volume holding a 1MiB
//     regular file, a 100MiB sparse file, a hardlink to the regular file, a
//     symlink and a small file reported 105906193 bytes, byte-identical to
//     `du -sb` on the mountpoint.
//   - podman 4.9.3 IGNORES both the `type=volume` and the `verbose` query
//     parameters: all three responses (no params, type=volume, verbose=1)
//     were byte-identical at 151995 bytes, and per-volume UsageData was
//     populated in each. Both options are still passed, because a real
//     docker engine honours them and one of them is load-bearing there —
//     see DiskUsageOptions below.
//
// The alternative 論点 a-2 asked to compare — running `du` inside a throwaway
// container — was rejected: it needs a container start per volume (against
// one df call for the whole listing), an image to run, and a second
// init-container-shaped failure surface, in exchange for a number in the
// SAME units df already reports. Its one advantage would have been not
// walking unrelated volumes, which matters only if these calls were hot, and
// they are not (see workspaceHomeVolumeTimeout).
//
// What changes for users: df's Size is real `du` semantics, so a hardlinked
// file is counted ONCE (the previous host-path implementation,
// humanize.ApparentSize, summed every name and counted it twice) and a
// humanize.ApparentSize, summed every name and counted it twice). Sparse
// files stay logical in both, and symlinks are counted the same way by both
// (ApparentSize sums FileInfo.Size() for every non-directory entry, which
// for a symlink is its target string's length — an earlier revision of this
// comment claimed ApparentSize skipped them, which is not what
// internal/humanize does). docs/{ja,en}/guide/workspace-home.md is updated
// accordingly.
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
// # The slug comes off the LABEL, not the name (論点 a-2, D7)
//
// dockerres.WorkspaceHomeVolumeName runs the slug through SanitizeNamePart,
// which is not invertible — "team/a" and "team-a" produce the same volume
// name — so there is no way back from a name to the workspace it belongs to.
// dockerres.LabelWorkspaceHome carries the slug verbatim, and PR6 made both
// volume-creating paths (resolveWorkspaceHome's EnsureWorkspaceHomeVolume and
// containerBackend.ensureNamedVolumes) attach it with the already-normalized
// slug, so the label is the authority here.
//
// # Enumerating on presence, and scoping by name reconstruction
//
// The engine query filters on the mere PRESENCE of dockerres.
// LabelWorkspaceHome. It deliberately does NOT filter on
// dockerres.LabelWorkspaceHomeInstallID the way internal/reap.unionResources
// does, because that label is only attached when the install HAS an id (see
// ensureNamedVolumes) — an install-id-scoped query lists NOTHING on an
// install without one, which is a fine fail-safe for reap (it destroys) and
// exactly backwards for a listing (it reports). Verified against podman
// 4.9.3 (2026-07-27): a bare `label=<key>` term matches on presence.
//
// Install scoping is then done by reconstructing the name —
// dockerres.WorkspaceHomeVolumeName(s.InstallID, labelSlug) == v.Name — which
// is both stricter and independent of the optional label: two boid installs
// sharing one engine never list each other's homes, and the rule reads the
// same whether or not the install-id label happens to be there. The one case
// it degrades on is a volume this install created BEFORE it had an install id
// (named ...-noinst-<slug>): the daemon no longer mounts that volume either,
// so leaving it out of "this install's homes" keeps the listing consistent
// with what dispatch actually uses. It is still visible to
// `docker volume ls`, which the user-facing doc points at for manual cleanup.
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
// deleted is true only when the volume existed AND the removal succeeded —
// mirroring the host-path implementation, where os.RemoveAll returning nil on
// a path that was never there did not count as a deletion.
//
// # In-use volumes surface as an error (論点 a-2, D6)
//
// Force is set, but only one of its two meanings is being relied on. Measured
// against rootless podman 4.9.3 (2026-07-27): with a running container
// holding the volume, DELETE /volumes/<name> and DELETE /volumes/<name>?
// force=1 BOTH return 409 ("volume is being used by the following
// container(s): <id>"); against a name that does not exist, force=1 returns
// 204 where the unforced call returns 404. So force buys tolerance of a
// volume that vanished between the inspect and the remove — os.RemoveAll's
// tolerance, which the host-path version had — and buys nothing at all
// against a job that is currently running in that workspace. That 409 is
// returned to the caller rather than swallowed: `workspace remove` reports it
// as a part-completed remove (the row is gone, the volume is not), which is
// the honest answer. Silently reporting success would tell an operator their
// credentials were destroyed when they are still sitting on the engine.
//
// Sizing stays best-effort and never gates the delete attempt (PR #791
// review, Should-fix #2, carried across the mechanism change): neither an
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
	return context.WithTimeout(ctx, workspaceHomeVolumeTimeout)
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
