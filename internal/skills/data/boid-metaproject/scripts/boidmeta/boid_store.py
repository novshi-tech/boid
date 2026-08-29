"""boid CLI (sandbox 内の shim) の薄いラッパ (設計 §6 の `adapters/boid_store.py`)。

**`Any` を扱ってよいのはこのファイルだけ** —— ここから先 (`domain/` と `app/`) は
型だけを相手にする。旧実装の規律をそのまま承継する。


## boid 側の契約 (ソースを読んで確認した事実)

- `boid action send --task <id> --type <type> [--payload -]` —— `--payload` は
  **ファイルパスか `-`** で、インライン JSON ではない (`internal/sandbox/boid_shim.go`)
- payload を消費する action type は限られる。それ以外に payload を付けると daemon が
  消費せず `task.Payload` へ merge してしまう (エラーにならないので気づけない)
- `boid task resolve-or-capture <identity>` は **`--status` を受け付けない**。
  着地 status は **`parked`** (`task_resolve_or_capture.go`。2026-08-25 訂正 ——
  card 機械 v2 以前は `captured` だった、D-15)。渡すと新規起票が恒久的に失敗する。
  **返るのは JSON** (`{"task_id": ..., "created": ...}`) で、素の id ではない
- `boid task update <id>` に **`--description-file` は無い** (`--patch-file` / `--title` /
  `--description` / `--payload-file` のみ)。description は `--patch-file -` に
  `{"description": ...}` を stdin で渡す
- **2026-08-25、card モデル整理 (`docs/plans/card-model-cleanup.md` PR-3) で wire 名が
  rename された。** `boid task triage <task-id>` / `boid task triage --list` は
  `boid card get <task-id>` / `boid card list` になった (broker op も
  `task_triage_get`/`task_triage_list` → `card_get`/`card_list`)。互換 alias は無い ——
  旧コマンドは daemon 側で消えている。応答の形は変わらない: 1 枚の projection で
  フィールドは task_id / project_id / title / status / description / kind / urgency /
  wake_at / wake_task_id / parked_from / detail (`internal/api/card_read.go` の
  `CardView`、旧 `TaskTriageView`) —— **top-level の `summary` は無い**。差分ガード
  (S-8) の比較元は `detail.attrs`
- `boid action list --since <cursor> --limit N` は workspace スコープの一括読み。
  `since=""` は「最初から」、返る `next_cursor` は行が 0 件なら渡した since と同じ
- `child_specced` の `project` は**解決済み UUID でなければならない**。`action send` は
  broker で名前解決されず、executor の `AllowsProject` は UUID の完全一致比較
- **`boid task notify --progress` は単独で通る。** broker が `--message` を要求する drift が
  あった (shim もサービス層も progress 単独を許すのに broker だけ厳しかった)。
  novshi-tech/boid#980 で修正され、**2026-08-22 にデプロイして実機で確認済み** ——
  ダミーの `-m` を併記する回避は撤去した
- **2026-08-27、検知を boid signal inbox 読みへ切り替えた** (`boidmeta.inbox`)。
  `boid signal list [--claim] [--source <pack>/<connector>] [--state pending|dead|acked|all]
  [--limit N]` は `{"signals": [...]}` を返す。1 件の envelope は
  `id`/`occurred_at`/`source.{pack,connector,service}`/`identity`/`url`/`author`/`title`/
  `received_at`/`attempts`/`acked_at` を持つ (`internal/skills/data/boid-signal/SKILL.md`)。
  `boid signal ack <id>...` は id 単位で idempotent (既に ack 済みでも成功) だが、
  存在しない id は呼び出し全体を失敗させる (typo guard)。
- **2026-08-28 に `--claim` を使う側に切り替えた。** boid 内部
  action も signal inbox 経由になった (boid core 側) のに合わせ、workspace 側の独自
  attempts (`domain/attempts.py`、3 回) を撤去して boid 側の `MaxSignalAttempts` (5) に
  そのまま乗る。`--claim` は選択と同時に `attempts` を進めるので、誰も ack しない
  signal はいずれ boid 側で dead になる (`internal/skills/data/boid-signal/SKILL.md`
  「Dead Signals」)。
- **`boid task wait <id>`** (2026-08-29、boid #1032) は task が終端になるまでブロックし、
  `done` のときだけ exit 0 で返す (`aborted`/`dropped` は非ゼロ + 理由が stderr)。
  trigger の `run:` がこれを使うので、**この repo の Python から task を起こす/待つ
  コードは無くなった** —— `create_task`/`create_task_idempotent`/`reopen_task`/
  `task_list`/`list_cards`/`actions_for` は 2026-08-29 に削除済み
  (`.boid/project.yaml` の `triggers.sweep` 参照)。
"""
from __future__ import annotations

