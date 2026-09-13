"""boidmeta.sweep のテスト (2026-08-28、PR-2)。

対象の組み立て (`boid signal list --claim` を読む・identity を解決する・篩う・畳む)
はここに移った。**判断ロジック自体 (`app/detect.plan_candidates`) は変えていない**
ので、その篩いの細部 (self-authored の判定条件など) は `test_detect_candidates.py`
が単体で固めている。ここで見るのは配線: `build()` が正しい入力で
`plan_candidates`/`resolve_identities`/`merge_targets` を呼び、正しい集合を
ack すること。boid-pack signal 特有の挙動 (identity=task id の解決) はここでしか
固めていない。
"""
from __future__ import annotations

import io
import unittest
from datetime import datetime, timedelta, timezone

from boidmeta.detect import MAX_TARGETS
from boidmeta.sweep import DEFAULT_WRITE_COMMAND, build, main

T0 = datetime(2026, 8, 28, 0, 0, tzinfo=timezone.utc)


def _rfc3339(dt: datetime) -> str:
    return dt.isoformat().replace("+00:00", "Z")


def slack_envelope(ts: str = "1.0", *, channel: str = "C1", thread: str = "100.0", minutes: int = 0,
                    author: "str | None" = None, url: "str | None" = None) -> dict:
    row: dict = {
        "id": f"{channel}:{ts}",
        "occurred_at": _rfc3339(T0 + timedelta(minutes=minutes)),
        "source": {"pack": "slack", "connector": "mentions", "service": "slack-cloud"},
        "identity": f"slack-thread:{thread}",
    }
    if author is not None:
        row["author"] = author
    if url is not None:
        row["url"] = url
    return row


def bitbucket_envelope(comment_id: str = "7", *, repo: str = "khi-task-collector", pr_id: str = "1",
                        jira_key: str = "KT-1", minutes: int = 0) -> dict:
    return {
        "id": f"{repo}:{pr_id}:comment:{comment_id}",
        "occurred_at": _rfc3339(T0 + timedelta(minutes=minutes)),
        "source": {"pack": "bitbucket-cloud", "connector": "pr-comments", "service": "bitbucket-cloud"},
        "identity": f"jira:{jira_key}",
    }


def real_jira_envelope(key: str = "PROJ-1", *, minutes: int = 0) -> dict:
    """jira-cloud/assigned-issues の実物 id 形式 (`jira:` prefix 無し)。"""
    at = T0 + timedelta(minutes=minutes)
    occurred_at = _rfc3339(at)
    return {
        "id": f"{key}:{occurred_at}",
        "occurred_at": occurred_at,
        "source": {"pack": "jira-cloud", "connector": "assigned-issues", "service": "jira-cloud"},
        "identity": f"jira:{key}",
    }


class FakeCLI:
    def __init__(self, *, signals=(), signals_error=None, ack_error=None, claim_error=None, identities=None,
                 task_statuses=None, behaviors=None, own_task_id="sweep-1", description_of=None) -> None:
        self.calls: list[tuple] = []
        self._signals = list(signals)
        self._signals_error = signals_error
        self._ack_error = ack_error
        self._claim_error = claim_error
        self._identities = identities or {}
        self._task_statuses = task_statuses or {}
        self._behaviors = behaviors or {}
        self._own_task_id = own_task_id
        self.acked: list[str] = []
        self.claimed: list[str] = []
        self.descriptions: dict[str, str] = dict(description_of or {})

    def list_signals(self, *, state="pending", limit=None):
        self.calls.append(("list_signals", state, limit))
        if self._signals_error is not None:
            raise self._signals_error
        return {"signals": list(self._signals)}

    def claim_signals(self, ids):
        ids = list(ids)
        self.calls.append(("claim_signals", tuple(ids)))
        if self._claim_error is not None:
            raise self._claim_error
        self.claimed.extend(ids)

    def ack_signals(self, ids):
        ids = list(ids)
        self.calls.append(("ack_signals", tuple(ids)))
        if self._ack_error is not None:
            raise self._ack_error
        self.acked.extend(ids)

    def resolve_identity(self, identity):
        self.calls.append(("resolve_identity", identity))
        return self._identities.get(identity)

    def task_field(self, task_id, field):
        self.calls.append(("task_field", task_id, field))
        if field == "status":
            if task_id not in self._task_statuses:
                raise RuntimeError(f"no such task: {task_id}")
            return self._task_statuses[task_id]
        return self._behaviors.get(task_id, "implement")

    def current_field(self, field):
        self.calls.append(("current_field", field))
        if field == "id":
            return self._own_task_id
        raise AssertionError(f"unexpected field: {field}")

    def update_description(self, task_id, description):
        self.calls.append(("update_description", task_id, description))
        self.descriptions[task_id] = description

    def named(self, name):
        return [c for c in self.calls if c[0] == name]

    def wrote(self, name):
        return bool(self.named(name))


