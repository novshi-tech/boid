"""boidmeta.write の実行部のテスト (設計 §5.3 / §5.4)。

verb ごとに boid へ何を書くか、差分ガード (S-8) がどこで効くか、処理記録 (S-11) が
どこへ載るか、通知 (S-7) がいつ飛ぶか。**boid の CLI は fake で差し替える** ——
ここで見たいのは「どの op をどの順で呼ぶか」であって、CLI の引数の組み立てではない
(それは `tests/adapters/test_boid_store.py`)。
"""
from __future__ import annotations

import contextlib
import io
import unittest

from boidmeta.write import CommandError, Executor, Result, validate

SIGNALS = ["boid:a1"]


class FakeCLI:
    """`BoidCLI` の書き込み口と読み口を記録するダブル。"""

    def __init__(
        self, *, view=None, attrs=None, created=True, projects=None, behaviors=None, readonly="false"
    ) -> None:
        self.calls: list[tuple] = []
        # **既定は `parked`** (2026-08-25、PR-K レビュー MEDIUM 3 — 以前は v1 の legacy
        # status `triaged` がデフォルトで、card 機械 v2 で新しく suggestion verb の
        # status ゲート (HIGH 1) を足したときに、bare `FakeCLI()` を使う既存テストの
        # 大半がこのデフォルト経由で無自覚に `triaged` を渡していた。card は v2 で必ず
        # `parked` として誕生するので、それに揃える —— status に関心があるテストは
        # 明示的に `view=` を渡すこと)。
        self._view = view or {"task_id": "t1", "status": "parked", "description": ""}
        self._attrs = attrs if attrs is not None else {}
        self._created = created
        self._projects = projects or {"rook-tools": "p-uuid-1"}
        #: project id -> その project が持つ behavior 名。既定は「照合できない」——
        #: 既存テストの大半は behavior の可否に関心が無いので、そこを素通しにする。
        self._behaviors = behaviors if behaviors is not None else {}
        #: **今動いている自分自身** の readonly。`main()` の `_readonly_forces_report` が
        #: `current_field("readonly")` (`boid task current --field readonly`) で引く値。
        #: 既定は real sweep 相当の `"false"` —— readonly に関心の無い既存テストを
        #: report 強制の対象にしない。
        self._readonly = readonly

    # -- 読み --
    def get_card(self, task_id):
        self.calls.append(("get_card", task_id))
        if task_id == "sweep-1" and getattr(self, "_sweep_project", None):
            # Executor は「自分の project」を sweep task から引く。対象 task と
            # 同じ view を返すと、別 project のテストが自分自身と一致してしまう。
            return {"task_id": task_id, "status": "executing", "project_id": self._sweep_project}
        return self._view

    def attrs_of(self, task_id):
        self.calls.append(("attrs_of", task_id))
        return self._attrs

    def resolve_project(self, name_or_id):
        self.calls.append(("resolve_project", name_or_id))
        return self._projects.get(name_or_id, name_or_id)

    def behaviors_of(self, project_id):
        self.calls.append(("behaviors_of", project_id))
        return self._behaviors.get(project_id)

    def current_field(self, field):
        self.calls.append(("current_field", field))
        return self._readonly if field == "readonly" else ""

    # -- 書き --
    def send_action(self, task_id, action_type, payload=None):
        self.calls.append(("send_action", task_id, action_type, payload))

    def resolve_or_capture(self, identity, *, title, description):
        self.calls.append(("resolve_or_capture", identity, title, description))
        return "new-task", self._created

    def update_description(self, task_id, description):
        self.calls.append(("update_description", task_id, description))

    def notify_progress(self, task_id, message):
        self.calls.append(("notify_progress", task_id, message))

    def notify(self, task_id, message):
        self.calls.append(("notify", task_id, message))

    def link_identity(self, identity, task_id):
        self.calls.append(("link_identity", identity, task_id))

    def ack_signals(self, ids):
        self.calls.append(("ack_signals", tuple(ids)))

    # -- テスト用の問い合わせ --
    def named(self, name: str) -> list[tuple]:
        return [c for c in self.calls if c[0] == name]

    def actions(self, action_type: str) -> list[tuple]:
        return [c for c in self.named("send_action") if c[2] == action_type]

    def wrote(self, name: str) -> bool:
        return bool(self.named(name))


def run(verb: str, cli: FakeCLI, *, report: bool = False, **fields) -> None:
    payload = {"signals": SIGNALS}
    payload.update(fields)
    Executor(cli, sweep_task_id="sweep-1", report=report).run(validate(verb, payload))


class RecordTest(unittest.TestCase):
    """S-11: どの verb でも、書き込みが成功したら処理記録を自動で付ける。

    **2026-08-29、PR-2やり直しv2: `attrs_set` への構造化書き込みをやめ、常に平文の
    `notify_progress` を sweep task 自身の timeline へ書く** (`domain/record.py` の
    `encode_attrs`/`ATTRS_KEY`/`PROGRESS_PREFIX` はどれも削除した)。task に書き込む
    verb (`done-signal` 等) でも、記録自体は task の attrs ではなく sweep task の
    timeline に残る —— 対象 task への書き込みはハンドラ本体 (`_do_*`) の役目。
    """

    def test_a_task_bound_verb_records_as_plain_progress(self):
        cli = FakeCLI()
        run("done-signal", cli, task_id="t1")
        (_, task_id, message), = cli.named("notify_progress")
        self.assertEqual(task_id, "sweep-1")
        self.assertIn("handled", message)
        self.assertIn(SIGNALS[0], message)

    def test_skip_records_onto_the_sweep_task_itself(self):
        """見送った候補には task が無い —— どのみち記録は常に sweep task 自身へ書く
        (§5.3 S-11 後半)。"""
        cli = FakeCLI()
        run("skip", cli, reason="自分の発言だけ")
        (_, task_id, message), = cli.named("notify_progress")
        self.assertEqual(task_id, "sweep-1")
        self.assertIn("skipped", message)
        self.assertFalse(cli.wrote("send_action"))

    def test_the_reason_rides_along_as_the_note(self):
        cli = FakeCLI()
        run("skip", cli, reason="自分の発言だけ")
        (_, _, message), = cli.named("notify_progress")
        self.assertIn("自分の発言だけ", message)

    def test_done_signal_only_records(self):
        cli = FakeCLI()
        run("done-signal", cli, task_id="t1")
        self.assertEqual(len(cli.named("notify_progress")), 1)
        self.assertFalse(cli.wrote("send_action"))
        self.assertFalse(cli.wrote("update_description"))


