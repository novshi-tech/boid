"""boidmeta.inbox のテスト。

envelope 列 → `Signal` 列の写像と、inbox の 3 段 (`list` / `claim` / `ack`) の
呼び分けを固定する。**source ごとにテストを分けない** —— 写像は pack 名を
そのまま `Signal.source` にするだけで source を知らないので、pack を変えても
通る道が変わらないこと自体がこの層の契約である。

固定している性質:

- 読み (`read_pending`) は副作用なし。`claim` を打たない
- `claim` / `ack` は **connector が発行した元の id** で打つ (event_key の
  prefix を剥がす)
- prefix は**無条件**なので、id が偶然 `"<source>:"` で始まっていても逆変換は
  厳密に戻る (khi の旧実装が「既知の限界」として抱えていた曖昧さの不在)
- 壊れた envelope は 1 件だけ諦め、同じ巡の他の行を道連れにしない
- `ack` は一括が失敗したら 1 件ずつ再試行する。`claim` はしない (落としても
  次巡やり直せるので恒久ロスにならない)
"""
from __future__ import annotations

import unittest
from datetime import datetime, timezone

from boidmeta import inbox
from boidmeta.screen import self_authored

_AT = "2026-08-27T01:23:45Z"


def _envelope(pack: str, connector: str, envelope_id: str, identity: str, **fields) -> dict:
    row = {
        "id": envelope_id,
        "occurred_at": fields.pop("occurred_at", _AT),
        "source": {"pack": pack, "connector": connector, "service": fields.pop("service", pack)},
        "identity": identity,
    }
    row.update(fields)
    return row


class FakeCLI:
    def __init__(self, rows=(), *, list_error=None, claim_error=None, ack_errors=()):
        self._rows = list(rows)
        self._list_error = list_error
        self._claim_error = claim_error
        self._ack_errors = list(ack_errors)
        self.claimed: list[list[str]] = []
        self.ack_calls: list[list[str]] = []
        self.acked: list[str] = []

    def list_signals(self, *, state, limit):
        if self._list_error is not None:
            raise self._list_error
        return {"signals": list(self._rows)}

    def claim_signals(self, ids):
        self.claimed.append(list(ids))
        if self._claim_error is not None:
            raise self._claim_error

    def ack_signals(self, ids):
        ids = list(ids)
        self.ack_calls.append(ids)
        for bad in self._ack_errors:
            if bad in ids:
                raise RuntimeError(f"unknown id(s): {bad}")
        self.acked.extend(ids)


def _read(rows):
    return inbox.read_pending(FakeCLI(rows))


class MappingTest(unittest.TestCase):
    """pack を変えても通る道が変わらないこと自体が契約。"""

    CASES = [
        # (pack, connector, envelope id, identity)
        ("slack", "mentions", "C0123:1699999999.000100", "slack-thread:1699999999.000100"),
        ("jira-cloud", "assigned-issues", "ROOKPF-311:2026-08-27T01:23:45Z", "jira:ROOKPF-311"),
        ("bitbucket-cloud", "pr-comments", "repo:12:comment:7", "jira:KT-1"),
        ("github", "assigned-issues", "novshi-tech/boid#1033:2026-08-27T01:23:45Z", "github:novshi-tech/boid#1033"),
        ("boid", "actions", "b1549c70-6e46-4c1c-9092-4ed8fe5d40e6", "b1549c70-6e46-4c1c-9092-4ed8fe5d40e6"),
    ]

    def test_the_pack_becomes_the_source_verbatim(self):
        """対応表は持たない。pack 名を別名へ写すと、その表がメタプロジェクトごとの
        設定項目になり、子 task の `ref` にも焼き付く。"""
        for pack, connector, envelope_id, identity in self.CASES:
            with self.subTest(pack=pack):
                signals, ok = _read([_envelope(pack, connector, envelope_id, identity)])
                self.assertTrue(ok)
                self.assertEqual(len(signals), 1)
                self.assertEqual(signals[0].source, pack)
                self.assertEqual(signals[0].identity, identity)

    def test_the_event_key_round_trips_back_to_the_envelope_id(self):
        """**この 1 本がこの層の要。** claim/ack は元の id で打たないと inbox の行が
        引けず、typo guard で呼び出し全体が失敗する。"""
        for pack, connector, envelope_id, identity in self.CASES:
            with self.subTest(pack=pack):
                signals, _ok = _read([_envelope(pack, connector, envelope_id, identity)])
                self.assertEqual(inbox.envelope_id_of(signals[0].event_key), envelope_id)

    def test_an_id_that_already_looks_prefixed_still_round_trips(self):
        """khi の旧実装は「既に `<source>:` で始まっていたら付けない」条件付き付与
        だったため、逆変換が「元から付いていたのか」を区別できず、**ack が別 id で
        飛んで無言に失敗する**余地を「既知の限界」として抱えていた。無条件付与なら
        この形でも厳密に戻る。"""
        row = _envelope("slack", "mentions", "slack:C1:1.0", "slack-thread:1.0")
        signals, _ok = _read([row])
        self.assertEqual(signals[0].event_key, "slack/mentions:slack:C1:1.0")
        self.assertEqual(inbox.envelope_id_of(signals[0].event_key), "slack:C1:1.0")

    def test_author_and_url_ride_along_but_title_is_discarded(self):
        """`title` は `Signal` に無い —— 素通しにするだけで「中身は運ばない」設計が
        自動的に満たされる。"""
        row = _envelope("slack", "mentions", "C1:1.0", "slack-thread:1.0",
                        author="U0ABC", url="https://example.invalid/p", title="someone mentioned you")
        signals, _ok = _read([row])
        self.assertEqual(signals[0].author, "U0ABC")
        self.assertEqual(signals[0].url, "https://example.invalid/p")
        self.assertFalse(hasattr(signals[0], "title"))

    def test_a_missing_author_stays_none_so_self_screening_fails_open(self):
        """`author` 不明は「self 判定にかけない」の表現。落とす側へ倒さない。"""
        signals, _ok = _read([_envelope("slack", "mentions", "C1:1.0", "slack-thread:1.0")])
        self.assertIsNone(signals[0].author)
        self.assertFalse(self_authored(signals))

    def test_occurred_at_becomes_tz_aware(self):
        signals, _ok = _read([_envelope("slack", "mentions", "C1:1.0", "slack-thread:1.0")])
        self.assertEqual(signals[0].at, datetime(2026, 8, 27, 1, 23, 45, tzinfo=timezone.utc))


