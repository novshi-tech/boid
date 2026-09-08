"""1 巡の骨格 —— inbox を読み、対象を組み、自分の description に書く。

sweep task が**最初の一手**として実行する。入口は
`scripts/sweep_targets.py --intake-skill /<スキル> [--max-targets N]`。

やること:

1. `boid signal list` で pending signal を読む (`inbox`)。**副作用は無い**
2. identity を解決し (`resolve_identities`)、篩いをかけて対象へ畳む
   (`merge_targets`、`detect.plan_candidates`)
3. 機構が決定的に落とした signal はその場で ack する —— 判断が要らないので、
   判断待ちの subagent を待たずに決着させてよい
4. **判断に回す signal を `boid signal claim` で名指しする**
5. 残った対象を自分の description に書き込む (`instruction`)。以降は sweep behavior の
   instruction が対象ごとに subagent を fork する

## 何を claim するか

`attempts` は「諦めるまでの回数」なので、数える対象は**判断に回した**ものでなければ
ならない。claim するのは 2 種類だけ:

- **target になった signal** —— subagent に渡す

claim しないもの:

- **篩いで落とした signal** —— 同じ巡でそのまま ack するので数える意味が無い
- **`max_targets` から溢れて次巡送りにした signal** —— 読み出しが返した行を一律で
  数える形 (`boid signal list --claim`) はここを壊していた。`max_targets` と読む件数の
  差が毎巡課金され、5 巡で**誰も判断していない signal が無言で dead に落ちる**

3 つの集合 (`to_claim` / `screened_out` / どちらでもない) は重ならない。重なると
ack が先に飛んで、まだ判断していない signal が決着する。

## target になった signal は ack しない

判断が要るので、担当した subagent が判断を書いた直後に自分で ack する
(`write` の `Executor._record`)。ack を先に打つと、subagent が crash した signal が
pending から消えたまま誰も処理していない状態になる。

## 自分自身の書き込みを落とす篩いはここに無い

inbox に届く signal は外部コネクタ発のものだけ —— card 自身のアクションは signal に
ならず、card_events 経由でその card の判断へ直接向かう。だから「自分が書いたものを
自分で拾う」経路がそもそも無い。

## dead-letter は core に乗る

独自の attempts 機構は持たない。`claim` が進める `attempts` が
`MaxSignalAttempts` (5) に達した signal を core が dead にし、pending の一覧から外す。
"""
from __future__ import annotations

import sys
from dataclasses import dataclass
from typing import Mapping, Sequence

from boidmeta import inbox
from boidmeta.boid_store import BoidCLI, BoidError
from boidmeta.detect import MAX_TARGETS, Target, plan_candidates
from boidmeta.signal import Signal

#: boid 内部 action 由来の signal の `Signal.source` (`inbox` の写像 (envelope の `source.pack` そのまま))。
#: **この source だけ identity が task id そのもの** —— `resolve_identities` 参照。

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
    #: 唯一の役目** —— 溢れは無言で起きるので、`READ_MULTIPLIER` と `MAX_TARGETS` の差が
    #: 実運用で効いているかどうかを実データで判断する材料が他に無い。
    read: int = 0
    ok: bool = True

    @property
    def deferred(self) -> int:
        """この巡が触らなかった signal の件数 (`MAX_TARGETS` 溢れ)。次巡そのまま
        読み直される。**0 でない巡が続くなら `READ_MULTIPLIER`/`MAX_TARGETS` を見直す
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
    candidates = plan_candidates(
        signals,
        resolved=resolved,
        max_targets=max_targets,
    )
    targets = merge_targets(candidates.targets)
    # **溢れた signal はここに入らない。** `plan_candidates` が `max_targets` で
    # 切った分は targets にも screened_out にも現れないので、claim もされず
    # ack もされず、次巡そのまま読み直される。
    to_claim = frozenset(key for target in targets for key in target.signals)
    return Round(
        targets=targets,
        screened_out=candidates.screened_out,
        to_claim=to_claim,
        read=len(signals),
        ok=True,
    )


def resolve_identities(cli, signals: Sequence[Signal]) -> Mapping[str, tuple[str, str]]:
    """identity → **(task id, status)**。**引けたものだけ**を返す (未登録は新規候補)。

    **status を捨てない。** 篩い 5 は status で判定する —— 「`triage --list` の集合に
    居るか」で代用すると、あの一覧は pre-execution ∪ working なので **`done` まで
    落ちて S-9 の再燃経路が死ぬ** (2026-08-23 の Fable レビューで発覚)。
    """
    resolved: dict[str, tuple[str, str]] = {}
    for signal in signals:
        identity = signal.identity
        if identity in resolved:
            continue
        found = cli.resolve_identity(identity)
        if found is not None:
            resolved[identity] = found
    return resolved


def merge_targets(targets: Sequence[Target]) -> tuple[Target, ...]:
    """同じ task への対象を 1 つにまとめる。

    **2 対象にすると 2 枚の subagent が同じ task に同時に書く。** `app/detect.
    plan_candidates` は identity 単位で対象を組む (`grouped` は identity をキーにする)
    ので、**同じ task_id を異なる identity (例: jira の課題キーと bitbucket の PR)
    から指すことがあり得る**。そのケースを畳むのがこの関数の役目 —— 合流時に候補側の
    identity を拾う。

    """
    merged: dict[str, Target] = {}
    for target in targets:
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


def instruction(targets: Sequence[Target], *, intake_skill: str, write_command: str) -> str:
    """sweep task の description に埋める「対象の一覧」。

    **spool ファイルは作らない。** 対象は description に埋める —— ファイルを挟むと
    「組んだ時点の世界」を判断が読むことになり、読みを判断より前に固定する構造に
    なる。

    **指し方が 2 通りある** (`detect.Target`)。既存 card は id で、新規候補は
    identity で指す —— 新規候補に id を書けないし、identity が無いと subagent は
    何を読めばよいか分からない。

    `intake_skill` は 1 対象を仕分けるスキル、`write_command` は記録 CLI の叩き方。
    **どちらもこの機構は中身を知らない** —— 判断そのものは workspace 固有で、
    そこが唯一の付加価値だから。

    **どの段がどの verb を持つかはここで言う。** workspace のスキル 2 本に書かせると
    同じ表が 2 か所に増えて、片方だけ古くなる。
    """
    lines = [
        "この巡で仕分ける対象。**1 対象につき subagent を 1 枚 fork** して、",
        f"`{intake_skill}` の手順で仕分ける。",
        "",
        "**この巡の出口は `capture` / `link` / `note` / `skip` の 4 つだけ。**",
        "既に card がある対象は `note` で「何が新しいか」を渡す。",
        "card の中身を書くのはこの巡の仕事ではない —— `capture` / `link` / `note` は",
        "どれも card イベントとして記録され、続きの判断は daemon が card コマンドとして",
        "自動で起こす。ここで書き足すと同じ card を二重に判断することになる。",
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

    `--intake-skill` と `--max-targets` を**フラグで受ける**のは、メタプロジェクト側に
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
        "--intake-skill",
        help="1 対象を仕分けるスキル (例: /nvt-intake)。description に埋める",
    )
    # runner image のデプロイと `boid project fetch` は別手順で、同時には
    # 切り替えられない。移行窓のあいだ旧名も受ける。
    parser.add_argument("--judge-skill", help=argparse.SUPPRESS)
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
    intake_skill = args.intake_skill or args.judge_skill
    if not intake_skill:
        parser.error("--intake-skill が要る")
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
            instruction(round_.targets, intake_skill=intake_skill, write_command=args.write_command),
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
