"""検知 —— いつ判断を起こすかを決める。

**中身は見ない。** シグナルが来たかどうかだけを決めて、判断は sweep task の subagent に
渡す。ここが緩いと何も起きていないのに考え続け、厳しいと世界が動いても気づかない。

この関数が決めるのは:

- **対象** —— どの task について考え直すか (sweep task の instruction に埋める)
- **`screened_out`** —— 判断を経ずに ack してよい event_key (機構が決定的に落としたもの)

boid を叩く部分 (どの status を引くか、actor の behavior をどう解決するか) は
呼び出し側にある。ここは値を受けて値を返すだけ。

**2026-08-29 に `plan_boid`/`Plan` (boid source 専用の計画組み立て) と、
`dead letter` (workspace 側の独自 attempts 機構、`domain/attempts.py`) を削除した。**
`plan_boid` は 2026-08-28 の inbox 一本化 で `run_once`/
`app/sweep_targets.py` のどちらからも呼ばれなくなり (`boid action list` を読む検知は
`boid signal list --claim` の inbox 読みに一本化された)、ロールバック用にコードだけ
残っていたが、依存先 (`domain.attempts`/`domain.boid.action.cursor_of`/
`records_in`/`domain.screen.is_signal`) を今回まとめて削除するのに合わせて道連れに
した。dead letter (上限まで試して決着しなかったものを諦める) は boid 側の
`MaxSignalAttempts` (5、`internal/skills/data/boid-signal/SKILL.md`) にそのまま乗る
設計に変わったため、`plan_candidates` はもう `records`/`max_attempts` を受け取らない
(`app/sweep_targets.py` が渡していた `records=(), max_attempts=0` は常に no-op
だった —— この関数は元々「0 以下なら何も枯渇させない」規約だったので、実質的な
挙動は変わらない)。**時刻栞 (`Scan`/`domain.signal.mark_of`) を進める側
(`CandidatePlan.scans`、`domain/cursor.Bookmarks`/`advance`) も同時に削除した** ——
唯一の呼び出し元 (`sweep`) はこの栞を読んでおらず (検知が
`boid signal list --claim` の pending/ack へ一本化されているため)、栞ファイル
(`adapters/file_cursor.py`) も同時に削除した。**残っていた `Scan` (`NamedTuple`) を
持つ `domain/cursor.py` も 2026-08-29 に削除した** —— 型として参照する側が既に無かった。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping, Sequence

from boidmeta.screen import self_authored
from boidmeta.signal import Signal

#: 記録を書けない status (篩い 5)。**`done` は入らない** —— done の card には I-5b の
#: service 層ガード (`internal/api/attrs_set_done.go`) 経由で attrs_set が通り、
#: 判断側はそこへ `observed`/`reopen` を書ける (S-9 の再燃経路)。auto-reopen 機構
#: (`SweepReopen`) 自体は card 機械 v2 で撤去済み (設計 §3.3) —— 再燃は判断側が
#: `reopen` を suggest して人の accept を経る形に代わった。
#:
#: **`dropped` も 2026-08-25 (PR-K レビュー MEDIUM 2) にここから外したが、これは
#: 「到達している経路を救った」わけではない。** `drop` (直接 / accept どちらも)
#: は card の identity を全解放する
#: (`internal/orchestrator/task_identity.go` の `UnlinkAllForTask` —— done は
#: identity を保持するが drop は手放す、という意図的な非対称性)。identity が無い
#: card は `resolve_identity` に解決先を持たないので、**`resolved` (呼び出し側が
#: identity → (task_id, status) で解決した結果) に dropped の task_id が乗ること
#: は通常の巡では起きない** —— そのソースが再燃しても新規候補として扱われ、
#: 別の (元の card とは無関係な) task を `capture` する。
#:
#: **ただし「今日到達しうる経路」が実在する — 将来の変更を待つ話ではない。**
#: `LinkIdentity` (`internal/orchestrator/task_identity.go`) は空文字チェックと
#: 「別 task への既存 binding」チェックしかせず、対象 task の status を一切見ない。
#: drop は既に identity 行を消しているので衝突する既存 binding も無く、
#: `boid task identity link <identity> <dropped-card-id>` は今日でも成功する。
#: **カットオーバー runbook §6 手順3 がまさにこの `link`/`unlink` 操作を使う**
#: (`docs/plans/suggestion-as-state-transition-impl.md`)。オペレータが dropped card に
#: キーを再 link すれば、次の巡から `resolve_identity` が `(card, "dropped")` を返し、
#: この分岐が実際に発火する。**「識別子機構の将来変更に備えた保険」ではない** ——
#: 消してよい死んだコードだと判断しないこと (PR-K レビュー finding 2)。
NO_RECORD_STATUSES = frozenset({"aborted"})

#: 1 巡あたりに起こす対象の上限 (篩い 6)。溢れたぶんは次の巡に回す。
#:
#: **無駄な sweep task が立つコストは実行時間だけではない。終了済み task の一覧が汚れて、
#: 人が過去を追えなくなる。** 実測 (2026-08-22) で生きている task は 10 枚程度なので、
#: シグナル駆動なら 1 巡は通常 0〜数本。この桁を前提にした値。
MAX_TARGETS = 8


@dataclass(frozen=True)
class Target:
    """sweep task に渡す 1 対象 (1 対象 = 1 subagent、§5.2)。

    **指し方が 2 通りある。** boid シグナルと、既存 task に解決できた非 boid シグナルは
    `task_id` で指す。解決できなかった非 boid シグナルは新規候補なので `identity` だけを
    持つ —— 起票するかどうかは subagent の判断 (S-15) であり、検知は決めない。

    `identity` は解決できた対象にも入る (subagent が `link` verb を打つときに要る)。

    `url` は原文への入口 (`Signal.url`)。**新規候補には task も description も無い**ので、
    ここで運ばないと subagent は identity から探し直すことになる。boid シグナル発の
    対象は持たない (task を読めば済む)。
    """

    task_id: str = ""
    signals: tuple[str, ...] = ()
    identity: str = ""
    url: str = ""

    def __post_init__(self) -> None:
        if not self.task_id and not self.identity:
            # どちらも無いと instruction に「何を読めばよいか」が書けない。1 行だけ立って
            # subagent が何もできない対象になる。
            raise ValueError("Target は task_id か identity のどちらかを持つ必要がある")


# ---------------------------------------------------------------------------
# 非 boid source (slack / jira / bitbucket) —— boid source (旧 `plan_boid`/`Plan`) は
# 2026-08-29 に削除した (このファイルのモジュール docstring 参照)。
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class CandidatePlan:
    """非 boid source について、この巡で何をするか。

    `screened_out` は **機構の篩いで「この signal への判断はもう済んだ」と決定的に
    結論した event_key の集合** (self-authored、または `NO_RECORD_STATUSES` 合流) ——
    どちらも記録を書かずに済ませる。呼び出し側 (`app/sweep_targets.py`) はこの集合を
    `boid signal ack` する (2026-08-27 カットオーバー、inbox 化の Fix 2) ——
    「判断を下すまでもなく機構が決定的に落とした」もの。

    **含まれないもの** (まだ判断待ちなので ack してはいけない): `targets` になった
    signal (既存 target への合流も含む) と、篩い 6 (`max_targets`) で溢れて次巡に
    回った signal。どちらも「次に見るべきもの」であり続ける必要がある。

    **2026-08-29 に `scans`/`assignments`/`abandoned` を削除した。**
    workspace 側の独自 attempts 機構 を撤去し boid 側の
    `MaxSignalAttempts` にそのまま乗ったのに伴い、`assignments` (割り当ての記録) と
    `abandoned` (dead letter の記録) はどちらも `Record` を生成するだけの死んだ経路に
    なっていた —— 唯一の呼び出し元 `app/sweep_targets.py` の `build()` はこの 2
    フィールドを読んでいない (`records=(), max_attempts=0` で呼んでいたので `abandoned`
    は元々常に空だった)。`scans` (source ごとの時刻栞) も同じ呼び出し元では読まれて
    おらず、`domain/cursor.py` の `Bookmarks`/`advance` (この栞を実際に進める側) を
    ここで削除したのに合わせて道連れにした (`domain/cursor.py` 自体も 2026-08-29 に削除)。
    """

    targets: tuple[Target, ...] = ()
    screened_out: frozenset[str] = frozenset()


def plan_candidates(
    signals: Sequence[Signal],
    *,
    resolved: Mapping[str, tuple[str, str]],
    max_targets: int = MAX_TARGETS,
) -> CandidatePlan:
    """非 boid source のシグナルから、この巡の計画を組む。

    `signals` は **source ごとに古い順**に並んでいること (このリストの並び順で
    `url` の「最後のもの」判定などが決まる)。source をまたいだ順序は問わない。

    `resolved` は identity → **(task id, status)**。**引けたものだけ**を入れる
    (`boid task identity resolve` は未登録を exit code で表す)。ここに無い identity は
    新規候補になる。**status まで持つ**のは篩い 5 が status で判定するため —— 「open な
    task の集合に居るか」で判定すると `done` まで落ちて、S-9 の再燃経路が死ぬ。

    **2026-08-29 に `records`/`max_attempts` を削除した。** workspace 側の独自
    attempts 機構を撤去したので、settled/dead letter 判定はもう行わない —— `signals`
    に渡ってくるのは `boid signal list --claim` が返す未処理 (pending) の signal
    そのものであり、「もう決着した event_key」を除く必要が無くなった (旧実装の
    `settled`/`dead` はこの呼び出し元では常に空集合だった、`app/sweep_targets.py` が
    `records=(), max_attempts=0` で呼んでいたため)。
    """
    grouped: dict[str, list[Signal]] = {}
    for signal in signals:
        grouped.setdefault(signal.identity, []).append(signal)

    targets: list[Target] = []
    #: 機構の篩いで「判断済み」と決定的に結論した event_key (2026-08-27、Fix 2)。
    screened_out: set[str] = set()
    slots: set[str] = set()

    for identity, group in grouped.items():
        task_id, status = resolved.get(identity, ("", ""))
        if task_id and status in NO_RECORD_STATUSES:
            # 篩い 5: aborted のみ (2026-08-25 訂正、PR-K レビュー MEDIUM 2)。card 機械
            # v2 はこの status に到達しないので、通常タスクの aborted を拾う必要は無い。
            #
            # **`done` は落とさない** —— done の card には `reopen` を suggest
            # できる経路が開いている (I-5b ガード)。ここで落とすと「終わったと思って
            # いた件に続きが来た」が二度と検知されず、シグナルは栞に跨がれて**静かに
            # 消える**。設計が「現行実装は観測側が done を除外していたためこの経路が
            # 一度も発火しなかった。同じ轍を踏まない」と名指しで警告している当のもの。
            #
            # **`dropped` は通常の巡ではこの分岐に来ない** —— `drop` は card の
            # identity を解放する (`internal/orchestrator/task_identity.go` の
            # `UnlinkAllForTask`) ので、`resolved` (呼び出し側の identity 解決結果) に
            # dropped の task_id が乗ることは普段は無い。
            #
            # **ただし今日到達しうる経路がある。** `LinkIdentity` は対象 task の status を
            # 検査しない (空文字チェックと別 task への既存 binding チェックのみ) ので、
            # drop 済みで binding が消えた card へは `boid task identity link` が
            # 今日でも成功する。カットオーバー runbook §6 手順3 がこの link/unlink 操作を
            # 実際に使う。オペレータが再 link すれば次の巡でこの分岐が発火するので、
            # `NO_RECORD_STATUSES` から dropped を外したことは「将来の変更に備えた保険」
            # ではなく**今日到達しうる経路への対応** —— 死んだコードと判断して消さない
            # こと (PR-K レビュー finding 2)。
            #
            # **記録は書かないが、判断は済んでいる** (Fix 2)。この identity の group は
            # 二度と対象にならない決定的な判定なので、呼び出し側が ack してよい対象として
            # `screened_out` に積む。
            screened_out.update(signal.event_key for signal in group)
            continue

        if self_authored(group):
            # 篩い 2: 自分の発言だけが実体。
            #
            # **記録は書かないが、判断は済んでいる** (Fix 2、上の NO_RECORD_STATUSES と
            # 同じ理由)。self-authored は author を見るだけの決定的な判定なので、次巡
            # 読み直しても同じ結論になる —— ack してよい。
            screened_out.update(signal.event_key for signal in group)
            continue

        keys = [signal.event_key for signal in group]
        if not keys:
            continue
        # 原文への入口は**最後のもの**を採る —— 古い permalink より、いま起きたことの
        # 入口のほうが役に立つ。
        url = next((signal.url for signal in reversed(group) if signal.url), "")
        if task_id and task_id in slots:
            # 既に枠を取っている task。合流させるだけなので枠は消費しない。
            targets.append(Target(task_id=task_id, signals=tuple(keys), identity=identity, url=url))
            continue
        if len(slots) >= max_targets:
            # 篩い 6: 溢れたぶんは次の巡に回す。
            continue
        slots.add(task_id or identity)
        targets.append(Target(task_id=task_id, signals=tuple(keys), identity=identity, url=url))

    return CandidatePlan(targets=tuple(targets), screened_out=frozenset(screened_out))
