"""boidmeta.event_key のテスト。

**event_key はこの機構が組み立てる唯一の識別子** —— inbox の行を ack/claim で
引き当てるにも、子 task の `ref` を組むにも、1 巡の中で集合の要素を区別するにも
使う。だから固定するのは 3 つ:

1. `event_key_of` → `envelope_id_of` が**厳密な逆**であること (どんな id が来ても)
2. namespace が **service instance まで含む**こと。pack だけだと同じ pack の
   instance が 2 つある workspace で別々の出来事が 1 本の鍵に潰れる
3. 組み立て側のバグを黙って通さないこと
"""
from __future__ import annotations

import unittest

from boidmeta.event_key import envelope_id_of, event_key_of, namespace_from_key, namespace_of


class NamespaceTest(unittest.TestCase):
    def test_the_service_instance_names_the_namespace(self):
        """同じ pack の instance が 2 つあっても別の namespace になる —— これが無いと
        両者の同じ id が 1 本の event_key に潰れ、片方が判断前に ack される。"""
        a = namespace_of(service="jira-cloud", connector="assigned-issues", pack="jira-cloud")
        b = namespace_of(service="jira-ubs", connector="assigned-issues", pack="jira-cloud")
        self.assertNotEqual(a, b)

    def test_the_connector_splits_one_service(self):
        a = namespace_of(service="slack-cloud", connector="mentions", pack="slack")
        b = namespace_of(service="slack-cloud", connector="dms", pack="slack")
        self.assertNotEqual(a, b)

    def test_an_empty_service_falls_back_to_the_pack(self):
        """boid 内部 action の envelope は service が空 (service instance という概念が
        無いため)。"""
        self.assertEqual(namespace_of(service="", connector="actions", pack="boid"), "boid/actions")

    def test_it_refuses_what_it_cannot_encode(self):
        for kwargs in (
            {"service": "", "connector": "actions", "pack": ""},
            {"service": "s", "connector": "", "pack": "p"},
            {"service": "a:b", "connector": "c", "pack": "p"},
            {"service": "s", "connector": "a:b", "pack": "p"},
        ):
            with self.subTest(**kwargs), self.assertRaises(ValueError):
                namespace_of(**kwargs)


class RoundTripTest(unittest.TestCase):
    """**厳密な逆**であること。ここが崩れると ack が別の id で飛び、boid の typo guard に
    呼び出し全体を落とされるか、最悪 別の行を決着させる。"""

    IDS = [
        "C0123:1699999999.000100",
        "ROOKPF-311:2026-08-27T01:23:45Z",
        "novshi-tech/boid#1033:2026-08-27T01:23:45Z",
        "b1549c70-6e46-4c1c-9092-4ed8fe5d40e6",
        "slack-cloud/mentions:C1:1.0",   # 既に namespace そのものに見える id
        ":leading-colon",
        "a:b:c:d",
        "日本語の id",
        "x" * 300,
    ]

    def test_every_id_shape_round_trips(self):
        for ns in ("slack-cloud/mentions", "jira-cloud/assigned-issues", "boid/actions"):
            for raw in self.IDS:
                with self.subTest(namespace=ns, id=raw):
                    key = event_key_of(ns, raw)
                    self.assertEqual(namespace_from_key(key), ns)
                    self.assertEqual(envelope_id_of(key), raw)

    def test_prefixing_is_unconditional(self):
        """条件付き付与 (「既に付いていたら付けない」) は逆変換を曖昧にする —— 最初の
        実装がこの形で、「ack が別 id で飛びうる」を既知の限界として抱えていた。"""
        key = event_key_of("slack-cloud/mentions", "slack-cloud/mentions:C1:1.0")
        self.assertEqual(key, "slack-cloud/mentions:slack-cloud/mentions:C1:1.0")
        self.assertEqual(envelope_id_of(key), "slack-cloud/mentions:C1:1.0")


class GuardTest(unittest.TestCase):
    """組み立て側のバグを黙って通さない。握り潰すと「どの source instance のものか
    分からないまま子 ref を組む」という追いにくい壊れ方になる。"""

    def test_event_key_of_refuses_a_bad_namespace_or_id(self):
        for ns, raw in (("", "x"), ("a:b", "x"), ("ns/c", "")):
            with self.subTest(namespace=ns, id=raw), self.assertRaises(ValueError):
                event_key_of(ns, raw)

    def test_reading_refuses_a_malformed_key(self):
        for key in ("no-separator", ":body-only", "ns/c:"):
            with self.subTest(key=key):
                with self.assertRaises(ValueError):
                    namespace_from_key(key)
                with self.assertRaises(ValueError):
                    envelope_id_of(key)


if __name__ == "__main__":
    unittest.main()
