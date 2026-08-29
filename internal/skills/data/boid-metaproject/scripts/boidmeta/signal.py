"""外部/内部を問わず「新着があった」1 件。

**中身は持たない。** 検知の責務は「いつ判断を起こすかを決める」ことだけで、何が書かれて
いたかは判断 (subagent) が自分で読みに行く (§5.2「読みは自由」)。だからこの型には
`title` も `body` も無い —— あるのは「どの出来事か」(`event_key`)、「どの件か」
(`identity`)、「いつか」(`at`)、それに篩いと入口のための `author` / `url` だけ。

旧実装の `domain/models.py` の `Candidate` を置き換える。あちらは本文まで運んでいた ——
Slack のスレッド全文を展開し、Jira の description を ADF から平文へ潰して持ち歩いていた。
§9 の表「`adapters/` の Slack / Jira / Bitbucket の**中身取得** → 判断へ」がこの差。

## boid 内部 action もこの型に乗る (2026-08-28、PR-2)

かつては boid だけ別の型 (`domain/boid/action.Action`、削除済み) で読んでいた —— 対象を task id で
指し、栞が boid 発行の opaque cursor だったため。**PR-1 (boid core) が内部 action を
signal inbox へ ingest するようになり、読み口も
`boid signal list --claim` に一本化されたので、その差は消えた**
(`domain/boid/` は 2026-08-29 に削除)。boid 由来の signal は `source == "boid"` で、
`identity` が task id そのものである点だけが他 source と違う
(`app/sweep_targets.resolve_identities` がそこだけ別扱いする)。
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

from boidmeta.event_key import namespace_from_key


@dataclass(frozen=True)
class Signal:
    """新着 1 件。

    `identity` は「この出来事がどの件に属しうるか」の索引 (`slack-thread:<ts>` /
    `jira:<KEY>`)。**「どの件か」の最終判断は subagent が持つ** (S-15) —— ここに載るのは
    source が構造から機械的に決められる範囲の帰属であって、別 source 発の同一件を
    合流させる判断ではない。

    `author` は self-authored の篩い (`domain/screen.py`) が読む。**adapter がやるのは
    「本人の発言か」の 1 ビットへの正規化だけ** で、落とすかどうかは screen が決める。
    判定できない (投稿者が分からない / そもそも該当しない) 候補は `None` のままにする ——
    screen はこれをフェイルクローズ (落とさない) の合図として扱う。

    `url` は人と subagent が原文へ辿る入口。**本文の代わりではない** —— 判断は
    gateway 経由で読み直す。
    """

    source: str
    #: event_key の namespace (`<service>/<connector>`、service が空なら
    #: `<pack>/<connector>`)。**`source` とは別物** —— `source` は pack で、
    #: 「boid 内部 action か」の判定にだけ使う。同じ pack の service instance が
    #: 2 つあると `source` は同じでも namespace は違う。
    namespace: str
    event_key: str
    identity: str
    at: datetime
    author: str | None = None
    url: str | None = None

    def __post_init__(self) -> None:
        if self.at.tzinfo is None:
            # 栞になる値なので、naive を通すと adapter 側の比較で TypeError になるか、
            # tz を仮定した分だけずれた窓を読む。
            raise ValueError("Signal.at は tz-aware な datetime である必要がある")
        if not self.identity:
            raise ValueError("Signal.identity は空にできない")
        if not self.namespace:
            raise ValueError("Signal.namespace は空にできない")
        # **event_key の namespace と `namespace` フィールドの一致は、この型の要**。
        # `childid.child_id` が event_key から namespace を取り出して子 task の `ref`
        # に埋めるので、ズレると同じ出来事に別々の ref が付いて子が重複起票される。
        # ack/claim の逆変換 (`event_key.envelope_id_of`) も同じ前提に立つ。
        key_namespace = namespace_from_key(self.event_key)
        if key_namespace != self.namespace:
            raise ValueError(
                f"Signal.namespace と event_key の namespace が食い違う: "
                f"{self.namespace!r} vs {key_namespace!r} ({self.event_key!r})"
            )
