"""記録 CLI —— 判断を boid へ書く唯一の口。

**判断から呼ばれる唯一の書き込み経路** (S-14)。subagent は読みは自由に、書きはここだけを
通す —— boid CLI を直に叩かれると、差分ガード (S-8) も処理記録 (S-11) も dry-run (§10)
も置き場が無くなる。

    python3 ~/.claude/skills/boid-metaproject/scripts/write.py <verb> < payload.json

引数は**全 verb で stdin の JSON 1 個**に統一してある。verb ごとに呼び方が違うと
subagent が間違えるし、日本語の本文をコマンドラインに載せるとクオートで事故る。
report モード (dry-run) で入力をそのまま残せるのも同じ形の効用。

## verb がそのまま契約 (§3.4「判断 → 記録」)

**2026-08-25、suggestion 状態遷移化 (`docs/plans/suggestion-as-state-transition.md`
§3.7) で語彙を刷新した。** boid 側の card 機械が v2 (parked/working/done/dropped の
4 状態、機械遷移は capture だけ) なのに合わせ、判断側は「状態遷移そのもの」
を直接打つのをやめ、**全部 suggestion (verb + reason + 任意の params) として提案し、
人の accept が実際の遷移を適用する** 形に揃えた。`wake`/`canonical` は廃止、
`manual` は「人が手を動かす」の定義そのものである `start` (旧 `working`) に改名、
`go`/`complete` (旧 `done`) /`reopen` を新設した。

| verb | 渡すもの |
|---|---|
| `capture` | identity, title, body, urgency |
| `link` | task_id, identity |
| `summary` | task_id, body |
| `spec` | task_id, work, origin, title, project, behavior, description (+ 任意で instruction) |
| `drop-child` | task_id, child_id, reason |
| `observed` | task_id, source_closed |
| `urgency` | task_id, urgency |
| `park` | task_id, reason, wake_at か wake_task_id — **suggestion として提案する** |
| `start` | task_id, reason — 人が手を動かす、を suggest (旧 `working`/`manual`) |
| `go` | task_id, reason — specced 子の dispatch + working、を suggest (実行承認) |
| `complete` | task_id, reason — 完了、を suggest (旧 `done`) |
| `reopen` | task_id, reason — 再オープン、を suggest |
| `drop` | task_id, reason — 取り下げ、を suggest |
| `skip` | signals, reason |
| `done-signal` | task_id, signals |

`signals` (この呼び出しで処理済みにする event_key 群) は**どの verb でも必須**。
書き込みが成功したら処理記録を自動で付ける (§5.3) ので、**subagent は記録の書式も
置き場も知らない** —— 知るのは「何を処理したか」だけで、それは sweep task の
instruction に載っている。

## 実行部への申し送り

- **`spec` の `project` は解決済み UUID でなければ boid が受けない。** `action send` は
  broker で project 名の解決をしてもらえず (`internal/sandbox/broker.go` は
  「project 検証は boid_executor 側で行う」)、executor 側の `AllowsProject` は UUID の
  完全一致比較 (`internal/sandbox/protocol.go`)。名前を渡すと毎回
  "child_specced project is restricted to the current workspace" で落ちる
  (2026-08-21 に本番で踏んだ形)。**実行部が送る直前に名前 → ID を解決すること。**
  ここで検証しないのは、名前で渡すのが subagent にとって自然な書き方だから
- **`spec` は `child_added` → `child_specced` の 2 本を送り切ること** (`domain/childid.py`
  の順序契約)。片方だけだと詰む
- **`urgency` はどの status でも同じ 1 本 (`attrs_set{urgency, kind}`) で書く。**
  v1 は「someday だけ captured に留める」「triaged 以降は someday に下げない」という
  status 依存の分岐を持っていたが、**urgency は並び順だけの属性になった**
  (設計 §3.6 —— card の可視性は suggestion の有無で決まり、urgency はもう queue や
  Open タブの出/非出を左右しない) ため、その分岐ごと消えている
- **`capture` の返り値の `created` を使う。** `resolve-or-capture` は既存 task に当たった
  場合も成功で返るので、初期化 (S-2) を毎回走らせると人が触った値を上書きする。
  **urgency も新規のときだけ置く** —— 合流先の急ぎ具合は人が触っているかもしれない
- **`capture` は起票と urgency を 1 回で済ませる** (S-11)。別 verb に分けると打ち忘れが
  起き、captured のまま queue に出ない。2026-08-23 の本番投入で起票 8 件のうち 5 件が
  そうなった —— **打ち忘れうる形にした時点で機構側の欠陥**であって、スキルの書き方で
  埋めるものではない
- **`park`/`start`/`go`/`complete`/`reopen`/`drop` は全部 `attrs_set{suggestion:{verb,
  reason, params?}}` を書くだけ。** 実際の遷移は boid 側で人が accept したときに初めて
  適用される (`internal/api/suggestion_accept.go` の `applyAnswered`)。verb は
  boid 自身の状態遷移語彙 (go/start/park/drop/complete/reopen の 6 つ) に固定されており、
  それ以外を送ると daemon が 400 で拒否する (`validateSuggestionAttr`)。**1 枚の card に
  suggestion は 1 つだけ**で、書き直すと前の提案を上書きする (最新が勝つ、設計 §3.4) ——
  判断を更新したことの表現としてそれが正しい
- **`reopen` は `done`/`dropped` の card にも書ける** (`observed` は `done` にも)。
  boid 側は `resolveAttrsSetDoneTransition` (I-5b、done 専用の service 層ガード) と
  `cardActiveAndDroppedStatuses` (dropped は attrs_set の通常の FromStatus) の 2 つの
  経路でこれを支えている —— 終わった/取り下げた件に続きが来たとき、reopen を提案できる
  必要があるため。他の verb (start/go/park/complete/drop) はこの 2 status には書けない
  (`_TERMINAL_ALLOWED_VERBS`)

## エラーメッセージは LLM へのフィードバック

相手は人ではなく subagent なので、「何が足りないか」が読めない拒否は同じ間違いを
繰り返させるだけになる。メッセージには**欠けたフィールド名と、使える値**を必ず入れる。

未知のフィールドを黙って無視せずエラーにするのも同じ理由 —— typo を無視すると
「書いたつもりで書けていない」という、後から人が見て初めて分かる壊れ方をする。
"""
from __future__ import annotations

import json
import re
import sys
from dataclasses import dataclass
from datetime import datetime
from types import MappingProxyType
from typing import Mapping, Sequence

from boidmeta import inbox
from boidmeta.boid_store import BoidCLI, BoidError, detail_of
from boidmeta import summary
from boidmeta.childid import child_id, normalize_work
from boidmeta.event_key import namespace_from_key

#: Go の `time.RFC3339` (`2006-01-02T15:04:05Z07:00`) が受ける形だけを通す。
#: `T` とオフセットのコロンは必須、`Z` は大文字のみ、秒の小数は任意。
_RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")

#: `_record` が書く progress 1 通の上限 (**文字数**)。boid は progress の生文字列を
#: `orchestrator.ValidateContentSize` (`MaxContentBytes` = 64KiB) に通し、超えると 400 を
#: 返す (`internal/api/task_notify.go`)。日本語は 1 文字 3 バイト前後になりうるので、
#: 文字数ベースで大きく余裕を持たせて切る —— 1 回の判断で渡る signals は通常 1〜数件
#: (1 対象 = 1 subagent、§5.2) なので、実運用でこの上限に達することはまず無い。
_MAX_PROGRESS_MESSAGE_CHARS = 4000

