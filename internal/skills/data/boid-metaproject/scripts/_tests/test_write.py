"""boidmeta.write の入力検証のテスト (設計 `docs/plans/signal-and-summary.md` §5.3)。

記録 CLI の verb と引数が、そのまま「判断 → 記録」の契約になる (§3.4)。相手は LLM
なので、**エラーメッセージがそのままフィードバック**になる —— 「何が足りないか」が
読めない拒否は、subagent が同じ間違いを繰り返す原因になる。だからメッセージの中身も
テストで固定する。

未知のフィールドを**エラーにする**のは、typo が silent に無視されると「書いたつもりで
書けていない」という、後から人が見て初めて分かる壊れ方をするため (`ChildSpec` の
全フィールド必須と同じ思想、`domain/models.py`)。
"""
from __future__ import annotations

import unittest

from boidmeta.write import CommandError, URGENCIES, VERBS, validate

#: どの verb でも必須なので、テストの既定として持っておく。
SIGNALS = ["boid:a1b2"]


def ok(verb: str, **fields) -> dict:
    """signals を補って validate を呼ぶ (signals そのものを見るテスト以外はこれを使う)。"""
    payload = {"signals": SIGNALS}
    payload.update(fields)
    return validate(verb, payload)


class VerbTest(unittest.TestCase):
    def test_unknown_verb_lists_the_vocabulary(self):
        with self.assertRaises(CommandError) as caught:
            validate("summarise", {"task_id": "t1"})
        message = str(caught.exception)
        self.assertIn("summarise", message)
        for verb in ("summary", "spec", "skip"):
            self.assertIn(verb, message)

    def test_every_verb_in_the_design_table_is_implemented(self):
        """§5.3 の verb 表がそのまま契約。抜けがあれば subagent に出口が無くなる。

        suggestion 状態遷移化 (`docs/plans/suggestion-as-state-transition.md` §3.7) で
        語彙が変わった: `wake`/`canonical` 廃止、`manual` → `working` (現 `start`) に改名、
        `go`/`complete` (旧 `done`) /`reopen` を新設。"""
        self.assertEqual(
            set(VERBS),
            {
                "capture", "link", "summary", "spec", "drop-child",
                "observed", "urgency", "park", "start", "go", "complete", "reopen",
                "drop", "skip", "done-signal",
            },
        )

    def test_the_command_carries_its_verb(self):
        """実行部はこれで分岐する。落とすと「どの書き込みか」が失われる。"""
        self.assertEqual(ok("start", task_id="t1", reason="r")["verb"], "start")