class ReadTest(unittest.TestCase):
    def test_the_read_is_pending_and_free(self):
        """**読みは副作用なし** (2026-08-29、boid #1033) —— `build` は 1 件も課金
        しない。課金するのは `main` が `to_claim` を渡したときだけ。"""
        cli = FakeCLI(signals=[slack_envelope()])
        build(cli)
        (_call, state, _limit), = cli.named("list_signals")
        self.assertEqual(state, "pending")
        self.assertFalse(cli.wrote("claim_signals"))

    def test_a_failed_read_reports_not_ok(self):
        cli = FakeCLI(signals_error=RuntimeError("boom"))
        round_ = build(cli)
        self.assertFalse(round_.ok)
        self.assertEqual(round_.targets, ())
        self.assertEqual(round_.screened_out, frozenset())
        self.assertEqual(round_.to_claim, frozenset())


class BasicTargetTest(unittest.TestCase):
    def test_a_fresh_signal_becomes_a_new_candidate_target(self):
        cli = FakeCLI(signals=[slack_envelope()])
        round_ = build(cli)
        targets, screened_out, ok = round_.targets, round_.screened_out, round_.ok
        self.assertTrue(ok)
        self.assertEqual(len(targets), 1)
        self.assertEqual(targets[0].identity, "slack-thread:100.0")
        self.assertEqual(screened_out, frozenset())

    def test_nothing_pending_means_no_targets(self):
        round_ = build(FakeCLI())
        targets, screened_out, ok = round_.targets, round_.screened_out, round_.ok
        self.assertEqual((targets, screened_out, ok), ((), frozenset(), True))


class WhatGetsClaimedTest(unittest.TestCase):
    """**この変更の本体** (2026-08-29、boid #1033)。`attempts` は「諦めるまでの回数」
    なので、数える対象は「判断に回した」でなければならない。"""

    def test_target_signals_are_claimed(self):
        cli = FakeCLI(signals=[slack_envelope()])
        round_ = build(cli)
        self.assertEqual(round_.to_claim, frozenset({"slack-cloud/mentions:C1:1.0"}))

    def test_screened_out_signals_are_not_claimed(self):
        """篩いで落とした signal は同じ巡でそのまま ack する —— 判断に回していない
        ので数えない。"""
        cli = FakeCLI(signals=[slack_envelope(author="self")])
        round_ = build(cli)
        self.assertEqual(round_.screened_out, frozenset({"slack-cloud/mentions:C1:1.0"}))
        self.assertEqual(round_.to_claim, frozenset())

    def test_signals_that_overflow_max_targets_are_neither_claimed_nor_acked(self):
        """**旧 `list --claim` が壊していたのがここ。** 読み出しが返した行を一律で
        課金していたので、`MAX_TARGETS` から溢れて次巡送りにしただけの signal が毎巡
        attempts を焼き、5 巡で誰も判断していない signal が無言で dead に落ちていた。
        溢れた分は claim も ack もせず、次巡そのまま読み直す。"""
        signals = [slack_envelope(ts=f"{i}.0", thread=f"{i}00.0", minutes=i) for i in range(MAX_TARGETS + 3)]
        cli = FakeCLI(signals=signals)
        round_ = build(cli)
        self.assertEqual(len(round_.targets), MAX_TARGETS, "上限どおりに切れていること")
        self.assertEqual(len(round_.to_claim), MAX_TARGETS, "claim するのは対象になった分だけ")
        self.assertEqual(round_.screened_out, frozenset())
        claimed_or_acked = round_.to_claim | round_.screened_out
        read_keys = {f"slack-cloud/mentions:{s['id']}" for s in signals}
        deferred = read_keys - claimed_or_acked
        self.assertEqual(len(deferred), 3, "溢れた 3 件はどちらの集合にも入らない")
        self.assertEqual(round_.deferred, 3, "溢れた件数が人から見えること")


