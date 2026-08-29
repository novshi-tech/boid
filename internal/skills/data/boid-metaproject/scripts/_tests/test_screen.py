"""boidmeta.screen のテスト (設計 §7.1 第2相、直す項目 4)。

引き継ぎメモ §6.2 の規則がそのまま domain の純粋関数として成立していることを見る。
adapters (jira/bitbucket) 側にあった同種のテストはここへ移した (直す項目 4)。
"""
from __future__ import annotations

import unittest
from datetime import datetime, timedelta, timezone

from boidmeta.screen import SELF, self_authored
from boidmeta.signal import Signal

T0 = datetime(2026, 8, 20, 0, 0, tzinfo=timezone.utc)


def at(hours: float) -> datetime:
    return T0 + timedelta(hours=hours)


def candidate(event_key: str, author: str | None, hour: float = 1) -> Signal:
    """`Authored` を満たす候補 1 件。

    `self_authored` が見るのは `author` だけだが、あえて実型 (`Signal`) を渡す ——
    いま `Authored` を満たすのはこれだけなので、スタブを置くと「実際に流れてくる値で
    通るか」を誰も見ていない状態になる。
    """
    return Signal(source="jira-cloud", namespace="jira-cloud/assigned-issues",
                  event_key=f"jira-cloud/assigned-issues:{event_key}", identity="jira:X",
                  at=at(hour), author=author)


class SelfAuthoredTest(unittest.TestCase):
    def test_empty_group_is_not_self_authored(self):
        self.assertFalse(self_authored([]))

    def test_all_self_is_self_authored(self):
        candidates = [candidate("jira:X:comment:1", SELF), candidate("jira:X:comment:2", SELF)]
        self.assertTrue(self_authored(candidates))

    def test_one_other_author_keeps_the_whole_group(self):
        """引き継ぎメモ §6.2: 他人の発言が 1 つでも混ざっていれば B 側に倒す (落とさない)。"""
        candidates = [candidate("jira:X:comment:1", SELF), candidate("jira:X:comment:2", "other-user")]
        self.assertFalse(self_authored(candidates))

    def test_unknown_author_fails_closed(self):
        """author が分からない候補が 1 件でもあれば落とさない (フェイルクローズ)。"""
        candidates = [candidate("jira:X:comment:1", SELF), candidate("jira:X:comment:2", None)]
        self.assertFalse(self_authored(candidates))

    def test_all_unknown_authors_is_not_self_authored(self):
        candidates = [candidate("jira:X:comment:1", None)]
        self.assertFalse(self_authored(candidates))

    def test_custom_mine_value_is_honored(self):
        candidates = [candidate("jira:X:comment:1", "me-account-id")]
        self.assertTrue(self_authored(candidates, mine="me-account-id"))
        self.assertFalse(self_authored(candidates, mine=SELF))

    def test_none_author_exempts_a_candidate_from_screening_entirely(self):
        """jira の `<KEY>:issue` 候補のような『self 判定にかけない候補』は author=None
        として混ぜれば自動的にグループ全体を残す (引き継ぎメモ §6.2)。"""
        issue_candidate = candidate("jira:X:issue", None)
        self_comment = candidate("jira:X:comment:1", SELF)
        self.assertFalse(self_authored([issue_candidate, self_comment]))


# ---------------------------------------------------------------------------
# 旧 signal-and-summary S-9: boid source の 2 段の絞り (`is_signal`) は
# 2026-08-29、PR-2やり直しv2 で削除した (`app/detect.plan_boid` 専用だったため、
# `domain/screen.py` のモジュールコメント参照)。
# ---------------------------------------------------------------------------


if __name__ == "__main__":
    unittest.main()
