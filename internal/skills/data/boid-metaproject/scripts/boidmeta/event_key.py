"""event_key の書式 —— `<namespace>:<connector が発行した id>`。

## namespace は「どの source instance か」を一意に指す

inbox の行は `(workspace_id, service, connector, id)` で索かれる —— **id だけでは
一意にならない** (migration 0046 が明記)。同じ workspace に同じ pack の service
instance が 2 つ居れば (jira サイト 2 つ、等)、両方が `PROJ-1:<時刻>` のような同じ
id を発行しうる。

id だけ、あるいは pack + id を鍵にすると、その 2 行が 1 本の event_key に潰れる。
潰れると 1 巡の中で **同じ鍵が「篩いで落とした」と「判断に回す」の両方に入り**、
`sweep` は ack を先に打つので、**まだ判断していない signal が判断前に決着する**。
2026-08-29 のレビューで実際に再現した。

なので namespace は `<service>/<connector>` —— service instance 名は workspace の
config のキーなので一意で、connector 名がその中を割る。service が空の envelope
(boid 内部 action) だけ pack で代用する。

## 付与は無条件

connector が発行した id が偶然 `"<namespace>:"` で始まっていても剥がさず、もう一度
付ける。条件付きにすると `envelope_id_of` の逆変換が「元から付いていたのか、こちらが
付けたのか」を区別できなくなり、**ack が別の id で飛んで無言に失敗する**。無条件なら
最初の `:` で切るだけで厳密に戻せる。

## 残っている限界 (core 側)

`boid signal ack <id>` は `(workspace_id, id)` だけで照合し、service/connector を
問わず**同じ id の行を全部 ack する** (`orchestrator.AckSignals` の doc comment が
そう明言している)。event_key を一意にしてもここは変わらない —— 2 つの instance が
同じ id を出していれば、片方を ack すると両方が決着する。1 巡の中では対象の組み立てが
先に済んでいるので判断そのものは行われるが、sweep がその間に死ぬと片方を取り逃す。
解くなら ack を `(service, connector, id)` に絞る core 側の変更が要る。
"""
from __future__ import annotations


def namespace_of(*, service: str, connector: str, pack: str) -> str:
    """envelope の source ブロックから event_key の namespace を組む。

    `service` が空 (boid 内部 action) のときだけ `pack` で代用する —— 内部 action に
    service instance という概念が無いため。
    """
    origin = service or pack
    if not origin:
        raise ValueError("service も pack も空")
    if not connector:
        raise ValueError("connector が空")
    for part in (origin, connector):
        if ":" in part:
            raise ValueError(f"namespace の部品に区切り文字 ':' を含められない: {part!r}")
    return f"{origin}/{connector}"


def event_key_of(namespace: str, envelope_id: str) -> str:
    """namespace と envelope の id から event_key を組む。"""
    if not namespace:
        raise ValueError("namespace が空")
    if ":" in namespace:
        raise ValueError(f"namespace に区切り文字 ':' を含められない: {namespace!r}")
    if not envelope_id:
        raise ValueError("envelope id が空")
    return f"{namespace}:{envelope_id}"


def namespace_from_key(event_key: str) -> str:
    """event_key の namespace。

    **黙って落とさない。** event_key はこの機構自身が組み立てるものなので、区切りが
    無い / 本体が空なのは呼び出し側のバグであり、握り潰すと「どの source instance の
    ものか分からないまま子 ref を組む」という追いにくい壊れ方になる。
    """
    namespace, separator, rest = event_key.partition(":")
    if not separator:
        raise ValueError(f"event_key に namespace の区切りが無い: {event_key!r}")
    if not namespace:
        raise ValueError(f"event_key の namespace が空: {event_key!r}")
    if not rest:
        # `jira-cloud/assigned-issues:` のような prefix だけの key。namespace は
        # 読めるが「どの出来事か」を指していない —— 子 ref に使えばその source の
        # 全イベントが同じ id に潰れる。
        raise ValueError(f"event_key に本体が無い: {event_key!r}")
    return namespace


def envelope_id_of(event_key: str) -> str:
    """event_key から connector が発行した元の id を戻す (`event_key_of` の逆)。

    `boid signal claim` / `boid signal ack` は**元の id** で打つ —— inbox の行は
    connector が発行した id で索かれるので、namespace を付けたままでは見つからず、
    typo guard で呼び出し全体が失敗する。

    付与が無条件なので、最初の `:` で切るだけで厳密に戻る (モジュール docstring)。
    """
    namespace_from_key(event_key)  # 形の検証。ここで落ちるなら組み立て側のバグ
    return event_key.partition(":")[2]