#: urgency の語彙 (§5.2 S-2)。boid 側の `promotedAttrVocabulary["urgency"]`
#: (`internal/api/workflow_card.go`、旧 `workflow_triage.go`) と一致させること ——
#: ここが語彙外だと
#: attrs_set 自体が 400 で拒否される。**urgency は並び順だけの属性**になった
#: (設計 `docs/plans/suggestion-as-state-transition.md` §3.6) ので、v1 時代のように
#: card の可視性 (queue に出るかどうか) を左右することはもう無い。
URGENCIES = ("now", "today", "week", "someday")

#: 語彙が決まっているフィールド。**欠けたことを告げるときにも使える値を並べる** ——
#: 「urgency が要る」だけだと subagent は値を推測するしかなく、語彙外で 2 度目の拒否を
#: 食う (冒頭「エラーメッセージは LLM へのフィードバック」)。
#:
#: **`behavior` はここに無い。** 打てる behavior は**振り先の project 側**で決まる
#: ので、この機構が持つ固定の一覧は「どの project にあるか」を答えられない ——
#: 手で並べた一覧はメタプロジェクトごとに古くなり、実物と食い違ったときに嘘の
#: エラーを返す。代わりに `Executor._refuse_absent_behavior` が送信の直前に
#: `boid project behaviors` で実物と照合し、そのとき初めて**実在する値**を並べる。
_VOCABULARIES: Mapping[str, tuple[str, ...]] = MappingProxyType({"urgency": URGENCIES})


class CommandError(ValueError):
    """subagent に返す拒否。メッセージがそのままフィードバックになる。"""


def _missing(verb: str, name: str, note: str = "") -> CommandError:
    """欠けたフィールドを告げる。語彙のあるフィールドなら使える値も並べる。"""
    vocabulary = _VOCABULARIES.get(name)
    tail = f" (使えるのは {', '.join(vocabulary)})" if vocabulary else ""
    return CommandError(f"{verb}: {name} が要る{note}{tail}")


#: verb -> (必須フィールド, その verb だけの任意フィールド)。
_VERB_FIELDS: dict[str, tuple[tuple[str, ...], tuple[str, ...]]] = {
    # urgency を必須にしているのは S-11。**起票と urgency を分けると打ち忘れが起きる** ——
    # 2026-08-23 の本番投入で起票 8 件のうち 5 件が urgency 無しの captured で止まり、
    # queue に出なかった。うち 2 件は子を specced まで作ってあった。
    "capture": (("identity", "title", "body", "urgency"), ()),
    "link": (("task_id", "identity"), ()),
    "summary": (("task_id", "body"), ()),
    # **`instruction` は任意。** 子の instruction は behavior の `default_instruction` を
    # フィールド単位で上書きする (`internal/orchestrator/payload_merge.go` の
    # `MergeDefaultInstructions` —— override が 1 件なら非空フィールドが勝つ) ので、
    # 1 行でも書くと **behavior 側の手順が丸ごと消える。**
    #
    # 2026-08-24 に本番で踏んだ形: `respond` の子に「投稿後に done」という instruction が
    # 載っていたため、project.yaml の respond に焼いてあった「投稿前に `boid task ask` で
    # 承認を取る」契約 (決定 D-24) が消え、承認なしで Slack に返信が飛んだ。同じ形が
    # triaged 6 枚すべてに載っていて、review / research / drive では workspace default の
    # 実務手順 (repo slug の確認、gateway の叩き方、GO/NO-GO の明示、`ask` 契約) が
    # 消えていた。**必須にしていたのが原因** —— CLI が要求すれば subagent は必ず埋める。
    #
    # 塞がずに任意にしたのは、既定を意図的に変える口が要るから (nose 2026-08-24:
    # 「instruction 書き換えは柔軟性の担保のために必要な仕掛け」)。伝えたいことは
    # description に書き、instruction は「behavior の既定手順を変える」ときだけ書く。
    "spec": (
        ("task_id", "work", "origin", "title", "project", "behavior", "description"),
        ("instruction",),
    ),
    # 取り下げ。**訂正の手段** —— 立ててしまった子が間違っていたとき、直す口が無いと
    # 正しいものを別 id で作り直すしかなく、訂正が必ず重複の形になる (2026-08-23 実測)。
    # boid 側は `open` / `specced` だけ受け、走っている (`dispatched`) 子は 409 で拒む。
    "drop-child": (("task_id", "child_id", "reason"), ()),
    "observed": (("task_id", "source_closed"), ()),
    "urgency": (("task_id", "urgency"), ()),
    # park は suggestion 化された (設計 §3.7)。wake 条件必須 (S-13) は維持。
    # **reason も必須にした** (2026-08-25、PR-K レビュー LOW 8 — park は accept 時に
    # wake 条件という追加の state を書く唯一の suggestion verb で、「何を承認するのか」
    # が最も重要なケースなのに元々 reason だけ任意だった。boid 側の
    # `TaskDetailSuggestionSection` は `suggestion.Reason != ""` のときだけ理由ブロックを
    # 描画するので、reason 無しの park は verb バッジと confirm ダイアログだけになり、
    # 他の suggestion verb と足並みが揃っていなかった。他の 5 verb と揃えて必須にする)。
    "park": (("task_id", "reason"), ("wake_at", "wake_task_id")),
    # start (旧 manual/working) / go / complete (旧 done) / reopen / drop —— 全部 boid
    # 自身の状態遷移語彙 (`orchestrator.IsCardTransitionAction`) を提案するだけの同じ形。
    # reason は全部必須 (「なぜその遷移を勧めるか」が accept 画面の主表示になる、boid PR #988)。
    "start": (("task_id", "reason"), ()),
    "go": (("task_id", "reason"), ()),
    "complete": (("task_id", "reason"), ()),
    "reopen": (("task_id", "reason"), ()),
    "drop": (("task_id", "reason"), ()),
    "skip": (("signals", "reason"), ()),
    "done-signal": (("task_id", "signals"), ()),
}

VERBS = tuple(_VERB_FIELDS)

#: **どの verb でも必須。** §5.3 は「どの verb でも、書き込みが成功したら処理記録を
#: 自動で付ける —— subagent は記録を意識しない」と決めているが、機構が自動で打てるのは
#: **何を処理したのかを渡されたときだけ**である。任意にすると「記録を付ける」が実質
#: スキル側の義務に戻り、§4 が「機構で縛れるものはスキルに書かない」と決めた当の項目が
#: 守られないことがある。
#:
#: 渡さずに書き込むと: 書き込みは成功、記録はゼロ → 次の巡が同じシグナルを再検知して
#: subagent を再 fork → 差分ガード (S-8) で何も書かれないので `handled` も付かず →
#: attempts を焼き切って「判断できなかった」通知が人に飛ぶ。**正しく処理済みの件について**。
#:
#: subagent が意識しないのは記録の**書式と置き場**であって、「何を処理したか」ではない。
#: それは sweep task の instruction に載っている (§3.4「trigger → 判断: 対象 identity の
#: 一覧」) ので、そこから写して渡す。
_COMMON_REQUIRED = ("signals",)

#: 真偽値で受けるフィールド。`"false"` を真と読む事故を防ぐため、文字列は受けない。
_BOOL_FIELDS = frozenset({"source_closed"})