import json
import subprocess
from typing import Any, Callable, Mapping, Sequence

DEFAULT_BIN = "boid"
DEFAULT_TIMEOUT_SECONDS = 60.0

#: `boid signal list --limit N` の既定値 (2026-08-27 カットオーバー設計の確定事項)。
#: shim (`internal/sandbox/boid_shim.go` の `parseBoidSignalList`) 自体には
#: `action list` のような hard cap は無いが、1 巡で無制限に読もうとしないための
#: 上限としてここで明示する。
DEFAULT_SIGNAL_LIST_LIMIT = 1000

#: payload を消費する action type。これ以外に payload を付けると daemon が消費せず
#: `task.Payload` へ merge する (`internal/api/workflow_action.go` の
#: `sideEffectConsumesPayload`、6 つ: park/attrs_set/child_added/child_specced/
#: child_dropped/noted)。**`answered` も消費側だがこの map には無い** —— `answered` は
#: card 遷移の accept/reject 専用実装 (`applyAnswered`,
#: `internal/api/suggestion_accept.go`) に完全にバイパスされ、`sideEffectConsumesPayload`
#: を経由する汎用マージパイプラインへ一度も到達しないので、そもそも `task.Payload` へ
#: 誤って merge される心配が無い (旧 `scripts/daemon_sync.py` の集合には無かったが、
#: boid のソースで確認した)。過剰に厳しいと「daemon が正しく消費するのに送らせない」で詰まる。
#: (`reopen` は payload に `instruction` があるときだけ条件付きで消費するのでこの集合には
#: 入れない。`reopen_triaged` は v1 の決定17 name-rewrite ルーティング専用の名前で、
#: card 機械分離により v2 では両機械とも素の `reopen` を使うため消滅した
#: (`internal/api/workflow_action.go`)。)
#: **`child_dropped` を落としていて本番で詰まった** (2026-08-23)。boid #982 で daemon 側に
#: 足したのに、この集合と drift テストが 6 つのまま「daemon と同じ」と主張して green だった
#: —— `drop-child` verb は adapter に弾かれて一度も boid へ届いていなかった。
#: **boid 側に action type を足したら、必ずここも足す。**
PAYLOAD_CONSUMING = frozenset({
    "park", "attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered",
})

#: `internal/sandbox/protocol.go` の値。stderr 文字列ではなく exit code で判定する契約。
IDENTITY_CONFLICT_EXIT_CODE = 3

#: 同上。`identity resolve` の「そんな束縛は無い」。**エラーではない** —— boid 側の設計が
#: 明示している (get-or-create をする呼び出し側が、stderr の pattern match 無しで
#: 「無かった」と「本当の失敗」を見分けられるようにするため)。
IDENTITY_NOT_FOUND_EXIT_CODE = 2


class BoidError(RuntimeError):
    """boid CLI の呼び出しが失敗した / 応答を解釈できなかった。"""


