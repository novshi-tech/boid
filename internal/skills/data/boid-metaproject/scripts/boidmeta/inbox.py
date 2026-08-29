"""signal inbox を読み、判断に回す行を申告し、決着した行を決着させる。

boid の connector が inbox に書いた envelope 列を読み、`signal.Signal` へ写す層。
**判断は一切しない** —— 篩いは `detect`、判断は workspace のスキル。

## 3 段に分かれている (boid #1033)

- `read_pending` (`boid signal list`) —— **副作用なし**。判断する件数より広く読んで
  よい。複数 signal が 1 枚の card に畳まれるので、読む前には何件必要か分からない
- `claim` (`boid signal claim <id>...`) —— **判断に回す行を名指しする**。boid の
  `attempts` が進み、5 回 (`MaxSignalAttempts`) 誰も ack しなければ dead に落ちる
- `ack` (`boid signal ack <id>...`) —— 判断を書き終えた行

**読み出しが返した行を一律で数える形 (`list --claim`) は使わない。** 「読んだ」と
「判断に回した」は、読みが判断より広い consumer では別の数になり、次巡送りにしただけの
行が 5 巡で dead に落ちる。理由は `boid signal claim` の組み込みスキル
(`boid-signal`) と `orchestrator.ClaimSignalIDs` の doc comment に詳しい。

## envelope → Signal

`Signal.source` は envelope の `source.pack` **そのまま**。対応表は持たない ——
pack 名を別名へ写すと、その対応表がメタプロジェクトごとの設定項目になり、
子 task の `ref` にも焼き付く。

`Signal.event_key` は `<service>/<connector>:<envelope の id>`
(`event_key.namespace_of` + `event_key_of`)。**pack だけを鍵にしない** —— id は
workspace で一意ではないので、同じ pack の instance が 2 つあると別々の出来事が
1 本の鍵に潰れ、判断前に ack される。
ack / claim は元の envelope id へ戻して打つ (`event_key.envelope_id_of`)。

**読めない行は 1 件だけ諦める。** 壊れた envelope 1 件で巡全体を止めない。
"""
from __future__ import annotations

import sys
from datetime import datetime, timezone
from typing import Mapping, Sequence

from boidmeta.event_key import envelope_id_of, event_key_of, namespace_of
from boidmeta.signal import Signal

#: `boid signal list --limit N` の既定値。読みは副作用が無いので、判断する件数より
#: 広く読んでよい —— この既定は「1 巡で無制限に読もうとしない」ための上限であって、
#: 課金の綱渡りではない (旧 `list --claim` 時代は claim 件数の上限を兼ねており、
#: `MAX_TARGETS` から離すほど「見ただけの signal」が dead へ近づいた)。呼び出し側
#: (`sweep.READ_MULTIPLIER`) が対象上限に見合う値を渡す。
DEFAULT_LIMIT = 50

def read_pending(cli, *, limit: int = DEFAULT_LIMIT) -> "tuple[tuple[Signal, ...], bool]":
    """`boid signal list --state pending --limit <limit>` を読み、`Signal` 列にする。

    **副作用は無い** (2026-08-29、boid #1033)。読みは無料なので、判断する件数より
    広く読んでよい —— 複数 signal が 1 枚の card に畳まれるため、読む前には何件
    必要か分からない。判断に回す行は `claim` で別途名指しする。

    返り値は `(signals, ok)`。

    - **`ok=False` は「この巡は candidate 検知を諦める」の合図** —— 呼び出し側は
      この巡の判断を丸ごとスキップしてよい (フェイルオープンにする理由が無い ——
      2026-08-27 カットオーバー以前は「boid source は action list で引き続き読める」
      という代替経路があったが、PR-2 で boid source もこの inbox 読みに統合されたため、
      inbox が読めない巡は本当に何も読めていない)。
    """
    try:
        payload = cli.list_signals(state="pending", limit=limit)
    except Exception as exc:  # noqa: BLE001 - 読めなかった巡はこの source だけ諦める
        print(f"[inbox] boid signal list に失敗した: {exc}", file=sys.stderr)
        return (), False

    signals: list[Signal] = []
    for row in payload.get("signals") or ():
        signal = _to_signal(row)
        if signal is None:
            continue
        signals.append(signal)
    return tuple(signals), True


def claim(cli, event_keys: Sequence[str]) -> bool:
    """判断に回す signal (event_key) を名指しして `attempts` を進める
    (`boid signal claim <元の envelope id>...`)。

    **ack と同じく元の envelope id で打つ** —— `envelope_id_of` (Fix 1 の逆変換) で
    event_key から求める。

    **失敗は fatal にしない。** 課金できなかった巡は、その signal が dead へ 1 歩
    近づかないだけで、判断そのものは進む —— ここで巡を止める方が損失が大きい。
    次巡また同じ signal を読み直すので、そこで課金し直せばよい。

    **`ack` のような 1 件ずつの再試行はしない。** ack が再試行するのは取り逃しが
    恒久ロスだからで、claim は落としても次巡やり直せる。失敗した巡をそのまま諦める
    方が単純で害が無い。
    """
    if not event_keys:
        return True
    ids = [envelope_id_of(key) for key in event_keys]
    try:
        cli.claim_signals(ids)
        return True
    except Exception as exc:  # noqa: BLE001 - 次巡リトライに任せる、巡は止めない
        print(f"[inbox] boid signal claim に失敗した (次巡リトライ): {', '.join(ids)}: {exc}", file=sys.stderr)
        return False