#: この status には記録を書く経路が全く無い。`aborted` は card 機械 v2 が到達しない
#: 状態そのもの (設計 §3.2 「card は aborted にも到達しなくなる」) —— card 側の
#: task_triage には現れ得ない。
_NO_WRITE_STATUSES = frozenset({"aborted"})

#: 終端 status (`done`/`dropped`) でも書ける verb。**設計が名指しで守れと言っている
#: 経路** (S-9、および card 機械 v2 の `done → parked : reopen` /
#: `dropped → parked : reopen`、設計 §3.2)。
#:
#: - `done`: `observed` (I-5b ガード、`internal/api/attrs_set_done.go`。**現行実装は
#:   観測側が done を除外していたためこの経路が一度も発火しなかった。同じ轍を踏まない**
#:   —— 2026-08-23 に一度塞いで Fable レビューで指摘された) と `reopen`
#:   (同じ I-5b ガードを通って suggestion として書かれる。**PR-K ブリーフの受け入れ条件**:
#:   done card への reopen suggest が成立することをテストで実証済み、
#:   `_tests/test_write_executor.py` の
#:   `TerminalTaskGuardTest.test_reopen_is_allowed_on_a_done_card`)
#: - `dropped`: `reopen` のみ。boid 側は attrs_set の通常の FromStatus に dropped を
#:   含む (`cardActiveAndDroppedStatuses`、I-5b のような service 層ガードは不要) ので、
#:   done より単純に通る
#:
#: これ以外の verb (start/go/park/complete/drop/summary/link/urgency/spec/drop-child/
#: capture) はこの 2 status には書けない —— 終わった/取り下げた件を書き換える意味が無い。
_TERMINAL_ALLOWED_VERBS: Mapping[str, frozenset[str]] = MappingProxyType(
    {
        "done": frozenset({"observed", "reopen"}),
        "dropped": frozenset({"reopen"}),
    }
)

#: card 機械 v2 の遷移 verb ごとの適用可能 FromStatus
#: (`internal/orchestrator/machine_card.go`: go/start/drop は parked のみ、
#: park は working のみ、complete は parked/working、reopen は done/dropped のみ)。
#:
#: **`done` が parked からも打てるのは 2026-08-25 に足した 8 本目の辺** ——
#: 「外で片付いていた」「重複と判明した」card、つまり**ここでは誰も working して
#: いない**件を 1 手で畳むため。それまでは working → 次の巡で done の 2 手を
#: 指示していたが、中間の working が「誰かが手を動かしている」という事実でないことを
#: 台帳に書くうえ、1 手で済ませたい判断が identity を解放する `drop` に流れる誘因に
#: なっていた (2026-08-25、card ad8c6808 → cba7c559 で実際に 1 周した)。
#:
#: **この対応表を変えるときに触る場所 (2026-08-25、PR-K レビュー finding C)**:
#: 1. `internal/orchestrator/machine_card.go` (boid 側の正、`NewCardMachine` の
#:    遷移ルール) —— これが変わらない限り以下は変えない
#: 2. ここ (`_TRANSITION_VERB_STATUSES`)
#: 3. `_tests/test_write_executor.py` の `TransitionVerbStatusGuardTest`
#:    (`SINGLE_STATUS_VERBS`/`DONE_VALID_STATUSES`/`REOPEN_VALID_STATUSES` ——
#:    わざと `_TRANSITION_VERB_STATUSES` を import せず書き下している。実装と
#:    テストの単一情報源化はしない、テストは独立した正解を持つ)
#: 4. メタプロジェクトの判断スキル (`--judge-skill` が指すもの) の status→verb 表 (「ただし、どの verb を
#:    書けるかは card の現在の status で決まる」の下)
#: 3 と 4 が実装からズレても実行時のエラーメッセージ (`_transition_verbs_from` 由来) が
#: 訂正するので実害は緩いが、subagent/人が読む文書として揃えておくこと。
#:
#: **2026-08-25、PR-K レビュー HIGH 1 で追加。** boid 側の `validateSuggestionAttr` は
#: suggestion の verb 文字列が boid の状態遷移語彙に属するかしか見ず、**現在の card の
#: status とは一切突き合わせない** —— なので不整合な組は書き込み時には弾かれず、
#: 人が accept を押した瞬間に `sm.Apply` が失敗して 409 になる。そのときには
#: `answered` action は記録されるが遷移は起きず、suggestion も (正しく) strip されない
#: ので、次の巡も同じ verb が書き直され、**queue に「accept できない」提案が
#: 永久に居座る**。ここで書き込み前に status を見て拒めば、この壊れ方自体が起きない
#: (boid 側にも Accept ボタンを描画しない防御を別途入れる予定 — 二重防御)。
_TRANSITION_VERB_STATUSES: Mapping[str, frozenset[str]] = MappingProxyType(
    {
        # go は parked/working の両方から提案できる (boid card-next-step-and-timeline.md
        # §3.1: working 中に用意できた次の specced 子も Go で走らせられる自己遷移が
        # boid 側の machine_card.go に追加された)。
        "go": frozenset({"parked", "working"}),
        "start": frozenset({"parked"}),
        "drop": frozenset({"parked"}),
        "park": frozenset({"working"}),
        "complete": frozenset({"parked", "working"}),
        "reopen": frozenset({"done", "dropped"}),
    }
)

#: 上のエントリを人が読みやすい順で束ねたもの。**エラーメッセージは LLM への
#: フィードバック** (module docstring) なので、「この status からは何が提案できるか」を
#: 単一の情報源 (`_TRANSITION_VERB_STATUSES`) から導出する —— 手で別に書くと、
#: verb を足したときに片方だけ更新し忘れて drift する (boid 側が `cardTransitionActions`
#: をルール表からの導出に変えた理由と同じ、boid PR #987 review round2 MEDIUM 5)。
_TRANSITION_VERB_ORDER = ("go", "start", "park", "complete", "reopen", "drop")


def _transition_verbs_from(status: str) -> tuple[str, ...]:
    """`status` から提案できる遷移 verb (順序は `_TRANSITION_VERB_ORDER` 固定)。"""
    return tuple(
        verb for verb in _TRANSITION_VERB_ORDER if status in _TRANSITION_VERB_STATUSES.get(verb, ())
    )

#: event_key の形であることを要求するフィールド。形が違うと検知が記録を読めず、
#: 同じシグナルを永久に再検知する (§3.4)。
_EVENT_KEY_FIELDS = frozenset({"origin"})


def validate(verb: str, payload: Mapping[str, object]) -> dict[str, object]:
    """stdin の JSON を検証し、正規化した命令を返す。

    純粋関数。boid への書き込みも記録も、ここから先の実行部の仕事。
    """
    if verb not in _VERB_FIELDS:
        raise CommandError(f"知らない verb: {verb!r} (使えるのは {', '.join(VERBS)})")
    verb_required, verb_optional = _VERB_FIELDS[verb]
    required = set(verb_required) | set(_COMMON_REQUIRED)
    allowed = required | set(verb_optional)

    unknown = sorted(set(payload) - allowed)
    if unknown:
        raise CommandError(
            f"{verb}: 知らないフィールド {', '.join(unknown)} "
            f"(使えるのは {', '.join(sorted(allowed))})"
        )

    command: dict[str, object] = {"verb": verb, "signals": _signals(payload.get("signals"))}
    if not command["signals"]:
        raise CommandError(
            f"{verb}: signals が要る —— この呼び出しで処理済みにする event_key を "
            "1 件以上渡す (sweep task の instruction に載っている、この対象の event_key)"
        )

    for name in sorted(allowed - {"signals"}):
        if name not in payload:
            if name in required:
                raise _missing(verb, name)
            continue
        command[name] = _field(verb, name, payload[name], required=name in required)

    _check_verb_rules(verb, command)
    return command