class RequiredFieldTest(unittest.TestCase):
    def test_missing_field_names_it(self):
        with self.assertRaises(CommandError) as caught:
            ok("summary", task_id="t1")
        self.assertIn("body", str(caught.exception))

    def test_empty_string_counts_as_missing(self):
        with self.assertRaises(CommandError) as caught:
            ok("summary", task_id="t1", body="   ")
        self.assertIn("body", str(caught.exception))

    def test_unknown_field_is_rejected(self):
        with self.assertRaises(CommandError) as caught:
            ok("done-signal", task_id="t1", reasons="x")
        self.assertIn("reasons", str(caught.exception))

    def test_task_id_is_required_where_the_table_says_so(self):
        with self.assertRaises(CommandError) as caught:
            ok("done-signal")
        self.assertIn("task_id", str(caught.exception))

    def test_capture_needs_no_task_id(self):
        """まだ task が無い —— `resolve-or-capture` で立てるのがこの verb (S-15)。"""
        self.assertEqual(
            ok("capture", identity="jira:X-1", title="t", body="b", urgency="week")["identity"],
            "jira:X-1",
        )

    def test_capture_requires_an_urgency(self):
        """**起票と urgency を別 verb に分けない** (S-11)。分けると urgency が打たれない
        まま captured で止まる — v1 時代は `QueueEligible` が urgency ∈ {now, today, week}
        を要求していたため queue に出なかった (urgency は v2 では並び順だけの属性になり
        可視性の関所ではなくなったが、打ち忘れ自体は依然として起きうる)。
        2026-08-23 の本番投入で起票 8 件のうち 5 件がそうなった。うち 2 件は子を
        `specced` まで作ってあり、「Go を押せば動く」ものが人の画面に現れていなかった。
        **打ち忘れうる時点で機構の側の欠陥**なので、ここで縛る。"""
        with self.assertRaises(CommandError) as caught:
            ok("capture", identity="jira:X-1", title="t", body="b")
        self.assertIn("urgency", str(caught.exception))

    def test_a_missing_vocabulary_field_lists_its_values(self):
        """「urgency が要る」だけでは subagent が値を推測して、語彙外で **2 度目の拒否**を
        食う。冒頭の方針どおり、欠けたフィールド名と一緒に使える値も並べる。"""
        with self.assertRaises(CommandError) as caught:
            ok("capture", identity="jira:X-1", title="t", body="b")
        message = str(caught.exception)
        for value in URGENCIES:
            self.assertIn(value, message)

    def test_an_empty_vocabulary_field_also_lists_its_values(self):
        """空文字は「渡さなかった」と同じ間違い方なので、返すものも同じにする。"""
        with self.assertRaises(CommandError) as caught:
            ok("capture", identity="jira:X-1", title="t", body="b", urgency="   ")
        message = str(caught.exception)
        for value in URGENCIES:
            self.assertIn(value, message)

    def test_an_optional_field_may_be_empty(self):
        """任意フィールドの空文字は「渡さなかった」と同じ扱い —— 拒否すると、
        subagent が「空でもいいから埋めておく」形に流れて意味の無い値が残る。

        `spec` の `instruction` で見る (`park` の `reason` は 2026-08-25 に必須化した ——
        PR-K レビュー LOW 8。他の suggestion verb と揃え、park だけ理由なしで accept
        できてしまうのを塞いだ)。"""
        command = ok(
            "spec", task_id="t1", work="w", origin="jira:X-1:comment:1",
            title="t", project="p", behavior="implement", description="d",
            instruction="",
        )
        self.assertEqual(command["instruction"], "")


class SignalsTest(unittest.TestCase):
    """§5.3「どの verb でも、書き込みが成功したら処理記録を自動で付ける」。

    機構が自動で打てるのは**何を処理したのかを渡されたときだけ**なので、必須にして
    機構側で縛る —— 任意にすると「記録を付ける」が実質スキル側の義務に戻り、§4 が
    「機構で縛れるものはスキルに書かない」と決めた当の項目が守られないことがある。
    """

    def test_required_for_every_verb(self):
        for verb, fields in (
            ("done-signal", {"task_id": "t1"}),
            ("summary", {"task_id": "t1", "body": "b"}),
            ("observed", {"task_id": "t1", "source_closed": True}),
            ("urgency", {"task_id": "t1", "urgency": "week"}),
            ("start", {"task_id": "t1", "reason": "r"}),
            ("go", {"task_id": "t1", "reason": "r"}),
            ("complete", {"task_id": "t1", "reason": "r"}),
            ("reopen", {"task_id": "t1", "reason": "r"}),
            ("skip", {"reason": "r"}),
        ):
            with self.assertRaises(CommandError, msg=verb) as caught:
                validate(verb, dict(fields))
            self.assertIn("signals", str(caught.exception), verb)

    def test_the_error_says_where_to_find_them(self):
        """subagent が意識しないのは記録の**書式と置き場**であって「何を処理したか」ではない。
        それは sweep task の instruction に載っている (§3.4)。"""
        with self.assertRaises(CommandError) as caught:
            validate("done-signal", {"task_id": "t1"})
        self.assertIn("instruction", str(caught.exception))

    def test_a_list_is_kept(self):
        command = validate("done-signal", {"task_id": "t1", "signals": ["boid:a1", "boid:a2"]})
        self.assertEqual(command["signals"], ("boid:a1", "boid:a2"))

    def test_a_bare_string_is_accepted(self):
        """1 件だけのときに文字列を渡すのは自然な書き方。受けて正規化する。"""
        self.assertEqual(validate("done-signal", {"task_id": "t1", "signals": "boid:a1"})["signals"], ("boid:a1",))

    def test_surrounding_whitespace_is_stripped(self):
        """記録は event_key の完全一致で引く。空白が混ざると検知が読めない (§3.4)。"""
        self.assertEqual(validate("done-signal", {"task_id": "t1", "signals": [" boid:a1 "]})["signals"], ("boid:a1",))

    def test_a_malformed_event_key_is_rejected(self):
        with self.assertRaises(CommandError) as caught:
            validate("done-signal", {"task_id": "t1", "signals": ["not-an-event-key"]})
        self.assertIn("not-an-event-key", str(caught.exception))

    def test_a_prefix_without_a_body_is_rejected(self):
        """`jira:` は「どの出来事か」を指していない。"""
        with self.assertRaises(CommandError):
            validate("done-signal", {"task_id": "t1", "signals": ["jira:"]})

    def test_a_wrong_type_is_rejected(self):
        with self.assertRaises(CommandError) as caught:
            validate("done-signal", {"task_id": "t1", "signals": 42})
        self.assertIn("signals", str(caught.exception))


