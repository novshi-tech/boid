package orchestrator

import "encoding/json"

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): auto-reopen と done への着地 ----
//
// This is 決定15's mirror (I-5): where ShouldAutoDone closes a working triage
// task the moment its source reports closed, ShouldAutoReopen reopens a DONE
// one the moment the SAME source reports open again. Same shape as
// ShouldAutoDone/ShouldWake — a pure predicate over stored fields, no agent
// judgment (決定12) — and it lives here, next to triage_done.go's
// ShouldAutoDone/SourceClosed, because it reads the identical single key
// those already read.

// ShouldAutoReopen evaluates I-5: "observed.source_closed が true → false に
// 戻ったら reopen_triaged". The daemon does NOT read a second key to detect
// the flip — a done triage task can only have REACHED done via triage_done
// (machine.go), which itself requires SourceClosed(detail)==true at that
// moment (ShouldAutoDone's own first conjunct), so simply re-reading the
// SAME key now and finding it false again already IS the true→false
// transition. This is exactly what I-5's own text promises: "読むキーは
// 増やさない".
//
// priorAutoReopens is 12節 B-6 の既定案 (フラップ対策) — SCOPED TO THE
// TASK'S CURRENT DONE EPISODE, not its lifetime (2026-08-19, nose 判断 —
// see CountAutoReopens's own doc comment for why the scope changed). Once
// the daemon has already auto-reopened this task once WITHIN THIS SAME
// EPISODE, a further flip does not auto-reopen again. A priorAutoReopens of
// 0 is "never auto-reopened before in this episode" and is the only value
// that admits a reopen; everything else falls to the caller's フラップ通知
// path instead.
func ShouldAutoReopen(detail json.RawMessage, priorAutoReopens int) bool {
	if SourceClosed(detail) {
		return false
	}
	return priorAutoReopens == 0
}

// isDoneEntryAction reports whether a is an action that transitioned a task
// INTO "done" — the event CountAutoReopens uses to bound one "done episode".
//
// triage_done (machine.go, working→done) is the only rule that can actually
// fire against a triage-carrying task. Type=="done" (machine.go's ordinary
// Manual rule, executing→done / awaiting→done) is included too even though
// no transition rule ever lets a triage card's own statuses
// (captured/triaged/parked/ready/working) reach executing or awaiting — so
// this branch cannot be exercised by a real triage card today (判断,
// 2026-08-19: included anyway, purely so a future change to that
// reachability doesn't silently miss an episode boundary here as well; the
// cost is one extra type-string check, not extra state).
//
// attrs_set landing on a done triage task (I-5b/I-5c, attrs_set_done.go) is
// deliberately EXCLUDED even though it too sets ToStatus==TaskStatusDone
// (it is a non-transitioning noop, done→done — resolveAttrsSetDoneTransition
// returns the SAME status unchanged). Keying this on ToStatus alone instead
// of Type would let a chatty source (repeated attrs_set while
// source_closed stays true, I-5c's own "続報が来た" case) keep pushing the
// episode boundary forward on every attrs_set and wrongly reset the
// フラップ budget mid-episode, even though the task never actually left
// "done" in between.
func isDoneEntryAction(a *Action) bool {
	if a == nil {
		return false
	}
	return a.Type == "triage_done" || (a.Type == "done" && a.ToStatus == TaskStatusDone)
}

// CountAutoReopens derives 「この task を daemon が"今のdoneエピソード内で"
// 何回自動 reopen したか」from the task's own action history (決定13: state
// は導出、専用カウンタ列を作らない) — a filter over already-recorded
// actions, scoped to everything AFTER the most recent isDoneEntryAction.
//
// なぜ生涯合計ではなくエピソード単位か (2026-08-19, nose 判断 — Opus review
// で指摘): SweepReopen が列挙する候補は Status=="done" の task だけ
// (triage_done.go) で、auto-reopen した card は必ず "triaged" に着地する
// — つまり reopen された瞬間、その card は構造上二度と SweepReopen の対象
// にならない。次に候補になるのは triaged→ready→working→auto-done
// (triage_done) と1周回りきって再び done に落ちた後だけ。旧実装 (action
// 履歴の生涯合計) はこの「2周目の正当な自動 reopen」まで機械的に止めて
// いた — 長生きする Jira 課題の card が「2周目からは必ず人が押す」運用に
// なってしまう実バグだった。止めたいのは「同一 done エピソード内で2回」
// という、本来のフラップだけ。30日 GC でこのカウントがリセットされる心配
// は成立しない — GCTasks は削除対象の task と同じ集合の action しか消さず
// (task 自身も同じ Tx で消える)、かつ動いている card は reopen のたびに
// updated_at が更新されるので 30日ルールにそもそも引っかからない
// (レビュアー確認済み)。
//
// 同着 (created_at が同一) の扱い: この関数は渡された actions スライスの
// 順序をそのまま信じる — created_at を自前で比較・再ソートすることは一切
// しない。ListActionsByTask の `ORDER BY created_at` (store.go) が返す
// 1回分の結果は、同着があっても何らかの確定した順序で返る (SQLite が
// tie をどちらに倒すかは仕様として保証されないが、1回のクエリ結果の中では
// 一貫している) ため、この関数はそれをそのまま辿るだけで十分。
// action_list.go が引用する実際の同着源 —
// internal/dispatcher/store.go の markStaleTasksAborted (ループ外で
// `now := time.Now()` を1回取り、複数 task にまたがって同じ値を書く一括
// abort 経路) — は executing/awaiting にしか触れない。triage card はそも
// そも executing/awaiting に到達しない (isDoneEntryAction の doc comment
// 参照) ため、この tie 源が同一 triage card の複数 action を巻き込むこと
// はない。単一 task の action 列は常に「前の action が commit してから次
// が読める」という因果関係で作られる (autoDone/autoReopen/ApplyAction は
// いずれも fresh な in-Tx 読み直しを経てから書く) ので、同一 task 内で
// isDoneEntryAction な行と reopen_triaged な行が同着するケースは実質発生
// しない。
//
// triage_done (または他の done-entry) が1本も無い task は、履歴全体を
// カウント対象にする (フォールバック)。CountAutoReopens は autoReopen の
// 事前チェックで「現在 Status==done」の task に対してしか呼ばれず、triage
// card が done に到達できる経路は isDoneEntryAction が数える2つのルール
// しかない (N-2 の machine.go 全走査と同じ結論) ので、この分岐は実運用で
// は到達しない — 空/欠損した履歴が「生涯 reopen 0 回」のように黙って
// 誤魔化されるより、素直に「全部数える」側に倒しておくための保険。
//
// Only reopen_triaged actions stamped Actor==ActorDaemon count within the
// episode. A human's own manual reopen of a done triage task (the Web UI
// button; resolveReopenVariant routes it through the identical
// reopen_triaged verb, workflow_action.go) is a deliberate human judgment,
// not automation flapping, so it must not consume the automatic budget — a
// human who reopens a card by hand can do so as many times as they like,
// and does not itself start a new episode either (a reopen never sets
// ToStatus==done).
func CountAutoReopens(actions []*Action) int {
	lastEpisodeStart := -1
	for i, a := range actions {
		if isDoneEntryAction(a) {
			lastEpisodeStart = i
		}
	}
	n := 0
	for _, a := range actions[lastEpisodeStart+1:] {
		if a != nil && a.Type == "reopen_triaged" && a.Actor == ActorDaemon {
			n++
		}
	}
	return n
}