def _signals(value: object) -> tuple[str, ...]:
    """`signals` を正規化する。1 件だけのときに文字列を渡すのは自然な書き方なので受ける。"""
    if value is None:
        return ()
    if isinstance(value, str):
        items: Sequence[object] = [value]
    elif isinstance(value, list):
        items = value
    else:
        raise CommandError(f"signals は文字列か文字列の配列 (受け取った型: {type(value).__name__})")
    keys: list[str] = []
    for item in items:
        if not isinstance(item, str) or not item.strip():
            raise CommandError(f"signals の要素が event_key でない: {item!r}")
        key = item.strip()
        _require_event_key("signals", key)
        keys.append(key)
    return tuple(keys)


def _field(verb: str, name: str, value: object, *, required: bool) -> object:
    if name in _BOOL_FIELDS:
        if not isinstance(value, bool):
            raise CommandError(
                f"{verb}: {name} は true / false で渡す (受け取った値: {value!r})"
            )
        return value
    if not isinstance(value, str):
        raise CommandError(f"{verb}: {name} は文字列 (受け取った型: {type(value).__name__})")
    text = value.strip()
    if required and not text:
        raise _missing(verb, name, " (空文字だった)")
    if name in _EVENT_KEY_FIELDS:
        _require_event_key(f"{verb}: {name}", text)
    vocabulary = _VOCABULARIES.get(name)
    if vocabulary and text not in vocabulary:
        raise CommandError(
            f"{verb}: {name} が語彙外: {text!r} (使えるのは {', '.join(vocabulary)})"
        )
    if name == "wake_at":
        _require_timestamp(verb, text)
    if name == "work":
        # 実際に子 id を作る `normalize_work` は記号だけの識別子で例外を投げる。ここで
        # 通してしまうと、実行部で裸の ValueError になって「欠けたフィールド名と使える値」
        # という契約 (このモジュールの docstring) を満たさないフィードバックになる。
        try:
            normalize_work(text)
        except ValueError as exc:
            raise CommandError(f"{verb}: work が識別子にならない: {text!r} ({exc})") from exc
    return text


def _require_event_key(where: str, key: str) -> None:
    try:
        namespace_from_key(key)
    except ValueError as exc:
        raise CommandError(f"{where}: event_key の形でない: {key!r} ({exc})") from exc


def _require_timestamp(verb: str, text: str) -> None:
    """wake_at を **RFC3339 として**検証する。

    `datetime.fromisoformat` だけでは緩すぎる —— 受け取る boid は
    `time.Parse(time.RFC3339, ...)` (`internal/api/workflow_card.go`、旧
    `workflow_triage.go`) で読むので、
    Python が通す `+0900` (コロン無し) / `2026-09-01 09:00:00+09:00` (空白区切り) /
    秒の省略は全部 Go 側で 400 になる。**`+0900` は Jira が返す形式**で、subagent が
    source の時刻を写して渡す経路がそのまま該当する。

    ここで通してしまうと「条件付き park のつもりで park されていない」状態になり、
    その task は誰にも拾われなくなる (S-13 が防ごうとしている状態そのもの)。
    """
    if not _RFC3339.match(text):
        raise CommandError(
            f"{verb}: wake_at は RFC3339 で渡す: {text!r} "
            "(例: 2026-09-01T09:00:00+09:00 —— T 区切り・秒まで・オフセットにコロンが要る)"
        )
    try:
        # 形が合っていても値が無い日時 (`2026-02-30`、`+99:99`) はここで落ちる。
        # LLM が月末を書き間違える経路なので、正規表現だけでは足りない。
        parsed = datetime.fromisoformat(text)
    except ValueError as exc:
        raise CommandError(f"{verb}: wake_at が日時として存在しない: {text!r}") from exc
    if parsed.tzinfo is None:  # pragma: no cover - _RFC3339 がオフセットを必須にしている
        raise CommandError(f"{verb}: wake_at にタイムゾーンが要る: {text!r} (どこの時刻か決まらない)")


def _check_verb_rules(verb: str, command: Mapping[str, object]) -> None:
    """フィールド単体では見られない、verb ごとの条件。"""
    if verb == "park" and not (command.get("wake_at") or command.get("wake_task_id")):
        # S-13: 条件の無い park は誰も拾えない。**機構で必須にできる**のでスキルには書かない。
        raise CommandError(
            "park: 再浮上の条件が要る —— wake_at (この日時が来たら起こす) か "
            "wake_task_id (この子 task が終わったら起こす) のどちらかを渡す"
        )


# ---------------------------------------------------------------------------
# 実行部 —— 検証済みの命令を boid の操作へ落とす
# ---------------------------------------------------------------------------


class UnsupportedVerbError(CommandError):
    """語彙にはあるが、まだ実装していない verb。**黙って何もしない**より落とす。"""


@dataclass(frozen=True)
class Result:
    """1 つの verb を実行した結果。

    - `task_id` 処理記録の書き先。空なら sweep task 自身の timeline へ (§5.3 S-11 後半)
    - `changed` 差分ガード (S-8) の結果。通知 (S-7) はこれが True のときだけ
    - `note`    記録に残す 1 行 (人が timeline / attrs を読むときの手がかり)
    - `notify`  人に届ける文面。空なら通知しない
    """

    task_id: str = ""
    changed: bool = False
    note: str = ""
    notify: str = ""


