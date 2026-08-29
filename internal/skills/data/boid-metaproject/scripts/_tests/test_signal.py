"""boidmeta.signal のテスト。

非 boid source (slack / jira / bitbucket) が返す「新着があった」1 件の型。
**値の入れ物だが、構築時の不変条件がこの設計の要になっている** —— `event_key` の
source と `source` フィールドがズレると、`domain/childid.child_id` が子 task の `ref` に
別の source を埋め、同じ出来事に別々の ref が付いて子が重複起票される。
`adapters/inbox` の namespace 付与とその逆変換 (`envelope_id_of`) も同じ不変条件の上に
立っている。
"""
from __future__ import annotations

import unittest
from datetime import datetime, timezone

from boidmeta.signal import Signal

T0 = datetime(2026, 8, 22, 12, 0, tzinfo=timezone.utc)


class SignalTest(unittest.TestCase):
    def test_a_naive_timestamp_is_rejected(self):
        """`at` は栞になる。naive を許すと、比較する相手 (adapter が parse した since) の
        tz 有無次第で `TypeError` が実行時に飛ぶか、9 時間ずれた窓を読む。"""
        with self.assertRaises(ValueError):
            Signal(source="slack", event_key="slack:C1:1.0", identity="slack-thread:1.0", at=T0.replace(tzinfo=None))

    def test_the_event_key_source_must_match_the_source_field(self):
        """**この 2 つがズレると壊れ方が静か。** 栞は `source` をキーに保存されるので、
        jira の event を slack の栞として書くと、次の巡は slack を jira の時刻から
        読み直す (取りこぼすか、全件読み直す)。"""
        with self.assertRaises(ValueError):
            Signal(source="slack", event_key="jira:X-1:issue:2026-08-22", identity="jira:X-1", at=T0)

    def test_a_source_containing_the_separator_is_rejected(self):
        """source は event_key の先頭を占める —— `:` を含むと `source_of` が別の
        ところで切り、`childid` が別の source を子 ref に埋める。"""
        with self.assertRaises(ValueError):
            Signal(source="a:b", event_key="a:b:1", identity="x", at=T0)

    def test_an_empty_identity_is_rejected(self):
        """identity は sweep task の instruction に載る「対象の指し方」そのもの。
        空だと subagent が何を読めばよいか分からない候補が 1 行立つだけになる。"""
        with self.assertRaises(ValueError):
            Signal(source="slack", event_key="slack:C1:1.0", identity="", at=T0)

    def test_it_carries_the_author_and_url_but_no_body(self):
        """**中身は持たない** (§5.1「検知は中身を見ない」)。`author` は self-authored の
        篩い (`domain/screen.py`) が読み、`url` は人と subagent が原文へ辿る入口。"""
        signal = Signal(
            source="slack",
            event_key="slack:C1:1.0",
            identity="slack-thread:1.0",
            at=T0,
            author="self",
            url="https://example.invalid/p",
        )
        self.assertEqual(signal.author, "self")
        self.assertEqual(signal.url, "https://example.invalid/p")
        self.assertFalse(hasattr(signal, "body"))


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