class MergeTest(unittest.TestCase):
    def test_two_signals_resolving_to_the_same_task_merge_into_one_target(self):
        """同じ task を異なる identity から指すシグナルが 2 本来たら 1 対象にまとめる
        —— `plan_candidates` は identity 単位でグループ化するので、放置すると task_id が
        同じでも 2 つの Target ができ、2 枚の subagent が同じ task に同時に書く。"""
        cli = FakeCLI(
            signals=[real_jira_envelope("KT-9"), bitbucket_envelope()],
            identities={"jira:KT-9": ("triage-1", "parked"), "jira:KT-1": ("triage-1", "parked")},
        )
        targets = build(cli).targets
        self.assertEqual(len(targets), 1)
        self.assertEqual(
            set(targets[0].signals),
            {
                f"jira-cloud/assigned-issues:KT-9:{_rfc3339(T0)}",
                "bitbucket-cloud/pr-comments:khi-task-collector:1:comment:7",
            },
        )


class ScreeningTest(unittest.TestCase):
    def test_a_self_authored_slack_signal_is_screened_and_acked(self):
        cli = FakeCLI(signals=[slack_envelope(author="self")])
        round_ = build(cli)
        targets, screened_out = round_.targets, round_.screened_out
        self.assertEqual(targets, ())
        self.assertEqual(screened_out, frozenset({"slack-cloud/mentions:C1:1.0"}))

    def test_an_aborted_merge_is_screened_and_acked(self):
        cli = FakeCLI(identities={"jira:KT-1": ("triage-1", "aborted")}, signals=[bitbucket_envelope()])
        round_ = build(cli)
        targets, screened_out = round_.targets, round_.screened_out
        self.assertEqual(targets, ())
        self.assertEqual(screened_out, frozenset({"bitbucket-cloud/pr-comments:khi-task-collector:1:comment:7"}))

    def test_a_pack_the_library_has_never_heard_of_still_maps(self):
        """**対応表が無いことの確認。** 新しい connector を足しても、この層に手を
        入れずに signal が通る —— pack 名がそのまま `Signal.source` になる。
        khi は `PACK_TO_SOURCE` の白名簿を持っていたので、pack を足すたびに
        workspace 側のコードを直す必要があった。"""
        cli = FakeCLI(signals=[{
            "id": "novshi-tech/boid#1033:2026-08-28T00:00:00Z",
            "occurred_at": "2026-08-28T00:00:00Z",
            "source": {"pack": "github", "connector": "assigned-issues", "service": "github-api"},
            "identity": "github:novshi-tech/boid#1033",
        }])
        round_ = build(cli)
        self.assertEqual(len(round_.targets), 1)
        self.assertEqual(round_.targets[0].identity, "github:novshi-tech/boid#1033")
        self.assertEqual(
            round_.to_claim,
            frozenset({"github-api/assigned-issues:novshi-tech/boid#1033:2026-08-28T00:00:00Z"}),
        )


class JiraNamespaceTest(unittest.TestCase):
    """2026-08-27 Fix 1 が `main()` を通しても崩れないことを固定する。"""

    def test_a_real_jira_envelope_becomes_a_target(self):
        cli = FakeCLI(signals=[real_jira_envelope()])
        targets = build(cli).targets
        self.assertEqual(len(targets), 1)
        self.assertEqual(targets[0].signals, (f"jira-cloud/assigned-issues:{real_jira_envelope()['id']}",))

    def test_main_acks_screened_signals_with_the_original_envelope_id(self):
        """ack は namespace を補う前の元の envelope id で打つ。"""
        envelope = real_jira_envelope()
        # aborted 合流で screen される形にする。
        cli = FakeCLI(identities={f"jira:{envelope['identity'].split(':', 1)[1]}": ("triage-1", "aborted")},
                      signals=[envelope])
        out = io.StringIO()
        code = main(["--judge-skill", "/judge"], cli=cli, stdout=out)
        self.assertEqual(code, 0)
        self.assertEqual(cli.acked, [envelope["id"]])


