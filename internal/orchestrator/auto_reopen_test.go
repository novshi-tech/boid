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
			// would be the FIRST automatic reopen.
			name:             "source flipped to false, never auto-reopened before",
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
			// フラップ対策 (12節 B-6 既定案): the daemon has already
			// auto-reopened this exact task once before — a second flip must
			// NOT auto-reopen again.
			name:             "source flipped to false again, already auto-reopened once",
			detail:           `{"attrs":{"observed":{"source_closed":false}}}`,
			priorAutoReopens: 1,
			want:             false,
		},
		{
			name:             "source flipped to false again, already auto-reopened twice",
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

// TestCountAutoReopens pins 決定13 (state は導出、専用カウンタ列を作らない):
// the フラップ count comes from filtering the task's own action history, not
// a dedicated column. Only reopen_triaged actions stamped ActorDaemon count —
// a human's own manual reopen (資格ある判断、フラップではない) must not eat
// into the automatic budget.
func TestCountAutoReopens(t *testing.T) {
	cases := []struct {
		name    string
		actions []*Action
		want    int
	}{
		{name: "no actions", actions: nil, want: 0},
		{
			name: "one daemon reopen_triaged",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "reopen_triaged", Actor: ActorDaemon},
			},
			want: 1,
		},
		{
			name: "two daemon reopen_triaged",
			actions: []*Action{
				{Type: "reopen_triaged", Actor: ActorDaemon},
				{Type: "triage_done", Actor: ActorDaemon},
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
				{Type: "reopen_triaged", Actor: ActorHuman},
			},
			want: 0,
		},
		{
			name: "mixed human and daemon",
			actions: []*Action{
				{Type: "reopen_triaged", Actor: ActorHuman},
				{Type: "reopen_triaged", Actor: ActorDaemon},
				{Type: "attrs_set", Actor: ActorDaemon},
			},
			want: 1,
		},
		{
			name: "unrelated action types are ignored",
			actions: []*Action{
				{Type: "triage_done", Actor: ActorDaemon},
				{Type: "attrs_set", Actor: ActorDaemon},
				{Type: "child_closed", Actor: ActorDaemon},
			},
			want: 0,
		},
	}
	for _, tc := range cases {
		if got := CountAutoReopens(tc.actions); got != tc.want {
			t.Errorf("%s: CountAutoReopens = %d, want %d", tc.name, got, tc.want)
		}
	}
}