class AckTest(unittest.TestCase):
    """2026-08-28、PR-2 §6.1 決定事項 6: 「ack を打つ順序: sweep が判断を書いた直後に
    自分で ack する」。`Executor._record` が記録の書き込み成功直後に signal inbox も
    ack することを固定する。
    """

    def test_a_task_bound_verb_acks_its_signals_after_recording(self):
        cli = FakeCLI()
        run("done-signal", cli, task_id="t1")
        self.assertTrue(cli.wrote("notify_progress"))
        (_, ids), = cli.named("ack_signals")
        # ack は event_key ("boid:a1") ではなく envelope_id_of() で逆算した元の
        # envelope id ("a1") で打つ (2026-08-28 訂正: boid-pack の実物 id は prefix
        # 無しなので、jira と同じく namespace 付与の対象になった)。
        self.assertEqual(ids, ("a1",))
        # 記録 (notify_progress) の後に ack が来る —— 順序が S-6.1 決定事項 6 の要。
        order = [c[0] for c in cli.calls]
        self.assertLess(order.index("notify_progress"), order.index("ack_signals"))

    def test_a_skip_verb_also_acks(self):
        """task を持たない見送りも「判断を書いた」ことに変わりない。"""
        cli = FakeCLI()
        run("skip", cli, reason="自分の発言だけ")
        (_, ids), = cli.named("ack_signals")
        self.assertEqual(ids, ("a1",))

    def test_multiple_signals_are_all_acked(self):
        cli = FakeCLI()
        run("done-signal", cli, task_id="t1", signals=["boid:a1", "jira:KT-1:issue:2026-08-28T00:00:00Z"])
        (_, ids), = cli.named("ack_signals")
        self.assertEqual(set(ids), {"a1", "KT-1:issue:2026-08-28T00:00:00Z"})

    def test_dry_run_does_not_ack(self):
        """`--report` は書きを全部止める約束 —— ack も含む。"""
        cli = FakeCLI()
        run("done-signal", cli, report=True, task_id="t1")
        self.assertFalse(cli.wrote("ack_signals"))


class CrashSafetyTest(unittest.TestCase):
    """§6.1 決定事項 6 の crash 安全性: **記録の書き込み → ack** の順序が守られて
    いれば、記録の書き込みが例外で落ちたとき ack は 1 本も呼ばれない —— signal は
    pending のまま残り、crash した巡を次巡が取り逃さない (2026-08-29、PR-2やり直しv2
    で `_record` を平文 progress 一本化した際に新たに固定した)。

    `_record` は verb に関わらず同じ経路 (`notify_progress` → `inbox.ack`) を通る
    ので、代表的な verb (task に書く `capture`/`spec`、task を持たない `skip`、
    task へ何も書かない `done-signal`) で同じ性質を確認する。
    """

    class CrashingCLI(FakeCLI):
        """`notify_progress` (記録の書き込み) だけを落とす。"""

        def notify_progress(self, task_id, message):
            super().notify_progress(task_id, message)
            raise RuntimeError("sweep task crashed while recording")

    def test_done_signal_does_not_ack_if_recording_fails(self):
        cli = self.CrashingCLI()
        with self.assertRaises(RuntimeError):
            run("done-signal", cli, task_id="t1")
        self.assertFalse(cli.wrote("ack_signals"))

    def test_skip_does_not_ack_if_recording_fails(self):
        cli = self.CrashingCLI()
        with self.assertRaises(RuntimeError):
            run("skip", cli, reason="自分の発言だけ")
        self.assertFalse(cli.wrote("ack_signals"))

    def test_capture_does_not_ack_if_recording_fails(self):
        cli = self.CrashingCLI(created=True)
        with self.assertRaises(RuntimeError):
            run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="week")
        # capture 自体 (resolve_or_capture / attrs_set) は crash 前に完了している ——
        # ここで固定したいのは「その後の ack だけは行われない」こと。
        self.assertTrue(cli.wrote("resolve_or_capture"), "前提: capture 本体は実行された")
        self.assertFalse(cli.wrote("ack_signals"))

    def test_spec_does_not_ack_if_recording_fails(self):
        cli = self.CrashingCLI()
        with self.assertRaises(RuntimeError):
            run(
                "spec",
                cli,
                task_id="t1",
                work="ROOKPF-307 の修正",
                origin="jira:ROOKPF-307:comment:2026-08-22",
                title="DLQ codec を直す",
                project="rook-tools",
                behavior="implement",
                description="背景",
            )
        self.assertTrue(cli.actions("child_specced"), "前提: spec 本体は実行された")
        self.assertFalse(cli.wrote("ack_signals"))


class DeliveryTest(unittest.TestCase):
    """S-7: **変わったときだけ**通知する。判断が 0 件の巡は無言。

    今のハンドラは変化が無ければ `notify` を空にして返すので、`changed` の判定は
    実質冗長になっている。それでも残しているのは、**意図を型に載せる**ため ——
    通知を返すハンドラが増えたとき、「変わっていないのに人へ飛ぶ」事故をここで止める。
    """

    def deliver(self, **fields) -> FakeCLI:
        cli = FakeCLI()
        Executor(cli, sweep_task_id="sweep-1")._deliver(Result(**fields))
        return cli

    def test_a_change_with_a_message_is_delivered(self):
        self.assertTrue(self.deliver(task_id="t1", changed=True, notify="変わった").wrote("notify"))

    def test_no_change_means_no_notification(self):
        self.assertFalse(self.deliver(task_id="t1", changed=False, notify="変わってない").wrote("notify"))

    def test_no_message_means_no_notification(self):
        self.assertFalse(self.deliver(task_id="t1", changed=True).wrote("notify"))

    def test_no_task_means_no_notification(self):
        """見送り (task を持たない) は人へ飛ばさない —— それは記録に残るだけでよい。"""
        self.assertFalse(self.deliver(changed=True, notify="見送った").wrote("notify"))