class BoidCLI:
    """`boid` を subprocess で叩く。

    `run` はテスト用の差し替え口 (既定は `subprocess.run`)。ここを差し替えれば、
    テストは boid にもネットワークにも触れずに「どんな引数を組み立てるか」を固定できる。
    """

    def __init__(
        self,
        *,
        bin: str = DEFAULT_BIN,
        timeout: float = DEFAULT_TIMEOUT_SECONDS,
        run: Callable[..., "subprocess.CompletedProcess[str]"] = subprocess.run,
    ) -> None:
        self._bin = bin
        self._timeout = timeout
        self._run_subprocess = run
        self._project_index: dict[str, str] | None = None
        #: project id -> その project の behavior 名 (読めなかったら None)。
        self._behavior_index: dict[str, tuple[str, ...] | None] = {}

    # -- 低レベル ---------------------------------------------------------

    def _exec(self, args: Sequence[str], *, stdin: str | None = None) -> tuple[int, str, str]:
        # `encoding` を明示する —— `text=True` だけだとロケール依存になり、C ロケールでは
        # 日本語の payload が UnicodeEncodeError になる。この層は `ensure_ascii=False` で
        # 日本語をそのまま通すことを設計として選んでいるので、入口側も固定しておく。
        proc = self._run_subprocess(
            [self._bin, *args],
            input=stdin,
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=self._timeout,
        )
        return proc.returncode, proc.stdout, proc.stderr

    def _run(self, args: Sequence[str], *, stdin: str | None = None) -> str:
        code, out, err = self._exec(args, stdin=stdin)
        if code != 0:
            raise BoidError(f"boid {' '.join(args)} が exit={code} で失敗した: {err.strip() or out.strip()}")
        return out

    def _run_json(self, args: Sequence[str], *, stdin: str | None = None) -> Any:
        out = self._run(args, stdin=stdin).strip()
        if not out:
            return None
        try:
            return json.loads(out)
        except ValueError as exc:
            raise BoidError(f"boid {' '.join(args)} の出力を JSON として読めなかった: {out[:200]!r}") from exc

    # -- 書き込み ---------------------------------------------------------

    def send_action(self, task_id: str, action_type: str, payload: Mapping[str, Any] | None = None) -> None:
        args = ["action", "send", "--task", task_id, "--type", action_type]
        stdin = None
        if payload is not None:
            if action_type not in PAYLOAD_CONSUMING:
                raise BoidError(
                    f"action type {action_type!r} は payload を消費しない —— "
                    f"送ると daemon が task.Payload へ merge する (許容: {sorted(PAYLOAD_CONSUMING)})"
                )
            args += ["--payload", "-"]
            stdin = json.dumps(payload, ensure_ascii=False)
        self._run(args, stdin=stdin)

    def resolve_or_capture(self, identity: str, *, title: str, description: str) -> tuple[str, bool]:
        """identity で task を引き当て、無ければ立てる。返るのは `(task_id, created)`。

        **返るのは JSON** —— `{"task_id": "...", "created": true}` (`internal/server/
        boid_executor.go`)。素の id として読むと、その文字列が以降の `--task` に載って
        全部 404 になる。`created` は S-2 の「立てた直後に初期化するか / 合流なら
        しないか」の分岐に使える情報なので捨てない。

        **`--status` は渡さない** (D-15)。衝突 (exit=3) は例外にする —— 新実装では
        合流の判断を subagent がしてから `link` verb で明示的に繋ぐ (S-15) ので、
        ここで暗黙に解決すると判断を機構が横取りすることになる。
        """
        args = ["task", "resolve-or-capture", identity, "--title", title, "--description-file", "-"]
        code, out, err = self._exec(args, stdin=description)
        if code == IDENTITY_CONFLICT_EXIT_CODE:
            raise BoidError(f"identity が既存 task と衝突した: {identity!r} ({err.strip()})")
        if code != 0:
            raise BoidError(f"resolve-or-capture が exit={code} で失敗した ({identity!r}): {err.strip()}")
        try:
            result = json.loads(out.strip())
        except ValueError as exc:
            raise BoidError(f"resolve-or-capture の応答を JSON として読めなかった ({identity!r}): {out[:200]!r}") from exc
        task_id = result.get("task_id") if isinstance(result, Mapping) else None
        if not isinstance(task_id, str) or not task_id:
            raise BoidError(f"resolve-or-capture が task_id を返さなかった ({identity!r}): {result!r}")
        return task_id, bool(result.get("created"))

    def update_description(self, task_id: str, description: str) -> None:
        """description を差し替える。

        **`--description-file` は無い。** shim の `task update` が受けるのは
        `--patch-file` / `--title` / `--description` / `--payload-file` だけで、
        `--description-file` は `resolve-or-capture` 専用 (`internal/sandbox/boid_shim.go`)。
        インラインの `--description` は日本語の長文をコマンドラインに載せることになるので、
        argv 長と改行を避けて `--patch-file -` (= stdin) に JSON で渡す。
        """
        patch = json.dumps({"description": description}, ensure_ascii=False)
        self._run(["task", "update", task_id, "--patch-file", "-"], stdin=patch)

    def notify_progress(self, task_id: str, message: str) -> None:
        """timeline に progress action を 1 本書く (§5.3 の記録の置き場)。"""
        self._run(["task", "notify", task_id, "--progress", message])

    def notify(self, task_id: str, message: str) -> None:
        """人に届ける通知 (§5.4 伝達)。progress と違い hook が発火する。"""
        self._run(["task", "notify", task_id, "-m", message])

    def resolve_identity(self, identity: str) -> tuple[str, str] | None:
        """identity の帰属先 `(task_id, status)`。**未登録は `None`** (§5.1)。

        非 boid シグナルを「既存の件の続き」と「新しい件」に分けるのがこの呼び出し。
        未登録を例外にすると新規候補が 1 件も立たなくなる —— **未登録の identity こそが
        起票の候補**なので、そこで落ちると経路全体が死ぬ。

        返るのは task の id と status だけ (`internal/server/boid_executor.go` が
        「delivery-address lookup であって task-detail read ではない」と明示)。
        """
        code, out, err = self._exec(["task", "identity", "resolve", identity])
        if code == IDENTITY_NOT_FOUND_EXIT_CODE:
            return None
        if code != 0:
            raise BoidError(f"boid task identity resolve {identity} が exit={code} で失敗した: {err.strip() or out.strip()}")
        try:
            parsed = json.loads(out.strip() or "{}")
        except ValueError as exc:
            raise BoidError(f"boid task identity resolve {identity} の出力を JSON として読めなかった: {out[:200]!r}") from exc
        if not isinstance(parsed, Mapping):
            raise BoidError(f"boid task identity resolve {identity} の応答が想定外: {parsed!r}")
        task_id = str(parsed.get("task_id") or "")
        if not task_id:
            raise BoidError(f"boid task identity resolve {identity} が task_id を返さなかった: {parsed!r}")
        return task_id, str(parsed.get("status") or "")

    def link_identity(self, identity: str, task_id: str) -> None:
        """別 source 発のシグナルを既存の件へ合流させる (S-15)。

        引数の順は `boid task identity link <identity> <task-id>`
        (`internal/sandbox/boid_shim.go`)。**逆にすると identity として task id が
        登録され、以降その task を identity で引けなくなる。**
        """
        self._run(["task", "identity", "link", identity, task_id])

    # -- 読み -------------------------------------------------------------

    def get_card(self, task_id: str) -> Mapping[str, Any]:
        """1 枚の card projection (`internal/api/card_read.go` の `CardView`、旧
        `TaskTriageView`)。**2026-08-25 に `boid task triage <id>` → `boid card get <id>`
        へ rename** (`docs/plans/card-model-cleanup.md` §4)。互換 alias は無い。
        """
        view = self._run_json(["card", "get", task_id])
        if not isinstance(view, Mapping):
            raise BoidError(f"card get {task_id} の応答が想定外: {view!r}")
        return view

    def task_field(self, task_id: str, field: str) -> str:
        """task の 1 フィールドを引く (`boid task show <id> --field <path>`)。

        S-9 の actor 判別に使う —— この機構は「自分が起こした task の id」を覚えないので、
        **action に現れた actor の task を引いて behavior を見る**。`task triage --list` の
        行にも `task list` の出力にも behavior は載っていないので、経路はここだけ。

        **`task_id` は引数で渡した文字列をそのまま送るだけ** —— 呼び出し側が
        `BOID_TASK_ID` 環境変数由来の値をここに渡すと、その値は subagent の shell が
        自由に書き換えられる (env spoof)。security-critical な自己判定 (「今動いている
        自分がどの task か」) には使わないこと —— そちらは `current_field` を使う。
        """
        return self._run(["task", "show", task_id, "--field", field]).strip()

    def current_field(self, field: str) -> str:
        """**実行中の job そのもの**の 1 フィールドを引く (`boid task current --field <path>`)。

        `task_field` と違い対象を引数で渡さない —— daemon が「この sandbox が今どの
        task の job として動いているか」を直接答える経路なので、**`BOID_TASK_ID` 環境
        変数を書き換えても偽装できない** (2026-08-27、Opus レビュー実機確認:
        `BOID_TASK_ID=<別task> boid task current --field readonly` は daemon に
        `boid task current is restricted to the current task` で拒否される)。

        `app/write.py` が readonly な task (shadow-b 等) から呼ばれたかどうかを機械的に
        判定する (`_readonly_forces_report`) のに使う —— behavior 名のハードコードや
        project.yaml との drift 管理が要らず、env 経由の偽装も塞げる。
        """
        return self._run(["task", "current", "--field", field]).strip()

    def attrs_of(self, task_id: str) -> Mapping[str, Any]:
        """差分ガード (S-8) の比較元。`detail.attrs` を取り出す。

        `CardView.Detail` (旧 `TaskTriageView.Detail`) は `json.RawMessage` なので実際には
        常にオブジェクトで返る。`detail_of` が文字列も読むのは、生の blob を別経路
        (DB ダンプ、旧 API) から食わせても壊れないようにするための防御であって、boid の
        現行応答の形ではない。
        """
        return detail_of(self.get_card(task_id)).get("attrs") or {}

    def list_signals(
        self,
        *,
        state: str = "pending",
        limit: int | None = DEFAULT_SIGNAL_LIST_LIMIT,
    ) -> Mapping[str, Any]:
        """workspace の signal inbox を読む (`boid signal list [--state S] [--limit N]`、
        `internal/skills/data/boid-signal/SKILL.md`)。**副作用は無い。**

        **2026-08-29、boid #1033 で `--claim` を渡さない側に戻した。** `--claim` は
        読み出しが返した行に一律で `attempts` を +1 する形で、「読んだ」と「判断に
        回した」を区別できなかった —— 読み手は合流を見込んで対象上限の数倍を読み、
        判断するのは最大 `MAX_TARGETS` 件なので、次巡送りにしただけの行の attempts が
        毎巡焼かれ、5 巡で誰も判断していない signal が dead に落ちる。読みは無料に
        戻し、判断に回す行は `claim_signals` で名指しする。

        返るのは `{"signals": [...]}` の生の JSON (envelope 列)。マッピング
        (envelope → `domain.signal.Signal`) はここではやらない —— この層は
        `Any` を扱ってよい唯一の場所という規律を守り、型付けは呼び出し側
        (`boidmeta.inbox`) がやる。
        """
        args = ["signal", "list", "--state", state]
        if limit is not None:
            args += ["--limit", str(limit)]
        payload = self._run_json(args)
        if not isinstance(payload, Mapping):
            raise BoidError(f"signal list の応答が想定外: {payload!r}")
        return payload

    def claim_signals(self, ids: Sequence[str]) -> None:
        """判断に回す signal を名指しして `attempts` を進める
        (`boid signal claim <id>...`、boid #1033)。

        **`attempts` は「諦めるまでの回数」なので、数える対象は「判断に回した」で
        なければならない。** 見ただけの行を数えると、この巡で扱いきれず次巡へ送った
        signal が 5 巡で dead に落ちる。

        **ack と同じ typo guard がある** —— 1 件でも boid 側に無い id が混じると
        呼び出し全体が失敗し、何も課金されない。id は同じ巡の `list_signals` が
        返したものをそのまま渡すので、通常は外れない (外れるとすれば、読んでから
        claim するまでに GC 等でその行が消えた場合)。
        """
        if not ids:
            return
        self._run(["signal", "claim", *ids])

    def ack_signals(self, ids: Sequence[str]) -> None:
        """`boid signal ack <id>...`。**id が 1 つも無ければ何もしない**
        (無駄な subprocess 起動を避ける —— 呼び出し側の `boidmeta.inbox.ack`
        も同じ理由で空リストを先に弾いているが、ここでも独立に守る)。
        """
        if not ids:
            return
        self._run(["signal", "ack", *ids])

    def resolve_project(self, name_or_id: str) -> str:
        """project 名を UUID へ。既に UUID ならそのまま返す。

        解決できなければ例外 —— **そのまま送ってはいけない**。`child_specced` 自体は
        受け取られて、dispatch のときに初めて落ちる。その頃には task に specced な子が
        残っていて `spec_needed` が False になり、二度と正しい spec が書かれない。
        """
        index = self._project_index_map()
        if name_or_id in index.values():
            return name_or_id
        resolved = index.get(name_or_id)
        if resolved is None:
            raise BoidError(f"project 名を id に解決できなかった: {name_or_id!r} (既知: {sorted(index)})")
        return resolved

    def behaviors_of(self, project_id: str) -> tuple[str, ...] | None:
        """その project が持つ task_behavior 名。**読めなければ `None`。**

        `None` は「無い」ではなく「照合できなかった」を意味する —— 呼び出し側
        (`app/write.py` の `_refuse_absent_behavior`) はこれを素通しに倒す。
        project が消えている / CLI が一時的に応答しない、で判断そのものを落とさない。

        JSON のキーだけ要る (値は instruction 本文まで含んでいて大きい)。
        プロセス内でキャッシュする —— 1 巡で同じ project へ複数の子を立てることがある。
        """
        if project_id in self._behavior_index:
            return self._behavior_index[project_id]
        try:
            raw = json.loads(self._run(["project", "behaviors", project_id]))
        except (BoidError, ValueError):
            names: tuple[str, ...] | None = None
        else:
            names = tuple(raw) if isinstance(raw, Mapping) else None
        self._behavior_index[project_id] = names
        return names

    def _project_index_map(self) -> Mapping[str, str]:
        """`boid project list` から name -> id を作る (プロセス内で 1 度だけ)。

        **出力形式はホストとサンドボックスで違う** —— ホストの実 CLI は plain テキスト
        (`<id> <status> <name> (<path>) ...`)、sandbox の shim は JSON で、しかも
        `--output json` を受け付けない。両方読む (旧 `scripts/project_id.py` の実測知見)。
        """
        if self._project_index is not None:
            return self._project_index
        text = self._run(["project", "list"]).strip()
        index = _parse_project_list(text)
        self._project_index = index
        return index


