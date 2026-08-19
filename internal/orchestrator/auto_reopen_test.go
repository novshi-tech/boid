package orchestrator

import (
	"encoding/json"
	"testing"
)

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): auto-reopen ----
//
// ShouldAutoReopen is I-5's rule, in the SAME pure-predicate shape as
// ShouldAutoDone/ShouldWake: no store, no transaction, no agent judgment
// anywhere (決定12). It reads exactly the ONE key ShouldAutoDone already
// reads (attrs.observed.source_closed) — I-5's own text promises "読むキー
// は増やさない".

func TestShouldAutoReopen(t *testing.T) {
	cases := []struct {
		name             string
		detail           string
		priorAutoReopens int
		want             bool
	}{
		{
			// The ordinary case: a done card's source moved again, and this
			// would be the FIRST automatic reopen THIS EPISODE.
			name:             "source flipped to false, never auto-reopened before this episode",
			detail:           `{"attrs":{"observed":{"source_closed":false}}}`,
			priorAutoReopens: 0,
			want:             true,
		},
		{
			// Canonical source is still closed — nothing to reopen. This is
			// I-5c's "配送はされたが reopen しない" case: the caller still logs
			// visibility for it, but ShouldAutoReopen itself says no.
			name: "source still closed", detail: `{"attrs":{"observed":{"source_closed":true}}}`,
			priorAutoReopens: 0,
			want:             false,
		},
		{
			// source_closed never observed at all (absent key) — SourceClosed
			// treats absent the same as false, so this behaves like "not
			// closed" here too. In practice a DONE triage task always has
			// source_closed:true already recorded (triage_done never fires
			// without it — ShouldAutoDone), so this case is defensive rather
			// than expected to occur.
			name: "source_closed absent", detail: `{"attrs":{"summary":"s"}}`,
			priorAutoReopens: 0,
			want:             true,
		},
		{
			// フラップ対策 (12節 B-6 既定案, エピソード単位 — 2026-08-19 nose
			// 判断): この同じ done エピソード内で既に自動 reopen 済み — 二度目
			// の flip は自動 reopen しない。
			name:             "source flipped to false again, already auto-reopened once THIS episode",
			detail:           `{"attrs":{"observed":{"source_closed":false}}}`,
			priorAutoReopens: 1,
			want:             false,
		},
		{
			name:             "source flipped to false again, already auto-reopened twice this episode",
			detail:           `{"attrs":{"observed":{"source_closed":false}}}`,
			priorAutoReopens: 2,
			want:             false,
		},
	}
	for _, tc := range cases {
		if got := ShouldAutoReopen(json.RawMessage(tc.detail), tc.priorAutoReopens); got != tc.want {
			t.Errorf("%s: ShouldAutoReopen = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCountAutoReopens pins 決定13 (state は導出、専用カウンタ列を作らない)
// AND the 2026-08-19 episode-scoping fix (Opus review): the フラップ count
// comes from filtering the task's own action history to everything AFTER
// the most recent isDoneEntryAction (最後に "done" へ入った action) — NOT
// the task's entire lifetime. Only reopen_triaged actions stamped
// ActorDaemon count within that window — a human's own manual reopen
// (資格ある判断、フラップではない) must not eat into the automatic budget,
// and does not itself start a new episode either.
func TestCountAutoReopens(t *testing.T) {
	cases := []struct {
		name    string
		actions []*Action
		want    int
	}{
		{name: "no actions", actions: nil, want: 0},
		{
			name: "one daemon reopen_triaged this episode",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 1,
		},
		{
			// THE core regression this fix addresses: two full done-episodes,
			// one daemon reopen each. A lifetime count would report 2 (and
			// ShouldAutoReopen would then refuse EVERY reopen after the
			// first one, ever — the bug the design doc's original "自動
			// reopen は1回だけ" text described before this fix). Scoped to
			// the CURRENT episode (everything after the SECOND triage_done),
			// only the second episode's own reopen counts.
			name: "two separate done episodes, one daemon reopen each — episode-scoped, not lifetime",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon}, // episode 1 starts
				{Type: "reopen_triaged", Actor: ActorDaemon},
				{Type: "triage_done", Actor: ActorDaemon}, // episode 2 starts — budget resets
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 1,
		},
		{
			// Same-episode double flap (no fresh triage_done between the two
			// reopen_triaged rows) — the actual フラップ this budget exists
			// to catch. Directly-constructed history (not necessarily
			// reachable through SweepReopen's own normal single-tick flow,
			// which always leaves the task via done→triaged after the FIRST
			// reopen — see auto_reopen.go's own doc comment) — this pins the
			// pure function's contract regardless of how the history arose
			// (a race, a manual DB correction, ...).
			name: "same episode, two daemon reopen_triaged rows",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 2,
		},
		{
			// A human's own manual reopen (Web UI button on a done card,
			// resolveReopenVariant routes it to the same reopen_triaged verb)
			// must not count toward the automatic フラップ budget.
			name: "human reopen_triaged does not count",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorHuman},
			},
			want: 0,
		},
		{
			name: "mixed human and daemon within the same episode",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorHuman},
				{Type: "reopen_triaged", Actor: ActorDaemon},
				{Type: "attrs_set", Actor: ActorDaemon},
			},
			want: 1,
		},
		{
			name: "unrelated action types after the episode boundary are ignored",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "attrs_set", Actor: ActorDaemon},
				{Type: "child_closed", Actor: ActorDaemon},
			},
			want: 0,
		},
		{
			// Defensive fallback (auto_reopen.go's own doc comment): no
			// done-entry action anywhere in the history at all. In
			// production this cannot happen — CountAutoReopens is only ever
			// called against a task currently sitting in "done", and the
			// only two rules that can put a triage task there both count as
			// a done-entry — but the function still needs a defined answer:
			// count the whole history rather than silently reporting 0.
			name: "no triage_done anywhere: falls back to the whole history",
			actions: []*Action{
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 1,
		},
		{
			// attrs_set landing on a done triage task (I-5b/I-5c) is a
			// non-transitioning done→done noop that ALSO sets
			// ToStatus==TaskStatusDone — it must NOT be mistaken for a fresh
			// episode boundary. If it were (keying on ToStatus alone instead
			// of Type), the reopen BEFORE the noop would fall out of the
			// counted window and this would wrongly report 1 instead of 2.
			name: "attrs_set noop on a done triage task does not reset the episode boundary",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorDaemon},
				{Type: "attrs_set", Actor: ActorDaemon, ToStatus: TaskStatusDone},
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 2,
		},
		{
			// The ordinary Manual "done" rule (executing→done / awaiting→done)
			// is treated as a done-entry too, defensively (see
			// isDoneEntryAction's own doc comment for why this branch is
			// unreachable for a real triage card today).
			name: "Manual done rule also counts as a done-entry (defensive)",
			actions: []*Action{
				{Type: "done", Actor: ActorHuman, ToStatus: TaskStatusDone},
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 1,
		},
	}
	for _, tc := range cases {
		got := CountAutoReopens(tc.actions)
		if got != tc.want {
			t.Errorf("%s: CountAutoReopens = %d, want %d", tc.name, got, tc.want)
		}
	}
}