class UrgencyTest(unittest.TestCase):
    def test_accepts_the_vocabulary(self):
        for urgency in URGENCIES:
            self.assertEqual(ok("urgency", task_id="t1", urgency=urgency)["urgency"], urgency)

    def test_rejects_anything_else(self):
        with self.assertRaises(CommandError) as caught:
            ok("urgency", task_id="t1", urgency="high")
        message = str(caught.exception)
        self.assertIn("high", message)
        self.assertIn("someday", message)


class SpecTest(unittest.TestCase):
    BASE = {
        "task_id": "t1",
        "work": "ROOKPF-307 の修正",
        "origin": "jira:ROOKPF-307:comment:2026-08-22",
        "title": "DLQ codec を直す",
        "project": "rook-tools",
        "behavior": "implement",
        "description": "背景",
    }

    def test_accepts_the_full_shape(self):
        self.assertEqual(ok("spec", **self.BASE)["behavior"], "implement")

    def test_instruction_is_optional(self):
        """**既定では書かない。** 子の instruction は behavior の `default_instruction` を
        フィールド単位で上書きする (`internal/orchestrator/payload_merge.go` の
        `MergeDefaultInstructions`: override が 1 件なら非空フィールドが勝つ) ので、
        1 行でも書くと **behavior 側の手順が丸ごと消える。**

        2026-08-24 に本番で踏んだ形: `respond` の子に sweep が「投稿後に done」という
        instruction を書いていたため、project.yaml の respond に焼いてあった
        「投稿前に `boid task ask` で承認を取る」契約 (決定 D-24) が消え、承認なしで
        Slack に返信が飛んだ。同じ形が triaged 6 枚すべてに載っていて、review /
        research / drive では workspace default の実務手順 (repo slug の確認、
        gateway の叩き方、GO/NO-GO の明示、`ask` 契約) が消えていた。

        **伝えたいことは description に書く。** instruction は「behavior の既定手順を
        意図的に変える」ときだけ書くフィールドとして残す。
        """
        self.assertNotIn("instruction", ok("spec", **self.BASE))

    def test_an_explicit_instruction_still_passes_through(self):
        """既定を意図的に変える口は塞がない (nose 2026-08-24: 「instruction 書き換えは
        柔軟性の担保のために必要な仕掛け」)。"""
        parsed = ok("spec", **dict(self.BASE, instruction="既定と違う手順"))
        self.assertEqual(parsed["instruction"], "既定と違う手順")

    def test_origin_is_required(self):
        """子 id の世代を決めるのは起点シグナル (S-12)。無いと差し戻しに反応できない。"""
        fields = dict(self.BASE)
        del fields["origin"]
        with self.assertRaises(CommandError) as caught:
            ok("spec", **fields)
        self.assertIn("origin", str(caught.exception))

    def test_origin_must_be_an_event_key(self):
        with self.assertRaises(CommandError):
            ok("spec", **dict(self.BASE, origin="なんとなく"))

    def test_any_behavior_string_passes_validation(self):
        """**validate は behavior の値を判定しない** —— 打てる behavior は振り先の
        project 側で決まるので、この層に固定の一覧を置くと、メタプロジェクトごとに
        古くなって実物と食い違ったときに嘘のエラーを返す。実物との照合は送信の直前に
        `Executor._refuse_absent_behavior` が `boid project behaviors` で行う
        (`test_write_executor.py` の `SpecTest`)。"""
        command = ok("spec", **dict(self.BASE, behavior="whatever-the-target-project-declares"))
        self.assertEqual(command["behavior"], "whatever-the-target-project-declares")

    def test_behavior_is_still_required(self):
        with self.assertRaises(CommandError) as caught:
            spec = dict(self.BASE)
            spec.pop("behavior")
            ok("spec", **spec)
        self.assertIn("behavior", str(caught.exception))

    def test_description_may_not_be_empty(self):
        """description が空だと Web UI の task detail に背景が出ず、人が経緯を追えない
        (nose 2026-08-14 指摘。instruction 1 本に全部詰め込む書き方への歯止め)。"""
        with self.assertRaises(CommandError) as caught:
            ok("spec", **dict(self.BASE, description=""))
        self.assertIn("description", str(caught.exception))

    def test_work_must_survive_normalisation(self):
        """子 id を実際に作る `normalize_work` は記号だけの識別子で例外を投げる。ここで
        通すと、実行部で裸の ValueError になって subagent へのフィードバックにならない。"""
        with self.assertRaises(CommandError) as caught:
            ok("spec", **dict(self.BASE, work="→→→"))
        self.assertIn("work", str(caught.exception))

    def test_a_japanese_work_is_fine(self):
        """機構が受け止められるなら、スキルに「英数字で書け」という規則を足さない (§4)。"""
        self.assertEqual(ok("spec", **dict(self.BASE, work="DLQ の取りこぼし"))["work"], "DLQ の取りこぼし")