class Executor:
    """検証済みの命令を boid の操作へ落とし、処理記録と通知まで面倒を見る。

    **記録と通知は verb ハンドラの仕事ではない。** ハンドラは「何を書いたか」を
    `Result` で返すだけで、記録を付けるのも通知を出すのもここ —— §4 の「機構で縛れる
    ものはスキルに書かない」を、実装の内側でももう一段やっている (ハンドラを足す人が
    記録を書き忘れられない)。
    """

    def __init__(self, cli, *, sweep_task_id: str, report: bool = False) -> None:
        self.reported: list[str] = []
        #: このメタプロジェクト自身の project id (`_own_project` が sweep task から 1 回だけ引く)。
        self._own_project_id: str | None = None
        self.cli = _Reporter(cli, self.reported) if report else cli
        self.sweep_task_id = sweep_task_id
        self.report = report

    def run(self, command: Mapping[str, object]) -> Result:
        verb = str(command["verb"])
        handler = getattr(self, "_do_" + verb.replace("-", "_"), None)
        if handler is None:
            raise UnsupportedVerbError(f"{verb}: まだ実装していない")
        self._refuse_terminal(verb, command)
        result = handler(command)
        self._record(verb, command, result)
        self._deliver(result)
        return result

    def _refuse_terminal(self, verb: str, command: Mapping[str, object]) -> None:
        """書けない相手への verb を、**正しい verb を名指しして**拒む。

        boid の card 機械 v2 は `aborted` に到達しない (設計 §3.2) ので、そこへの
        書き込み経路はそもそも存在しない。`done`/`dropped` は違う —— **終端扱いにしない**。
        判断側は「終わった/取り下げた件に続きが来た」ことを観測し、`reopen` を提案できる
        必要がある (`done`→`parked`/`dropped`→`parked`、設計 §3.2)。ただし
        `summary`/`link` のような他の verb をそこへ打つと生のエラー
        (`no transition for action "attrs_set" from status "done"`) が返り、subagent は
        「システムに拒否された」としか読めずに諦める —— 2026-08-23 の評価で実際に起き、
        同じ状況で次の巡はたまたま `skip` を選んで通った。**同じ状況で判断が割れる**ので、
        ここで選択肢を絞る。

        **書き先を変えて逃がさない。** 「書けないから sweep の progress へ」とすると、
        更新していない task について `handled` (= この task を更新した) の記録が残る。
        履歴を読んだ人が誤解し、差分ガード (S-8) が見る材料も狂う。**outcome は判断
        そのもの**であって、書き込みが通る方へ倒すものではない —— 更新していないなら
        `skipped` が正しく、その channel は元から progress になる。

        **boid 側の制限は緩めない。** 2026-08-23 の事例では、拒否は仕事をしていた ——
        subagent はメタプロジェクトの identity を **別 project の終端の子タスク**へ結びつけようと
        しており、通っていれば以降その identity は書けない task に解決していた。
        **失敗の本体は「終端」ではなく「別 project」だった**ので、project を独立した
        条件にしてある (status に関係なく拒む)。

        **遷移 verb は現在の status とも突き合わせる** (2026-08-25、PR-K レビュー
        HIGH 1)。go/start/drop は parked から、park は working から、complete は
        parked/working から、reopen は done/dropped からしか適用できない
        (`internal/orchestrator/machine_card.go`)。
        boid の `validateSuggestionAttr` は verb 文字列しか見ず status とは突き合わせ
        ないので、ここで拒まないと「書き込みは成功するが accept すると 409」という
        queue に居座り続ける壊れた suggestion ができる —— これが本命の欠陥だった。

        status を読めないとき (task が消えている等、または `get_card()` は成功したが
        応答に `status` が無い) は素通しする。ここで塞ぐと、読めない理由が何であれ
        判断が止まる —— 迷ったら handler に任せて、boid のエラーを見せる方が情報が多い。
        **status ベースの各チェックは status が空文字のときは一律スキップする**
        (2026-08-25、PR-K レビュー finding D — 遷移 verb のチェックだけこの姿勢を
        暗黙に反転させていたのを訂正)。
        """
        task_id = str(command.get("task_id") or "")
        if not task_id:
            return
        try:
            view = self.cli.get_card(task_id)
        except Exception:  # noqa: BLE001 - 読めないなら判断材料が無い。素通しする
            return
        status = str(view.get("status") or "")
        project = str(view.get("project_id") or "")

        own = self._own_project()
        if project and own and project != own:
            raise self._use_skip(
                verb, task_id, f"別の project ({project}) の task です",
                "メタプロジェクトは自分の project の card にしか書きません",
            )
        if status in _NO_WRITE_STATUSES:
            raise self._use_skip(
                verb, task_id, f"{status} です",
                "この status には記録を書く経路がありません (boid の machine が attrs_set を拒否する)",
            )
        allowed_here = _TERMINAL_ALLOWED_VERBS.get(status)
        if allowed_here is not None and verb not in allowed_here:
            raise self._use_skip(
                verb, task_id, f"{status} です",
                f"{status} の card に打てるのは {', '.join(sorted(allowed_here))} だけです "
                f"(再燃の観測/提案経路)",
            )
        transition_statuses = _TRANSITION_VERB_STATUSES.get(verb)
        if status and transition_statuses is not None and status not in transition_statuses:
            next_verbs = _transition_verbs_from(status)
            hint = (
                f"status={status} から提案できるのは {', '.join(next_verbs)} です"
                if next_verbs
                else f"status={status} からはどの遷移も提案できません"
            )
            raise self._use_skip(
                verb, task_id, f"status={status} なので {verb} は提案できません",
                hint,
            )

    def _own_project(self) -> str:
        """このメタプロジェクト自身の project id。sweep task から引く (1 回だけ)。"""
        if self._own_project_id is None:
            try:
                self._own_project_id = str(self.cli.get_card(self.sweep_task_id).get("project_id") or "")
            except Exception:  # noqa: BLE001 - 引けないなら project 判定を諦める
                self._own_project_id = ""
        return self._own_project_id

    @staticmethod
    def _use_skip(verb: str, task_id: str, why: str, detail: str) -> CommandError:
        return CommandError(
            f"{verb}: 対象 task {task_id} は{why}。{detail}。\n"
            f"この巡の判断は `skip` で記録してください —— reason に「なぜやることが無いか」"
            f"(例: 既に別 task で完了済み、その task id) を書く。\n"
            f"**`skip` に書き換えるだけでよく、判断をやり直す必要はありません。**"
        )

    # -- 記録と伝達 -------------------------------------------------------

    def _record(self, verb: str, command: Mapping[str, object], result: Result) -> None:
        """S-11: どの verb でも、書き込みが成功したら処理記録を付ける。

        **2026-08-29、PR-2やり直しv2: `attrs_set` への構造化書き込み (構造化キー、
        `domain/record.py` の `encode_attrs`/`channel_of`/`Record`/`Outcome`) をやめ、
        常に平文の `boid task notify --progress` で人が読めるサマリを sweep task 自身の
        timeline に書く。** 旧実装の docstring にあった「書かないと検知が同じシグナルを
        再検知して attempts を焼く」という理由は、`boid action list` を読んで判断済みの
        record を機械的に照合していた旧 detection 前提のもので、現行の検知
        (`boid signal list --claim` の pending/ack) は記録の中身を一切読まないので
        該当しない —— 記録はもう「機械可読な処理済みマーカー」である必要が無く、
        人が timeline を読んで経緯を追えれば足りる。

        **差分ガードの対象外。何も変わらなかった巡でも書く** (S-8)。

        **crash 安全性の順序 (記録の書き込み → ack) は維持する。** 記録の書き込みが
        例外で落ちれば `run()` はここまで到達せず、signal は pending のまま残る
        (§6.1 決定事項 6「ack を打つ順序: sweep が判断を書いた直後に自分で ack する。
        ack を先に打ってはいけない」——crash した signal を pending のまま取り逃さない)。

        **記録は全部 1 本の sweep task の timeline に集約される** (2026-08-29、
        Opus レビュー major 指摘)。旧実装は task ごとの attrs に記録が散っていたが、
        いまはどの verb でも sweep task 自身の timeline に書く。**どの task の話かが
        本文から分からないと、この 1 本の timeline を後から読んだ人が混乱する**ので、
        `task_id` があるときは必ず本文に含める。
        """
        signals = tuple(str(key) for key in (command.get("signals") or ()))  # type: ignore[union-attr]
        if not signals:
            return
        outcome = "skipped" if verb == "skip" else "handled"
        note = result.note.strip()
        message = f"{outcome} ({verb})"
        if result.task_id:
            message += f" task={result.task_id}"
        message += f" {', '.join(signals)}"
        if note:
            message += f": {note}"
        self.cli.notify_progress(self.sweep_task_id, message[:_MAX_PROGRESS_MESSAGE_CHARS])
        inbox.ack(self.cli, signals)

    def _deliver(self, result: Result) -> None:
        """S-7: サマリーが変わった、または新しい spec が出た task についてだけ通知する。
        **判断が 0 件の巡は無言。**"""
        if result.changed and result.notify and result.task_id:
            self.cli.notify(result.task_id, result.notify)

    # -- verb ごとの処理 ---------------------------------------------------

    def _do_capture(self, c: Mapping[str, object]) -> Result:
        identity = str(c["identity"])
        urgency = str(c["urgency"])
        task_id, created = self.cli.resolve_or_capture(
            identity, title=str(c["title"]), description=str(c["body"])
        )
        if created:
            # identity を attrs にも置く —— 検知が task を identity で引ける索引になる。
            # **既存 task には書かない**: `resolve-or-capture` は合流時も成功で返るので、
            # 毎回初期化すると人が触った値を上書きする。urgency も同じ理由で、合流先の
            # 値には触れない (変えたいなら `urgency` verb を使う)。
            self.cli.send_action(task_id, "attrs_set", {"identity": identity})
            self._write_urgency(task_id, urgency)
        return Result(
            task_id=task_id,
            changed=created,
            note=f"起票 (urgency={urgency})" if created else "既存に合流",
        )

    def _write_urgency(self, task_id: str, urgency: str) -> None:
        """urgency を書く (S-2 の機構側)。

        **並び順だけの属性になった** (設計 §3.6) ので、v1 にあった status 依存の分岐
        (「someday だけ captured に留める」「triaged 以降は someday に下げない」——
        いずれも `triage: captured → triaged` という v1 の card 遷移を前提にしていた)
        は丸ごと不要になった。card は新規作成時点で既に parked (`ResolveOrCapture` の
        初期 status、boid PR #987) で、urgency はどの status でも同じ 1 本で足りる。
        """
        self.cli.send_action(task_id, "attrs_set", {"urgency": urgency, "kind": "signal"})

    def _do_link(self, c: Mapping[str, object]) -> Result:
        self.cli.link_identity(str(c["identity"]), str(c["task_id"]))
        return Result(task_id=str(c["task_id"]), changed=True, note=f"{c['identity']} を合流")

    def _do_summary(self, c: Mapping[str, object]) -> Result:
        task_id = str(c["task_id"])
        current = str(self.cli.get_card(task_id).get("description") or "")
        merged = summary.merge(current, str(c["body"]))
        if merged == current:
            return Result(task_id=task_id, changed=False, note="サマリーに変化なし")
        self.cli.update_description(task_id, merged)
        line = summary.headline(str(c["body"]))
        if line:
            # 行バッジ (queue 一覧) が読む 1 行。**body から導出する** ——
            # subagent に別途書かせると本文とずれ、しかも機械では検査できない。
            self.cli.send_action(task_id, "attrs_set", {"summary": line})
        return Result(task_id=task_id, changed=True, note="サマリーを更新", notify=line or "サマリーを更新した")

    def _do_spec(self, c: Mapping[str, object]) -> Result:
        task_id = str(c["task_id"])
        cid = child_id(str(c["work"]), str(c["origin"]))
        children = detail_of(self.cli.get_card(task_id)).get("children") or ()
        if any(isinstance(ch, Mapping) and ch.get("id") == cid for ch in children):
            # boid 側も冪等 noop だが、**通知が毎巡飛ぶのを止める**のはこちらの役目。
            return Result(task_id=task_id, changed=False, note=f"子 {cid} は既にある")
        project = self.cli.resolve_project(str(c["project"]))
        self._refuse_absent_behavior(project, str(c["project"]), str(c["behavior"]))
        # **2 本を送り切る。** `child_specced` は update-only なので、`child_added` が
        # 先に無いと 409 (`domain/childid.py` の順序契約)。
        self.cli.send_action(task_id, "child_added", {"id": cid, "title": str(c["title"])})
        spec: dict[str, str] = {
            "id": cid,
            "project": project,
            "behavior": str(c["behavior"]),
            "description": str(c["description"]),
        }
        # **書かなかったら、キーごと送らない。** 空文字でも boid の per-field merge は
        # default から継承する (空は「未指定」) が、spec に空の instruction が residue と
        # して残ると、次に読んだ subagent が「ここは埋める欄だ」と解釈する。
        if c.get("instruction"):
            spec["instruction"] = str(c["instruction"])
        self.cli.send_action(task_id, "child_specced", spec)
        return Result(task_id=task_id, changed=True, note=f"子 {cid} を用意", notify=f"次の一手: {c['title']}")

    def _refuse_absent_behavior(self, project_id: str, project_ref: str, behavior: str) -> None:
        """振り先の project がその behavior を持っていなければ、**送る前に**拒む。

        `resolve_project` が project 名を送らせないのと同じ理由で、こちらも送らせない ——
        `child_specced` 自体は受け取られ、落ちるのは人が Go を押した (accept(go) した)
        あとの子タスク作成。card v2 ではこの失敗は同期エラーとして accept 操作自体に
        返るが (`internal/api/workflow_card.go`、旧 `workflow_triage.go`、の `acceptGo`)、
        **カードは parked の
        まま何も走らない**ことに変わりはない。そのとき task には specced な子が残って
        いて `spec_needed` は False —— 次の巡も正しい spec を書き直さない。

        最初のメタプロジェクト (khi-task-collector) が 2026-08-24 に踏んだ形:
        `respond` behavior はそのメタプロジェクトにしか無いのに、
        subagent が仕事の対象 repo (rook-server / mera-ui / rook-tools) を project に
        選んでいた。triaged 9 枚のうち 4 枚がこの状態だった。**返信・連絡系の子は
        メタプロジェクト自身へ振るのが正しい** —— そういう behavior は「人に文面を
        飛ばす」ための置き場としてメタプロジェクト側に作るもので、相手のリポジトリで
        動かす必要が無い。

        **照合できないときは通す。** 目的は「dispatch できない子を作らない」ことであって、
        `boid project behaviors` の可用性に判断を人質に取ることではない。読めないから
        書かない、にすると一時的な不調でその巡の判断そのものが失われる。**ただし
        「読めなかった」(`None`) と「1 つも宣言していない」(空) は別物**で、後者は
        まさに dispatch できない project なので拒む —— この 2 つを同じ「通す」側に
        倒していたのが 2026-08-29 のレビューで見つかった穴 (mutation M10 が生き残った)。

        **通した巡は stderr に残す。** ここは窓であって保証ではないので、後から人が
        「なぜ実在しない behavior の子ができたのか」を追える手がかりが要る。
        """
        available = self.cli.behaviors_of(project_id)
        if available is None:
            # 読めなかった。**照合できないときは通す** —— 目的は「dispatch できない子を
            # 作らない」ことであって、`boid project behaviors` の可用性に判断を人質に
            # 取ることではない。ただしこれは窓であって保証ではないので、後から人が
            # 「なぜ通ったのか」を追えるように残す (`behaviors_of` は project ごとに
            # 結果をキャッシュするので、1 巡の中では 1 回しか出ない)。
            print(
                f"[write] project {project_ref} の behavior 一覧を読めなかった —— "
                f"{behavior!r} の実在を確認せずに spec を書く",
                file=sys.stderr,
            )
            return
        if behavior in available:
            return
        if not available:
            # **空は「読めなかった」ではない。** behavior を 1 つも宣言していない
            # project は、まさに何も dispatch できない project —— ここを `None` と
            # 同じ「通す」側に倒すと、人が Go を押した瞬間に落ちて card が parked の
            # まま詰まる (この関数が防ぎたい当のもの)。
            raise CommandError(
                f"project {project_ref} は task_behaviors を 1 つも宣言していない。"
                "その project へは子を dispatch できない"
            )
        raise CommandError(
            f"project {project_ref} は behavior {behavior!r} を持っていない "
            f"(持っている値: {', '.join(sorted(available))})。"
            "返信・連絡系の子は project をメタプロジェクト自身にする"
        )

    def _do_drop_child(self, c: Mapping[str, object]) -> Result:
        """子を取り下げる。**間違えて立てた子を直す唯一の口。**

        `spec` は同じ id を弾く (差分ガード) ので、内容が違う正しい子を立てるには
        別 id になる —— つまり訂正が重複の形でしか表せない。閉じる口があれば
        「壊れた方を閉じて、正しい方を立てる」で済む。

        走っている子 (`dispatched`) は boid 側が 409 で拒む —— 実体が動いている
        最中に親から closed に見せると、記録と実体がずれる。
        """
        task_id = str(c["task_id"])
        child_id_ = str(c["child_id"])
        self.cli.send_action(task_id, "child_dropped", {"id": child_id_, "reason": str(c["reason"])})
        return Result(task_id=task_id, changed=True, note=f"子 {child_id_} を取り下げ")

    def _do_urgency(self, c: Mapping[str, object]) -> Result:
        task_id = str(c["task_id"])
        urgency = str(c["urgency"])
        self._write_urgency(task_id, urgency)
        return Result(task_id=task_id, changed=True, note=f"urgency={urgency}")

    def _do_park(self, c: Mapping[str, object]) -> Result:
        """park を suggest する (設計 §3.7: 「park を suggest (現行は即時実行 → 提案型に
        揃える)」)。v1 は `park` action を直接打っていたが、v2 では `park` が
        human-only の直接遷移になった (boid PR #987) ので、判断側は他の suggestion verb と
        同じ `attrs_set{suggestion:...}` でしか触れない。wake 条件必須 (S-13) は
        `_check_verb_rules` が引き続き守る —— params に載せて運ぶ。
        """
        params = {k: str(c[k]) for k in ("wake_at", "wake_task_id") if c.get(k)}
        return self._write_suggestion(c, "park", params=params)

    def _do_start(self, c: Mapping[str, object]) -> Result:
        return self._write_suggestion(c, "start")

    def _do_go(self, c: Mapping[str, object]) -> Result:
        return self._write_suggestion(c, "go")

    def _do_complete(self, c: Mapping[str, object]) -> Result:
        return self._write_suggestion(c, "complete")

    def _do_reopen(self, c: Mapping[str, object]) -> Result:
        return self._write_suggestion(c, "reopen")

    def _do_drop(self, c: Mapping[str, object]) -> Result:
        return self._write_suggestion(c, "drop")

    def _write_suggestion(
        self, c: Mapping[str, object], verb: str, *, params: Mapping[str, str] | None = None
    ) -> Result:
        """`attrs.suggestion = {verb, reason, params?}` を書く (設計 §3.1/§3.3)。

        verb は boid 自身の状態遷移語彙 (go/start/park/drop/complete/reopen) — daemon の
        `validateSuggestionAttr` がこの 6 つだけを受け付け、それ以外は 400 で拒否する。
        **1 枚の card に suggestion は 1 つだけで、書き直すと前の提案を上書きする**
        (最新が勝つ) —— 書き手はメタプロジェクト単独なので、上書きは「判断を
        更新した」の意味しか持たない。適用 (遷移の実行) は人の accept を経由する
        (`internal/api/suggestion_accept.go` の `applyAnswered`) —— ここでは提案する
        だけで、card の status は変わらない。
        """
        task_id = str(c["task_id"])
        # `reason` は 6 verb 全部で必須 (`_VERB_FIELDS`、park も 2026-08-25〜 LOW 8) なので
        # `validate()` を通った時点で必ず非空 —— ここでの空文字フォールバックは要らない
        # (2026-08-25、PR-K レビュー finding E: 旧コードの `c.get("reason") or ""` と
        # `if reason else verb` は両方とも到達不能な dead code だった)。
        reason = str(c["reason"])
        suggestion: dict[str, object] = {"verb": verb, "reason": reason}
        if params:
            suggestion["params"] = params
        self.cli.send_action(task_id, "attrs_set", {"suggestion": suggestion})
        return Result(task_id=task_id, changed=True, note=f"{verb}: {reason}")

    def _do_observed(self, c: Mapping[str, object]) -> Result:
        task_id = str(c["task_id"])
        closed = bool(c["source_closed"])
        current = self.cli.attrs_of(task_id).get("observed") or {}
        if isinstance(current, Mapping) and current.get("source_closed") is closed:
            # §5.1 の篩い 4 が読む値。**毎巡書くと 2026-08-20 の事故 (毎巡 judge が恒真) が
            # 再発する。**
            return Result(task_id=task_id, changed=False, note="observed に変化なし")
        self.cli.send_action(task_id, "attrs_set", {"observed": {"source_closed": closed}})
        return Result(task_id=task_id, changed=True, note=f"source_closed={closed}")

    def _do_skip(self, c: Mapping[str, object]) -> Result:
        # 書き先の task が無い。記録だけが残る (それがこの verb の全部)。
        return Result(changed=False, note=str(c["reason"]))

    def _do_done_signal(self, c: Mapping[str, object]) -> Result:
        return Result(task_id=str(c["task_id"]), changed=False, note="処理済み")