class SummaryTest(unittest.TestCase):
    BODY = "## いま何が起きているか\n\nDLQ に溜まっている。\n"

    def test_writes_the_description_and_the_one_line_badge(self):
        cli = FakeCLI()
        run("summary", cli, task_id="t1", body=self.BODY)
        (_, task_id, description), = cli.named("update_description")
        self.assertEqual(task_id, "t1")
        self.assertIn("DLQ に溜まっている", description)
        # 行バッジは body から導出する (subagent に 1 行を書かせない)
        attrs = [c for c in cli.actions("attrs_set") if "summary" in (c[3] or {})]
        self.assertEqual(attrs[0][3]["summary"], "DLQ に溜まっている。")

    def test_keeps_the_human_part(self):
        cli = FakeCLI(view={"task_id": "t1", "status": "parked", "description": "人のメモ\n"})
        run("summary", cli, task_id="t1", body=self.BODY)
        (_, _, description), = cli.named("update_description")
        self.assertTrue(description.startswith("人のメモ"))

    def test_notifies_when_it_changed(self):
        """S-7: サマリーが変わった task についてだけ通知する。"""
        cli = FakeCLI()
        run("summary", cli, task_id="t1", body=self.BODY)
        self.assertTrue(cli.wrote("notify"))

    def test_unchanged_summary_writes_nothing_and_stays_quiet(self):
        """S-8 の差分ガード。**判断が 0 件の巡は無言** (S-7)。"""
        cli = FakeCLI()
        run("summary", cli, task_id="t1", body=self.BODY)
        settled = [c for c in cli.named("update_description")][0][2]

        again = FakeCLI(view={"task_id": "t1", "status": "parked", "description": settled})
        run("summary", again, task_id="t1", body=self.BODY)
        self.assertFalse(again.wrote("update_description"))
        self.assertFalse(again.wrote("notify"))

    def test_the_record_is_written_even_when_nothing_changed(self):
        """**処理記録は差分ガードの対象外。毎回書く** (S-8)。"""
        cli = FakeCLI()
        run("summary", cli, task_id="t1", body=self.BODY)
        settled = cli.named("update_description")[0][2]

        again = FakeCLI(view={"task_id": "t1", "status": "parked", "description": settled})
        run("summary", again, task_id="t1", body=self.BODY)
        self.assertTrue(again.wrote("notify_progress"))


class SpecTest(unittest.TestCase):
    BASE = dict(
        task_id="t1",
        work="ROOKPF-307 の修正",
        origin="jira:ROOKPF-307:comment:2026-08-22",
        title="DLQ codec を直す",
        project="rook-tools",
        behavior="implement",
        description="背景",
    )

    def test_an_omitted_instruction_is_not_sent(self):
        """**キーごと落とす。** 空文字を送っても boid 側の per-field merge は空を
        「未指定」として default から継承するが、spec に空の instruction が residue として
        残ると、次に読んだ subagent が「ここは埋める欄だ」と解釈する。書かなかったことが
        読み返しても分かる形にする。"""
        cli = FakeCLI()
        run("spec", cli, **self.BASE)
        (_, _, _, payload), = cli.actions("child_specced")
        self.assertNotIn("instruction", payload)

    def test_an_explicit_instruction_is_sent(self):
        cli = FakeCLI()
        run("spec", cli, **dict(self.BASE, instruction="既定と違う手順"))
        (_, _, _, payload), = cli.actions("child_specced")
        self.assertEqual(payload["instruction"], "既定と違う手順")

    def test_sends_child_added_then_child_specced(self):
        """**2 本を送り切る。** `child_specced` は update-only なので、`child_added` が
        先に無いと 409 になる (`domain/childid.py` の順序契約)。"""
        cli = FakeCLI()
        run("spec", cli, **self.BASE)
        types = [c[2] for c in cli.named("send_action")]
        self.assertLess(types.index("child_added"), types.index("child_specced"))

    def test_resolves_the_project_to_a_uuid(self):
        """名前のまま送ると dispatch のときに初めて落ちる (2026-08-21 に本番で踏んだ形)。"""
        cli = FakeCLI()
        run("spec", cli, **self.BASE)
        (_, _, _, payload), = cli.actions("child_specced")
        self.assertEqual(payload["project"], "p-uuid-1")

    def test_the_child_id_carries_the_generation(self):
        cli = FakeCLI()
        run("spec", cli, **self.BASE)
        (_, _, _, added), = cli.actions("child_added")
        self.assertIn("jira:ROOKPF-307:comment:2026-08-22", added["id"])

    def test_the_same_child_is_not_written_twice(self):
        """差分ガード。既に同じ id の子が居るなら noop —— boid 側も冪等だが、
        **通知が毎巡飛ぶのを止める**のはこちらの役目。"""
        cli = FakeCLI()
        run("spec", cli, **self.BASE)
        child_id = cli.actions("child_added")[0][3]["id"]

        again = FakeCLI(view={"task_id": "t1", "status": "parked", "description": "",
                              "detail": {"children": [{"id": child_id}]}})
        run("spec", again, **self.BASE)
        self.assertFalse(again.actions("child_added"))
        self.assertFalse(again.wrote("notify"))

    def test_notifies_when_a_new_spec_appears(self):
        cli = FakeCLI()
        run("spec", cli, **self.BASE)
        self.assertTrue(cli.wrote("notify"))

    def test_a_behavior_the_target_project_lacks_is_refused(self):
        """**project と同じ理由で、送る前に弾く** (`resolve_project` の docstring)。

        `child_specced` 自体は受け取られ、落ちるのは Go を押したあとの `Dispatch`。
        しかもそのエラーは boid 側で意図的にログ止まり (`workflow_action.go`
        「a task stuck in ready is the intended, visible failure mode」) なので、
        カードは `ready` のまま何も走らない。そのとき task には specced な子が
        残っていて `spec_needed` が False —— **二度と正しい spec が書かれない。**

        実際に踏んだ形: `respond` は khi-task-collector にしか無いのに、subagent が
        仕事の対象 repo (rook-server 等) を project に選んだ。2026-08-24 時点で
        triaged 9 枚のうち 4 枚がこの状態だった。
        """
        cli = FakeCLI(behaviors={"p-uuid-1": ("implement", "plan", "research")})
        with self.assertRaises(CommandError) as caught:
            run("spec", cli, **{**self.BASE, "behavior": "respond"})
        self.assertFalse(cli.wrote("send_action"), "弾くなら 1 本も送らない")
        self.assertIn("respond", str(caught.exception))

    def test_the_refusal_lists_the_behaviors_that_project_does_have(self):
        """語彙のあるフィールドは、欠けたことを告げるときも語彙を並べる
        (`_VOCABULARIES` と同じ理由 —— 名前だけ言われても subagent は言い直せない)。"""
        cli = FakeCLI(behaviors={"p-uuid-1": ("implement", "plan", "research")})
        with self.assertRaises(CommandError) as caught:
            run("spec", cli, **{**self.BASE, "behavior": "respond"})
        message = str(caught.exception)
        for available in ("implement", "plan", "research"):
            self.assertIn(available, message)

    def test_a_behavior_the_project_has_passes(self):
        cli = FakeCLI(behaviors={"p-uuid-1": ("implement", "plan", "research")})
        run("spec", cli, **self.BASE)
        self.assertTrue(cli.actions("child_specced"))

    def test_an_unreadable_behavior_list_does_not_block_the_write(self):
        """**照合できないことを理由に書き込みを止めない。** 目的は「dispatch できない子を
        作らない」ことであって、`boid project behaviors` の可用性に spec を人質に
        取ることではない (S-11「落ちた source の栞は進めない」と同じで、こちらは
        止める方が害が大きい —— 判断そのものが失われる)。"""
        cli = FakeCLI(behaviors={})  # その project の behaviors を返せない
        run("spec", cli, **self.BASE)
        self.assertTrue(cli.actions("child_specced"))