class ParkTest(unittest.TestCase):
    """S-13: 条件の無い park は誰も拾えない。**機構で必須にできる**ので、スキルには書かない。"""

    def test_wake_at_is_enough(self):
        command = ok("park", task_id="t1", wake_at="2026-09-01T09:00:00+09:00", reason="来週見直す")
        self.assertEqual(command["wake_at"], "2026-09-01T09:00:00+09:00")

    def test_wake_task_id_is_enough(self):
        self.assertEqual(
            ok("park", task_id="t1", wake_task_id="child-9", reason="子待ち")["wake_task_id"], "child-9"
        )

    def test_neither_is_rejected(self):
        with self.assertRaises(CommandError) as caught:
            ok("park", task_id="t1", reason="来週見直す")
        message = str(caught.exception)
        self.assertIn("wake_at", message)
        self.assertIn("wake_task_id", message)

    def test_reason_is_required(self):
        """**2026-08-25、PR-K レビュー LOW 8**: park も他の suggestion verb と同じく
        reason 必須にした。boid 側の accept 画面 (`TaskDetailSuggestionSection`) は
        `suggestion.Reason != ""` のときだけ理由ブロックを描画するので、reason 無しの
        park は「何を承認するのか」が読めない accept ボタンになっていた。"""
        with self.assertRaises(CommandError) as caught:
            ok("park", task_id="t1", wake_task_id="child-9")
        self.assertIn("reason", str(caught.exception))


