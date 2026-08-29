"""子 id の導出。

    ch:<正規化した仕事の識別子>:<起点になったシグナルの event_key>

**subagent は「どの仕事か」と「どのシグナル起点か」を渡すだけでよい** ——
衝突回避と世代の管理は機構がやる (§4「機構で縛れるものはスキルに書かない」)。

## なぜ世代 (起点シグナル) を含めるのか

boid 側の冪等性がこう効くため (`internal/orchestrator/card.go`、2026-08-25 の card
モデル整理で `task_triage.go` から rename、`docs/plans/card-model-cleanup.md` §4):

- `AddDetailChild` は同 id なら **closed でも noop**
- `SpecDetailChild` は dispatched/closed に冪等 noop

仕事の識別子だけを id にすると、同じ仕事に対する 2 回目の spec はどちらも無言で noop し、
**新しい子が永遠に現れない**。S-12 の「子が終了済みで、その後に新しいシグナルが来て
いるなら新しい spec を書く」(= 差し戻しに反応する) が機械的に不可能になる。

逆に、同じシグナルからのリトライでは同じ id になるので、途中で落ちた巡をやり直しても
子は二重にならない —— これが「一度に 1 つ」(S-12) を機構側で支える。

## 使う側への順序契約

boid で子 1 件を立てるには **`child_added` → `child_specced` の 2 本**を送る。
`child_added` は create-or-noop だが、**`child_specced` は update-only** で、その id の子が
無ければ 409 になる (`internal/api/workflow_card.go`、旧 `workflow_triage.go`「child_specced は update-only,
unlike child_added's create-or-noop」)。片方だけ送ると詰むので、記録 CLI の `spec` verb は
必ず 2 本を送り切ること。

**title だけの子が残るとその task は永久に終われない。** spec の無い子は dispatch されず
`open` のまま残り、khi の done 判断基準 (`.claude/skills/khi-sweep/SKILL.md` —— 全子が
終端であること) を永久に満たさない —— ゴールの分解「終わった件は閉じられる」が死ぬ。
2026-08-21 に本番で踏んだ形。
"""
from __future__ import annotations

import hashlib
import re
import unicodedata

from boidmeta.event_key import source_of

#: id の先頭。boid 側に文字種の制約は無い (child の id は単なる文字列比較) ので、
#: これは人が Web UI の子一覧を見たときの目印。
PREFIX = "ch"

#: 正規化後の仕事の識別子の上限。
MAX_WORK_CHARS = 40

#: 子 id 全体の上限。実データの event_key は 100 文字未満なので、通常は効かない。
MAX_CHILD_ID_CHARS = 200

#: 短縮するときに残す digest の長さ。sha256 の先頭 12 文字 (48 bit) ——
#: 1 つの task に載る子の数 (せいぜい数十) に対して衝突は現実的に起きない。
_DIGEST_CHARS = 12

#: 識別子に残さない文字 —— 空白・制御文字・記号を `-` にする。**`:` は id の区切りと
#: 衝突するので、ここで潰れることが要件**。`\w` (Unicode の語構成文字) は残すので、
#: 日本語はそのまま通る。
_PUNCTUATION = re.compile(r"[^\w\-]+", re.UNICODE)

_HYPHEN_RUN = re.compile(r"-{2,}")


def normalize_work(work: str) -> str:
    """仕事の識別子を id に載せられる形へ正規化する。

    ASCII に縛らない (日本語はそのまま残る) —— 縛るとスキルに「英数字で書け」という
    規則が要ることになり、§4 の「機構で縛れるものはスキルに書かない」の裏返し
    (**機構が受け止められるならスキルに規則を足さない**) に反する。

    正規化して空になる (記号だけの) 識別子は例外にする。黙って空を返すと
    `ch::<event_key>` という id ができ、別々の仕事の子が 1 枚に潰れる。
    """
    # **NFC に寄せてから潰す。** `\w` は結合文字 (U+3099 濁点等) を語構成文字と見なさないので、
    # NFD の「ガイド」は `カ-イト` になる。同じ仕事名でも正規化形が違えば別の子 id になり、
    # S-12 が防ごうとしている「同じ仕事に子が 2 枚立つ」がそのまま起きる。
    normalized = _PUNCTUATION.sub("-", unicodedata.normalize("NFC", work).lower())
    normalized = _HYPHEN_RUN.sub("-", normalized).strip("-")
    if not normalized:
        raise ValueError(f"仕事の識別子が正規化して空になった: {work!r}")
    if len(normalized) > MAX_WORK_CHARS:
        # **素朴に切らない。** 先頭 40 文字が一致する 2 つの仕事 (「… in the worker」と
        # 「… in the consumer」) が同じ id になり、2 つめの spec が `AddDetailChild` の
        # create-or-noop に吸われて**子が現れない**。title だけの子が残ったまま親は
        # 全子 closed にならず、khi の done 判断基準が永久に成立しない。
        # signal 側 (`child_id`) と同じく digest へ退避して一意性を保つ。
        digest = _digest(normalized)
        keep = MAX_WORK_CHARS - len(digest) - 1
        normalized = normalized[:keep].rstrip("-") + "-" + digest
    return normalized


def child_id(work: str, signal: str) -> str:
    """子 id を導出する。

    `signal` は起点になったシグナルの event_key。**形を検証する** —— ここに event_key
    でない文字列を渡すのは呼び出し側のバグで、通すと「世代が進んだつもりで進んでいない」
    という、子が現れないことでしか気づけない壊れ方をする。
    """
    if not signal:
        raise ValueError("起点シグナルの event_key が空")
    source_of(signal)  # 形の検証。語彙外なら ValueError が飛ぶ
    candidate = f"{PREFIX}:{normalize_work(work)}:{signal}"
    if len(candidate) <= MAX_CHILD_ID_CHARS:
        return candidate
    # 異常に長い event_key のときだけ。**決定的**であることが要る —— リトライのたびに
    # 別 id になると冪等性 (S-12) が壊れるので、乱数や時刻は使わない。
    return f"{PREFIX}:{normalize_work(work)}:{source_of(signal)}:{_digest(signal)}"


def _digest(text: str) -> str:
    """長すぎるものを畳むときの決定的な短縮。"""
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:_DIGEST_CHARS]