def ack(cli, event_keys: Sequence[str]) -> bool:
    """settled / screened_out になった signal (event_key) をまとめて ack する
    (`boid signal ack <元の envelope id>...`)。

    **ack は元の envelope id で打つ** —— `envelope_id_of` (Fix 1 の逆変換) で
    event_key から求める。

    **失敗は fatal にしない** —— 握り潰さずログするが、例外は外に出さない。次巡も
    同じ id が pending のまま `read_pending` に出てくるので、そのときにまた ack を
    試みればよい (設計 §2「ack の失敗は fatal にしない (ログして次巡リトライ)」)。
    `event_keys` が空なら CLI を呼ばない (無駄な subprocess 起動を避ける)。

    **バッチが失敗したら 1 件ずつ ack し直す** (2026-08-28、Opus レビュー finding 5)。
    `boid signal ack <id>...` は typo guard 付き —— **1 件でも boid 側に無い id が
    混じると、呼び出し全体が失敗し、他の正しい id も 1 件も ack されない**
    (`internal/skills/data/boid-signal/SKILL.md`「存在しない id は呼び出し全体を
    失敗させる」)。**取り逃しは恒久ロス**なので、1 件の未知 id (同時に GC された、
    別経路で消えた等) に他の正しい signal を巻き込まないよう、バッチ呼び出しが失敗した
    ときだけ 1 件ずつ再試行する —— 大半の巡はバッチが通るので、subprocess の増加は
    失敗時だけに限られる。
    """
    if not event_keys:
        return True
    ids = [envelope_id_of(key) for key in event_keys]
    try:
        cli.ack_signals(ids)
        return True
    except Exception as batch_exc:  # noqa: BLE001 - 1 件ずつ再試行してから諦める
        print(
            f"[inbox] boid signal ack (一括) に失敗した、1 件ずつ再試行する: "
            f"{', '.join(ids)}: {batch_exc}",
            file=sys.stderr,
        )
        ok = True
        for id_ in ids:
            try:
                cli.ack_signals([id_])
            except Exception as exc:  # noqa: BLE001 - 次巡リトライに任せる、巡は止めない
                print(f"[inbox] boid signal ack に失敗した (次巡リトライ): {id_}: {exc}", file=sys.stderr)
                ok = False
        return ok


def _to_signal(row: object) -> "Signal | None":
    """envelope 1 行を `Signal` にする。**読めない/マッピングできない行は `None`** ——
    1 件の壊れた envelope で巡全体を止めない (他の adapter の `fetch` と同じ「読めた分
    だけ返す」規律)。
    """
    if not isinstance(row, Mapping):
        return None

    source_block = row.get("source") if isinstance(row.get("source"), Mapping) else {}
    pack = source_block.get("pack")
    connector = source_block.get("connector")
    service = source_block.get("service") or ""
    if not isinstance(pack, str) or not pack or not isinstance(connector, str) or not connector:
        print(
            f"[inbox] source.pack / source.connector の無い envelope を無視した: {row.get('id')!r}",
            file=sys.stderr,
        )
        return None
    source = pack
    # **namespace は service instance まで含める。** id は workspace で一意ではない
    # ので、pack だけで鍵を作ると別 instance の同じ id が 1 本に潰れる
    # (`event_key` のモジュール docstring)。
    try:
        namespace = namespace_of(service=str(service), connector=connector, pack=pack)
    except ValueError as exc:
        print(f"[inbox] source から namespace を組めない envelope を無視した ({row.get('id')!r}): {exc}", file=sys.stderr)
        return None

    raw_id = row.get("id")
    identity = row.get("identity")
    at = _parse_at(row.get("occurred_at"))
    if not isinstance(raw_id, str) or not raw_id:
        print(f"[inbox] id の無い envelope を無視した: {row!r}", file=sys.stderr)
        return None
    if not isinstance(identity, str) or not identity:
        print(f"[inbox] identity の無い envelope を無視した (id={raw_id!r})", file=sys.stderr)
        return None
    if at is None:
        print(f"[inbox] occurred_at を読めない envelope を無視した (id={raw_id!r})", file=sys.stderr)
        return None

    author = row.get("author")
    author = author if isinstance(author, str) else None
    url = row.get("url")
    url = url if isinstance(url, str) else None
    # `title` はここで読まない —— `Signal` に無いフィールドなので、素通しにするだけで
    # 「捨てる」設計 (§1) が自動的に満たされる。

    # event_key は `<source>:<envelope の id>`。**無条件に付ける** —— 条件付きだと
    # ack/claim の逆変換が「元から付いていたのか」を区別できない (`event_key` の
    # モジュール docstring)。
    try:
        return Signal(
            source=source,
            namespace=namespace,
            event_key=event_key_of(namespace, raw_id),
            identity=identity,
            at=at,
            author=author,
            url=url,
        )
    except ValueError as exc:
        # 不変条件を満たさない envelope。この 1 件だけ諦め、同じ巡の他の envelope は
        # 道連れにしない。
        print(f"[inbox] envelope を Signal にマッピングできなかった (id={raw_id!r}): {exc}", file=sys.stderr)
        return None


def _parse_at(value: object) -> "datetime | None":
    """`occurred_at` (RFC3339) を tz-aware な `datetime` にする。**読めなければ None**
    —— 他の adapter の壊れた値の扱いと同じ「壊れた 1 件で止めない」規律
    (壊れた 1 件で列全体を落とさない、という他の adapter と同じ判断)。
    """
    if not isinstance(value, str) or not value:
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    return parsed if parsed.tzinfo is not None else parsed.replace(tzinfo=timezone.utc)