#: boid CLI 側の書き込みメソッドと、止めたときに返す値。`resolve_or_capture` だけは
#: 返り値が後続の処理に要るので、偽の task id を返して**その先も report できるようにする**。
_CLI_WRITES: Mapping[str, object] = MappingProxyType(
    {
        "send_action": None,
        "update_description": None,
        "notify_progress": None,
        "notify": None,
        "link_identity": None,
        "resolve_or_capture": ("dry-run-task", True),
        # `inbox.ack` が呼ぶ。dry-run では ack もしない —— 試し打ちで本物の signal が
        # 処理済み扱いになると、`--report` の「何も残さない」約束が崩れる。
        "ack_signals": None,
        # `inbox.claim` は今のところ write からは呼ばれないが、**これは allowlist**
        # なので載せ忘れは「黙って本物を書く」側に倒れる。attempts を進めるのは
        # 立派な副作用 (5 回で signal が dead に落ちる) で、試し打ちで消費される
        # 筋合いは無い。verb が 1 つ増えた日に気づける形にしておく。
        "claim_signals": None,
    }
)


class _Reporter:
    """dry-run (§10 step 4): 読みは素通し、**書きは実行せず記録する**。

    `readonly: true` では止められない —— boid の op は readonly のゲート対象外で、
    readonly が縛るのは API gateway の非 safe メソッドだけ。だから止める場所はここしか
    無く、**書き込みが記録 CLI を必ず通ることがこの step の前提**になる (S-14)。

    **`report=True` にするかどうかを argv (`--report`) だけに委ねない。** 2026-08-27、
    Opus レビュー指摘 (1 巡目): shadow task の subagent は
    shadow 用スキルの instruction が `--report` を必ず
    付けるよう書いてあることだけを根拠にしており、これは prompt 遵守の話でしかない ——
    subagent が読みに行く Jira/Slack/Bitbucket の本文経由のプロンプトインジェクション
    等で「`--report` を付けずに書け」という指示が紛れ込めば、readonly をすり抜けて
    実 card に書き込める余地が理論上あった。

    最初の実装は「sweep task の behavior が shadow behavior 名と一致するか」を
    `BOID_TASK_ID` env 経由で問い合わせる形にしていたが、**2 巡目の Opus レビューで
    その env 自体が subagent の shell 変数であり、`--report` を落とすのと同じ攻撃者が
    自由に書き換えられる (`BOID_TASK_ID=<偽の task id>`) ことを指摘された** —— behavior
    名判定は argv 依存と脅威モデル上等価で、安全装置になっていなかった。

    `main()` は代わりに `boid task current --field readonly` (`BoidCLI.current_field`)
    を問い合わせる。これは「今動いている sandbox の job そのもの」を daemon に直接
    尋ねる経路で、**`BOID_TASK_ID` を書き換えても偽装できない**
    (実機確認: `BOID_TASK_ID=<別task> boid task current --field readonly` は daemon に
    `restricted to the current task` で拒否される)。readonly が `"false"` でなければ
    (= `"true"` か、読めない/想定外の値) `report=True` を強制する (`_readonly_forces_report`)
    —— fail-closed。read 失敗時に fail-open (real 扱い) していた最初の実装は、
    「task_field が失敗しても send_action は通る」ような部分故障を安全装置の常設
    バイパスにしてしまう (2 巡目レビュー指摘) ので、判断できないときは書かない側に倒す。

    **この機構が守るのはこの CLI (`scripts/write.py`) を通る書き込みだけ。**
    readonly が縛るのは API gateway の非 safe メソッドとファイルシステムだけなので、
    もし subagent が `boid action send` を直接叩けばこの enforcement は素通しになる
    (判断スキルが「書きはここだけを通す」と instruction で縛っている前提の
    上に立つ、最後の 1 段の機械的な裏付けであって、それ自体が唯一の防衛線ではない)。

    書き手は boid CLI (`_CLI_WRITES`) だけ (canonical 撤去以前は Jira 起票側
    `_ISSUER_WRITES` を渡す非デフォルト呼び出しがあった —— 2026-08-25、PR-K レビュー
    LOW 9 で削除。差し替え可能にしておく理由が無くなったので、パラメータ自体を消して
    素朴にした)。
    """

    def __init__(self, inner: object, sink: list[str]) -> None:
        self._inner = inner
        self._sink = sink
        self._writes = _CLI_WRITES

    def __getattr__(self, name: str):
        if name not in self._writes:
            return getattr(self._inner, name)
        value = self._writes[name]

        def stub(*args, **kwargs):
            shown = ", ".join([*(repr(a) for a in args), *(f"{k}={v!r}" for k, v in kwargs.items())])
            self._sink.append(f"{name}({shown})")
            return value

        return stub


