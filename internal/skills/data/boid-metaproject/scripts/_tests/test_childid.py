"""boidmeta.childid のテスト (設計 `docs/plans/signal-and-summary.md` S-12)。

    ch:<正規化した仕事の識別子>:<起点になったシグナルの event_key>

**世代 (起点シグナル) を含めることが、このモジュールが在る理由**。boid の
`AddDetailChild` は同 id なら closed でも noop、`SpecDetailChild` は dispatched/closed に
冪等 noop (`internal/orchestrator/task_triage.go`) なので、仕事の識別子だけを id にすると
「子が終わった後に差し戻しが来たら新しい spec を書く」(S-12 の 3 つめ) が**機械的に
不可能**になる —— どちらも無言で noop し、新しい子が永遠に現れない。

だから中心のテストは 2 本:
- 同じシグナルのリトライでは同じ id (冪等)
- 新しいシグナルでは新しい id (新ラウンド)
"""
from __future__ import annotations

import unicodedata
import unittest

from boidmeta.childid import MAX_CHILD_ID_CHARS, MAX_WORK_CHARS, child_id, normalize_work


class NormalizeWorkTest(unittest.TestCase):
    def test_lowercases(self):
        self.assertEqual(normalize_work("ROOKPF-307-Fix"), "rookpf-307-fix")

    def test_spaces_and_colons_become_hyphens(self):
        """`:` は id の区切りなので必ず潰す —— 残すと世代の境目が読めなくなる。"""
        self.assertEqual(normalize_work("fix: the DLQ codec"), "fix-the-dlq-codec")

    def test_runs_are_collapsed_and_edges_trimmed(self):
        self.assertEqual(normalize_work("  fix   ::  codec  "), "fix-codec")

    def test_non_ascii_survives(self):
        """LLM が日本語で仕事名を書いても id は作れる (id は opaque な文字列比較)。

        ASCII に縛らないのは、縛るとスキルに「英数字で書け」という規則が要るため ——
        §4「機構で縛れるものはスキルに書かない」の裏返しで、**機構が受け止められるなら
        スキルに規則を足さない**。
        """
        self.assertEqual(normalize_work("DLQ の取りこぼし"), "dlq-の取りこぼし")

    def test_decomposed_and_composed_forms_agree(self):
        """NFD (濁点が分解された形) でも同じ id になること。

        `\\w` は結合文字 (U+3099 等) を語構成文字と見なさないので、正規化しないと
        NFD の「ガイド」が `カ-イト` になる。**同じ仕事名で子が 2 枚立つ** ——
        S-12 が防ごうとしている事故そのもの。macOS 由来のテキストや一部の API が
        NFD を返すので、実際に踏みうる。
        """
        composed = unicodedata.normalize("NFC", "ガイドを直す")
        decomposed = unicodedata.normalize("NFD", "ガイドを直す")
        self.assertNotEqual(composed, decomposed)  # 前提: 入力としては別物
        self.assertEqual(normalize_work(decomposed), normalize_work(composed))
        self.assertEqual(child_id(decomposed, "boid:a1"), child_id(composed, "boid:a1"))

    def test_truncated_at_the_limit(self):
        self.assertEqual(len(normalize_work("a" * 200)), MAX_WORK_CHARS)

    def test_truncation_does_not_leave_a_dangling_hyphen(self):
        work = normalize_work("a" * (MAX_WORK_CHARS - 1) + " tail")
        self.assertFalse(work.endswith("-"))

    def test_two_long_names_sharing_a_prefix_stay_distinct(self):
        """**素朴に切ると衝突する。** 先頭 40 文字が同じ 2 つの仕事が同じ子 id になると、
        2 つめの spec が `AddDetailChild` の create-or-noop に吸われて**子が現れない**。
        title だけの子が残ったまま親は全子 closed にならず、khi の done 判断基準が
        永久に成立しない (ゴールの分解 f が死ぬ)。
        """
        worker = "fix the dlq codec dropped message handling in the worker"
        consumer = "fix the dlq codec dropped message handling in the consumer"
        self.assertNotEqual(normalize_work(worker), normalize_work(consumer))
        self.assertNotEqual(child_id(worker, "boid:a1"), child_id(consumer, "boid:a1"))

    def test_truncation_is_deterministic(self):
        long = "fix the dlq codec dropped message handling in the worker"
        self.assertEqual(normalize_work(long), normalize_work(long))

    def test_empty_after_normalisation_is_rejected(self):
        """記号だけの仕事名は id にならない。黙って空 id を作ると子が全部 1 枚に潰れる。"""
        for bogus in ("", "   ", ":::", "!!!"):
            with self.assertRaises(ValueError):
                normalize_work(bogus)


class ChildIdTest(unittest.TestCase):
    SIGNAL = "jira:ROOKPF-307:comment:2026-08-22T00:00:00.000+0900"

    def test_shape(self):
        self.assertEqual(
            child_id("ROOKPF-307 fix", "boid:a1b2"),
            "ch:rookpf-307-fix:boid:a1b2",
        )

    def test_same_signal_is_idempotent(self):
        """同じシグナルのリトライは同じ id —— `AddDetailChild` の noop に乗って二重に作らない。"""
        self.assertEqual(child_id("fix", self.SIGNAL), child_id("fix", self.SIGNAL))

    def test_new_signal_starts_a_new_generation(self):
        """S-12 の核心。差し戻し (新しいコメント) が来たら別 id になり、新しい子が立つ。"""
        self.assertNotEqual(child_id("fix", self.SIGNAL), child_id("fix", "jira:ROOKPF-307:comment:later"))

    def test_different_work_differs(self):
        self.assertNotEqual(child_id("fix", self.SIGNAL), child_id("review", self.SIGNAL))

    def test_work_normalisation_is_applied(self):
        self.assertEqual(child_id("Fix  DLQ", "boid:a1"), child_id("fix-dlq", "boid:a1"))

    def test_rejects_an_empty_signal(self):
        with self.assertRaises(ValueError):
            child_id("fix", "")

    def test_rejects_a_malformed_signal(self):
        """起点は必ず event_key。source の無い文字列を渡すのは呼び出し側のバグ。"""
        with self.assertRaises(ValueError):
            child_id("fix", "not-an-event-key")


class LengthTest(unittest.TestCase):
    """Web UI の子一覧に出る文字列なので、青天井にはしない。

    実データの event_key はどれも 100 文字未満なので、この短縮が働くのは異常値のときだけ。
    **短縮も決定的**であることが要る —— そうでないとリトライのたびに別 id になり、
    冪等性 (S-12) が壊れる。
    """

    LONG_SIGNAL = "jira:" + "X" * 300 + ":issue"

    def test_normal_ids_are_left_alone(self):
        cid = child_id("fix", "jira:ROOKPF-307:comment:2026-08-22T00:00:00.000+0900")
        self.assertLessEqual(len(cid), MAX_CHILD_ID_CHARS)
        self.assertIn("jira:ROOKPF-307", cid)

    def test_long_ids_are_shortened(self):
        cid = child_id("fix", self.LONG_SIGNAL)
        self.assertLessEqual(len(cid), MAX_CHILD_ID_CHARS)

    def test_shortening_is_deterministic(self):
        self.assertEqual(child_id("fix", self.LONG_SIGNAL), child_id("fix", self.LONG_SIGNAL))

    def test_shortening_still_separates_generations(self):
        self.assertNotEqual(child_id("fix", self.LONG_SIGNAL), child_id("fix", self.LONG_SIGNAL + "2"))


if __name__ == "__main__":
    unittest.main()