class BehaviorAvailabilityTest(unittest.TestCase):
    """`_refuse_absent_behavior` の 3 分岐。**「読めなかった」と「1 つも無い」を同じ側に
    倒さない** —— 2026-08-29 のレビューで mutation (`not available` → `available is
    None`) が生き残った箇所。"""

    BASE = dict(SpecTest.BASE)

    def test_a_declared_behavior_passes(self):
        cli = FakeCLI(behaviors={"p-uuid-1": ("implement", "respond")})
        run("spec", cli, **self.BASE)
        self.assertTrue(cli.actions("child_specced"))

    def test_an_undeclared_behavior_is_refused_and_names_the_real_values(self):
        cli = FakeCLI(behaviors={"p-uuid-1": ("respond", "review")})
        with self.assertRaises(CommandError) as caught:
            run("spec", cli, **self.BASE)
        message = str(caught.exception)
        self.assertIn("respond", message)
        self.assertIn("review", message)
        self.assertFalse(cli.actions("child_specced"))

    def test_a_project_declaring_no_behaviors_is_refused(self):
        """**空は「読めなかった」ではない。** behavior を 1 つも持たない project へは
        何も dispatch できないので、通すと人が Go を押した瞬間に落ち、card は parked の
        まま・子は specced のままで詰まる (この関数が防ぎたい当のもの)。"""
        cli = FakeCLI(behaviors={"p-uuid-1": ()})
        with self.assertRaises(CommandError) as caught:
            run("spec", cli, **self.BASE)
        self.assertIn("task_behaviors", str(caught.exception))
        self.assertFalse(cli.actions("child_specced"))

    def test_an_unreadable_list_passes_but_says_so(self):
        """一時的な不調で巡の判断そのものを失わせない。**ただし黙って通さない** ——
        後から人が「なぜ実在しない behavior の子ができたのか」を追う手がかりが要る。"""
        cli = FakeCLI()  # 既定は「照合できない」(None)
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            run("spec", cli, **self.BASE)
        self.assertTrue(cli.actions("child_specced"))
        self.assertIn("読めなかった", err.getvalue())


class UrgencyTest(unittest.TestCase):
    """urgency は並び順だけの属性になった (設計 `docs/plans/suggestion-as-state-transition.md`
    §3.6)。card の可視性を左右する時代の `triage` 送信・`someday` の captured カーブアウトは
    どちらも消えている —— status を問わず同じ書き方で足りる。"""

    def test_writes_urgency_and_kind(self):
        cli = FakeCLI(view={"task_id": "t1", "status": "parked", "description": ""})
        run("urgency", cli, task_id="t1", urgency="week")
        payload = cli.actions("attrs_set")[0][3]
        self.assertEqual(payload["urgency"], "week")
        self.assertEqual(payload["kind"], "signal")

    def test_someday_is_not_special_anymore(self):
        """v1 は someday だけ status を巻き戻さない特別扱いだった。v2 は urgency が
        可視性を持たないので、他の値と同じ 1 本の attrs_set で済む。"""
        cli = FakeCLI(view={"task_id": "t1", "status": "parked", "description": ""})
        run("urgency", cli, task_id="t1", urgency="someday")
        self.assertEqual(cli.actions("attrs_set")[0][3]["urgency"], "someday")

    def test_no_status_lookup_is_needed_to_decide_what_to_write(self):
        """v1 は「triaged から下げていいか」を判断するため `_do_urgency` 自身が
        `get_card()` (旧 `triage()`) を追加で読んでいた。v2 は urgency が可視性を
        左右しないので、この追加読みが要らない —— `run()` が誰に対しても行う
        `_refuse_terminal` の読み (対象 task 分 + `_own_project` の sweep task 分、
        計 2 回) 以外は増えないことを、他の task-bound verb と同じ回数になることで見る。

        比較対象には `link` を使う (2026-08-25 修正) —— `link`/`urgency` はどちらも
        status に制約が無い非遷移 verb で、ハンドラ自身が追加で `get_card()` を読まない。
        遷移 verb (`working` 等) は特定の status からしか適用できない (HIGH 1) ので、
        この比較の baseline には使えない。"""
        cli = FakeCLI(view={"task_id": "t1", "status": "working", "description": ""})
        run("urgency", cli, task_id="t1", urgency="now")
        baseline = FakeCLI(view={"task_id": "t1", "status": "working", "description": ""})
        run("link", baseline, task_id="t1", identity="jira:X-9")
        self.assertEqual(len(cli.named("get_card")), len(baseline.named("get_card")))

    def test_a_working_task_can_be_raised_freely(self):
        cli = FakeCLI(view={"task_id": "t1", "status": "working", "description": ""})
        run("urgency", cli, task_id="t1", urgency="now")
        self.assertEqual(cli.actions("attrs_set")[0][3]["urgency"], "now")


class ParkTest(unittest.TestCase):
    """park は suggestion 化された (設計 §3.7: 「park を suggest (現行は即時実行 →
    提案型に揃える)」)。直接 action を打つのではなく `attrs_set{suggestion:...}` を書き、
    人の accept が実際の working→parked 遷移を適用する。wake 条件必須 (S-13) は維持。

    **park の唯一の適用可能 FromStatus は working** (`internal/orchestrator/
    machine_card.go`)。ここのテストは全部 `status=working` の card を明示的に使う
    (PR-K レビュー MEDIUM 3 — bare `FakeCLI()` の既定 status に暗黙で乗らない)。
    """

    WORKING = {"task_id": "t1", "status": "working", "description": ""}

    def test_writes_a_park_suggestion_with_the_wake_condition_in_params(self):
        cli = FakeCLI(view=self.WORKING)
        run("park", cli, task_id="t1", wake_task_id="child-9", reason="子待ち")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "park")
        self.assertEqual(payload["suggestion"]["reason"], "子待ち")
        self.assertEqual(payload["suggestion"]["params"], {"wake_task_id": "child-9"})

    def test_wake_at_lands_in_params_too(self):
        cli = FakeCLI(view=self.WORKING)
        run("park", cli, task_id="t1", wake_at="2026-09-01T09:00:00+09:00", reason="来週見直す")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["params"], {"wake_at": "2026-09-01T09:00:00+09:00"})

    def test_no_direct_park_action_is_sent(self):
        """v1 は `park` action を直接送っていた。v2 は human-only の直接遷移になった
        (boid PR #987) ので、khi は `attrs_set` の suggestion 経由でしか触れない。"""
        cli = FakeCLI(view=self.WORKING)
        run("park", cli, task_id="t1", wake_task_id="child-9", reason="子待ち")
        self.assertFalse(cli.actions("park"))

    def test_reason_is_required(self):
        """**2026-08-25、PR-K レビュー LOW 8**: park も他の suggestion verb と揃えて
        reason を必須にした (以前は任意で、accept 画面に理由が出ない park が作れた)。"""
        with self.assertRaises(CommandError):
            validate("park", {"signals": SIGNALS, "task_id": "t1", "wake_task_id": "child-9"})

    def test_no_captured_triage_workaround_remains(self):
        """v1 は captured から park すると先に `triage` action を送っていた
        (新設計では card は必ず parked で誕生するので、この経路自体が要らなくなった)。"""
        cli = FakeCLI(view=self.WORKING)
        run("park", cli, task_id="t1", wake_task_id="child-9", reason="子待ち")
        self.assertFalse(cli.actions("triage"))


