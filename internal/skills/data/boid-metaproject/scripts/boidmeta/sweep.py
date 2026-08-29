"""sweep task 自身が起動直後に対象一覧を組む (2026-08-28、PR-2 §6.1 決定事項 2)。

    対象一覧を誰が組むか: sweep task 自身が起動直後に `boid signal list --claim` を
    叩いて対象を組む。trigger の `run:` は sweep task を起こすだけにする
    (daemon 側から workspace の手順を指示するとバグる、という既存の教訓に沿う)

    python3 ~/.claude/skills/boid-metaproject/scripts/sweep_targets.py --judge-skill /<スキル>

`.boid/project.yaml` の `sweep` behavior の `default_instruction` が、最初の一手として
これを実行する。やることは:

1. `boid signal list` で pending signal を読む (`boidmeta.inbox`)。**副作用は無い**
2. identity を解決し (`resolve_identities`)、篩いをかけて対象へ畳む
   (`merge_targets`、`app/detect.plan_candidates`)
3. 機構が決定的に落とした signal (self-authored、aborted 合流) はその場で ack する ——
   判断が要らないので、判断待ちの subagent を待たずに ack してよい (§6.1 決定事項 4
   「篩いで落とした signal を ack するか: ack する」)
4. **判断に回す signal を `boid signal claim` で名指しする** (2026-08-29、boid #1033)
5. 残った対象を自分の description に書き込む (`instruction`、`boid task update`)。
   以降は `sweep` behavior の instruction が対象ごとに subagent を fork する

**target になった signal は ack しない。** 判断が要るので、担当した subagent が判断を
書いた直後に自分で ack する (`app/write.py` の `Executor._record`、§6.1 決定事項 6
「ack を打つ順序: sweep が判断を書いた直後に自分で ack する。ack を先に打ってはいけない」)。

**boid-pack で identity (= task id) を解決できなかった signal も ack しない**
(2026-08-28、Opus レビュー finding 7)。「本当に task が無い」のか「daemon の瞬断等で
一時的に引けなかっただけ」かを区別できないので、対象にもせず (`capture` の誤爆を
防ぐ)、ack もしない (誤って恒久ロスさせない) —— 次巡の読みに委ねる。
**ただし claim はする** (2026-08-29): 「渡そうとしたが解決できなかった」も 1 回の
試行であって、これを数えないと真に解決できない signal が永久に毎巡返り続ける。
5 回で boid 側の `MaxSignalAttempts` により dead に落ちる、という緩やかな諦め方は
そのまま残る。

## 何を claim するか (2026-08-29、boid #1033)

`attempts` は「諦めるまでの回数」なので、数える対象は「判断に回した」でなければ
ならない。この巡が claim するのは 2 種類だけ:

- **target になった signal** —— subagent に渡す
- **identity を解決できなかった boid signal** —— 渡そうとして失敗した

claim しないもの:

- **篩いで落とした signal** —— 同じ巡でそのまま ack するので数える意味が無い
- **`MAX_TARGETS` から溢れて次巡送りにした signal** —— **これがこの変更の目的**。
  旧 `list --claim` は読み出しが返した行を一律で数えたので、`CLAIM_LIMIT`
  (`MAX_TARGETS * 4`) と `MAX_TARGETS` の差 (最大 24 件) が毎巡課金され、5 巡で
  誰も判断していない signal が無言で dead に落ちていた

## khi 自身の書き込みを落とす篩いは boid core が持っている (2026-08-29)

旧 `_split_own_writes`/`_is_own_write` (boid-pack signal の `author` を引いて
「khi 自身の sweep task が書いたか」を判定していた、旧 S-9 の actor 軸) は削除した。
**同じ判定を boid core が ingest の時点で行っている** ——
`internal/orchestrator/signal_ingest_bridge.go` の `IngestActionSignal` が、書き込み元
job の project (`WriterProjectIDFromContext`) が対象 workspace のメタプロジェクトと
一致する action を ingest しない (§4.3 actor 軸)。khi の sweep task も、そこから
fork される subagent も、`respond` の子 task も全部 khi-task-collector project の
sandbox で走るので、書いた action は inbox に載らない。

実データでも確認済み (2026-08-29): PR-1 デプロイ後 24 時間で inbox に載った
boid-pack signal は 2 件だけで、どちらも**ホスト側 CLI から手で打った動作確認用の
notify** (sandbox 経由でないので `WriterProjectIDFromContext` が writer を持たず、
遮断の対象外になる)。同じ期間に khi の sweep task が card へ書いた action
(`attrs_set`/`child_added`/`child_specced` 等) は 1 件も ingest されていない。

なお **`actor == "daemon"` の action (子 task 終端の `child_closed` 等) は意図して
通る** —— 「外で仕事が終わった」検知そのものであって、khi 自身の書き込みではない。

## khi 独自の attempts は撤去した (§6.1 決定事項 5)

**2026-08-29、PR-2やり直しv2 で `domain/attempts.py` をファイルごと削除し、
`plan_candidates` から `records`/`max_attempts` 引数自体を消した**。dead-letter は
boid 側の `MaxSignalAttempts` (5、`--claim` が進める、
`internal/skills/data/boid-signal/SKILL.md`「Dead Signals」) にそのまま乗る。
"""
from __future__ import annotations

