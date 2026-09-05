package dispatcher

import (
	"context"
	"fmt"
	"sync"
)

// workspaceHomeInFlight serializes a workspace HOME volume's destructive
// replacement (Runner.ImportWorkspaceHome) against the two things that must
// not overlap it — the dispatches about to mount that volume
// (Runner.Dispatch) and the removal of the workspace itself
// (api.WorkspaceHandler.Remove) — across the intervals neither the flock nor
// the engine covers. See docs/plans/workspace-home-volume-persistence.md for
// the full race analysis this closes.
//
// Three participants, three behaviours: a dispatch that meets a migration
// WAITS, a migration that meets either of the others is REFUSED, and a
// removal is refused by a migration but ignores dispatches entirely. Each
// asymmetry is argued at the function that implements it.
//
// # The dispatch window
//
// resolveWorkspaceHome only takes the per-workspace flock on its slow path;
// a settled workspace returns from the fast path holding nothing, and its
// dispatch then spends real time building a sandbox spec before
// containerBackend.Launch finally mounts the volume by name. Across that
// span a migration can remove and re-create the volume under a dispatch
// that already resolved the old one:
//
//	dispatch: resolve volume A, observe identity A          ...
//	migration:                          remove A, create B, extract into B
//	dispatch:                                     ... ensureNamedVolumes(name)
//	                                                  -> B, ContainerCreate(B)
//	both:                                             the agent and tar in B
//
// verifyWorkspaceHomeIdentity narrows this window (it compares the identity
// the dispatch's own VolumeCreate answered against the one resolved
// earlier) but is not atomic with ContainerCreate, so a replacement landing
// between them is mounted by name with nothing left to compare.
//
// # Why an in-memory registry rather than a longer-lived flock
//
// Both parties are goroutines in one process — a boid installation runs one
// daemon, and the migration exists only as a route on it — so there is no
// second process for a file lock to talk to that the engine's own 409 does
// not already handle. Extending the flock across the whole dispatch instead
// would interleave it with Runner.WithProjectLock and the git gateway's own
// staging in an order nobody could establish by reading either side; a
// mutex-guarded map with an explicit begin/release is the smaller thing
// that actually holds.
//
// # The third participant: `boid workspace remove`
//
// A removal deletes the workspace row and then the home volume
// (api.WorkspaceHandler.Remove). Left outside the registry it can produce a
// HOME volume with no workspace row, with no crash or engine failure
// involved:
//
//	import:  GetWorkspace("myws") -> ok
//	remove:                          delete the row, delete the volume
//	import:                                                            remove the
//	                                 volume (absent — a legitimate state, the
//	                                 never-dispatched-into case), create it,
//	                                 extract the operator's credentials into it
//	import:  200 OK
//
// What survives is a volume full of harness credentials that `boid
// workspace remove` can no longer target (404s on the missing row), that
// `boid gc` reports as an orphan, and that nothing will ever mount.
//
// Removals therefore register here too: a migration is refused while one is
// in flight, and vice versa. A removal that starts and finishes before the
// migration registers leaves nothing to observe, so the migration also
// confirms the row while holding its registration — see
// Runner.ConfirmWorkspaceExists.
//
// # What this does NOT close
//
//   - Another process: the registry is per-daemon, and two daemons sharing
//     one installation is already unsupported.
//   - The remaining delete-only paths (`boid reap --include-workspace-homes`,
//     an operator's own `docker volume rm`): they only delete, so a
//     dispatch whose home vanishes gets a fresh volume with an identity
//     that is not the resolved one and fails loudly — the case
//     verifyWorkspaceHomeIdentity already covers. The migration is unique
//     in deleting, re-creating under the same name, and then writing.
//   - Deletion landing between ensureNamedVolumes and ContainerCreate, from
//     any of those paths: the engine implicitly creates an unlabelled
//     volume and the job runs with an empty home. Unchanged by this file.
//
// The zero value is ready to use; a *Runner embeds one by value.
type workspaceHomeInFlight struct {
	mu sync.Mutex
	// dispatches counts, per slug, the dispatches that have registered and not
	// yet released. A COUNT rather than a flag because several jobs in one
	// workspace are dispatched concurrently as a matter of course, and a flag
	// would let the first to reach Launch clear the registration out from under
	// the others.
	dispatches map[string]int
	// removals counts, per slug, the `boid workspace remove` calls that have
	// registered and not yet released.
	//
	// A count for the same reason dispatches is one, and NOT a channel like
	// migrations because nothing ever waits on a removal: a migration that
	// collides with one is refused, and a dispatch is deliberately not excluded
	// by one at all (see beginRemoval).
	removals map[string]int
	// migrations holds, per slug, a channel closed when that migration
	// finishes. Waiting dispatches select on it, which is what makes the wait
	// context-cancellable — a sync.Cond cannot be.
	migrations map[string]chan struct{}
}