class SuggestionVerbsTest(unittest.TestCase):
    """working (旧 manual) / go / done / reopen — いずれも `_write_suggestion` の薄い
    ラッパで、verb だけが違う (設計 §3.7)。

    **各テストは適用可能な FromStatus を明示的に指定する** (PR-K レビュー MEDIUM 3 ——
    bare `FakeCLI()` の既定 status に暗黙で乗ると、HIGH 1 の status-aware guard が
    効いているかどうかをこのクラス自身が検証できなくなる)。
    """

    def view(self, status: str) -> dict:
        return {"task_id": "t1", "status": status, "description": ""}

    def test_start_is_manuals_replacement(self):
        """start は parked からのみ提案できる。"""
        cli = FakeCLI(view=self.view("parked"))
        run("start", cli, task_id="t1", reason="人手が要る")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"], {"verb": "start", "reason": "人手が要る"})

    def test_go_suggests_the_go_verb(self):
        """go も parked からのみ提案できる。"""
        cli = FakeCLI(view=self.view("parked"))
        run("go", cli, task_id="t1", reason="子の spec が揃った")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"], {"verb": "go", "reason": "子の spec が揃った"})

    def test_complete_suggests_the_complete_verb(self):
        """complete は working からのみ提案できる。"""
        cli = FakeCLI(view=self.view("working"))
        run("complete", cli, task_id="t1", reason="全子 closed、source も閉じた")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"], {"verb": "complete", "reason": "全子 closed、source も閉じた"})

    def test_reopen_suggests_the_reopen_verb_from_done(self):
        """reopen は done/dropped からのみ提案できる。"""
        cli = FakeCLI(view=self.view("done"))
        run("reopen", cli, task_id="t1", reason="source が再オープンされた")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"], {"verb": "reopen", "reason": "source が再オープンされた"})

    def test_reopen_suggests_the_reopen_verb_from_dropped(self):
        cli = FakeCLI(view=self.view("dropped"))
        run("reopen", cli, task_id="t1", reason="drop 後に続きが来た")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"], {"verb": "reopen", "reason": "drop 後に続きが来た"})


class TransitionVerbStatusGuardTest(unittest.TestCase):
    """PR-K レビュー HIGH 1 の受け入れ条件: status × verb の適用可否を網羅的に固定する。

    **正解はここに直接書き下す** (`internal/orchestrator/machine_card.go` の遷移ルール表
    そのもの) —— write.py の `_TRANSITION_VERB_STATUSES` を import して比較すると、
    実装がその表ごと間違っていた場合にテストが実装と一緒に間違う (トートロジー)。

        start / drop        : parked からのみ
        go                  : parked / working から (working 中の Go 自己遷移)
        park                : working からのみ
        complete            : parked / working から (2026-08-25、8 本目の辺)
        reopen              : done / dropped からのみ

    適用可能な組は **書き込まれ、拒否されない**こと。適用不能な組は
    **書き込み前に (accept 時の 409 を待たず) 拒否され、`send_action` が 1 本も
    呼ばれない**ことの両方を見る。"""

    #: verb -> その verb を唯一適用できる status (go / complete / reopen は複数あるので別扱い)。
    SINGLE_STATUS_VERBS = {
        "start": "parked",
        "drop": "parked",
        "park": "working",
    }
    #: go は parked と working の両方から打てる。working からも打てるのは、用意できた
    #: 次の specced 子を working 中に走らせる自己遷移が追加されたため (boid
    #: `internal/orchestrator/machine_card.go` の 9 本目の辺)。
    GO_VALID_STATUSES = ("parked", "working")
    #: complete は parked と working の両方から打てる。parked から打てるのは「ここでは
    #: 誰も working していない card」——外で片付いていた / 重複と判明した——を 1 手で
    #: 畳むため (boid `internal/orchestrator/machine_card.go` の 8 本目の辺)。
    DONE_VALID_STATUSES = ("parked", "working")
    REOPEN_VALID_STATUSES = ("done", "dropped")
    ALL_CARD_STATUSES = ("parked", "working", "done", "dropped")

    def _attempt(self, verb: str, status: str) -> tuple[bool, str, "FakeCLI"]:
        cli = FakeCLI(view={"task_id": "t1", "status": status, "description": ""})
        kwargs: dict = {"reason": "r"}
        if verb == "park":
            kwargs["wake_task_id"] = "child-9"
        try:
            run(verb, cli, task_id="t1", **kwargs)
        except CommandError as exc:
            return False, str(exc), cli
        return True, "", cli

    def test_single_status_transition_verbs_are_gated_to_their_real_fromstatus(self):
        for verb, valid_status in self.SINGLE_STATUS_VERBS.items():
            for status in self.ALL_CARD_STATUSES:
                accepted, message, cli = self._attempt(verb, status)
                if status == valid_status:
                    self.assertTrue(accepted, f"{verb} from {status} should be accepted: {message}")
                    self.assertTrue(
                        cli.actions("attrs_set"), f"{verb} from {status} did not write a suggestion"
                    )
                else:
                    self.assertFalse(accepted, f"{verb} from {status} should have been refused")
                    self.assertFalse(
                        cli.wrote("send_action"), f"{verb} from {status} wrote despite refusal"
                    )
                    self.assertIn("skip", message, f"{verb} from {status}: message doesn't point to skip")

    def test_go_is_gated_to_parked_or_working(self):
        """go は parked / working の両方から。**dropped と done からは拒む** ——
        終端 card に次の一手を提案しても走らせようがない。"""
        for status in self.ALL_CARD_STATUSES:
            accepted, message, cli = self._attempt("go", status)
            if status in self.GO_VALID_STATUSES:
                self.assertTrue(accepted, f"go from {status} should be accepted: {message}")
                self.assertTrue(cli.actions("attrs_set"), f"go from {status} did not write")
            else:
                self.assertFalse(accepted, f"go from {status} should have been refused")
                self.assertFalse(cli.wrote("send_action"), f"go from {status} wrote despite refusal")

    def test_complete_is_gated_to_parked_or_working(self):
        """complete は parked / working の両方から。**dropped と done からは拒む** ——
        「やらないと決めた card」を終わったことにはできないし、既に done の card に
        complete は打てない (boid の rule 表に無い辺なので accept で 409 になる)。"""
        for status in self.ALL_CARD_STATUSES:
            accepted, message, cli = self._attempt("complete", status)
            if status in self.DONE_VALID_STATUSES:
                self.assertTrue(accepted, f"complete from {status} should be accepted: {message}")
                self.assertTrue(cli.actions("attrs_set"), f"complete from {status} did not write")
            else:
                self.assertFalse(accepted, f"complete from {status} should have been refused")
                self.assertFalse(cli.wrote("send_action"), f"complete from {status} wrote despite refusal")

    def test_reopen_is_gated_to_done_or_dropped(self):
        for status in self.ALL_CARD_STATUSES:
            accepted, message, cli = self._attempt("reopen", status)
            if status in self.REOPEN_VALID_STATUSES:
                self.assertTrue(accepted, f"reopen from {status} should be accepted: {message}")
                self.assertTrue(cli.actions("attrs_set"), f"reopen from {status} did not write")
            else:
                self.assertFalse(accepted, f"reopen from {status} should have been refused")
                self.assertFalse(cli.wrote("send_action"), f"reopen from {status} wrote despite refusal")

    def test_the_refusal_message_names_what_can_be_proposed_instead(self):
        """エラーメッセージは LLM へのフィードバック (module docstring) —— 拒否された
        とき、その status から何を提案できるかを名指しすること。

        題材は `reopen` from `parked`。2026-08-25 までは `complete` from `parked` を
        使っていたが、8 本目の辺でその組が**適用可能**になったので差し替えた。"""
        _accepted, message, _cli = self._attempt("reopen", "parked")
        self.assertIn("go", message)
        self.assertIn("start", message)
        self.assertIn("drop", message)
        self.assertIn("complete", message)

    def test_an_unknown_status_passes_through_instead_of_being_refused(self):
        """**PR-K レビュー finding D**: `_refuse_terminal` の docstring は「status を
        読めないときは素通しする」と明記している (project 不明時・task 消失時と同じ
        姿勢)。`triage()` 自体は成功したが応答に `status` が無い (空文字になる) ケースで、
        遷移 verb のチェックだけがこの姿勢を暗黙に反転させて拒否していたのを訂正した。
        実 CLI では常に status が返るので通常は起きないが、documented な契約として
        固定する。"""
        cli = FakeCLI(view={"task_id": "t1", "description": ""})  # status キーが無い
        run("go", cli, task_id="t1", reason="r")
        self.assertTrue(cli.actions("attrs_set"), "status 不明なのに拒否されている")


