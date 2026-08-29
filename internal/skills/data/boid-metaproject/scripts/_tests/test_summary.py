"""boidmeta.summary のテスト (設計 `docs/plans/signal-and-summary.md` S-1)。

    人の編集を巻き戻さないため、境界マーカーを置く。マーカーより上は人の領域、
    下は khi が毎回上書きする。

サマリーは**毎回書き直す** (原文のコピーは持たない) ので、この境界が無いと人が足した
メモが毎巡消える。消えたことに気づく手段が無いのがまずい —— 人は「書いたはずのものが
無い」としか分からない。
"""
from __future__ import annotations

import unittest

from boidmeta.summary import MARKER, MAX_DESCRIPTION_BYTES, merge

HUMAN = "これは私が書いたメモ。\n消えては困る。\n"
BODY = "## いま何が起きているか\n\nDLQ に溜まっている。\n"


class MergeTest(unittest.TestCase):
    def test_an_empty_description_gets_the_marker_and_the_body(self):
        merged = merge("", BODY)
        self.assertIn(MARKER, merged)
        self.assertIn("DLQ に溜まっている", merged)

    def test_text_without_a_marker_is_kept_above(self):
        """起票時の原文など、マーカーの無い既存の本文は**人の領域として扱う**。
        khi の領域はその下に足す —— 消すと起票時の文脈が失われる。"""
        merged = merge(HUMAN, BODY)
        self.assertTrue(merged.startswith(HUMAN.rstrip()))
        self.assertIn(MARKER, merged)
        self.assertIn("DLQ に溜まっている", merged)

    def test_the_khi_section_is_replaced_not_appended(self):
        first = merge(HUMAN, "古いサマリー")
        second = merge(first, "新しいサマリー")
        self.assertIn("新しいサマリー", second)
        self.assertNotIn("古いサマリー", second)
        self.assertEqual(second.count(MARKER), 1)

    def test_the_human_part_survives_every_round(self):
        current = ""
        for i in range(5):
            current = merge(current, f"サマリー {i}")
            current = HUMAN + current if i == 0 else current
        self.assertIn("消えては困る", merge(current, "最新"))

    def test_it_is_idempotent(self):
        once = merge(HUMAN, BODY)
        self.assertEqual(merge(once, BODY), once)

    def test_a_human_edit_above_the_marker_is_preserved(self):
        merged = merge(HUMAN, BODY)
        edited = merged.replace("消えては困る", "消えては困る (追記した)")
        again = merge(edited, "別のサマリー")
        self.assertIn("追記した", again)
        self.assertNotIn("DLQ に溜まっている", again)


class SizeTest(unittest.TestCase):
    """`description` の上限は 64KiB (`orchestrator.MaxContentBytes`)。超えると boid が
    400 を返し、**サマリーが 1 文字も書けない**。判断が残らないより、切って残す方がよい。
    """

    def test_a_long_body_is_truncated(self):
        merged = merge("", "あ" * 40000)
        self.assertLessEqual(len(merged.encode("utf-8")), MAX_DESCRIPTION_BYTES)

    def test_truncation_leaves_a_mark(self):
        """黙って切ると、人は「LLM が途中で書くのをやめた」と読む。"""
        merged = merge("", "あ" * 40000)
        self.assertIn("以下略", merged)

    def test_the_human_part_is_never_truncated(self):
        """人の領域は khi のものではない。切るのは khi が書いた側だけ。"""
        human = "大事なメモ\n"
        merged = merge(human, "あ" * 40000)
        self.assertTrue(merged.startswith(human.rstrip()))

    def test_a_short_body_is_left_alone(self):
        self.assertNotIn("以下略", merge(HUMAN, BODY))


if __name__ == "__main__":
    unittest.main()