// beginDispatch registers a dispatch's interest in slug's home volume and
// returns the function that releases it.
//
// A migration already in flight makes this WAIT rather than fail. The
// asymmetry with beginMigration is deliberate:
//
//   - Refusing a MIGRATION costs an operator, at a terminal watching the
//     command, one retry — and nothing else, because the refusal happens
//     before the volume is touched.
//   - Refusing a DISPATCH costs a failed job. Dispatches come out of hook
//     evaluation, not from a person, for a condition that clears itself in
//     seconds.
//
// This extends existing behavior rather than adding new: a dispatch whose
// marker does NOT match already blocks on this workspace's flock for
// however long a concurrent init or migration takes. This just gives the
// fast path the same property, which it lacked only because it takes no
// lock at all.
//
// Bounded by ctx and nothing else: a bound of its own would have to end in
// a refusal, the outcome this design rejected, and the thing being waited
// for is already bounded (a migration lives inside one HTTP request whose
// context dies with its connection).
func (w *workspaceHomeInFlight) beginDispatch(ctx context.Context, slug string) (release func(), err error) {
	for {
		w.mu.Lock()
		migration, migrating := w.migrations[slug]
		if !migrating {
			if w.dispatches == nil {
				w.dispatches = make(map[string]int)
			}
			w.dispatches[slug]++
			w.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { w.endDispatch(slug) }) }, nil
		}
		w.mu.Unlock()

		select {
		case <-migration:
			// Re-loop rather than register directly: another migration can have
			// started in the gap, and the registration has to be made under the
			// same lock acquisition that observed no migration.
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"workspace home %q: waiting for an in-progress `boid workspace import-home` to finish before dispatching into it: %w",
				slug, ctx.Err())
		}
	}
}

// endDispatch drops one dispatch registration, deleting the map entry when the
// count reaches zero so a long-lived daemon's registry does not accumulate one
// entry per workspace it has ever dispatched into.
func (w *workspaceHomeInFlight) endDispatch(slug string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dispatches[slug]--
	if w.dispatches[slug] <= 0 {
		delete(w.dispatches, slug)
	}
}

// beginMigration registers a migration of slug's home volume, or refuses when
// something else in this daemon is already using it.
//
// Refusing rather than waiting, for the reason beginDispatch's doc comment
// gives: the caller is an operator whose command has not destroyed anything
// yet, and telling them to come back is both honest and cheap. The two refusal
// messages are separate because the operator's wait is different — a job
// finishing versus another migration finishing — even though both resolve on
// their own.
//
// The returned function MUST be called (defer) once the migration is over,
// success or failure: it is what unblocks the dispatches parked in
// beginDispatch.
func (w *workspaceHomeInFlight) beginMigration(slug string) (done func(), err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if n := w.dispatches[slug]; n > 0 {
		return nil, fmt.Errorf(
			"workspace home %q: refusing to migrate while %d job(s) are being started in this workspace — a job "+
				"container mounts the home volume by NAME, so replacing it now would hand a starting job the volume this "+
				"migration is extracting into. Nothing has been changed; wait for the dispatch to finish (`boid job list`) "+
				"and re-run: %w",
			slug, n, ErrWorkspaceHomeBusy)
	}
	if n := w.removals[slug]; n > 0 {
		return nil, fmt.Errorf(
			"workspace home %q: refusing to migrate while `boid workspace remove` is deleting this workspace — the "+
				"migration would create a fresh HOME volume for a row that is on its way out, leaving the imported "+
				"credentials in a volume nothing can dispatch into and `workspace remove` can no longer target. Nothing "+
				"has been changed: %w",
			slug, ErrWorkspaceHomeBusy)
	}
	if _, ok := w.migrations[slug]; ok {
		return nil, fmt.Errorf(
			"workspace home %q: another `boid workspace import-home` for this workspace is already running in this "+
				"daemon. Nothing has been changed; wait for it to finish and re-run if you still need to: %w",
			slug, ErrWorkspaceHomeBusy)
	}

	if w.migrations == nil {
		w.migrations = make(map[string]chan struct{})
	}
	ch := make(chan struct{})
	w.migrations[slug] = ch
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.migrations, slug)
			w.mu.Unlock()
			// Closed AFTER the entry is gone, so a dispatch woken by it re-reads
			// a map that no longer names this migration and registers instead of
			// looping. sync.Once rather than a bare close because done() is
			// deferred and a second call would panic the daemon.
			close(ch)
		})
	}, nil
}

