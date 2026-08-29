# `project.yaml` リファレンス

プロジェクトのルートに置く `.boid/project.yaml` の全フィールドをまとめたリファレンスです。

このページは仕様の網羅を目的にしています。 用語の解説は [概念](../guide/concepts.md) を、 動かし方は [Getting started](../getting-started/) を参照してください。

## 役割と配置

- パス: プロジェクトルート直下の `.boid/project.yaml`
- 役割: そのディレクトリを `boid` プロジェクトとして登録し、タスクの種類 (behavior) を宣言する。 ポータブルで git 管理される
- 登録: `boid project add <project-root>` で `boid` の DB に取り込まれる
- 変更後の反映: `boid project reload` で再読み込みする

> **注意:** `project.yaml` はもう実行環境 (kits / `host_commands` / `env` / `secret_namespace` / `capabilities`) を設定しません。 これらの machine-local な設定は代わりに **workspace** に置きます (`boid workspace create/edit`)。 何がどこへ移動したかは下記 [トップレベルのフィールド](#トップレベルのフィールド) の表を、 現行のセットアップ手順は [オンボーディング](../guide/onboarding.md) を参照してください。 なお `additional_bindings` は project.yaml でも workspace でも撤去済みで、ツールチェーンの永続化は [workspace home の `init.sh`](../guide/workspace-home.md) に移りました (Phase 4 PR4)。

## 最小例

```yaml
id: demo
name: Demo
task_behaviors:
  supervisor:
    name: Supervisor
```

## トップレベルのフィールド

| キー | 型 | 必須 | 役割 |
|---|---|---|---|
| `id` | string | レガシーなホストディレクトリ登録 (`boid project add <project-root>`) では**必須**。git-URL 登録 (`boid project add <git-url>`) では**省略可** (docs/plans/workspace-default-project.md 論点h 案1) | `boid` 内でプロジェクトを一意に識別する文字列。タスク作成時に `project_id` で参照される。**git-URL 登録で省略時**: `boid project add <git-url>` 時点で origin URL から導出した id (`url-` + slug の sha256 先頭 16 文字) を使う。一度この URL 由来 id で登録された project は、後から project.yaml に `id:` が追加/変更されても id は変わらない (既存タスクとの紐付けを切らないため) — ただしこれは登録されている id が実際にその project の upstream URL から導出されたものである場合に限る。たまたま `url-` で始まるだけの手書き `id:` はこの例外の対象外であり、後からの変更は通常の id drift として扱われる。レガシーなホストディレクトリ登録には導出元となる URL が無いため、`id:` を省略した project.yaml は引き続き拒否される。**注**: id 導出には slug 化可能な URL が必要であり、`https://` と異なり `file://` に正規化された origin は slug 化できない。そのため `file://` 形式の git URL で登録する場合、`file://` 自体は git-URL 登録として通常受理されるにもかかわらず、`id:` を省略した project.yaml はレガシーなホストディレクトリ登録と同様に拒否される。 |
| `name` | string | はい | UI で表示するプロジェクト名 |
| `worktree` | bool | `false` | 以前は `true` で executor / supervisor タスクに専用の isolated branch (`boid/<id8>`) を割り当てていた。 **docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11) で per-task branch と fork point 概念が廃止**され、 root / child を問わず全タスクが sandbox 内 clone 上で `base_branch` を直接 checkout するようになったため、 このフィールドは現在 checkout 挙動に影響しない (スキーマ上は引き続き受理される)。 詳細は [タスク種別と HEAD branch](#タスク種別と-head-branch) を参照 |
| `base_branch` | string | (省略時は後述) | PR ターゲットとなるベースブランチ。 タスク作成時に解決して row に保存される。 **省略時**: root task は daemon の現 HEAD branch (`${current_branch}` 相当) に展開; child task は親の `base_branch` を継承。 detached HEAD で root task 作成時に省略すると 400 エラー。 `${TASK_REMOTE_ID}` / `${current_branch}` の展開をサポート (後述 [動的 base_branch](#動的-base_branch)) |
| `fork_point` | string | (省略時 `origin/HEAD` フォールバック) | `base_branch` がまだローカル / origin のどちらにも存在しない状態 (case 3) で branch を作るときの fork 起点。 任意の `git rev-parse --verify` で解決可能な ref を指定 (branch / tag / SHA / `origin/main` など)。 **未設定時は `refs/remotes/origin/HEAD` にフォールバック**。 origin/HEAD も未設定なら case 3 はエラー (`git remote set-head origin --auto` を実行するか、 `fork_point` を設定する)。 **project root の作業ツリー HEAD は意図的に参照されない** — タスク作成からディスパッチまでの間にユーザが root で別 branch をチェックアウトしていても、 fork 起点が暴れない。 詳細は [`fork_point` と case 3](#fork_point-と-case-3) を参照 |
| `task_behaviors` | map (string → TaskBehavior) | はい | このプロジェクトで作れる「タスクの種類」一覧 |
| `session_behaviors` | map (string → SessionBehavior) | いいえ | session (task を伴わない対話セッション) 起動時の既定 `harness_type`/`model` を用途キーごとに設定する辞書。`task_behaviors` とは別物 — 詳細は [`session_behaviors.<name>`](#session_behaviorsname) を参照 |
| `default_task_behavior` | string | いいえ | `boid task create` で `--behavior` を省略したときに使う behavior の名前。未指定の場合は `task_behaviors` に `supervisor` があれば暗黙で使う (WARN あり)、なければエラー |
| `triggers` | list of Trigger | いいえ | daemon が定期的に起こす readonly exec job の一覧 (docs/plans/ingestion-identity.md PR-4/B-5)。`task_behaviors` とは独立したトップレベルのフィールド。詳細は [`triggers[]`](#triggers) を参照 |
| `kits` | — | **撤去** | ロード時に reject される (`project.yaml: top-level "kits" is no longer supported`)。 kit 機構自体は Phase 2.5 PR6 で退役済み、 *workspace* 側の `kits` フィールド (`WorkspaceMeta.Kits`) も Phase 2.5 PR7 でコードから完全撤去 (`docs/plans/workspace-db-consolidation.md` 参照)。 `host_commands` / `env` を workspace に直接設定すること (`additional_bindings` は Phase 4 PR4 で撤去済み — [workspace home の `init.sh`](../guide/workspace-home.md) を使う)。 詳細は下記 [`KitRef`](#kitref) と [Kit 作者向け概要](../kit-authoring/overview.md) を参照 |
| `host_commands` | — | **撤去** | ロード時に reject される。 workspace に設定する (`boid workspace create/edit`) — ただし *workspace* の `host_commands:` は参照 **名前** のリストであり、 下記 [HostCommands](#hostcommands) で説明するマップ形式ではない点に注意 (そのマップ形式は `kit.yaml` と daemon-wide の `~/.config/boid/host_commands.yaml` レジストリで使われ、 workspace の名前はそのレジストリを参照して解決される)。 [オンボーディング / host_commands を定義する](../guide/onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ) を参照 |
| `additional_bindings` | — | **撤去** | `project.yaml` の top-level ではロード時に reject される。 **`workspace.yaml` 側の `additional_bindings` も `docs/plans/home-workspace-volume.md` Phase 4 PR4 で撤去済み** — key 自体はパースされる (エラーにはならない) が値は無視され、 サンドボックスに反映されない。 workspace 側でツールチェーンを永続化したい場合は [workspace home の `init.sh`](../guide/workspace-home.md) を使うこと。 詳細は [BindMount](#bindmount) 参照 |
| `env` | — | **撤去** | ロード時に reject される。 workspace に設定する (同じ map 形式) |
| `secret_namespace` | — | **撤去** | ロード時に reject される。 workspace に個別の secret-namespace フィールドはない — secret は workspace 自身の slug をネームスペースとして解決される |
| `capabilities` | — | **撤去** | ロード時に reject される。 workspace に設定する (`capabilities.docker`、 形は同じ) — [capabilities](#capabilities) 参照 |

## git gateway / sandbox 内 clone

project が可視なジョブ (hook / セッション / `boid exec` を問わず) は、 毎回 **daemon 内の git gateway** (認証注入リバースプロキシ) 経由で project を sandbox 内に新規 clone します。 host 側の project ディレクトリを直接マウントしたり、 host 側に git worktree を作ったりはしません。

- clone の origin は gateway を指す。 sandbox 内の git は credential レスの素の git バイナリで、 fetch / push はすべて gateway が上流 (GitHub 等) への認証注入を代行する
- **成果の共有は origin への push が唯一の経路**: commit しただけの変更は他セッション・他ホストには一切共有されない。 「done 前に push」が前提になる
- `readonly: true` の behavior では clone 自体はローカルに書き込めるが、 push が gateway 側で拒否される (fetch はできる)。 「何も書けない」ではなく「境界を越えられない」という読み書き対称な適用に変わった
- reopen は「再 clone + branch checkout」として実行される。 保証されるのは commit (+ push) 済みの内容のみ
- 同一 project・同一 HEAD branch を対象とする複数タスクも、 それぞれ独立した clone を持つため **並行して dispatch される** (以前あった branch 単位の直列ロックは廃止済み)。 同時に push すると通常の git のとおり non-fast-forward で reject されるので、 fetch + merge/rebase して再 push する
- workspace peer project は fetch-only でサンドボックス内から clone・reference 可能。 書き込みが必要な場合は peer への cross-project child task を作る。 peer の存在と clone URL / reference path は `boid project list` で発見できる (`internal/skills/data/boid-task/references/builtins.md` 参照)

## `task_behaviors.<name>`

map のキーが behavior の識別子で、 タスク作成時に `behavior:` で指定する名前です。 **canonical な名前は 2 つ** です:

| 名前 | 役割 |
|---|---|
| `supervisor` | readonly な統括役。 要求を triage し、 child executor task を作り、 監視する。 ファイル編集はしない |
| `executor` | 書き込み可能な実装役。 単一の集中したタスクを受けて成果物 (commit / PR / payload trait) を作る |

canonical 以外の任意のキー名も使用できます (Track A2 以降、`readonly` の既定値は `true` (fail-safe) です。writable にするには `readonly: false` を明示してください)。

behavior 名は完全一致で照合され、daemon が読み替えることはありません。 かつて `plan` は `supervisor` の、 `dev` は `executor` の alias として扱われ、 ロード時に canonical 名へ書き換えられていましたが、 この alias 表は撤去済みです。 `plan` / `dev` も他の名前と同様、 単なる project 固有の behavior 名として使えます (`supervisor` / `executor` と同じファイルに併記しても構いません)。 なお `dev` は alias だった頃 `executor` の `readonly: false` を引き継いでいましたが、 自由名になった今は fail-safe 既定の `readonly: true` になるため、 writable にしたい場合は明示してください。

各 behavior エントリの設定項目:

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `readonly` | bool | `true` (fail-safe) | タスクのワークディレクトリを read-only マウントするかどうか。`executor` のみ互換 override で `false` が保たれる (WARN あり)。 writable な behavior には `readonly: false` を明示する |
| `traits` | string のリスト | (空) | この behavior のタスクが扱う payload trait の宣言 (例: `[artifact]`) |
| `default_instruction` | Instruction | (空) | タスク作成時の active instruction として `Task.Instructions` 配列に積まれる雛形 (単一 Instruction object) |
| `hooks` | Hook のリスト | (空) | executing 状態遷移時に走る hook 定義。 詳細は [hooks](#hooks) を参照 |

> **注意:** `task_behaviors.<name>` 配下に `name` フィールドを書いてもローダーに無視されます。 behavior の識別子はマップキーを使ってください。

> **撤去済み:** `task_behaviors.<name>.kits` はロード時に reject されます (`project.yaml: task_behaviors.<name>.kits is no longer supported`)。 kit はもう `project.yaml` の概念ではありません — 上の [トップレベルのフィールド](#トップレベルのフィールド) 表の `kits` 行を参照してください。

### 動的 `base_branch`

`base_branch` には 2 つの interpolation token が使えます:

- `${TASK_REMOTE_ID}` — 親 supervisor がこのタスクに記録した remote 識別子 (GitHub PR 番号など)。 supervisor / executor 双方で解決される。 "1 Supervisor 1 PR" ワークフロー ([ワークフロー 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) で、 supervisor セッションごとに専用の統合ブランチを切るために使う
- `${current_branch}` — タスク作成時に project リポジトリの daemon の HEAD ブランチに解決される

**省略時の解決優先順位:**

1. `parent_id` あり (child task): 親タスクの `base_branch` をそのまま継承。 template 展開は行わない
2. `parent_id` なし + `base_branch` 省略 (root task): 作成時点の `${current_branch}` に展開してから row に保存。 detached HEAD の場合は 400 エラー
3. `parent_id` なし + `base_branch` 指定: template 展開 (`${TASK_REMOTE_ID}` / `${current_branch}`) を行う

エンドツーエンドの例 ([ワークフロー 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) は [docs/workflows.md](../../workflows.md) を参照。

`worktree` フィールドの経緯については [概念 / worktree](../guide/concepts.md#worktree) を参照してください。

### `fork_point` と case 3

`base_branch` (テンプレート展開後) がローカルにも `origin/<base>` にも存在しない状態を **case 3** と呼びます ([base_branch_classify.go](../../../internal/orchestrator/base_branch_classify.go))。 この場合 runner は sandbox 内 clone の中でそのブランチを **新規ローカル branch として作成** します (host 側の project ディレクトリは一切参照しません)。 問題は「どこから fork するか」で、 host git worktree を使っていた旧実装では project root の HEAD を起点にしていたため、 タスク作成からディスパッチまでの間にユーザが root で別 branch をチェックアウトしていると、 想定外の commit から base が切られる事故がありました。

新しい解決順:

1. **`fork_point` が project.yaml に設定されていれば** その ref を起点にする。 `git rev-parse --verify` で解決できるものなら何でも可 (branch / tag / commit SHA / `origin/main` 等)。 解決失敗時は明確なエラー
2. 未設定なら **`refs/remotes/origin/HEAD`** にフォールバック。 通常は `git clone` が自動で設定する。 既存 repo で未設定なら `git remote set-head origin --auto` で一度設定する
3. どちらも解決できなければ case 3 はエラー。 project root の HEAD は意図的に参照されない

典型的な使い分け:

- 普通の GitHub プロジェクトで `main` が default branch: 設定不要 (origin/HEAD で自動的に解決される)
- default branch が `master` や `develop` 等: 設定不要 (origin/HEAD が指してれば OK)
- リモートが無い / origin/HEAD を設定できない: `fork_point: main` のように明示
- 特殊な default 起点を使いたい: `fork_point: origin/release/2026` のように明示

### タスク種別と HEAD branch

**docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11) で per-task branch (`boid/<id8>`) と fork point 概念は廃止されました。** タスク種別 (root / child、 supervisor / executor) に関わらず、 sandbox 内 clone は常に `task.BaseBranch` を直接 checkout します。 worktree 時代に必要だった「child は隔離用の専用 branch を切る」という仕組みは、 clone 自体が isolation 単位になったことで不要になりました — 同じ branch 名を別々の sandbox 内 clone で checkout しても衝突しません。

| タスク種別 | HEAD branch | readonly |
|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | sup=true / exec=false |
| **child sup / child exec** | `task.BaseBranch` | sup=true / exec=false |

- **root タスク** (`parent_id == ""`): sandbox 内 clone した上で `base_branch` を直接 checkout する (新規 branch は作らない)。 `base_branch` が origin にまだ存在しない場合 (case 3) は [`fork_point` と case 3](#fork_point-と-case-3) の解決結果からローカル作成する
- **child タスク** (親あり): root タスクと全く同じ扱いで `base_branch` を直接 checkout する。 `base_branch` を省略すると親タスクの `base_branch` をそのまま継承する (template 展開なし、 [動的 base_branch](#動的-base_branch) 参照) ため、 明示指定がない限り親子は同じ branch 名を checkout する
- `task.BaseBranch` は PR target として全子タスクに継承され、 `BOID_BASE_BRANCH` env で executor に渡る

**並列に走る兄弟 executor が同じ base_branch へ同時に push すると衝突します**。 これは isolation の欠如ではなく、 従来から変わらない executor 側の rebase/retry 契約です (下記「同一 HEAD branch を対象とする複数タスクの並行実行」参照)。 真に isolate したい場合は、 supervisor が子ごとに異なる `base_branch` を明示指定してください (例: `feature/BGO-214`, `feature/BGO-215`, `feature/BGO-216`)。

### 同一 HEAD branch を対象とする複数タスクの並行実行

以前は同一 `<projectID>:<HEAD branch>` を対象とする複数タスクを FIFO ロックで直列化していました (同じ host git worktree を複数タスクが同時に使えないため)。 **この直列ロックは git gateway cutover で廃止済みです**: 各タスクは独立した sandbox 内 clone を持つため、 同じ branch を対象とする複数タスクも並行して dispatch されます。 同時に push した場合は通常の git のとおり non-fast-forward で reject されるので、 fetch + merge/rebase して再 push してください。 意図的に競合を避けたい場合は、 前節のとおり子ごとに異なる `base_branch` を割り当ててください。

### 依存子の最新化とマージ責務

boid コアは子タスクの dispatch 順序や base 同期には関与しません。 sub-sup (子 supervisor) が子タスクの dispatch 順序を制御しますが、 clone モデルでは sub-sup 自身が更新すべき「自分の working branch」はもう存在しません — sub-sup 自身も毎回 `base_branch` を直接 checkout するだけの読み取り専用 clone です:

```
A (executor) が done → A の PR を base_branch へ merge (origin 上)
                         ↓
            sub sup が B を dispatch → B の clone は origin から新規 fetch するため
                                        A の merge 済み内容を自動的に含む
```

merge のタイミング・コマンド・対象は **project 側 instruction の責務** であり、 skill / boid コアには記述しません。 boid コアの関与は `BOID_BASE_BRANCH` env を渡すことに限定されます (`BOID_PARENT_BRANCH` は docs/plans/branch-policy-simplification.md Phase 1 で廃止されました — production project.yaml / e2e script に実利用が確認されなかったため)。

### `default_instruction`

単一の Instruction object です。 タスク作成時に Task.Instructions 配列に append され、 これが executing で agent に渡される最初の active instruction になります。

reopen 時は `boid task reopen <id> --message "..."` で新しい Instruction を append し、 配列の最後の要素 (= 直近の active instruction) が agent に渡されます。 `agent` / `model` は前回 active から継承されます。

### `hooks`

`task_behaviors.<name>.hooks` は、 タスクが executing 状態に入るたびに走る処理を宣言するリストです。 各要素 (Hook) は次の 2 経路の **どちらか一方のみ** を持てます。 外部 `.sh` ファイルを `.boid/hooks/` に置いて参照する経路は存在しません (2026-07 に撤廃、 [docs/plans/script-hook-removal.md](../../plans/script-hook-removal.md) 参照)。

| 経路 | 宣言方法 | 実行先 |
|---|---|---|
| **inline command** | `command: \|` に複数行 shell script を書く | `sh -c <command>` としてサンドボックス内で実行 (shell adapter) |
| **agent hook** | `kind: agent` + `agent: <harness名>` | 対応する HarnessAdapter (`claude-code` / `codex` / `opencode`) がタスクの active instruction を渡して起動 |

```yaml
task_behaviors:
  executor:
    hooks:
      - id: assert-clone-cwd
        command: |
          set -eu
          test -d .git || { echo "not in git repo" >&2; exit 1; }
          echo "assert-clone-cwd ok"
        traits:
          produces: [artifact]
```

各 Hook のフィールド:

| キー | 型 | 役割 |
|---|---|---|
| `id` | string | hook の識別子 (必須) |
| `name` | string | 表示名 (省略可) |
| `kind` | string | `agent` を指定すると agent hook になる。 省略時は inline command hook |
| `command` | string | inline shell command (block scalar `\|` 推奨)。 `agent`/`kind: agent` と排他 |
| `agent` | string | agent hook が使う harness 名。 `command` と排他 |
| `traits` | HandlerTraits | この hook が消費/生成する payload trait (`consumes` / `produces`) |
| `requires` | string のリスト | 前提となる他 hook の `id` |

**validation ルール** (`internal/orchestrator/planner.go` の `validateHookCommandFields`):

1. `kind: agent` の hook に `command` を書くとエラー (agent hook は HarnessAdapter が自分で argv を組む)
2. `agent` と `command` を同時に書くとエラー (排他)
3. `kind: agent` でない hook に `command` も `agent` も無いとエラー (実行対象が無い)

**shell dialect の注意**: `command:` は sandbox 内の `/bin/sh` (多くの環境で dash) 上で実行されます。 dash は `set -o pipefail` や `[[ ]]` などの bash 拡張構文を **reject** します。 これらが必要な hook は body を `bash <<'HEREDOC'` で wrap してください。 詳細とサンプルは [docs/plans/script-hook-removal.md](../../plans/script-hook-removal.md) の §R6 を参照。

hook が宣言を持たない behavior では、 `default_instruction` から virtual agent hook が自動合成されます (`kind: agent` を明示宣言する必要はない) — 通常の `task_behaviors.<name>.default_instruction` を使う運用ではこの節は意識しなくても動きます。

## `session_behaviors.<name>`

`task_behaviors` と同じ free-naming の辞書ですが、対象は task ではなく **session** (task を伴わない対話セッション。Web UI のセッション起動ボタンや `boid agent` CLI から起動されるもので、`boid exec` (別の JobKind、`session_behaviors` は読まない) とは別物です) です。

session は task 作成時に解決される `ResolveBehavior` を経由しません — session と task は daemon 内部で別概念として扱われるため、`task_behaviors.<name>.default_instruction.model` のような値を session が参照することはありません。session が harness/model の既定値を project.yaml から得たい場合は、この `session_behaviors` を使います。

用途キー (map のキー) は呼び出し元が決める識別子です。現在参照しているのは Web UI の Shape ボタン (parked または working のカードから整形セッションを起動する機能。card machine v2 以降、triaged という状態自体が存在しない) のみで、キーは `shape` です。`task_behaviors` の `supervisor`/`executor` のような canonical name はありません — 今後別の呼び出し元が新しい用途キーを参照するようになった場合も、このセクションではなくその機能自身のドキュメントを参照してください。

```yaml
session_behaviors:
  shape:
    harness_type: codex
    model: o3-mini
```

各エントリの設定項目:

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `harness_type` | string | (空 — 呼び出し元がフォールバック値を使う) | session 起動時の harness (`claude` / `codex` / `opencode`)。不正な値の場合は呼び出し元がフォールバックする (エラーにはならない) |
| `model` | string | (空 — harness の既定モデル) | session 起動時に harness へ渡すモデル指定 |

> **注意:** `session_behaviors` は procedure (手順) を持ちません。ここで渡せるのは harness_type/model という data のみです — 「どうやって作業するか」は引き続き project 自身の CLAUDE.md / スキルの discovery に委ねられます。

## `triggers[]`

daemon が project.yaml から読み取り、周期的に readonly exec job として起こすコマンドの一覧です (docs/plans/ingestion-identity.md PR-4/B-5)。`task_behaviors` とは独立した**トップレベル**のフィールドで、workspace envelope 側 (`boid workspace apply` の `spec:`) には意図的に対応していません — `run:` は特定 1 project の tracked tree にしか存在しないスクリプトパスを指すことが普通で、workspace 内の複数 project へ機械的に共有できる保証が無いためです。

```yaml
triggers:
  - name: intake
    every: 10m
    run: python3 scripts/intake_tick.py
  - name: sweep
    every: 1h
    timeout: 30m       # 1 巡の実行時間の上限 (省略時は無制限)
    run: bash scripts/sweep_tick.sh
  - name: signal-sweep
    on: signals        # 省略時 "schedule"
    every: 2m          # signals でも必須 — 発火間隔の下限 = debounce
    run: python3 -m khi.app.scan
```

| キー | 型 | 必須 | 役割 |
|---|---|---|---|
| `name` | string | はい | このプロジェクト内でトリガを一意に識別する名前。空文字列・重複はロード時にエラー。`boid trigger run <name>` で参照する名前でもある |
| `on` | string (`schedule` \| `signals`) | いいえ (既定 `schedule`) | トリガの発火条件を選ぶ (docs/plans/signal-ingest-detailed-design.md §4)。`schedule` (既定/省略時) は `every` 経過のみで発火する従来どおりの挙動。`signals` は `every` 経過に加えて、このプロジェクトが紐づく workspace に未 ack の Signal が 1 件以上あることを要求する — 詳細は下記「`on: signals` の意味論」を参照。それ以外の値はロード時にエラー |
| `every` | string (`time.ParseDuration`) | はい | 前回の起動から次に起こすまでの最小間隔 (例: `10m`、`1h`)。`0` 以下は拒否。**実効下限は daemon の sweep 周期 (1 分)** — それより短い値 (例: `1s`) はロード時に拒否される。「毎秒」のように sweep 周期より高い頻度は表現できない。`on: signals` でも必須で、この場合は発火間隔の下限 = debounce 窓としても働く |
| `timeout` | string (`time.ParseDuration`) | いいえ (省略時は無制限) | **1 巡** の実行時間の上限 (例: `30m`)。超過した巡は daemon が打ち切り、失敗として記録する (下記「タイムアウト」)。`0` 以下、および `every` より短い値はロード時に拒否される — `every` は「どれくらいの頻度で見に行くか」、`timeout` は「1 巡がどれくらいの長さまで許されるか」で、**別々の問い**である。混同して `timeout < every` にすると、次の巡が来る前に毎回打ち切られることになる |
| `run` | string | はい | サンドボックス内で `sh -c` に渡されるコマンド文字列 (スクリプトパスではない)。sandbox の `/bin/sh` は dash なので bashism は `bash scripts/x.sh` のように明示すること。stdin は daemon 側が `exec 0</dev/null` 相当で閉じた状態で実行される (対話的な入力を待つスクリプトは書けない — attach するクライアントが存在しないため、閉じないと永久にハングする) |

**実行の性質:**

- 常に `Readonly: true` 固定の exec job として起動される (`boid exec --readonly` と同じ経路)。readonly でも `boid task create` / `boid task action-send` 等の boid op は通る (readonly はサンドボックスのファイルシステム書き込み可否だけを制御し、op の allowlist には影響しない)
- **single-flight**: 同じ `(project, trigger)` の組は同時に 1 つしか走らない。前回がまだ実行中なら次の周期は見送られる (DB の部分 UNIQUE インデックスで強制される — 複数 daemon プロセスが同じ DB を共有する場合でも保証される)。`on: signals` でもこの機構に変更はない
- **タイムアウト**: `timeout` を宣言すると、その時間を超えた巡を daemon が打ち切り、`trigger_runs` を失敗として閉じる。失敗は連続失敗の通知 (3 回連続で通知) にそのまま乗る。宣言しなければ無制限で、詰まりはログと通知でのみ検出される
    - 打ち切りは**起こされた task の方を abort する** — `run:` が `boid task wait` で task の終了を待っている場合、止めるべきは待っている job (起動係) ではなく走っている task (仕事本体) だから。job だけ止めると task が走り続けたまま single-flight が解放され、次の周期が同じ仕事の 2 巡目を並走させてしまう
    - abort の理由は `lifecycle.abort.code` に `trigger_timeout` として記録される (`boid task show <id> --field lifecycle.abort.code`)
    - 打ち切った tick では次の巡を起こさない。次の sweep tick (最大 1 分後) が改めて発火判定をする
    - **`run:` の中に `timeout 300 ...` を書く必要はもう無い。** その形は daemon から見えない上限になるので、`timeout:` フィールドで宣言すること
- daemon は `run:` の中身を一切解釈しない。何個 task を作るか・どう判断するかはスクリプト側の責務

**`on: signals` の意味論:**

`on: signals` は既存の `every` 経過判定に「未 ack の Signal が 1 件以上ある」という条件を AND するだけで、新しい debounce/single-flight 機構は作られていない (docs/plans/signal-ingest-detailed-design.md §4.2)。

- **debounce**: `every` 窓内に何件 Signal が届いても発火は 1 回だけ。窓が経過するまで再発火しない
- **crash からの回復**: 発火後、判断が crash した/捌き切れなかった等で未 ack の Signal が残っていれば、次の `every` 経過時に再びこのトリガが発火する — 別立ての再試行機構は無く、上記の 1 行の due 判定がそのまま crash 回復も兼ねる
- **single-flight**: 既存の `trigger_runs` の部分 UNIQUE インデックスがそのまま効く。変更なし
- **workspace 未所属の project**: プロジェクトがどの workspace にも紐づいていない場合、`on: signals` トリガは常に発火しない (エラーにはならず、daemon ログに debug レベルで記録されるのみ)
- **「未 ack」の定義**: attempts が上限 (`MaxSignalAttempts`) に達し dead-letter 化した Signal はここで言う「未 ack」に含まれない — dead-letter だけが残る workspace で `on: signals` トリガが永久に発火し続けることはない

デバッグ用の手動起動口は `boid trigger run -p <project-ref> <name>` (`every` の経過に加えて `on: signals` の Signal 有無判定もバイパスするが、single-flight は尊重する) — 詳細は [CLI リファレンス](cli.md#サンドボックス操作) を参照。

## `signals.sources[]`

Integration Pack の connector を定期実行する宣言です (docs/plans/signal-ingest-detailed-design.md §5.1)。書けるのはメタプロジェクトの project.yaml のみで、`signals.sources[]` の 1 件は hydrate 時に `signal:<pack>/<connector>` という名前の**導出 trigger** (`triggers[]` に自動追加される通常の `Trigger`) へ展開されます — スケジュール・single-flight・履歴・GC は上記 `triggers[]` の機構をそのまま流用し、新しい機構は作られません。

```yaml
signals:
  sources:
    - connector: slack/mentions      # <pack>/<connector>
      service: slack-api             # config.yaml services.<name> の instance 名
      every: 10m
      config:
        include_threads: true
```

| キー | 型 | 必須 | 役割 |
|---|---|---|---|
| `connector` | string (`<pack>/<connector>`) | はい | 実行する Integration Pack の connector。`integrations.dir` に導入済みの Pack (`internal/integrationpack`) 名と、その `integration.yaml` の `connectors[].name` を `/` で連結した形式。バージョン指定は無い — 同名 Pack が 2 バージョン以上導入されている場合は起動時ではなく実行時 (`boid exec` 経由の StartExec) にエラーになる |
| `service` | string | はい | この connector が API gateway 経由で到達できる service instance の名前 1 本 (`config.yaml` の `services.<name>`)。connector job の gateway token はこの 1 本にのみ絞られる — workspace の enabled services 全体ではない |
| `every` | string (`time.ParseDuration`) | はい | 導出 trigger の `every` と同じ (`triggers[]` の `every` と同一のバリデーション・下限を共有) |

> **`timeout` は書けません。** 導出 trigger には `timeout` が設定されないので、connector job は無制限に走ります。connector は外部 API を読んで inbox に書くだけの短命な job であり、`boid task wait` で task の終了を待つ `triggers[]` とは実行の形が違うためです。
| `config` | object | いいえ | connector 固有の設定。Pack の `configSchema` で検証され、JSON にエンコードされて `BOID_SIGNAL_CONFIG` env に渡される |

**名前衝突**: `signal:<pack>/<connector>` が既存の `triggers[].name` と衝突する場合、または `signals.sources[]` 内で同じ `connector` が複数回宣言されている場合は project.yaml のロード時 (`boid project add`/`fetch`) にエラーになります。

**connector job の権限**: 導出 trigger が発火すると、通常の `triggers[]` と同じ readonly exec job として起動されますが、権限は大きく絞られます (docs/plans/signal-ingest-detailed-design.md §5.2)。

- 呼べる boid builtin op は `signal_ingest`・`signal_cursor_get` の 2 つだけ。`task_create` 等の通常 job が使える op は broker が拒否する
- `fetch` builtin は渡されない — 外部到達は API gateway 経由のみ
- **metaproject の `host_commands:` は渡されない** — 通常の hook/exec job なら見える `host_commands.<name>` エントリ (実 credential を持つ場合がある) が connector job には一切見えない (broker の `entry.Commands` が空になる。`BuiltinPolicies` とは別系統の broker 側 gate であることに注意 — 両方を絞らないと権限は絞れない)
- API gateway token は `service:` で宣言した 1 service にのみ許可される
- 解決済みの Pack ディレクトリが `/run/boid/integrations/<pack>` へ read-only bind mount される
- `BOID_SIGNAL_SERVICE` / `BOID_SIGNAL_CONNECTOR` / `BOID_SIGNAL_CONFIG` / `BOID_CONNECTOR_EXEC` の 4 つの env が渡される — connector プロセスの契約は上記 doc の §5.3 を参照

**現時点で絞られていないもの (既知、`docs/plans/signal-ingest-detailed-design.md` §12 の follow-up 参照)**: `capabilities.docker` を宣言した project の connector job には docker proxy が生える (`DockerEnabled` は `BuiltinPolicies`/`HostCommands` と独立)。project/kit の `additional_bindings:` は connector job にもそのまま見える (Pack ディレクトリの bind は既存の binding に**追加**されるだけで、既存分は落とさない)。egress は workspace 全体の `allowed_domains` proxy のままで、connector 専用の縮小は無い (gateway を経由しない到達が塞がれているわけではない)。

**service の有効化チェック**: `service:` で宣言した名前が、紐づく workspace の enabled services (`workspace.yaml` の `services:`。daemon 全体の floor は含まない — 詳細下記) に含まれない場合、project.yaml のロードは失敗せず**警告ログ**のみが出ます (`boid project fetch` 自体は成功する)。この検査は project.yaml の parse 時ではなく、trigger sweep が project を hydrate するたび (既存の trigger loop の sweep 解像度 = 1 分毎) に実行される — 「project.yaml ロード時に 1 回だけ」ではなく、宣言漏れが解消されるまで**継続的にログへ出続ける**。daemon 全体の floor (`config.yaml` の `services_floor`) は考慮しない (この検査を行う `internal/orchestrator` 層が daemon 全体の config を参照できないため) ので、floor 経由でのみ有効になっている service は誤って警告される場合がある (安全側の誤検知)。ただし dispatch 時の API gateway token はこの workspace enabled services (floor も含めた実際の解決値) との積集合に絞られるため、宣言漏れの service は実行時に到達不能になります。

## 共通の構成要素

### KitRef

> **`project.yaml` のフィールドではありません。** `project.yaml` は top level・`task_behaviors.<name>` のどちらでも `kits:` を受け付けません (上の [トップレベルのフィールド](#トップレベルのフィールド) 表を参照)。 この節を載せているのは、 **legacy** な `project.yaml` (`boid project migrate` が読む旧スキーマ、 [`ReadProjectMetaLegacy`](../../../internal/orchestrator/spec_loader_legacy.go)) だけがこの map/文字列 2 形式の `KitRef` を受け付けるためです。
>
> **Phase 2.5 PR7** (`docs/plans/workspace-db-consolidation.md`) で `WorkspaceMeta.Kits` フィールドはコードから完全撤去されました — *workspace* 側の `kits:` はもう存在せず、 `POST`/`PUT`/`import /api/workspaces` に `kits:` キーを含む body を送ると 400 (`unknown field kits`) で reject されます。 `boid project migrate` は引き続き legacy project.yaml の `kits:` (top-level / `task_behaviors.<name>.kits`) を収集・名前検証しますが、 workspace への自動解決/materialize は行わなくなり、 dry-run/apply の出力に「未解決の kit 参照、 必要なら手動で追加を」という informational な note として表示されるのみです。 唯一残っている legacy `kits:` 対応経路は `boid workspace assign` の auto-create 補助 (`cmd/workspace.go` の `ensureWorkspaceExistsForAssign`) で、 手書き/e2e フィクスチャの workspace shadow yaml にある `kits:` をクライアント側でインストール済み kit ディレクトリに対して解決してから (kits: を含まない) body を送信します。

`project.yaml` の legacy `kits` フィールドの各要素は次のどちらかで書けます (`boid project migrate` の変換対象としてのみ有効。 現行スキーマの `project.yaml` では reject されます)。

- 文字列: `github.com/<owner>/<repo>/<sub-path>` の形 (例: `github.com/novshi-tech/boid-kits/claude-code`)
- map 形式:
  ```yaml
  kits:
    - ref: github.com/novshi-tech/boid-kits/claude-code
      as: agent
  ```
  `as` で alias を付けると、別の kit と agent 名が衝突するときに区別できます

`<sub-path>` は省略可。リポジトリ直下に kit がある場合は不要です。 `boid project migrate` はこの `ref` から最後のセグメント (例: `claude-code`) を名前検証と informational な出力のためだけに取り出します — 上記 Phase 2.5 PR7 の通り、 migrate 先の workspace には一切引き継がれません。

### HostCommands

> **`project.yaml` のフィールドではありません。** `project.yaml` にはもう `host_commands` フィールドがありません ([トップレベルのフィールド](#トップレベルのフィールド) 参照)。 このマップ形式は `kit.yaml` と daemon 側の集約レジストリ `~/.config/boid/host_commands.yaml` で使われます。 *workspace* 自身の `host_commands:` フィールド (`workspace.yaml`、 `boid workspace create/edit` で設定) はこれとは別物 — このマップ形式ではなく、 レジストリを引く参照 **名前** のプレーンなリストです。 [オンボーディング / host_commands を定義する](../guide/onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ) を参照してください。

サンドボックスは既定では host のコマンドを呼べません。 `host_commands` で許可リストを宣言した分だけ通します。 リストとマップの 2 種類の書き方があります。

リスト形式 (制約なしで許可):

```yaml
host_commands:
  - gh
  - aws
```

マップ形式 (各コマンドに細かい制約をかけられる):

```yaml
host_commands:
  gh:
    allow: [pr, issue, run]
    deny: ["* delete*"]
    env:
      GH_REPO: ${boid:repo_slug}
    reject:
      - match: "*--body-file*"
        reason: 'サンドボックスのファイルパスは host からは見えない。--body "$(cat <file>)" で内容を渡す'
  aws:
    path: /usr/local/bin/aws
    env:
      AWS_REGION: ap-northeast-1
```

各エントリ (`HostCommandSpec`) のフィールド:

| キー | 型 | 役割 |
|---|---|---|
| `allow` | string のリスト | 許可するサブコマンドまたはグロブパターン (`* ?` 含むパターンとして自動判別) |
| `deny` | string のリスト | 拒否するパターン (allow より優先) |
| `reject` | RejectRule のリスト | `match` (glob) にマッチした呼び出しを `reason` 付きで拒否する。 `reason` は必須で、 拒否時に `host_commands.<name>: rejected: <reason>` としてエージェントへ返る (下記「host command の実行契約」参照) |
| `stdin` | bool | **非推奨**。 パースはされるが常に無視される (下記「host command の実行契約」参照) |
| `path` | string | バイナリの絶対パス (host の `$PATH` 解決を上書きしたい場合) |
| `env` | map (string → string) | このコマンド呼び出し時に追加する環境変数。 値に `${boid:repo_slug}` と書くとコンテキスト変数として展開される (下記「host command の実行契約」参照) |

特殊な使い方として、 `path` に kit / プロジェクト内の相対パスを書くと、その path のコマンドだけがサンドボックスから host へ流れます (例: `path: e2e/run.sh`)。

> **予約名:** `git`、`boid`、`fetch` はサンドボックス組み込みコマンドです。 `host_commands` に宣言しても無視されます。

#### host command の実行契約

- **stdin は渡らない** — サンドボックス shim は stdin を読まず、 broker も受け取っても捨てる。 `stdin: true` は設定として受理されるが効果はない (deprecation warning が出る)。 ファイル内容や長文をコマンドへ渡したい場合は stdin ではなく引数 (例: `--body "$(cat <file>)"`) を使う
- **cwd は中立ディレクトリ固定** — host command は host 側で project の checkout ディレクトリではなく中立ディレクトリ (`os.TempDir()`) で実行される。 cwd から repo を推定する動作 (`gh` の暗黙 `-R` 等) には依存できない
- **repo 文脈は env で渡す** — cwd 推定の代わりに、 `env:` の値に `${boid:repo_slug}` と書くとトークン登録時に project の origin remote から導出した `host/owner/repo` 形式の文字列に展開される。 `gh` であれば `GH_REPO: ${boid:repo_slug}` で従来どおり透過的に動く
- **reject ルール** — `match` (allow/deny と同じ glob 意味論、 joined args に対して) にマッチした呼び出しは shim (早期) と broker (権威) の両方で拒否され、 `host_commands.<name>: rejected: <reason>` というメッセージがエージェントに返る。 `reason` は代替手段を書くこと (単に「使えません」ではなく次に何をすべきか)

`local/<name>` 形式の kit 参照 (例: `local/my-kit`) は、 プロジェクトルート相対でローカル kit ディレクトリを解決します。 リモートレジストリに公開せずに kit を開発する場合に便利です。

### BindMount

> **注意 (`docs/plans/home-workspace-volume.md` Phase 4 PR4):** `workspace.yaml` の
> `additional_bindings` は撤去済みで、現在これを有効にする経路はありません
> (`project.yaml` の top-level は元からロード時に reject、`workspace.yaml` 側は
> パースされても無視されるだけの死んだフィールドです)。以前この機構が主に使われていた
> 「workspace にツールチェーンを永続的に持たせる」用途は、[workspace home の
> `init.sh`](../guide/workspace-home.md) に置き換わりました。以下は当時の形の記録として
> 残していますが、実際にサンドボックスへの bind としては機能しません。

`additional_bindings` の各要素はサンドボックスにマウントしたい host 上のパスを表します。

```yaml
additional_bindings:
  - source: ${HOME}/.local/share/some-tool
  - source: ${HOME}/.config/some-tool
    mode: rw
  - source: ${HOME}/.netrc
    is_file: true
    optional: true
  # gitignored だがサンドボックス内の clone からも参照させたいファイル (例: .NET の global.json)
  - source: ${PROJECT_WORKDIR}/global.json
    target: ${WORKTREE}/global.json
    is_file: true
    optional: true
```

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `source` | string | (必須) | host 側のパス。 `${HOME}` 等の展開可 |
| `target` | string | `source` と同じ | サンドボックス内のマウント先パス |
| `mode` | string | `""` (ro) | `rw` で読み書き可。空文字列なら読み取り専用 |
| `is_file` | bool | `false` | source がファイルの場合 `true` |
| `optional` | bool | `false` | host に source が無くてもエラーにせずスキップする |

#### 動的トークン: `${WORKTREE}` / `${PROJECT_WORKDIR}`

`source` / `target` では通常の環境変数 (`${HOME}` 等) に加え、 boid が dispatch 時に解決する 2 つの動的トークンが使えます:

- `${PROJECT_WORKDIR}` — host 側のプロジェクトディレクトリ (例: `/home/you/src/your-project`)
- `${WORKTREE}` — タスクが実行されるサンドボックスの cwd。 project が可視なジョブ (git gateway 経由で sandbox 内に project が clone される場合) では sandbox 内 clone 先のパス (例: `/workspace/<project-name>`) に、 project が可視でないジョブでは `${PROJECT_WORKDIR}` と同じ値に解決される

主な用途は、 `.gitignore` してあるが sandbox 内の clone からも参照させたいファイル (`.NET` の `global.json`、 `.env.local`、 `appsettings.Development.json` など) をホストの project workdir からサンドボックス内の clone に bind することです。

`target` を **明示** し、 展開後 `source` と等値になった binding は self-mount を避けるため自動的に skip されます。 上の例の binding は:

- clone-mode のジョブ (project が可視な hook / session / exec のほぼ全て) では `/host/proj/global.json` → `/workspace/proj/global.json` のように別パスへ bind され、
- clone-mode でないジョブ (project 不可視、または dispatcher のテスト配線等) では同一 path に潰れて skip される (project ディレクトリは既に projectVisibilityMounts でサンドボックスに見えているため不要)

ので、 同じ宣言で clone モードかどうかに依存せず動作します。

> **注意:** 上記は `additional_bindings` がまだ機能していた頃の記述です。 前述の通り、
> `workspace.yaml` 側の `additional_bindings` は Phase 4 PR4 で撤去されており、
> 現在は `mode` を明示していても効果はありません。

### Instruction

`default_instruction` に書く構造体です。

```yaml
default_instruction:
  agent: claude-code
  model: sonnet
  message: |
    ...
```

| キー | 型 | 役割 |
|---|---|---|
| `agent` | string | この instruction を受け取る harness の識別子。`claude-code` は claude harness (boid 本体 builtin)、`codex` は builtin codex adapter、`opencode` は builtin opencode adapter、省略または未知値は shell adapter に fallback する |
| `name` | string | 同じ agent に複数 instruction を渡す場合の識別子 (省略可) |
| `message` | string | agent に渡される指示文 |
| `model` | string | agent が選ぶモデル名 (例: `opus`、 `sonnet`)。 kit 側で解釈される |

> **注意:** `type:` と `interactive:` は `Instruction` のフィールドではなく、 YAML に書いても黙殺されます。

### CommandSpec (廃止)

Phase 3-d (2026-06 リリース) で `commands:` map は廃止されました。 project.yaml / task_behaviors.<name> 配下のいずれに書かれていても **silent に無視され、 起動時に deprecation warning が 1 回出力されます** (boid daemon ログ)。 既存 yaml はそのままでも壊れません。

代替手段:

| 旧 | 新 |
|---|---|
| `boid exec <project_id> <command-name>` で名前付き登録コマンドを起動 | `boid exec -p <project_id> -- <argv...>` で任意 argv を直渡し |
| Web UI の **Commands** ボタンで claude セッションを起動 | Web UI の `/sessions/new` から harness (claude / codex / opencode / shell) を選んでセッション起動。 同等の `POST /api/projects/{id}/sessions` も提供 |
| task 詳細の **Commands** ボタンで behavior commands を実行 | task が要求する継続的な実行は behavior の hooks で記述する。 ad hoc な実行は task に紐付けず `boid exec` でよい |

## capabilities

> **`project.yaml` のフィールドではありません。** `capabilities` は現在 **workspace** に設定します (`workspace.yaml`、 `boid workspace create/edit` 経由) — `project.yaml` には設定できません ([トップレベルのフィールド](#トップレベルのフィールド) 参照)。 以下の内容自体は変わりません — 同じ `docker: {}` の形、 同じ proxy の挙動で、 workspace 経由になっただけです。

サンドボックスのオプション機能を有効化するフィールドです。

### `capabilities.docker`

`capabilities.docker: {}` を workspace に宣言すると、そのワークスペースのサンドボックスに **ネイティブ Docker プロキシ** が有効になります。

```yaml
capabilities:
  docker: {}   # 空オブジェクトが有効化マーカー
```

有効化すると boid daemon は自動的に次の処理を行います:

1. サンドボックス専用の proxy socket を起動（`/run/boid/docker-proxy.sock`）
2. その socket をサンドボックスに bind-mount
3. 以下の環境変数をサンドボックスに自動設定

| 環境変数 | 値 |
|---|---|
| `DOCKER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `CONTAINER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` | `/run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_RYUK_DISABLED` | `true` |

docker CLI・Docker SDK・TestContainers はいずれも `DOCKER_HOST` を参照するため、追加設定なしに proxy 経由で動作します。`TESTCONTAINERS_RYUK_DISABLED=true` は TestContainers の Ryuk reaper を無効化します（Ryuk はサンドボックス分離が禁止する docker.sock bind-mount を要求するため。boid は代わりにジョブ終了時にコンテナを掃除します）。

proxy のセキュリティモデル、ボディ検査ルール、コンテナ GC の詳細は [サンドボックス内部実装 / Docker プロキシ](../architecture/sandbox-internals.md#docker-プロキシ-capabilitiesdocker) を参照してください。

#### docker CLI と host_commands の注意

サンドボックス内の docker コマンドは proxy socket (`DOCKER_HOST`) 経由で動作します。**`capabilities.docker` が有効な workspace で `host_commands` に `docker` をサブコマンド制限なしで登録するとエラーになります。** `host_commands` への `docker` 登録はホスト直実行（proxy バイパス）になるため、boid が起動時に拒否します。

エラーメッセージ:
```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

image build だけをホスト側 docker で実行させたい場合は、 `~/.config/boid/host_commands.yaml` (daemon 側の集約レジストリ、 [オンボーディング / host_commands を定義する](../guide/onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ) 参照) にサブコマンド制限付きで定義し、 workspace の `host_commands: [docker]` からその名前を参照すれば可能です:

```yaml
# ~/.config/boid/host_commands.yaml
host_commands:
  docker:
    allow: [build]   # build サブコマンドのみ許可 (ホスト直実行)
```

ただしこれはホスト直実行なので `--network host` / `--secret` 等のリスクは残ります。通常の `docker run` / TestContainers は proxy 経由で十分動作するため、`host_commands` への `docker` 登録は不要です。

#### rootless Docker の推奨

proxy 自体が第一防衛線ですが、万一 proxy が迂回された場合の影響を限定するため、ホスト側 Docker daemon は **rootless** で動かすことを推奨します。rootless Docker ではコンテナが user namespace 内で動くため、host root へのエスカレーションが原理的に起きません。

```sh
# rootless Docker のセットアップ (初回のみ)
curl -fsSL https://get.docker.com/rootless | sh
# または distro パッケージ: apt install docker-ce-rootless-extras
```

boid は起動時に docker upstream socket を `DOCKER_HOST` 環境変数 → rootless path (`$XDG_RUNTIME_DIR/docker.sock`) → rootful `/var/run/docker.sock` の順で自動解決します。

docker kit (cetusguard ベース) からの移行手順は [Docker プロキシ移行ガイド](../guide/docker-proxy-migration.md) を参照してください。

## プロジェクトローカル設定 (`.boid/project.local.yaml`) — 廃止

> **廃止**: `project.local.yaml` は廃止されました。内容は `workspace.yaml` に集約されます。
> `boid project migrate <dir>` で自動変換できます。詳細は [移行ガイド](../guide/migration.md) を参照してください。

旧スキーマで `project.local.yaml` が担っていた `host_commands` / `env` / `secret_namespace` は、
現在は workspace (DB 側、machine-local) で設定します。`additional_bindings` は Phase 4 PR4 で撤去済みで、
ツールチェーンの永続化は [workspace home の `init.sh`](../guide/workspace-home.md) に移りました。

## 例: 実プロジェクトの構成

`boid` 自身のリポジトリ (このリポジトリ) にある `.boid/project.yaml` (抜粋) を載せておきます。 2 つの behavior (`supervisor`, `executor`) を定義しています。 トップレベルに `kits:` / `host_commands:` / `env` が **無い** 点に注意してください — このプロジェクトの実行環境 (`playwright-cli`、 `run-e2e` 等) はこのファイルではなく workspace 側で設定されています。

```yaml
id: 40652295-c610-42da-95c4-6c6e8d28b643
name: boid

task_behaviors:
  executor:
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        ...
  supervisor:
    default_instruction:
      agent: claude-code
      model: opus
      message: |
        ...
```

このスキーマで作れる 3 種類のワークフローの例は [ワークフロー](../../workflows.md) を参照してください。
