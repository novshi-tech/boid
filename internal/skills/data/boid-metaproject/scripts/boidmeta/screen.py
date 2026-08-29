"""自己発言 (メタプロジェクト自身が書いた発言) を落とす篩い。

以前は adapters (`jira.py`/`bitbucket.py`) がそれぞれ独自に self-authored 判定ロジックを
持っていた。**同じ関心事が 2 箇所に散っていた** — ここへ 1 箇所にまとめる。

adapters (`SignalSource`) は `Signal.author` に生の投稿者 ID を埋めるだけで、篩いはしない
(取ってくるだけ、設計 §5.1)。ここが「落とすかどうか」を判断する唯一の場所。

「自分が誰か」は設定値なので、判定関数に `mine` として引数で渡す。既定値 `SELF` は
adapters との共有語彙 — adapters は生の author ID (Jira の accountId、Bitbucket の
display_name 等、source ごとに形式が違う) を自分自身の識別子と比較し、一致すれば
`Signal.author` にこの定数を書く (adapters 側で行うのは「本人の発言か」という
1 ビットへの正規化だけであり、それを踏まえて「落とすかどうか」を判断するのはここ、
というのが項目 4 の分担)。
"""
from __future__ import annotations

from typing import Protocol, Sequence

#: `Signal.author` がこの値のとき「自分自身」を意味する共有語彙。
SELF = "self"


class Authored(Protocol):
    """投稿者が分かる (かもしれない) 候補。

    いまこれを満たすのは `domain/signal.Signal` だけ。**この関数が見るのは `author` の
    1 フィールドだけ**なので、それでも具体型に縛らない —— `screen.py` が `signal.py` を
    知る必要が無く、判定を値だけでテストできる状態が保てる。
    """

    author: str | None


def self_authored(candidates: Sequence[Authored], *, mine: str = SELF) -> bool:
    """`candidates` (同じ identity の、この巡の未処理候補群) が「自分自身の発言だけが
    実体」かどうか (引き継ぎメモ §6.2 原文)。

    > 自分自身の発言・操作だけが実体である候補は A に倒す (`skip_reason: "self-authored"`)。
    > ... 判定は候補の未処理 event の author で行う。他人の発言が 1 つでも混ざっていれば
    > B 側に倒す。

    - `candidates` が空なら False (何も無いものを self 認定しない)
    - `author` が不明 (`None`) な候補が 1 件でもあれば False —— フェイルクローズ。
      「迷ったら落とさず」の唯一の例外である self-authored 除外自体も、判定できない
      ときは除外しない (引き継ぎメモ §6.1「迷ったら落とさず」原則、§6.2)。
      adapters はこの性質を使って「self 判定にかけない候補」を表現できる —— 例えば
      Jira の `<KEY>:issue` 候補 (誰かが自分にアサインした、という他人からの push) は
      常に `author=None` で送られてくるので、この関数に混ぜても自動的に判定から除外される
      (引き継ぎメモ §6.2「adapter は『self 判定にかけない event』を定義してよい」)
    - `author` が全員 `mine` と一致すれば True。1 件でも違えば False

    引数名が `candidates` なのは引き継ぎメモ §6.2 の原文に合わせているため。渡るのは
    `Signal` (この巡の未処理の新着) であって、旧実装の `Candidate` ではない。
    """
    if not candidates:
        return False
    for c in candidates:
        if c.author is None or c.author != mine:
            return False
    return True


# ---------------------------------------------------------------------------
# 旧: boid source 由来 signal の 2 段の絞り (「自分自身の書き込みを落とす」) は
# ここにも `sweep` にも無い。**boid core が ingest の時点で同じ判定をする** ——
# `internal/orchestrator/signal_ingest_bridge.go` の `IngestActionSignal` が、書き込み元
# job の project が対象 workspace のメタプロジェクトなら signal を書かない。メタ
# プロジェクトの task も、そこから fork される subagent も、その子 task も全部その
# project の sandbox で走るので、書いた action は inbox に載らない。
#
# `actor == "daemon"` の action (子 task 終端の `child_closed` 等) は意図して通る ——
# 「外で仕事が終わった」検知そのもので、自分の書き込みではない。