import sys
from dataclasses import dataclass
from typing import Mapping, Sequence

from boidmeta import inbox
from boidmeta.boid_store import BoidCLI, BoidError
from boidmeta.detect import MAX_TARGETS, Target, plan_candidates
from boidmeta.signal import Signal

#: boid 内部 action 由来の signal の `Signal.source` (`adapters/inbox.PACK_TO_SOURCE`)。
#: **この source だけ identity が task id そのもの** —— `resolve_identities` 参照。
BOID_SOURCE = "boid"

#: 1 巡で読む件数 = `max_targets * READ_MULTIPLIER`。**読みは無料なので、判断する
#: 件数より広く読む** —— 複数 signal が同じ identity/card に畳まれるため、読む前には
#: 何件必要か分からない。倍率 4 はその合流ぶんの遊び。
#:
#: 旧 `list --claim` 時代はこの倍率が綱渡りだった (読み出しが返した行を一律で課金する
#: ので、`max_targets` から離すほど「見ただけの signal」が dead へ近づいた)。読みと
#: 申告を分けた (boid #1033) のでその危険は消え、純粋な読みの上限になった。
READ_MULTIPLIER = 4


@dataclass(frozen=True)
class Round:
    """1 巡ぶんの計画。`main` が ack / claim / description 書き込みに使う。

    3 つの集合が**重ならない**のがこの型の要 (モジュール docstring「何を claim
    するか」):

    - `targets` の signal —— 判断に回す。**claim する**、ack は subagent が判断を
      書いた直後に自分で打つ
    - `screened_out` —— 機構が決定的に落とした。**ack する**、claim はしない
    - どちらにも入らなかった signal (`MAX_TARGETS` 溢れ) —— 何もしない
    """

    targets: tuple[Target, ...] = ()
    screened_out: frozenset[str] = frozenset()
    to_claim: frozenset[str] = frozenset()
    #: この巡で読んだ signal の件数。`read - len(to_claim) - len(screened_out)` が
    #: 「次巡送りにした件数」で、**それを人が見られるようにするのがこのフィールドの
    #: 唯一の役目** —— 溢れは無言で起きるので、`READ_LIMIT` と `MAX_TARGETS` の差が
    #: 実運用で効いているかどうかを実データで判断する材料が他に無い。
    read: int = 0
    ok: bool = True

    @property
    def deferred(self) -> int:
        """この巡が触らなかった signal の件数 (`MAX_TARGETS` 溢れ)。次巡そのまま
        読み直される。**0 でない巡が続くなら `READ_LIMIT`/`MAX_TARGETS` を見直す
        合図**。"""
        return max(0, self.read - len(self.to_claim) - len(self.screened_out))


def build(cli, *, max_targets: int = MAX_TARGETS, limit: int | None = None) -> "Round":
    """この巡の対象一覧を組む。

    `Round.screened_out` は**判断を経ずに ack してよい** event_key の集合、
    `Round.to_claim` は**判断に回すので attempts を進める** event_key の集合
    (`main` がそれぞれ `inbox.ack` / `inbox.claim` に渡す)。`ok=False` は
    `boid signal list` が読めなかった合図 —— 何もせず次巡に委ねる。
    """
    limit = limit if limit is not None else max_targets * READ_MULTIPLIER
    signals, ok = inbox.read_pending(cli, limit=limit)
    if not ok:
        return Round(ok=False)

    resolved = resolve_identities(cli, signals)
    # boid-pack (identity = task id そのもの) で識別子を解決できなかった signal は、
    # 新規候補として `capture` させると task id をそのまま identity にした変な card が
    # 立つ —— 除外する。**ただし ack はしない** (2026-08-28、Opus レビュー finding 7)。
    # `resolve_identities` は「本当に task が無い」と「一時的に引けなかった」を区別せず
    # 例外を飲み込んで未解決扱いにする (`resolve_identities` の docstring)。ここで ack
    # してしまうと、後者 (daemon の瞬断等) のケースで本来まだ生きている signal を
    # 恒久的に取り逃す。**ack せず、対象にもしない** (=この巡は何もしない) ことで、
    # 迷ったら pending のまま残す側に倒す。**claim はする** —— 渡そうとして失敗した
    # のも 1 回の試行で、数えないと真に解決できない signal が永久に返り続ける。
    unresolvable_boid = frozenset(
        s.event_key for s in signals if s.source == BOID_SOURCE and s.identity not in resolved
    )
    candidates_input = tuple(s for s in signals if s.event_key not in unresolvable_boid)

    candidates = plan_candidates(
        candidates_input,
        resolved=resolved,
        taken=frozenset(),
        max_targets=max_targets,
    )
    targets = merge_targets(candidates.targets, ())
    # **溢れた signal はここに入らない。** `plan_candidates` が `max_targets` で
    # 切った分は targets にも screened_out にも現れないので、claim もされず
    # ack もされず、次巡そのまま読み直される。
    to_claim = frozenset(key for target in targets for key in target.signals) | unresolvable_boid
    return Round(
        targets=targets,
        screened_out=candidates.screened_out,
        to_claim=to_claim,
        read=len(signals),
        ok=True,
    )