# ---------------------------------------------------------------------------
# エントリポイント
# ---------------------------------------------------------------------------

#: 記録の書き先 (sweep task 自身) を知るための環境変数。sandbox 内では boid が入れる。
TASK_ID_ENV = "BOID_TASK_ID"


def _readonly_forces_report(cli: object, stderr: object) -> bool:
    """**今動いている自分自身**が readonly な task なら、argv 抜きでも report を強制する。

    `--report` は subagent の prompt 遵守に委ねられているだけなので、これだけを頼ると
    Jira/Slack/Bitbucket の本文経由のプロンプトインジェクションで「`--report` を付けずに
    書け」という指示が紛れ込んだとき、readonly をすり抜けて実 card に書き込める余地が
    残る (`_Reporter` の docstring)。`cli.current_field("readonly")`
    (`boid task current --field readonly`) は実行中の job そのものを daemon に直接
    問い合わせる経路で、**`BOID_TASK_ID` 環境変数を書き換えても偽装できない** —— shadow
    behavior 名のハードコードやそれの project.yaml との drift 管理も不要になる。

    **`"false"` 以外は全部 report 側に倒す (fail-closed)。** 読めない・想定外の値・
    `"true"` のいずれも区別せず report を強制する —— ここは security-critical な判定で、
    「boid が壊れていれば real の書き込みもどのみち失敗する」という fail-open の理屈は
    部分故障 (`current_field` だけ失敗し `send_action` は通る等) を考えていない。
    判断できないときに書いてしまう方が、判断できないときに書き損ねるより悪い。fail-closed
    の巡は「その回だけ書けなかった」として次巡が同じシグナルを拾い直す (S-6 の重複許容)
    ので、可用性の代償は一時的な遅延にとどまる。
    """
    try:
        readonly = cli.current_field("readonly")
    except Exception as exc:  # noqa: BLE001 - 読めないのも fail-closed の理由になる
        print(f"[write] readonly を引けなかった、安全側 (report) に倒す: {exc}", file=stderr)
        return True
    if readonly == "false":
        return False
    if readonly != "true":
        print(f"[write] readonly が想定外の値 ({readonly!r})、安全側 (report) に倒す", file=stderr)
    return True