class FlagsTest(unittest.TestCase):
    """設定ファイルを置かせない代わりに、behavior の `default_instruction` が
    フラグで渡す (`boidmeta.sweep.main` の docstring)。"""

    def test_the_intake_skill_lands_in_the_description(self):
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--intake-skill", "/nvt-intake"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        self.assertIn("/nvt-intake", description)

    def test_the_deprecated_judge_skill_alias_still_works(self):
        """runner image のデプロイと `boid project fetch` は別手順で、同時には
        切り替えられない。旧フラグ名は移行窓のあいだ受け続ける。"""
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--judge-skill", "/nvt-sweep"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        self.assertIn("/nvt-sweep", description)

    def test_the_new_flag_wins_when_both_are_given(self):
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--judge-skill", "/old", "--intake-skill", "/new"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        self.assertIn("/new", description)
        self.assertNotIn("/old", description)

    def test_the_write_command_defaults_to_this_skills_absolute_path(self):
        """**組み込みスキルとして配る意味がここに出る。** メタプロジェクト側にコピーが
        無いので、モジュール名や相対パスを書いた description を渡すと subagent は
        `No module named ...` で何も記録できず、それでも巡は exit 0 で終わる。"""
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        self.assertIn("~/.claude/skills/boid-metaproject/scripts/write.py", description)
        self.assertEqual(DEFAULT_WRITE_COMMAND, "python3 ~/.claude/skills/boid-metaproject/scripts/write.py")

    def test_an_overridden_write_command_reaches_the_description(self):
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--judge-skill", "/judge", "--write-command", "python3 /opt/other/write.py"],
             cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        self.assertIn("python3 /opt/other/write.py", description)

    def test_max_targets_reaches_build_through_main(self):
        """`build(max_targets=...)` を直接呼ぶテストだけだと、`main` がフラグを
        `build` へ渡し忘れても気づけない (2026-08-29 のレビューで実際に mutation が
        生き残った)。"""
        signals = [slack_envelope(ts=f"{i}.0", thread=f"{i}00.0", minutes=i) for i in range(6)]
        cli = FakeCLI(signals=signals, own_task_id="sweep-1")
        main(["--judge-skill", "/judge", "--max-targets", "2"], cli=cli, stdout=io.StringIO())
        self.assertEqual(len(cli.claimed), 2, "claim されるのは対象になった 2 件だけ")

    def test_max_targets_caps_the_round(self):
        signals = [slack_envelope(ts=f"{i}.0", thread=f"{i}00.0", minutes=i) for i in range(6)]
        round_ = build(FakeCLI(signals=signals), max_targets=2)
        self.assertEqual(len(round_.targets), 2)
        self.assertEqual(round_.deferred, 4)

    def test_an_intake_skill_is_required(self):
        with self.assertRaises(SystemExit):
            main([], cli=FakeCLI(), stdout=io.StringIO())