def resolve_identities(cli, signals: Sequence[Signal]) -> Mapping[str, tuple[str, str]]:
    """identity → **(task id, status)**。**引けたものだけ**を返す (未登録は新規候補)。

    **2026-08-28、PR-2: boid-pack signal の identity は task id そのもの** (jira の
    `jira:ROOKPF-309` のような opaque な識別子ではない、PR-1 の envelope 契約)。
    `boid task identity resolve` (identity link 索引) には登録されていないので、
    そちらを引いても素通り (未登録) になり、新規候補として `capture` を試みる誤動作に
    なる —— boid-pack signal だけ `task_field` で直接引く。

    **status を捨てない。** 篩い 5 は status で判定する —— 「`triage --list` の集合に
    居るか」で代用すると、あの一覧は pre-execution ∪ working なので **`done` まで
    落ちて S-9 の再燃経路が死ぬ** (2026-08-23 の Fable レビューで発覚)。

    boid-pack で `task_field` が失敗した (task が消えている等) identity は返さない ——
    呼び出し側 (`build`) がこれを「解決できない boid signal」として screen する。
    """
    resolved: dict[str, tuple[str, str]] = {}
    for signal in signals:
        identity = signal.identity
        if identity in resolved:
            continue
        if signal.source == BOID_SOURCE:
            try:
                status = cli.task_field(identity, "status")
            except Exception:  # noqa: BLE001 - 引けない task id はここでは解決しない
                continue
            resolved[identity] = (identity, status)
            continue
        found = cli.resolve_identity(identity)
        if found is not None:
            resolved[identity] = found
    return resolved


def merge_targets(boid_targets: Sequence[Target], candidate_targets: Sequence[Target]) -> tuple[Target, ...]:
    """同じ task への対象を 1 つにまとめる。

    **2 対象にすると 2 枚の subagent が同じ task に同時に書く。** `app/detect.
    plan_candidates` は identity 単位で対象を組む (`grouped` は identity をキーにする)
    ので、**同じ task_id を異なる identity (例: 外部発の jira identity と、boid-pack の
    identity=task_id そのもの) から指すことがあり得る** (2026-08-28、PR-2 で boid も
    plan_candidates を通るようになったため)。そのケースを畳むのがこの関数の役目 ——
    合流時に候補側の identity を拾う。

    引数を 2 本取る形は 2026-08-27 カットオーバー当時の名残 (boid Plan.targets /
    非 boid CandidatePlan.targets を別々に持っていた)。2026-08-28 以降は 1 回の
    `plan_candidates` 呼び出しの結果を `merge_targets(candidates.targets, ())` の形で
    渡すのが通常の呼び方になったが、シグネチャは変えていない (2 本取れることが害には
    ならず、テストの互換性を保てる)。
    """
    merged: dict[str, Target] = {}
    for target in [*boid_targets, *candidate_targets]:
        key = target.task_id or f"identity:{target.identity}"
        existing = merged.get(key)
        if existing is None:
            merged[key] = target
            continue
        merged[key] = Target(
            task_id=existing.task_id or target.task_id,
            signals=existing.signals + tuple(s for s in target.signals if s not in existing.signals),
            identity=existing.identity or target.identity,
            url=existing.url or target.url,
        )
    return tuple(merged.values())


