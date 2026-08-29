"""boidmeta.write の CLI エントリポイントのテスト。

    python3 -m boidmeta.write <verb> [--report] < payload.json

**メッセージは stderr に出る。** subagent が読んで言い直すための唯一の手がかりなので、
「何が悪いか」と「どう直すか」が読めることを固定する。exit code は 3 つに分ける ——
0 成功 / 1 拒否 (subagent が直せる) / 2 呼び出し方の誤り。
"""
from __future__ import annotations

import io
import json
import unittest
from pathlib import Path

from boidmeta.write import TASK_ID_ENV, main
from test_write_executor import FakeCLI

ENV = {TASK_ID_ENV: "sweep-1"}


def call(argv, payload, *, cli=None, env=ENV) -> tuple[int, str, str]:
    out, err = io.StringIO(), io.StringIO()
    body = payload if isinstance(payload, str) else json.dumps(payload)
    code = main(
        argv,
        stdin=io.StringIO(body),
        stdout=out,
        stderr=err,
        cli=cli if cli is not None else FakeCLI(),
        env=env,
    )
    return code, out.getvalue(), err.getvalue()


class UsageTest(unittest.TestCase):
    def test_no_verb_prints_the_vocabulary(self):
        code, _out, err = call([], {})
        self.assertEqual(code, 2)
        self.assertIn("summary", err)
        self.assertIn("spec", err)

    def test_two_verbs_is_a_usage_error(self):
        code, _out, _err = call(["summary", "spec"], {})
        self.assertEqual(code, 2)

    def test_malformed_stdin(self):
        code, _out, err = call(["done-signal"], "{ではない")
        self.assertEqual(code, 2)
        self.assertIn("JSON", err)

    def test_a_json_array_is_refused(self):
        code, _out, err = call(["done-signal"], [1, 2])
        self.assertEqual(code, 2)
        self.assertIn("オブジェクト", err)

    def test_a_missing_task_id_env_is_refused(self):
        """見送りの記録は sweep task 自身の timeline にしか置けない。書き先が無いまま
        進むと、記録の付かない書き込みができてしまう。"""
        code, _out, err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, env={})
        self.assertEqual(code, 2)
        self.assertIn(TASK_ID_ENV, err)


class RejectionTest(unittest.TestCase):
    def test_a_validation_error_lands_on_stderr(self):
        # `reason` (2026-08-25 に必須化、LOW 8) が無いフィールド名の中で辞書順最初に
        # 来るので、それより先にこちらが報告される。「wake_at 無し」だけを見たい場合は
        # 下の `test_a_missing_wake_condition_lands_on_stderr` を見ること。
        code, _out, err = call(["park"], {"signals": ["boid:a1"], "task_id": "t1"})
        self.assertEqual(code, 1)
        self.assertIn("reason", err)

    def test_a_missing_wake_condition_lands_on_stderr(self):
        code, _out, err = call(
            ["park"], {"signals": ["boid:a1"], "task_id": "t1", "reason": "来週見直す"}
        )
        self.assertEqual(code, 1)
        self.assertIn("wake_at", err)

    def test_an_unknown_verb(self):
        code, _out, err = call(["summarise"], {"signals": ["boid:a1"]})
        self.assertEqual(code, 1)
        self.assertIn("summarise", err)

    def test_a_boid_failure_is_reported(self):
        class Failing(FakeCLI):
            def notify_progress(self, task_id, message):
                from boidmeta.boid_store import BoidError

                raise BoidError("exit=1: nope")

        code, _out, err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=Failing())
        self.assertEqual(code, 1)
        self.assertIn("nope", err)


