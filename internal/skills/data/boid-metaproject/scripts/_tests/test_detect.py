"""boidmeta.detect の非 boid source 側のテスト (設計 §5.1)。

`plan_candidates` は Slack / Jira / Bitbucket の「新着があった」を読み、対象
(どの task について考え直すか) と `screened_out` (判断を経ずに ack してよい
event_key) を決める。**対象の指し方が boid と違う** —— boid は task id、こちらは
identity で、解決できたものだけが task id になる。

**2026-08-29、PR-2やり直しv2 で `records`/`max_attempts` (khi 独自の attempts 機構、
`domain/attempts.py`) と `scans`/`assignments`/`abandoned` (唯一の呼び出し元
`app/sweep_targets.py` が読んでいなかったフィールド) を削除した。** dead letter は
boid 側の `MaxSignalAttempts` にそのまま乗る設計に変わったため、それらを固定していた
テスト (`ScanTest`/`DeadLetterTest`/`AssignmentTest` など) は削除した。
"""
from __future__ import annotations

import unittest
from datetime import datetime, timedelta, timezone

from boidmeta.detect import plan_candidates
from boidmeta.signal import Signal

T0 = datetime(2026, 8, 22, 0, 0, tzinfo=timezone.utc)

#: 既存 task に解決できる identity。
#: identity -> (task id, status)。**status まで持つ** —— 篩い 5 は status で
#: 判定する (done を落とすと再燃経路が死ぬ、S-9)。
#: **`parked`/`working` を使う** (2026-08-25、card モデル整理 —— captured/triaged/ready
#: の legacy status は boid 側で廃止された)。
RESOLVED = {"jira:X-1": ("triage-1", "parked"), "slack-thread:100.0": ("triage-2", "working")}


def slack(ts: str, *, thread: str = "100.0", author: str | None = None, minutes: int = 0, url: str | None = None) -> Signal:
    return Signal(
        source="slack",
        namespace="slack-cloud/mentions",
        event_key=f"slack-cloud/mentions:C1:{ts}",
        identity=f"slack-thread:{thread}",
        at=T0 + timedelta(minutes=minutes),
        author=author,
        url=url,
    )


def jira(key: str = "X-1", *, updated: str = "2026-08-22T00:00", minutes: int = 0) -> Signal:
    return Signal(
        source="jira-cloud",
        namespace="jira-cloud/assigned-issues",
        event_key=f"jira-cloud/assigned-issues:{key}:issue:{updated}",
        identity=f"jira:{key}",
        at=T0 + timedelta(minutes=minutes),
    )


def bitbucket(comment: str, *, key: str = "X-1", author: str | None = None, minutes: int = 0) -> Signal:
    return Signal(
        source="bitbucket-cloud",
        namespace="bitbucket-cloud/pr-comments",
        event_key=f"bitbucket-cloud/pr-comments:repo:1:comment:{comment}",
        identity=f"jira:{key}",
        at=T0 + timedelta(minutes=minutes),
        author=author,
    )


def plan(signals, **kwargs):
    kwargs.setdefault("resolved", RESOLVED)
    return plan_candidates(signals, **kwargs)


class TargetTest(unittest.TestCase):
    def test_a_resolved_identity_becomes_a_task_target(self):
        """既存 task に解決できたら、boid source のシグナルと同じ形 (task id) の対象になる。
        subagent から見て「既存 task を 1 件担当する」は両者で同じ仕事。"""
        result = plan([jira()])
        self.assertEqual([(t.task_id, t.identity) for t in result.targets], [("triage-1", "jira:X-1")])
        self.assertEqual(result.targets[0].signals, ("jira-cloud/assigned-issues:X-1:issue:2026-08-22T00:00",))

    def test_an_unresolved_identity_becomes_a_new_candidate(self):
        """**task id を持たない対象**。起票するかどうかは subagent が決める (S-15) ので、
        検知は identity を渡すだけ。"""
        result = plan([slack("1.0", thread="999.0")])
        (target,) = result.targets
        self.assertEqual(target.task_id, "")
        self.assertEqual(target.identity, "slack-thread:999.0")

    def test_the_target_carries_a_way_into_the_original(self):
        """**`Signal.url` を対象まで運ぶ。** ここで落とすと、検知が持っていた「原文への
        入口」が誰にも届かない —— 新規候補には task も description も無いので、
        subagent は identity から探し直すことになる。
        """
        result = plan([slack("1.0", thread="999.0", url="https://slack.invalid/p1")])
        self.assertEqual(result.targets[0].url, "https://slack.invalid/p1")

    def test_the_newest_signal_provides_the_url(self):
        """同じ件に複数来たら**最後のもの** —— 古い permalink より、いま起きたことの
        入口のほうが役に立つ。"""
        result = plan([
            slack("1.0", thread="999.0", url="https://slack.invalid/old", minutes=0),
            slack("2.0", thread="999.0", url="https://slack.invalid/new", minutes=1),
        ])
        self.assertEqual(result.targets[0].url, "https://slack.invalid/new")

    def test_a_missing_url_is_not_a_problem(self):
        """Jira の signal は `site_base_url` が無ければ url を持たない。"""
        result = plan([jira()])
        self.assertEqual(result.targets[0].url, "")

    def test_signals_of_one_identity_group_into_one_target(self):
        """**1 対象 = 1 subagent** (§5.2)。同じ件に 3 つ新着があっても subagent は 1 枚 ——
        分けると同じ task に同時に書く。"""
        result = plan([jira(minutes=1), bitbucket("c1", minutes=2), bitbucket("c2", minutes=3)])
        (target,) = result.targets
        self.assertEqual(target.task_id, "triage-1")
        self.assertEqual(len(target.signals), 3)

    def test_identities_across_sources_share_a_target(self):
        """Bitbucket の PR コメントは `jira:<KEY>` へ合流する (`adapters` の identity 規約)。
        **source が違っても identity が同じなら 1 対象**。"""
        result = plan([jira(), bitbucket("c1")])
        self.assertEqual(len(result.targets), 1)