// beginRemoval registers a `boid workspace remove` of slug, or refuses when a
// migration of the same workspace is in flight.
//
// # Refusing rather than waiting
//
// The same reasoning beginMigration uses, applied to a third operator command,
// and the wait it declines is a long one: a migration is a multi-GB extraction,
// so waiting would mean `boid workspace remove` printing nothing for minutes.
// The refusal is free — it lands before Service.RemoveWorkspace deletes the row,
// so the caller still has exactly the workspace they had — and it surfaces the
// fact that two contradictory intents were issued for one workspace, which is
// worth an operator's attention rather than silent serialization.
//
// # Why this does NOT exclude dispatches
//
// Deliberate, and load-bearing in the other direction: a removal only DELETES,
// which is the case PR6's identity check genuinely covers (a dispatch whose home
// vanishes gets a fresh volume carrying an identity that is not the resolved one
// and fails loudly — verifyWorkspaceHomeIdentity). Excluding dispatches would
// also convert today's documented part-completed outcome — the row goes, the
// engine's 409 refuses the volume, the response reports it — into a hard refusal
// of `workspace remove` for any workspace with a job starting in it.
//
// The returned function MUST be called (defer) once the removal is over.
func (w *workspaceHomeInFlight) beginRemoval(slug string) (done func(), err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.migrations[slug]; ok {
		return nil, fmt.Errorf(
			"workspace home %q: refusing to remove this workspace while `boid workspace import-home` is replacing its "+
				"HOME volume — the migration is mid-extraction and would leave its volume behind for a row this command "+
				"is deleting. Nothing has been changed; wait for the migration to finish and re-run: %w",
			slug, ErrWorkspaceHomeBusy)
	}

	if w.removals == nil {
		w.removals = make(map[string]int)
	}
	w.removals[slug]++
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.removals[slug]--
			if w.removals[slug] <= 0 {
				delete(w.removals, slug)
			}
		})
	}, nil
}

// BeginWorkspaceHomeRemoval excludes a migration of workspaceID's HOME volume
// for as long as the returned function has not been called, and is what
// api.WorkspaceHandler.Remove brackets its row-and-volume deletion with.
//
// It lives on the Runner rather than on the volume store because the registry it
// consults is the Runner's, and it has to be THE SAME instance the dispatch and
// migration paths register in — two objects with two registries would exclude
// nothing while looking exactly like this. That is also why the API layer
// discovers it through the same interface value it calls ImportWorkspaceHome on
// (api.WorkspaceHomeImporter) rather than through a second field.
//
// The only error it returns wraps ErrWorkspaceHomeBusy, so the caller has one
// thing to map (409). A workspaceID that cannot name a workspace home at all —
// normalizeWorkspaceSlug rejects it — gets a no-op release and a nil error
// instead: there is no home for it to exclude anybody from, and the caller's own
// next step (Service.RemoveWorkspace) is the authority on rejecting the
// argument. Returning an error there would make the caller distinguish two
// failures where only one is about exclusion.
func (r *Runner) BeginWorkspaceHomeRemoval(workspaceID string) (done func(), err error) {
	slug, err := normalizeWorkspaceSlug(workspaceID)
	if err != nil {
		return func() {}, nil
	}
	return r.homeInFlight.beginRemoval(slug)
}

// beginWorkspaceHomeDispatch is Dispatch's entry into the registry.
//
// It normalizes the workspace id itself rather than taking the slug
// resolveWorkspaceHome returns, because the registration has to be in place
// BEFORE that call: a registration made after the resolve would leave the
// resolve itself — the marker read, the identity observation — inside the
// window it is supposed to close. normalizeWorkspaceSlug is a pure function of
// its argument, so the two call sites cannot disagree about which slug this is;
// what resolveWorkspaceHome remains the sole source of is the slug THREADED
// downstream (BOID_WORKSPACE_SLUG, label values), and this does not touch that.
func (r *Runner) beginWorkspaceHomeDispatch(ctx context.Context, workspaceID string) (release func(), err error) {
	slug, err := normalizeWorkspaceSlug(workspaceID)
	if err != nil {
		return nil, err
	}
	return r.homeInFlight.beginDispatch(ctx, slug)
}