class SuccessTest(unittest.TestCase):
    def test_a_write_goes_through(self):
        cli = FakeCLI()
        code, out, err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertEqual((code, err), (0, ""))
        self.assertEqual(out, "")
        self.assertTrue(cli.wrote("notify_progress"))

    def test_report_mode_writes_nothing_and_prints_the_plan(self):
        cli = FakeCLI()
        code, out, _err = call(["done-signal", "--report"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertEqual(code, 0)
        self.assertFalse(cli.wrote("notify_progress"))
        self.assertIn("notify_progress", out)

    def test_the_report_flag_is_not_taken_for_a_verb(self):
        code, _out, err = call(["--report", "done-signal"], {"signals": ["boid:a1"], "task_id": "t1"})
        self.assertEqual((code, err), (0, ""))


class ReadonlyForcesReportTest(unittest.TestCase):
    """2026-08-27、Opus レビュー指摘対応 (2 巡目)。

    shadow task からの書き込みが「実際に何も書かない」ことを、subagent の instruction
    (`--report` を必ず付ける) という prompt 遵守だけに委ねない —— `main()` が「今動いて
    いる自分自身」の readonly (`boid task current --field readonly`) を boid に問い合わせ、
    `"false"` でなければ argv に `--report` が無くても report を強制する。

    **1 巡目の実装は `BOID_TASK_ID` env 経由で sweep task の behavior を引いていたが、
    その env は `--report` を落とすのと同じ subagent の shell 変数であり偽装できる**、
    という指摘で `current_field` (env に依存しない、daemon が直接答える経路) に差し替えた。
    ここでのテストは「readonly の値」だけを制御し、`BOID_TASK_ID` の値そのものは
    enforcement に影響しないことも兼ねて確認する。
    """

    def test_a_readonly_task_is_forced_into_report_mode_even_without_the_flag(self):
        cli = FakeCLI(readonly="true")
        code, out, _err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertEqual(code, 0)
        self.assertFalse(cli.wrote("notify_progress"))
        self.assertIn("notify_progress", out)

    def test_a_writable_task_still_writes_without_the_flag(self):
        """既存の real sweep の挙動 (`--report` 無しなら普通に書き込む) が壊れていないこと。"""
        cli = FakeCLI(readonly="false")
        code, _out, _err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertEqual(code, 0)
        self.assertTrue(cli.wrote("notify_progress"))

    def test_spoofing_the_task_id_env_does_not_unlock_writes(self):
        """`BOID_TASK_ID` を real sweep の id に書き換えても、readonly 判定は
        `boid task current` (env に依存しない経路) を見るので誤魔化せない。"""
        cli = FakeCLI(readonly="true")
        code, out, _err = call(
            ["done-signal"],
            {"signals": ["boid:a1"], "task_id": "t1"},
            cli=cli,
            env={TASK_ID_ENV: "some-other-real-sweep-task"},
        )
        self.assertEqual(code, 0)
        self.assertFalse(cli.wrote("notify_progress"))
        self.assertIn("notify_progress", out)

    def test_an_unreadable_readonly_fails_closed(self):
        """readonly を引けない (boid 呼び出し失敗) ときは fail-closed —— 部分故障
        (`current_field` だけ落ちて書き込みは通る等) を安全装置の常設バイパス
        にしない。可用性より「判断できないときに書いてしまわない」ことを優先する。"""

        class Unreadable(FakeCLI):
            def current_field(self, field):
                raise RuntimeError("boid unreachable")

        cli = Unreadable()
        code, _out, err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertEqual(code, 0)
        self.assertFalse(cli.wrote("notify_progress"))
        self.assertIn("boid unreachable", err)

    def test_an_unexpected_readonly_value_fails_closed(self):
        """`"true"`/`"false"` 以外の値 (CLI の出力形式が変わった等) も fail-closed。"""
        cli = FakeCLI(readonly="")
        code, _out, err = call(["done-signal"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertEqual(code, 0)
        self.assertFalse(cli.wrote("notify_progress"))
        self.assertIn("想定外の値", err)

    def test_the_explicit_report_flag_still_works_on_a_writable_task(self):
        """明示的に `--report` を付ければ従来通り dry-run になる
        (readonly 判定が既存挙動を上書きしないことの確認)。"""
        cli = FakeCLI(readonly="false")
        code, out, _err = call(
            ["done-signal", "--report"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli
        )
        self.assertEqual(code, 0)
        self.assertFalse(cli.wrote("notify_progress"))
        self.assertIn("notify_progress", out)

    def test_the_explicit_report_flag_skips_the_readonly_lookup(self):
        """`--report` が既に付いているときは `current_field` を呼ぶ必要が無い —— 呼んで
        いないことを確認する (無駄な boid 呼び出しを増やさない)。"""
        cli = FakeCLI(readonly="false")
        call(["done-signal", "--report"], {"signals": ["boid:a1"], "task_id": "t1"}, cli=cli)
        self.assertFalse(cli.wrote("current_field"))


if __name__ == "__main__":
    unittest.main()
