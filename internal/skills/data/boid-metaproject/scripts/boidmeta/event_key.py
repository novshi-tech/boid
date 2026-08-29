"""event_key の書式 —— `<source>:<connector が発行した id>`。

**source を先頭に置くのは、event_key 1 本から source を復元できるようにするため。**
記録 CLI (`write`) は subagent から event_key の文字列しか受け取らない —— そこから
子 id を組む (`childid.child_id`) にも、ack する元の envelope id へ戻すにも、source が
要る。別フィールドで持ち回る形にすると、その 2 つの経路それぞれに引数が 1 本増え、
LLM が埋めるフィールドも 1 つ増える。

**付与は無条件。** connector が発行した id が偶然 `"<source>:"` で始まっていても
剥がさず、もう一度付ける。条件付きにすると `envelope_id_of` の逆変換が
「元から付いていたのか、こちらが付けたのか」を区別できなくなり、**ack が別の id で
飛んで無言に失敗する**。khi の初期実装がこの形で、「既知の限界」として docstring に
書かれたまま運用されていた。無条件なら最初の `:` で切るだけで厳密に戻せる。

見た目の代償は、id が自分で source を名乗る connector で `slack:slack:C1:1.0` の
ように重なること。**connector 側は id に source を含めなくてよい** (含めても壊れない)。
"""
from __future__ import annotations


def event_key_of(source: str, envelope_id: str) -> str:
    """envelope の `source.pack` と `id` から event_key を組む。"""
    if not source:
        raise ValueError("source が空")
    if ":" in source:
        raise ValueError(f"source に区切り文字 ':' を含められない: {source!r}")
    if not envelope_id:
        raise ValueError("envelope id が空")
    return f"{source}:{envelope_id}"


def source_of(event_key: str) -> str:
    """event_key の source。

    **黙って落とさない。** event_key はこの機構自身が組み立てるものなので、区切りが
    無い / 本体が空なのは呼び出し側のバグであり、握り潰すと「source が分からないまま
    子 id を組む」という追いにくい壊れ方になる。
    """
    source, separator, rest = event_key.partition(":")
    if not separator:
        raise ValueError(f"event_key に source の区切りが無い: {event_key!r}")
    if not source:
        raise ValueError(f"event_key の source が空: {event_key!r}")
    if not rest:
        # `jira:` のような prefix だけの key。source は読めるが「どの出来事か」を
        # 指していない —— 記録に書けばその source の全イベントと同じキーになる。
        raise ValueError(f"event_key に本体が無い: {event_key!r}")
    return source


def envelope_id_of(event_key: str) -> str:
    """event_key から connector が発行した元の id を戻す (`event_key_of` の逆)。

    `boid signal claim` / `boid signal ack` は**元の id** で打つ —— inbox の行は
    connector が発行した id で索かれるので、source を付けたままでは見つからず、
    typo guard で呼び出し全体が失敗する。

    付与が無条件なので、最初の `:` で切るだけで厳密に戻る (モジュール docstring)。
    """
    source_of(event_key)  # 形の検証。ここで落ちるなら組み立て側のバグ
    return event_key.partition(":")[2]