def instruction(targets: Sequence[Target], *, judge_skill: str, write_command: str) -> str:
    """sweep task の description に埋める「対象の一覧」。

    **spool ファイルは作らない。** 対象は description に埋める —— ファイルを挟むと
    「組んだ時点の世界」を判断が読むことになり、読みを判断より前に固定する構造に
    なる。

    **指し方が 2 通りある** (`detect.Target`)。既存 card は id で、新規候補は
    identity で指す —— 新規候補に id を書けないし、identity が無いと subagent は
    何を読めばよいか分からない。

    `judge_skill` は 1 対象を判断するスキル (`/nvt-sweep` 等)、`write_command` は
    記録 CLI の叩き方。**どちらもこの機構は中身を知らない** —— 判断そのものは
    workspace 固有で、そこが唯一の付加価値だから。
    """
    lines = [
        "この巡で考え直す対象。**1 対象につき subagent を 1 枚 fork** して、",
        f"`{judge_skill}` の手順で判断する。",
        "",
    ]
    for target in targets:
        if target.task_id:
            head = f"- task `{target.task_id}`"
            if target.identity:
                head += f" (identity `{target.identity}`)"
        else:
            head = f"- **新規候補** identity `{target.identity}` — まだ task は無い"
        line = f"{head} — signals: {', '.join(target.signals)}"
        if target.url:
            # 原文への入口。**新規候補には task も description も無い**ので、これが
            # 無いと subagent は identity から探し直すことになる。
            line += f" — 入口: {target.url}"
        lines.append(line)
    if not targets:
        lines.append("(この巡の対象は無い)")
    lines += [
        "",
        f"記録は `{write_command} <verb> < payload.json` を通す。",
        "`signals` には上に並んでいる event_key をそのまま渡すこと。",
        "**書き込みが成功すると、その event_key の signal も自動で ack される** ——",
        "ack 自体は subagent が意識しなくてよい。",
    ]
    return "\n".join(lines)


#: 記録 CLI の既定の叩き方。**このスキルの中のパスをそのまま指す** —— メタプロジェクト
#: 側にコピーが無いので、相対パスやモジュール名では届かない。
DEFAULT_WRITE_COMMAND = "python3 ~/.claude/skills/boid-metaproject/scripts/write.py"


def main(argv: Sequence[str] | None = None, *, cli=None, stdout=None) -> int:
    """sweep task が起動直後に実行する 1 巡の骨格。

    `--judge-skill` と `--max-targets` を**フラグで受ける**のは、メタプロジェクト側に
    設定ファイルを置かせないため。runner image には pyyaml が無いので YAML は読めず、
    JSON の設定ファイルを 1 枚増やすくらいなら、既に「最初の一手」を書いている
    behavior の `default_instruction` に 2 つ書いてもらう方が置き場が少ない。
    """
    import argparse

    parser = argparse.ArgumentParser(
        prog="sweep_targets.py",
        description="signal inbox を読み、この巡の対象を組んで自分の description に書く",
    )
    parser.add_argument(
        "--judge-skill",
        required=True,
        help="1 対象を判断するスキル (例: /nvt-sweep)。description に埋める",
    )
    parser.add_argument(
        "--max-targets",
        type=int,
        default=MAX_TARGETS,
        help=f"1 巡で起こす対象の上限 (既定 {MAX_TARGETS})。溢れた分は次巡に回る",
    )
    parser.add_argument(
        "--write-command",
        default=DEFAULT_WRITE_COMMAND,
        help="記録 CLI の叩き方。description に埋める",
    )
    args = parser.parse_args(list(argv) if argv is not None else None)
    if args.max_targets < 1:
        parser.error("--max-targets は 1 以上")

    stdout = stdout if stdout is not None else sys.stdout
    resolved_cli = cli if cli is not None else BoidCLI()
    try:
        round_ = build(resolved_cli, max_targets=args.max_targets)
        if not round_.ok:
            print("[sweep] boid signal list に失敗した。対象を組めない", file=sys.stderr)
            return 1
        # 順序: ack (判断不要と確定したもの) → claim (判断に回すもの) → description。
        # claim は description より先に打つ —— description を書いた瞬間から subagent が
        # 判断を始めるので、そのあとに数えると「判断に回した」の記録が実際の受け渡しより
        # 遅れる。
        inbox.ack(resolved_cli, tuple(round_.screened_out))
        inbox.claim(resolved_cli, tuple(round_.to_claim))
        own_task_id = resolved_cli.current_field("id")
        resolved_cli.update_description(
            own_task_id,
            instruction(round_.targets, judge_skill=args.judge_skill, write_command=args.write_command),
        )
    except BoidError as exc:
        print(f"[sweep] boid の呼び出しに失敗した: {exc}", file=sys.stderr)
        return 1
    print(
        f"[sweep] signal {round_.read} 件を読み、対象 {len(round_.targets)} 件 "
        f"(claim {len(round_.to_claim)} 件、篩いで {len(round_.screened_out)} 件 ack、"
        f"次巡送り {round_.deferred} 件)",
        file=stdout,
    )
    return 0


if __name__ == "__main__":  # pragma: no cover - モジュール実行の配線だけ
    sys.exit(main())