def main(
    argv: Sequence[str] | None = None,
    *,
    stdin: object = None,
    stdout: object = None,
    stderr: object = None,
    cli: object = None,
    env: Mapping[str, str] | None = None,
) -> int:
    """`python3 ~/.claude/skills/boid-metaproject/scripts/write.py <verb> [--report] < payload.json`

    exit code は 3 つに分ける: 0 成功 / 1 拒否 (subagent が直せる) / 2 呼び出し方の誤り。
    **メッセージは stderr に出す** —— subagent が読んで言い直すための唯一の手がかり。
    """
    import os
    import sys

    argv = list(sys.argv[1:] if argv is None else argv)
    stdin = stdin if stdin is not None else sys.stdin
    stdout = stdout if stdout is not None else sys.stdout
    stderr = stderr if stderr is not None else sys.stderr
    env = env if env is not None else os.environ

    report = "--report" in argv
    verbs = [a for a in argv if not a.startswith("-")]
    if len(verbs) != 1:
        print(
            "使い方: python3 ~/.claude/skills/boid-metaproject/scripts/write.py"
            " <verb> [--report] < payload.json\n"
            f"  verb: {', '.join(VERBS)}",
            file=stderr,
        )
        return 2

    try:
        payload = json.load(stdin)
    except ValueError as exc:
        print(f"stdin を JSON として読めなかった: {exc}", file=stderr)
        return 2
    if not isinstance(payload, Mapping):
        print(f"stdin は JSON オブジェクトで渡す (受け取った型: {type(payload).__name__})", file=stderr)
        return 2

    sweep_task_id = env.get(TASK_ID_ENV, "")
    if not sweep_task_id:
        # 見送りの記録は sweep task 自身の timeline にしか置けない (§5.3 S-11 後半)。
        # 書き先が無いまま進むと、記録の付かない書き込みができてしまう。
        print(f"{TASK_ID_ENV} が無い —— 記録の書き先 (sweep task) が決まらない", file=stderr)
        return 2

    resolved_cli = cli if cli is not None else BoidCLI()
    if not report and _readonly_forces_report(resolved_cli, stderr):
        # readonly な task からは argv に `--report` が無くても report を強制する
        # (`_Reporter` の docstring、2026-08-27 Opus レビュー指摘対応)。
        report = True

    executor = Executor(
        resolved_cli,
        sweep_task_id=sweep_task_id,
        report=report,
    )
    try:
        executor.run(validate(verbs[0], payload))
    except CommandError as exc:
        print(str(exc), file=stderr)
        return 1
    except BoidError as exc:
        print(f"boid への書き込みに失敗した: {exc}", file=stderr)
        return 1
    if report:
        for line in executor.reported:
            print(line, file=stdout)
    return 0


if __name__ == "__main__":  # pragma: no cover - モジュール実行の配線だけ
    import sys

    sys.exit(main())