class IntakeScopeTest(unittest.TestCase):
    """巡が担うのは仕分けまで。**肉付けは card コマンドの側**で、そちらは
    `capture`/`link`/`note` が書く action から daemon が自動で起こす。

    どの段がどの verb を持つかは機構の話なので、workspace のスキル 2 本に
    書かせず、機構が組む instruction の側で 1 度だけ言う。
    """

    def outlet_line(self):
        """出口を名指ししている**その 1 行**を取り出す。

        description 全体への部分一致では、他の行がたまたま同じ verb 名に触れて
        いるだけで緑になる (実際に一度そうなった)。
        """
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--intake-skill", "/intake"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        lines = [line for line in description.splitlines() if "この巡の出口" in line]
        self.assertEqual(len(lines), 1, f"出口を名指しする行が 1 行でない: {lines}")
        return lines[0]

    def test_the_outlet_line_names_every_verb_this_stage_owns(self):
        line = self.outlet_line()
        for verb in ("capture", "link", "note", "skip"):
            self.assertIn(verb, line, f"{verb} が出口の行に無い")

    def test_the_outlet_line_excludes_the_next_stages_verbs(self):
        """ここで summary や spec を書かせると、card コマンドと二重に判断が走る。"""
        line = self.outlet_line()
        for verb in ("summary", "spec", "go"):
            self.assertNotIn(verb, line, f"{verb} が出口の行に混じっている")

    def test_the_round_says_a_card_bearing_target_gets_a_note(self):
        """対象が card 付きで来たときの出口は 1 つしかない。巡の instruction が
        それを名指ししないと、subagent は「書くことが無い」と結論して黙る ——
        そこに落ちた続報は判断を起こさない。"""
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--intake-skill", "/intake"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        lines = [line for line in description.splitlines() if "既に card がある" in line]
        self.assertEqual(len(lines), 1, f"card 付き対象の出口を言う行が 1 行でない: {lines}")
        self.assertIn("note", lines[0])

    def test_the_round_limits_skip_to_a_taskless_candidate(self):
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--intake-skill", "/intake"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        lines = [line for line in description.splitlines() if "`skip` は" in line]
        self.assertEqual(len(lines), 1, f"skip の対象を限定する行が 1 行でない: {lines}")
        self.assertIn("新規候補", lines[0])
        self.assertIn("identity", lines[0])

    def test_the_round_requires_identity_for_skip(self):
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-1")
        main(["--intake-skill", "/intake"], cli=cli, stdout=io.StringIO())
        (_call, _task, description), = cli.named("update_description")
        self.assertIn("`skip` には対象行の `identity` も渡す", description)


class MainTest(unittest.TestCase):
    def test_updates_its_own_description_with_the_instruction(self):
        cli = FakeCLI(signals=[slack_envelope()], own_task_id="sweep-77")
        code = main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        self.assertEqual(code, 0)
        (_call, task_id, description), = cli.named("update_description")
        self.assertEqual(task_id, "sweep-77")
        self.assertIn("slack-thread:100.0", description)

    def test_a_failed_read_exits_nonzero_and_does_not_touch_the_description(self):
        cli = FakeCLI(signals_error=RuntimeError("boom"))
        code = main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        self.assertEqual(code, 1)
        self.assertFalse(cli.wrote("update_description"))

    def test_main_claims_target_signals_with_the_original_envelope_id(self):
        """claim も ack と同じく namespace を補う前の元の envelope id で打つ。"""
        envelope = real_jira_envelope()
        cli = FakeCLI(signals=[envelope])
        code = main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        self.assertEqual(code, 0)
        self.assertEqual(cli.claimed, [envelope["id"]])

    def test_main_claims_before_writing_the_description(self):
        """description を書いた瞬間から subagent が判断を始めるので、そのあとに
        数えると「判断に回した」の記録が実際の受け渡しより遅れる。"""
        cli = FakeCLI(signals=[slack_envelope()])
        main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        names = [c[0] for c in cli.calls]
        self.assertLess(names.index("claim_signals"), names.index("update_description"))

    def test_a_claim_failure_does_not_stop_the_round(self):
        """課金できなかった巡は、その signal が dead へ 1 歩近づかないだけ。判断は
        進める (`inbox.claim` が握り潰す)。"""
        cli = FakeCLI(signals=[slack_envelope()], claim_error=RuntimeError("boom"))
        code = main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        self.assertEqual(code, 0)
        self.assertTrue(cli.wrote("update_description"))

    def test_target_signals_are_not_acked_here(self):
        """判断待ちの signal は sweep task ではなく、担当した subagent が判断を書いた
        直後に自分で ack する (`app/write.py`)。"""
        cli = FakeCLI(signals=[slack_envelope()])
        main(["--judge-skill", "/judge"], cli=cli, stdout=io.StringIO())
        self.assertEqual(cli.acked, [])


if __name__ == "__main__":
    unittest.main()