class WakeAtFormatTest(unittest.TestCase):
    """**boid は `time.Parse(time.RFC3339, ...)` で読む** (`internal/api/workflow_card.go`、
    旧 `workflow_triage.go`)。

    `datetime.fromisoformat` はそれより広く、下の形を全部通してしまう。ここで通すと
    boid が 400 を返し、「条件付き park のつもりで park されていない」= 誰にも拾われない
    task ができる (S-13 が防ごうとしている状態そのもの)。

    **`+0900` は Jira が返す形式**で、subagent が source の時刻を写して渡す経路がそのまま
    該当する。「送っている値」ではなく「相手が要求する値」で検証する。
    """

    def accepts(self, text: str) -> None:
        self.assertEqual(ok("park", task_id="t1", wake_at=text, reason="r")["wake_at"], text)

    def rejects(self, text: str) -> None:
        with self.assertRaises(CommandError, msg=text) as caught:
            ok("park", task_id="t1", wake_at=text, reason="r")
        self.assertIn("wake_at", str(caught.exception))

    def test_accepts_rfc3339(self):
        self.accepts("2026-09-01T09:00:00+09:00")
        self.accepts("2026-09-01T00:00:00Z")
        self.accepts("2026-09-01T09:00:00.123+09:00")
        self.accepts("2026-09-01T09:00:00-05:00")

    def test_rejects_an_offset_without_a_colon(self):
        self.rejects("2026-09-01T09:00:00+0900")

    def test_rejects_a_space_instead_of_t(self):
        self.rejects("2026-09-01 09:00:00+09:00")

    def test_rejects_a_missing_seconds_field(self):
        self.rejects("2026-09-01T09:00+09:00")

    def test_rejects_a_naive_timestamp(self):
        """naive な日時は「どこの 9 時か」が決まらない。"""
        self.rejects("2026-09-01T09:00:00")

    def test_rejects_prose(self):
        self.rejects("来週")

    def test_rejects_a_date_that_does_not_exist(self):
        """形は合っているが値が無い日時。**LLM が月末を書き間違える経路そのもの**なので、
        正規表現だけでは足りない (Go の `time.Parse` も day out of range で拒否する)。"""
        self.rejects("2026-02-30T09:00:00+09:00")

    def test_rejects_an_impossible_offset(self):
        self.rejects("2026-09-01T09:00:00+99:99")


class ObservedTest(unittest.TestCase):
    def test_accepts_a_boolean(self):
        self.assertIs(ok("observed", task_id="t1", source_closed=False)["source_closed"], False)
        self.assertIs(ok("observed", task_id="t1", source_closed=True)["source_closed"], True)

    def test_rejects_a_string(self):
        """`"false"` を真と読むのは典型的な事故。真偽値でだけ受ける。"""
        with self.assertRaises(CommandError) as caught:
            ok("observed", task_id="t1", source_closed="false")
        self.assertIn("source_closed", str(caught.exception))

    def test_rejects_a_number(self):
        """`1` / `0` も受けない —— boid 側は bool でだけ closed と読む
        (`internal/orchestrator/triage_done.go`)。"""
        with self.assertRaises(CommandError):
            ok("observed", task_id="t1", source_closed=1)

    def test_requires_the_field(self):
        with self.assertRaises(CommandError):
            ok("observed", task_id="t1")


class SuggestionVerbTest(unittest.TestCase):
    """`start` / `go` / `complete` / `reopen` / `drop` は全部同じ形 (task_id, reason) の
    suggestion verb (設計 `docs/plans/suggestion-as-state-transition.md` §3.7)。

    `park` も reason は必須 (2026-08-25〜、LOW 8) だが、wake_at/wake_task_id という
    追加の必須条件を持つので別クラス (`ParkTest`) にある。
    """

    def test_each_requires_a_task_id_and_a_reason(self):
        for verb in ("start", "go", "complete", "reopen", "drop"):
            with self.assertRaises(CommandError, msg=verb) as caught:
                ok(verb, task_id="t1")
            self.assertIn("reason", str(caught.exception), verb)

    def test_each_carries_its_own_verb(self):
        for verb in ("start", "go", "complete", "reopen", "drop"):
            self.assertEqual(ok(verb, task_id="t1", reason="r")["verb"], verb)


class NormalisationTest(unittest.TestCase):
    def test_strings_are_stripped(self):
        command = ok("start", task_id="  t1  ", reason="  人手が要る  ")
        self.assertEqual(command["task_id"], "t1")
        self.assertEqual(command["reason"], "人手が要る")

    def test_body_keeps_its_internal_whitespace(self):
        """サマリーは markdown。中の改行やインデントを潰さない。"""
        body = "## いま何が起きているか\n\n- 一つ目\n- 二つ目\n"
        self.assertEqual(ok("summary", task_id="t1", body=body)["body"], body.strip())


if __name__ == "__main__":
    unittest.main()