class MalformedEnvelopeTest(unittest.TestCase):
    """壊れた 1 件で巡を止めない。"""

    GOOD = _envelope("slack", "mentions", "C1:1.0", "slack-thread:1.0")

    def _only_the_good_one_survives(self, bad):
        signals, ok = _read([bad, self.GOOD])
        self.assertTrue(ok)
        self.assertEqual([s.identity for s in signals], ["slack-thread:1.0"])

    def test_a_missing_source_pack_is_skipped(self):
        bad = dict(self.GOOD, id="x", source={"connector": "c"})
        self._only_the_good_one_survives(bad)

    def test_a_missing_id_is_skipped(self):
        bad = dict(self.GOOD)
        bad.pop("id")
        self._only_the_good_one_survives(bad)

    def test_a_missing_identity_is_skipped(self):
        bad = dict(self.GOOD, id="y")
        bad.pop("identity")
        self._only_the_good_one_survives(bad)

    def test_an_unparsable_occurred_at_is_skipped(self):
        self._only_the_good_one_survives(dict(self.GOOD, id="z", occurred_at="not a time"))

    def test_a_non_mapping_row_is_skipped(self):
        self._only_the_good_one_survives("not an envelope")


class ReadTest(unittest.TestCase):
    def test_the_read_never_charges_attempts(self):
        """**読みは無料。** 判断する件数より広く読んでよいのがこの分離の目的。"""
        cli = FakeCLI([_envelope("slack", "mentions", "C1:1.0", "slack-thread:1.0")])
        inbox.read_pending(cli)
        self.assertEqual(cli.claimed, [])

    def test_a_list_failure_reports_not_ok_with_no_signals(self):
        """読めなかった巡は判断を丸ごと諦める —— フェイルオープンにする代替経路が
        もう無い (inbox が唯一の読み口)。"""
        signals, ok = inbox.read_pending(FakeCLI(list_error=RuntimeError("daemon 落ちてる")))
        self.assertEqual(signals, ())
        self.assertFalse(ok)


class ClaimTest(unittest.TestCase):
    def test_claim_uses_the_original_envelope_ids(self):
        cli = FakeCLI()
        self.assertTrue(inbox.claim(cli, ("jira-cloud/assigned-issues:ROOKPF-1:2026-08-27T00:00:00Z", "slack-cloud/mentions:C1:1.0")))
        self.assertEqual(cli.claimed, [["ROOKPF-1:2026-08-27T00:00:00Z", "C1:1.0"]])

    def test_no_keys_does_not_call_the_cli(self):
        cli = FakeCLI()
        self.assertTrue(inbox.claim(cli, ()))
        self.assertEqual(cli.claimed, [])

    def test_a_failure_is_not_fatal_and_is_not_retried_one_by_one(self):
        """課金できなかった巡は signal が dead へ 1 歩近づかないだけ。ack と違い
        恒久ロスにならないので、1 件ずつの再試行はしない。"""
        cli = FakeCLI(claim_error=RuntimeError("boom"))
        self.assertFalse(inbox.claim(cli, ("slack/mentions:C1:1.0", "slack/mentions:C1:2.0")))
        self.assertEqual(len(cli.claimed), 1, "一括 1 回で諦める")


class AckTest(unittest.TestCase):
    def test_ack_uses_the_original_envelope_ids(self):
        cli = FakeCLI()
        self.assertTrue(inbox.ack(cli, ("jira-cloud/assigned-issues:ROOKPF-1:2026-08-27T00:00:00Z",)))
        self.assertEqual(cli.ack_calls, [["ROOKPF-1:2026-08-27T00:00:00Z"]])

    def test_no_keys_does_not_call_the_cli(self):
        cli = FakeCLI()
        self.assertTrue(inbox.ack(cli, ()))
        self.assertEqual(cli.ack_calls, [])

    def test_a_clean_batch_is_one_call(self):
        cli = FakeCLI()
        inbox.ack(cli, ("slack/mentions:C1:1.0", "slack/mentions:C1:2.0"))
        self.assertEqual(cli.ack_calls, [["C1:1.0", "C1:2.0"]])

    def test_one_bad_id_does_not_block_the_rest(self):
        """`boid signal ack` は 1 件でも未知の id が混じると呼び出し全体を失敗させる。
        取り逃しは恒久ロスなので、ここだけ 1 件ずつ再試行する。"""
        cli = FakeCLI(ack_errors=["C1:bad"])
        self.assertFalse(inbox.ack(cli, ("slack/mentions:C1:1.0", "slack/mentions:C1:bad", "slack/mentions:C1:2.0")))
        self.assertEqual(sorted(cli.acked), ["C1:1.0", "C1:2.0"])


if __name__ == "__main__":
    unittest.main()
