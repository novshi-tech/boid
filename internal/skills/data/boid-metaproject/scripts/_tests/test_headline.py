"""boidmeta.summary.headline のテスト (設計 S-1)。

    `task_triage` の `summary` は別に 1 行で書き続ける。queue 一覧の行バッジがこれを読む

**subagent には 1 行を書かせない。** body から導出できるものをスキルの入力に足すのは
§4 の 3 段の原則 (機構で縛れるものはスキルに書かない) に反する —— 入力が増えるほど
「本文と 1 行がずれる」失敗が生まれ、しかもそれは機械では検査できない。

行バッジは狭いので、**「いま何が問題か」の 1 行**が出ればよい。サマリーの要件
(§5.2 の 4 点) の 1 つめは「そもそも何の件か」だが、それは task の title が持っている。
"""
from __future__ import annotations

import unittest

from boidmeta.summary import MAX_HEADLINE_CHARS, headline


class HeadlineTest(unittest.TestCase):
    def test_takes_the_first_real_line(self):
        self.assertEqual(headline("DLQ に溜まっている。\n\n続きの段落。"), "DLQ に溜まっている。")

    def test_skips_markdown_headings(self):
        """サマリーは見出しから始まることが多い。見出しは構造であって中身ではない。"""
        body = "## いま何が起きているか\n\nDLQ に溜まっている。\n"
        self.assertEqual(headline(body), "DLQ に溜まっている。")

    def test_skips_several_headings(self):
        self.assertEqual(headline("# A\n## B\n### C\n本文"), "本文")

    def test_strips_bullet_markers(self):
        for prefix in ("- ", "* ", "+ ", "1. ", "> "):
            self.assertEqual(headline(f"{prefix}本文です"), "本文です")

    def test_strips_inline_emphasis(self):
        """行バッジは markdown を描画しない。記号がそのまま出ると読みにくい。"""
        self.assertEqual(headline("**至急** の対応が要る"), "至急 の対応が要る")
        self.assertEqual(headline("`boid task list` が落ちる"), "boid task list が落ちる")

    def test_ignores_blank_and_marker_only_lines(self):
        self.assertEqual(headline("\n\n---\n\n本文\n"), "本文")

    def test_truncates_long_lines(self):
        line = "あ" * 200
        self.assertEqual(len(headline(line)), MAX_HEADLINE_CHARS)

    def test_truncation_marks_the_cut(self):
        self.assertTrue(headline("あ" * 200).endswith("…"))

    def test_an_empty_body_is_empty(self):
        for body in ("", "   ", "## 見出しだけ", "---"):
            self.assertEqual(headline(body), "")

    def test_it_is_idempotent(self):
        once = headline("## 見出し\n\n**至急** の対応が要る")
        self.assertEqual(headline(once), once)


if __name__ == "__main__":
    unittest.main()
