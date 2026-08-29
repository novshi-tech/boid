"""サマリーの境界マーカー —— card の description を人の領域と機械の領域に割る。

    人の編集を巻き戻さないため、境界マーカーを置く。マーカーより上は人の領域、
    下は機構が毎回上書きする。

サマリーは**毎回書き直す**ので (原文のコピーは持たない)、この境界が無いと人が足した
メモが毎巡消える。しかも消えたことに気づく手段が無い —— 人からは「書いたはずのものが
無い」としか見えない。

マーカーは HTML コメントなので、Web UI (goldmark が raw HTML を落とす) では読者に見えない。

**マーカーの文字列は card の description に永続する。** 一度書いたら変えられない類の
値で、変えるときは旧い方を読み続ける手当て (`LEGACY_MARKERS`) が要る。

**マーカーが無い本文は丸ごと人の領域として扱う。** 起票時の原文がこれに当たる ——
消すと「そもそも何の件か」の出所が失われる。
"""
from __future__ import annotations

import re

MARKER = "<!-- boid:summary -->"

#: 最初のメタプロジェクト (khi-task-collector) が書いていたマーカー。**読むだけ、
#: もう書かない。**
#:
#: マーカーの改名は片道ドアに見えるが、片道にしないための唯一の手当てがこれ。
#: `_human_part` は「マーカーの無い本文は丸ごと人の領域」と読む —— 旧マーカーを
#: 認識しないまま改名すると、既存 card の機械領域がその瞬間から人の領域として凍り、
#: 二度と上書きされないまま新しい領域がその下に積まれる (同じ内容が 2 つ並ぶ)。
#: 両方読んで新しい方だけ書けば、次の `summary` で静かに移行する。
LEGACY_MARKERS = ("<!-- khi:summary -->",)

#: `description` の上限 (バイト)。boid は 64KiB (`orchestrator.MaxContentBytes`) で
#: 弾くので、そこより余裕を取る。超えると**サマリーが 1 文字も書けない**ので、
#: 判断が残らないより切って残す方を選ぶ。
MAX_DESCRIPTION_BYTES = 56 * 1024

#: 切ったことを人に見せる印。黙って切ると「LLM が途中で書くのをやめた」と読まれる。
TRUNCATION_MARK = "\n\n… (以下略。長すぎたので切った)"


def merge(current: str, body: str) -> str:
    """`current` (card の現在の description) の機械領域を `body` で差し替える。

    冪等 —— 同じ `body` で 2 回呼んでも結果は変わらない。差分ガード (S-8) はこの性質に
    乗っていて、「変わっていないなら書かない」を「merge の結果が現在値と同じか」で判定できる。
    """
    human = _human_part(current)
    machine = body.strip()
    budget = MAX_DESCRIPTION_BYTES - len((human + "\n\n" + MARKER + "\n").encode("utf-8"))
    machine = _truncate(machine, budget)
    if not human:
        return f"{MARKER}\n{machine}"
    return f"{human}\n\n{MARKER}\n{machine}"


#: 行バッジ (queue 一覧) に出す 1 行の上限。狭い列なので長くしても読まれない。
MAX_HEADLINE_CHARS = 60

#: 行頭の markdown 記号 (箇条書き・引用・番号)。中身ではないので落とす。
_LINE_MARKER = re.compile(r"^\s*(?:[-*+>]\s+|\d+[.)]\s+)")

#: 行内の強調・コード記号。行バッジは markdown を描画しないので、残すと読みにくい。
_INLINE_MARKUP = re.compile(r"[*_`]+")

#: 見出し・水平線など、それ自体が中身を持たない行。
_STRUCTURAL = re.compile(r"^\s*(?:#{1,6}\s|-{3,}\s*$|={3,}\s*$|\|)")


def headline(body: str) -> str:
    """サマリー本文から行バッジ用の 1 行を導出する (S-1)。

    **subagent には 1 行を書かせない。** body から導出できるものをスキルの入力に足すのは
    §4 の「機構で縛れるものはスキルに書かない」に反する —— 入力が増えるほど「本文と 1 行が
    ずれる」失敗が生まれ、しかもそれは機械では検査できない。

    取るのは**最初の意味のある行**。見出しは構造であって中身ではないので飛ばす。
    「そもそも何の件か」は task の title が持っているので、ここは「いま何が問題か」でよい。
    """
    for raw in body.splitlines():
        if _STRUCTURAL.match(raw):
            continue
        line = _INLINE_MARKUP.sub("", _LINE_MARKER.sub("", raw)).strip()
        if not line:
            continue
        if len(line) > MAX_HEADLINE_CHARS:
            return line[: MAX_HEADLINE_CHARS - 1] + "…"
        return line
    return ""


def _human_part(current: str) -> str:
    """マーカーより上 (人の領域)。マーカーが無ければ全体が人の領域。

    **旧マーカーも認識する** (`LEGACY_MARKERS`)。認識しないと、マーカーを改名した
    瞬間に既存 card の機械領域が人の領域として凍りつく。
    """
    for marker_text in (MARKER, *LEGACY_MARKERS):
        head, marker, _tail = current.partition(marker_text)
        if marker:
            return head.strip()
    return current.strip()


def _truncate(text: str, budget: int) -> str:
    """UTF-8 で `budget` バイトに収める。**人の領域は切らない** ので、budget が尽きて
    いれば機械領域は空になる (それでも超える場合は人の領域だけで上限を超えており、
    機構にできることは無い —— boid の 400 で気づく)。
    """
    if budget <= 0:
        return ""
    encoded = text.encode("utf-8")
    if len(encoded) <= budget:
        return text
    room = budget - len(TRUNCATION_MARK.encode("utf-8"))
    if room <= 0:
        return ""
    # バイト境界で切ると壊れた文字が残るので、デコードできるところまで戻す。
    return encoded[:room].decode("utf-8", errors="ignore") + TRUNCATION_MARK