def _parse_project_list(text: str) -> dict[str, str]:
    """`boid project list` の出力を name -> id にする。

    **JSON を途中まで読んで失敗したときに、その中途半端な結果を残さない。** 一度ローカルに
    組んで、完全に成功したときだけ採用する —— 混ぜると plain 解析が JSON 本文の上を走り、
    `'"name":' -> '[{"id":'` のようなゴミが index に入る。`resolve_project` は
    `name_or_id in index.values()` を「もう UUID だから素通し」の判定に使っているので、
    汚れた値がそのまま素通し集合になる。
    """
    try:
        rows = json.loads(text)
    except ValueError:
        # JSON ではない = ホストの plain 出力。
        index: dict[str, str] = {}
        for line in text.splitlines():
            parts = line.split()
            if len(parts) >= 3:
                index[parts[2]] = parts[0]
        return index
    # JSON として読めたなら plain 解析はしない —— **落ちた行があっても混ぜない**。
    # 混ぜると plain 解析が JSON 本文の上を走り、`'"name":' -> '[{"id":'` のようなゴミが
    # index に入る。`resolve_project` は values() を「もう UUID だから素通し」の判定に
    # 使っているので、汚れた値がそのまま素通し集合になる。読めた行だけ採る。
    if not isinstance(rows, list):
        return {}
    return {
        row["name"]: row["id"]
        for row in rows
        if isinstance(row, Mapping) and isinstance(row.get("name"), str) and isinstance(row.get("id"), str)
    }


def detail_of(view: Mapping[str, Any]) -> Mapping[str, Any]:
    detail = view.get("detail") or {}
    if isinstance(detail, str):
        try:
            detail = json.loads(detail) if detail.strip() else {}
        except ValueError:
            return {}
    return detail if isinstance(detail, Mapping) else {}