class CaptureTest(unittest.TestCase):
    def test_creates_and_initialises(self):
        cli = FakeCLI(created=True)
        run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="week")
        (_, identity, title, description), = cli.named("resolve_or_capture")
        self.assertEqual((identity, title, description), ("jira:X-1", "題", "本文"))
        self.assertEqual(cli.actions("attrs_set")[0][3]["identity"], "jira:X-1")

    def test_an_existing_task_is_not_re_initialised(self):
        """`resolve-or-capture` は既存に当たっても成功で返る。毎回初期化すると
        人が触った値を上書きする。"""
        cli = FakeCLI(created=False)
        run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="week")
        self.assertFalse([c for c in cli.actions("attrs_set") if "identity" in (c[3] or {})])

    def test_the_record_is_written_to_the_sweep_task_timeline(self):
        """**2026-08-29、PR-2やり直しv2**: 記録は task の attrs ではなく、常に
        sweep task 自身の timeline (`notify_progress`) へ書く。"""
        cli = FakeCLI(created=True)
        run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="week")
        (_, task_id, _), = cli.named("notify_progress")
        self.assertEqual(task_id, "sweep-1")

    def test_the_new_task_writes_urgency_in_the_same_breath(self):
        """S-11: 起票と urgency を分けない。分けた結果、打たれなかった 5 件が captured の
        まま queue に出なかった (2026-08-23 本番)。**v2 では urgency が可視性を持たない**
        (§3.6) ので、以前あった `triage` action の送信はもう無い — card は新規作成時点で
        既に parked (`ResolveOrCapture` の初期 status、boid PR #987)。"""
        cli = FakeCLI(created=True)
        run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="week")
        payload, = [c[3] for c in cli.actions("attrs_set") if "urgency" in (c[3] or {})]
        self.assertEqual((payload["urgency"], payload["kind"]), ("week", "signal"))
        self.assertFalse(cli.actions("triage"))

    def test_someday_is_written_the_same_way_as_any_other_urgency(self):
        """v1 は someday だけ captured に留める特別扱いだった。v2 は urgency が可視性を
        持たないので、その分岐自体が消えている。"""
        cli = FakeCLI(created=True)
        run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="someday")
        payload, = [c[3] for c in cli.actions("attrs_set") if "urgency" in (c[3] or {})]
        self.assertEqual(payload["urgency"], "someday")
        self.assertFalse(cli.actions("triage"))

    def test_an_existing_task_keeps_the_urgency_it_has(self):
        """合流先の urgency は人が触っているかもしれない —— identity の初期化を
        `created` で守っているのと同じ理由で、上書きしない。変えたいなら `urgency` verb。"""
        cli = FakeCLI(created=False)
        run("capture", cli, identity="jira:X-1", title="題", body="本文", urgency="now")
        self.assertFalse([c for c in cli.actions("attrs_set") if "urgency" in (c[3] or {})])
        self.assertFalse(cli.actions("triage"))


class SimpleVerbTest(unittest.TestCase):
    def test_link(self):
        cli = FakeCLI()
        run("link", cli, task_id="t1", identity="slack:C1:1.0")
        self.assertEqual(cli.named("link_identity"), [("link_identity", "slack:C1:1.0", "t1")])

    def test_drop(self):
        cli = FakeCLI()
        run("drop", cli, task_id="t1", reason="やらない")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "drop")

    def test_observed_writes_when_the_value_changed(self):
        cli = FakeCLI(attrs={"observed": {"source_closed": False}})
        run("observed", cli, task_id="t1", source_closed=True)
        payload = [c[3] for c in cli.actions("attrs_set") if "observed" in (c[3] or {})][0]
        self.assertIs(payload["observed"]["source_closed"], True)

    def test_observed_is_silent_when_unchanged(self):
        """§5.1 の篩い 4 が読む値。**毎巡書くと 2026-08-20 の事故が再発する。**"""
        cli = FakeCLI(attrs={"observed": {"source_closed": True}})
        run("observed", cli, task_id="t1", source_closed=True)
        self.assertFalse([c for c in cli.actions("attrs_set") if "observed" in (c[3] or {})])