class TargetShapeTest(unittest.TestCase):
    def test_a_target_must_point_at_something(self):
        """task id も identity も無い対象は、instruction に 1 行立つだけで subagent が
        何も読めない。**構築時に弾く。**"""
        from boidmeta.detect import Target

        with self.assertRaises(ValueError):
            Target(signals=("slack-cloud/mentions:C1:1.0",))


class ScreeningTest(unittest.TestCase):
    def test_a_self_authored_identity_is_dropped(self):
        """篩い 2 —— 自分の発言だけが実体の候補は落とす。khi が respond で投稿した返信が
        シグナルになり、それがまた task を動かして投稿する円環を防ぐ。"""
        result = plan([slack("1.0", thread="999.0", author="self")])
        self.assertEqual(result.targets, ())

    def test_one_other_voice_keeps_the_identity(self):
        """フェイルクローズ —— 他人の発言が 1 つでも混ざれば落とさない。"""
        result = plan([
            slack("1.0", thread="999.0", author="self"),
            slack("2.0", thread="999.0", author="someone"),
        ])
        self.assertEqual(len(result.targets), 1)

    def test_an_unknown_author_keeps_the_identity(self):
        """`author=None` は「判定できない」。self 認定しない (`domain/screen.py` の契約)。"""
        result = plan([slack("1.0", thread="999.0", author=None)])
        self.assertEqual(len(result.targets), 1)

    def test_an_aborted_identity_is_screened_out(self):
        """篩い 5 —— aborted に解決された identity は落とす。card 機械 v2 はこの status に
        到達しないので、通常タスクの aborted を拾う必要は無い。"""
        result = plan([jira()], resolved={"jira:X-1": ("triage-1", "aborted")})
        self.assertEqual(result.targets, (), "aborted が落ちていない")

    def test_a_done_identity_is_still_a_target(self):
        """**done は落とさない** (S-9、PR-K レビュー: done→reopen 経路)。

        done の card には I-5b ガード経由で attrs_set が通り、khi が `observed`/`reopen`
        を書ける (`internal/api/attrs_set_done.go`)。auto-reopen (`SweepReopen`) は
        card 機械 v2 で撤去済み (設計 §3.3) —— 再燃は khi が reopen を suggest し、
        人が accept する形に代わった。

        ここを落とすと「終わったと思っていた件に続きが来た」が二度と検知されない ——
        しかも**シグナルは栞に跨がれて静かに消える**ので、人にも気づけない。
        """
        result = plan([jira()], resolved={"jira:X-1": ("triage-1", "done")})
        self.assertEqual([(t.task_id, t.identity) for t in result.targets],
                         [("triage-1", "jira:X-1")])

    def test_a_dropped_identity_is_not_screened_by_status_if_it_somehow_resolves(self):
        """**`NO_RECORD_STATUSES` は dropped を持たない**
        (PR-K レビュー MEDIUM 2 → finding B/2 で訂正)。

        `drop` は card の identity を全解放する
        (`internal/orchestrator/task_identity.go` の `UnlinkAllForTask` — done は
        identity を保持するが drop は手放す、という意図的な非対称性)ので、
        **通常の巡では** `resolved` に dropped の task_id が乗ること自体が起きない
        —— `resolve_identity` は「未登録」を返し、そのシグナルは新規候補として
        `capture` される (下の `test_a_previously_dropped_source_becomes_a_fresh_candidate`
        が実際の挙動を pin する)。

        **ただし今日到達しうる経路がある** —— `LinkIdentity` は対象 task の status を
        検査しないので、オペレータが `boid task identity link` で dropped card に
        キーを再 link すれば (カットオーバー runbook §6 手順3 が実際に使う操作)、
        次の巡からこの分岐に本当に来る。「識別子機構の将来変更に備えた保険」ではなく、
        人の運用操作で今日から発火しうる経路として扱うこと —— 死んだコードと判断して
        `NO_RECORD_STATUSES` に `dropped` を戻さないこと。この関数自身は、無条件に
        捨てて栞だけ進めることをしない、という pure 関数としての振る舞いを見る。"""
        result = plan([jira()], resolved={"jira:X-1": ("triage-1", "dropped")})
        self.assertEqual([(t.task_id, t.identity) for t in result.targets],
                         [("triage-1", "jira:X-1")])

    def test_a_previously_dropped_source_becomes_a_fresh_candidate(self):
        """**実際の契約はこちら**: drop で identity が解放された後にソースが再燃すると、
        khi はそれを元の card と結び付けられず、新規候補として扱う。

        `resolved` に該当 identity のエントリが無いのが `resolve_identity` の「未登録」
        (`drop` 済みで identity が解放された状態と同じ形) —— この形は `task_id=""` の
        新規候補になる (`test_an_unresolved_identity_becomes_a_new_candidate` と同じ
        経路)。**元の card の summary・children・action 履歴・reopen 経路には
        辿り着けない** —— これが「dropped は本当にやらないと決めたときだけに使う」
        (`.claude/skills/khi-sweep/SKILL.md`) の根拠になっている事実。
        """
        result = plan([jira()], resolved={})
        (target,) = result.targets
        self.assertEqual(target.task_id, "")
        self.assertEqual(target.identity, "jira:X-1")