class ReportModeTest(unittest.TestCase):
    """§10 step 4 の dry-run。**`readonly: true` では止められない** (boid の op は
    readonly のゲート対象外) ので、書き込みが記録 CLI を必ず通ることが前提。
    """

    def test_reads_are_allowed_but_writes_are_not(self):
        cli = FakeCLI()
        run("summary", cli, report=True, task_id="t1", body="本文")
        self.assertTrue(cli.wrote("get_card"))
        self.assertFalse(cli.wrote("update_description"))
        self.assertFalse(cli.wrote("send_action"))
        self.assertFalse(cli.wrote("notify"))

    def test_the_intended_writes_are_reported(self):
        cli = FakeCLI()
        executor = Executor(cli, sweep_task_id="sweep-1", report=True)
        executor.run(validate("summary", {"signals": SIGNALS, "task_id": "t1", "body": "本文"}))
        joined = "\n".join(executor.reported)
        self.assertIn("update_description", joined)
        self.assertIn("attrs_set", joined)

    def test_capture_still_yields_a_task_id_so_the_rest_can_be_reported(self):
        cli = FakeCLI()
        executor = Executor(cli, sweep_task_id="sweep-1", report=True)
        executor.run(validate("capture", {"signals": SIGNALS, "identity": "jira:X-1",
                                          "title": "題", "body": "本文", "urgency": "week"}))
        self.assertFalse(cli.wrote("resolve_or_capture"))
        self.assertIn("resolve_or_capture", "\n".join(executor.reported))


if __name__ == "__main__":
    unittest.main()


class DryRunAllowlistTest(unittest.TestCase):
    """`_CLI_WRITES` は**許可リスト**なので、載せ忘れは「黙って本物を書く」側に倒れる。

    `--report` の約束は「何も残さない」で、attempts を進めるのも立派な副作用
    (5 回で signal が dead に落ちる)。write からまだ呼ばれない口も含めて、
    `BoidCLI` の書き込みメソッドが全部止まることを固定する。"""

    WRITES = ("send_action", "update_description", "notify_progress", "notify",
              "link_identity", "resolve_or_capture", "ack_signals", "claim_signals")

    def test_every_boid_cli_write_is_intercepted(self):
        from boidmeta.write import _CLI_WRITES
        for name in self.WRITES:
            with self.subTest(method=name):
                self.assertIn(name, _CLI_WRITES)

    def test_the_allowlist_names_only_real_methods(self):
        """架空のメソッド名を並べても止まらない —— 綴りが実物とずれた瞬間に
        そのメソッドは素通しになる。"""
        from boidmeta.boid_store import BoidCLI
        from boidmeta.write import _CLI_WRITES
        for name in _CLI_WRITES:
            with self.subTest(method=name):
                self.assertTrue(hasattr(BoidCLI, name), f"BoidCLI に {name} が無い")


class DropChildTest(unittest.TestCase):
    """要らない子を取り下げる。**訂正の手段が無いと重複になる。**

    2026-08-23 の評価で、前の巡が project を間違えた子を立て、次の巡がそれを正しく
    見抜いたのに直せず、正しい子を別 id で作り直すしかなかった。閉じる手段があれば
    「壊れた方を閉じて、正しい方を立てる」で済む。
    """

    BASE = dict(task_id="t1", child_id="ch:x:jira:KT-1", reason="project が khi 自身を指していた")

    def test_sends_child_dropped_with_the_reason(self):
        cli = FakeCLI()
        run("drop-child", cli, **self.BASE)
        (_, task_id, _, payload), = cli.actions("child_dropped")
        self.assertEqual(task_id, "t1")
        self.assertEqual(payload, {"id": "ch:x:jira:KT-1", "reason": "project が khi 自身を指していた"})

    def test_records_the_signals_as_plain_progress(self):
        """S-11: どの verb でも書き込みが成功したら処理記録が付く (sweep task の
        timeline へ平文で)。"""
        cli = FakeCLI()
        run("drop-child", cli, **self.BASE)
        (_, _, message), = cli.named("notify_progress")
        self.assertIn(SIGNALS[0], message)

    def test_the_reason_is_required(self):
        """**理由の無い取り下げは記録にならない。** なぜ消えたかが読めないと、
        人は「消していいものだったのか」を確かめる手段を失う。"""
        with self.assertRaises(Exception):
            validate("drop-child", {"signals": SIGNALS, "task_id": "t1", "child_id": "ch:x"})

    def test_report_mode_writes_nothing(self):
        """dry-run (--report) は契約だけ返す —— 探索が本物の子を触らないため。"""
        cli = FakeCLI()
        run("drop-child", cli, report=True, **self.BASE)
        self.assertFalse(cli.actions("child_dropped"))


class ExternallyResolvedParkedCardScenarioTest(unittest.TestCase):
    """「子を立てていない parked の card で `source_closed=true` と分かったとき」と
    「重複と判明した parked の card」—— どちらも**ここでは誰も working していない**
    件の畳み方。

    **2026-08-25 に経路が変わった。** 元は PR-K レビュー finding A の回帰テストで、
    `start` → 次の巡で `complete` の 2 手を固定していた (boid の card 機械に
    `parked → done` の辺が無かったため)。その回り道自体が「1 手で済ませたい」判断を
    identity 解放を伴う `drop` へ誘っており、実際に card ad8c6808 が drop されて
    次の巡で cba7c559 として再 capture された。boid 側に 8 本目の辺
    `done: parked→done` を足して回り道を無くしたので、ここも 1 手を固定する。

    `drop` を選ばないことは変わらない —— drop は identity を全解放するので再燃を
    `reopen` で拾えなくなる (`.claude/skills/khi-sweep/SKILL.md` の「done の判断基準」)。
    """

    def view(self, status: str) -> dict:
        return {"task_id": "t1", "status": status, "description": ""}

    def test_complete_is_suggested_in_one_step_from_parked(self):
        """1 手で `complete` を提案できる (`internal/orchestrator/machine_card.go` の
        8 本目の辺)。中間の `start` を挟まないので、誰も手を動かしていない card に
        ついて「手を動かしている」と台帳に書かずに済む。"""
        cli = FakeCLI(view=self.view("parked"))
        run("complete", cli, task_id="t1", reason="source が外で閉じた")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "complete")

    def test_start_is_still_available_when_work_actually_starts_here(self):
        """`start` が消えたわけではない —— **ここで実際に手を動かす**と決めたときの
        1 手目としては今までどおり正しい。8 本目の辺が置き換えたのは「畳むためだけに
        start を経由する」用法の方。"""
        cli = FakeCLI(view=self.view("parked"))
        run("start", cli, task_id="t1", reason="自分で対応するので working へ")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "start")

    def test_complete_is_still_reachable_from_working(self):
        """working の card を閉じる従来の経路は無傷 (辺を足しただけで消していない)。"""
        cli = FakeCLI(view=self.view("working"))
        run("complete", cli, task_id="t1", reason="source が外で閉じた、全子 closed")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "complete")

    def test_drop_is_still_the_wrong_move_and_stays_available_only_for_real_drops(self):
        """`drop` は「やらないと決めた」ときのための verb のまま —— parked から機械的に
        valid なので塞げない (塞ぐと正当な drop も打てなくなる)。畳む用途で選ばせない
        のは引き続き skill 文面の仕事で、8 本目の辺はその**誘因**を消しただけ。"""
        cli = FakeCLI(view=self.view("parked"))
        run("drop", cli, task_id="t1", reason="やらないと決めた")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "drop")