class ScreenedOutEventKeysTest(unittest.TestCase):
    """`CandidatePlan.screened_out` (2026-08-27、khi の検知を boid signal inbox 読みへ
    切り替える際の Fix 2)。

    機構の篩い (self-authored / `NO_RECORD_STATUSES` 合流) で落ちた候補は記録を書かずに
    `screened_out` へ積む。呼び出し側 (`app/sweep_targets.py`) はこの集合をそのまま
    `boid signal ack` する。

    ここで固定するのは:

    - (a) self-authored で篩われた signal は `screened_out` に入る
    - (b) `NO_RECORD_STATUSES` (現状は aborted のみ) の task に合流する signal は
      `screened_out` に入る
    - (c) target 化された (既存 target への合流も含む) signal は判断待ちなので
      **`screened_out` に入らない**
    - (d) `MAX_TARGETS` 溢れで次巡送りになった signal も判断待ちなので
      **`screened_out` に入らない**
    """

    def test_a_self_authored_signal_is_screened_out(self):
        result = plan([slack("1.0", thread="999.0", author="self")])
        self.assertEqual(result.screened_out, frozenset({"slack-cloud/mentions:C1:1.0"}))

    def test_a_self_authored_group_screens_out_every_member(self):
        """group 全体が篩いの対象 (`self_authored` は群で判定する)。"""
        result = plan([
            slack("1.0", thread="999.0", author="self", minutes=0),
            slack("2.0", thread="999.0", author="self", minutes=1),
        ])
        self.assertEqual(result.screened_out, frozenset({"slack-cloud/mentions:C1:1.0", "slack-cloud/mentions:C1:2.0"}))

    def test_an_aborted_status_merge_is_screened_out(self):
        """`NO_RECORD_STATUSES` (現状 aborted のみ) への合流 —— card 機械 v2 は
        attrs_set/noted を受け付けないので記録を書けない。"""
        result = plan([jira()], resolved={"jira:X-1": ("triage-1", "aborted")})
        self.assertEqual(result.screened_out, frozenset({"jira-cloud/assigned-issues:X-1:issue:2026-08-22T00:00"}))

    def test_a_regular_target_is_not_screened_out(self):
        """(c) target 化された signal はまだ判断待ち —— ack してはいけない。"""
        result = plan([jira()])
        self.assertTrue(result.targets, "前提: target が実際に立っていること")
        self.assertEqual(result.screened_out, frozenset())


    def test_an_overflowed_signal_is_not_screened_out(self):
        """(d) 篩い 6 (`max_targets`) で溢れた signal は次巡に回るだけ —— 判断は
        済んでいないので ack してはいけない。"""
        signals = [slack(f"{i}.0", thread=f"t{i}", minutes=i) for i in range(3)]
        result = plan(signals, max_targets=1)
        self.assertEqual(len(result.targets), 1)
        self.assertEqual(result.screened_out, frozenset())


class LimitTest(unittest.TestCase):
    def test_the_target_count_is_capped(self):
        """篩い 6 —— 溢れたぶんは次の巡に回す。**無駄な sweep task が立つコストは実行時間
        だけではない** (終了済み task の一覧が汚れて人が過去を追えなくなる)。"""
        signals = [slack(f"{i}.0", thread=f"t{i}", minutes=i) for i in range(5)]
        result = plan(signals, max_targets=2)
        self.assertEqual(len(result.targets), 2)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