class TerminalTaskGuardTest(unittest.TestCase):
    """書けない相手には書かない。**正しい verb を名指しして拒む。**

    2026-08-23 の評価で、終端 task 宛ての `link`/`observed`/`summary`/`done-signal` が
    全部拒否され、subagent は生のエラー (`no transition for action "attrs_set" from
    status "done"`) しか読めずに諦めた。次の巡はたまたま `skip` を選んで通った ——
    **同じ状況で判断が割れる**ので、機構が選択肢を絞る。

    **書き先を変えて逃がさない。** 更新していない task に `handled` (= この task を
    更新した) の記録を残すと、履歴を読んだ人が誤解し、差分ガードの材料も狂う。
    outcome は判断そのものであって、書き込みが通る方に倒すものではない。
    """

    OWN = "proj-khi"

    def view(self, *, status="working", project=None):
        return {"task_id": "t1", "status": status, "description": "",
                "project_id": project or self.OWN, "detail": {}}

    def cli_for(self, **kw):
        cli = FakeCLI(view=self.view(**kw))
        cli._sweep_project = self.OWN
        return cli

    def test_aborted_is_refused(self):
        """あの status には書く経路が無い (boid の machine が attrs_set を通さない)。"""
        cli = self.cli_for(status="aborted")
        with self.assertRaises(Exception) as caught:
            run("summary", cli, task_id="t1", body="本文")
        message = str(caught.exception)
        self.assertIn("aborted", message)
        self.assertIn("skip", message, "aborted: 正しい verb を名指ししていない")
        self.assertFalse(cli.wrote("send_action"))

    def test_observed_is_allowed_on_a_done_card(self):
        """**設計が名指しで守れと言っている経路** (S-9 / I-5b、`internal/api/
        attrs_set_done.go`)。done な task も観測の対象に含める —— done には I-5b の
        service 層ガード経由で attrs_set が通る。**現行実装は観測側が done を除外していた
        ためこの経路が一度も発火しなかった。同じ轍を踏まない**」。

        v2 では `source_closed` を読んだ daemon の自動 reopen (`SweepReopen`) は廃止された
        (設計 §3.3) —— 再燃をもたらすのは、この観測を根拠に khi 自身が suggest する
        `reopen` (下の `test_reopen_is_allowed_on_a_done_card`)。observed はその判断材料
        として書き続ける価値がある。
        """
        cli = self.cli_for(status="done")
        run("observed", cli, task_id="t1", source_closed=False)
        self.assertTrue(cli.wrote("send_action"), "done の card で observed が塞がれている")

    def test_reopen_is_allowed_on_a_done_card(self):
        """**受け入れ条件 (実装計画 §5): done card への reopen suggest が成立すること。**

        boid 側 (`internal/api/attrs_set_done.go` の `resolveAttrsSetDoneTransition`、
        I-5b) は task_triage 行が存在する done task への attrs_set を noop 遷移として
        通す — これが suggestion (`{"suggestion": {"verb": "reopen", ...}}`) の書き込み
        経路そのもの。人が accept すれば `answered` (`cardActiveAndTerminalStatuses` に
        done を含む、boid PR #987 BLOCKER 3) が done→parked を適用する。
        """
        cli = self.cli_for(status="done")
        run("reopen", cli, task_id="t1", reason="source が再オープンされた")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "reopen")

    def test_other_verbs_are_refused_on_a_done_card(self):
        """`observed`/`reopen` 以外は done に書く意味が無い (終わった件を書き換えることになる)。"""
        cli = self.cli_for(status="done")
        with self.assertRaises(Exception) as caught:
            run("summary", cli, task_id="t1", body="本文")
        self.assertIn("skip", str(caught.exception))

    def test_reopen_is_allowed_on_a_dropped_card(self):
        """boid の card 機械 v2 は `dropped → parked : reopen` を持つ (設計 §3.2)。
        attrs_set 自体も dropped を FromStatus に含む
        (`cardActiveAndDroppedStatuses`、boid PR #987 review round2 BLOCKER N1) ので、
        done と違って service 層ガードなしにそのまま通る。"""
        cli = self.cli_for(status="dropped")
        run("reopen", cli, task_id="t1", reason="drop 後に続きが来た")
        payload = [c[3] for c in cli.actions("attrs_set") if "suggestion" in (c[3] or {})][0]
        self.assertEqual(payload["suggestion"]["verb"], "reopen")

    def test_other_verbs_are_refused_on_a_dropped_card(self):
        """`reopen` 以外は dropped に書く意味が無い (人が drop と決めた件を尊重する)。"""
        cli = self.cli_for(status="dropped")
        with self.assertRaises(Exception) as caught:
            run("summary", cli, task_id="t1", body="本文")
        message = str(caught.exception)
        self.assertIn("dropped", message)
        self.assertIn("skip", message)
        self.assertFalse(cli.wrote("send_action"))

    def test_another_projects_task_is_refused(self):
        """**khi は自分の project の card にしか書かない。**

        2026-08-23 の失敗の本体がこれ —— subagent は khi の identity を別 project
        (mera-ui) の終端の子タスクへ結びつけようとしていた。通っていれば以降その
        identity は khi が書けない task に解決していた。status は関係ない。
        """
        cli = self.cli_for(status="working", project="proj-other")
        with self.assertRaises(Exception) as caught:
            run("summary", cli, task_id="t1", body="本文")
        message = str(caught.exception)
        self.assertIn("skip", message)
        self.assertFalse(cli.wrote("send_action"))

    def test_a_live_own_task_is_untouched(self):
        cli = self.cli_for(status="working")
        run("summary", cli, task_id="t1", body="本文")
        self.assertTrue(cli.wrote("update_description"))

    def test_a_verb_without_a_task_is_untouched(self):
        """`skip` は task を持たない —— 問う相手が居ない。"""
        cli = self.cli_for(status="done")
        run("skip", cli, reason="起票に値しない")
        self.assertTrue(cli.wrote("notify_progress"))
