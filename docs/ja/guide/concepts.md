# 概念

このページでは `boid` を構成する主要な概念を解説します。以降のドキュメントは、ここで定義した用語を前提として書かれています。

## タスク (task)

`boid` が依頼から完了まで追跡する作業の単位。各タスクは以下のフィールドを持ちます。

- **status** — タスクが今どの段階にあるかを表す値。 `pending → executing → done` を順に進み、失敗で終わった場合は `aborted` で終端します。各状態の意味と遷移条件は [状態機械](state-machine.md) で扱います
- **payload** — タスクが進行する過程で蓄積される JSON ドキュメント。実行スクリプトが書き残した成果物などをキー名 (後述する trait) ごとに格納します
- **behavior** — タスクの種類を表す名前。 サンドボックスの読み書き可否と紐付く hook セットが behavior ごとに決まります
- 所属する **プロジェクト**

タスクは `boid task create` で作成し、 `boid task list` / `boid task show` / `boid task watch`、Web UI で観察します。

## プロジェクト (project)

`.boid/project.yaml` を持つディレクトリのこと。 `project.yaml` には次を書きます。

- `id` (この `boid` 内でプロジェクトを一意に識別する文字列) と `name` (表示名)
- 任意の project トップ `worktree: true` フラグ — executor タスクごとに専用 git worktree を切るかどうか
- このプロジェクトで使う **kit** のリスト (`kits:`)
- 1 つ以上の **task_behaviors** — behavior 名をキーにして `default_instruction` 雛形を束ねたもの。 名前は自由 (free naming)。 `readonly` は behavior ごとに設定でき、 省略時は `true` (fail-safe)

プロジェクトは `boid project add <path>` で `boid` に登録します。プロジェクトは何個でも登録でき、各タスクはいずれか 1 つに属します。

## ワークスペース (workspace)

プロジェクトをまとめて分類するためのラベルです。 例えば 「個人」 「業務」 「OSS」 のようにグルーピングしておくと、 Web UI で表示を絞り込めます。 ワークスペースは `project.yaml` には書かず、 `boid workspace assign <project> <workspace-id>` で割り当てます (`boid workspace clear` で解除)。 1 つのプロジェクトは最大 1 つのワークスペースに所属します。

- `boid workspace list` で登録済みワークスペース一覧
- `boid workspace show <id>` でそのワークスペースに属するプロジェクトと最近のタスクを表示

ワークスペースは純粋に分類用のメタデータで、 サンドボックスの設定や hook 実行には影響しません。

## behavior

プロジェクトの `task_behaviors` マップに並ぶ、 「タスクの種類」を表すエントリ。 タスク作成時に behavior 名を選ぶと、 `boid` はそのタスクの隔離レベルと、 紐付いた hook を読み込み、 `executing` 状態で発火します。

**Track A2 から任意の名前 (free naming) が使えます。** behavior 名に制限はなく、 `plan` / `dev` / `review` のようにプロジェクトの文脈に合った名前をつけられます。

- `readonly` の既定値は **`true`** (fail-safe)。 サンドボックスを書き込み可能にするには `readonly: false` を明示してください
- `default_task_behavior` トップレベルキーで `boid task create` のデフォルト behavior を指定できます
- `supervisor` / `executor` は後方互換エイリアスとして引き続き動作しますが **deprecated** 扱いです。 デーモン起動時に WARN ログが出ます

`boid` の状態機械は behavior に関わらず 1 種類だけです。 タスクの動作の違いは、 hook の組み合わせと、 失敗時に `reopen` で executing に戻して新しい instruction を渡すかどうかで表現します。 検証ループはハーネスではなく agent instruction 側の責務です。

移行手順とテーブルは [task_behaviors 移行ガイド](../reference/task-behavior-migration.md) を参照してください。

## payload と trait

payload は、 タスクが進む過程で情報を蓄積していく JSON ドキュメントです。 payload のトップレベルに置けるキーは事前に決まっており、 各キーを **trait** と呼びます。

現在、 hook から書き込める trait は **`artifact`** のみで、 実装系タスクが残した成果物 (commit / PR URL / 変更ファイル等) を自由形マップで格納します。

`lifecycle.abort` のようなフィールドも payload 上に見えますが、 これは履歴から `boid` 本体が自動算出する仮想的な値で、 実体としては payload には保存されません。 詳細は [Payload trait リファレンス](../reference/traits.md) を参照してください。

instructions は payload の trait ではなく、 タスクの top-level フィールド (`Task.Instructions` 配列) に保持されます。 配列の最後の要素が active な指示で、 `boid task reopen <id> --message "..."` で append されます。

スクリプト側は **payload patch** (JSON のマージ指示) を出力して payload を更新します。 daemon は受け取った patch を順に保存しており、 各タスクの状態変化を後から再生してデバッグできます。

## hook

タスクの `executing` 状態で実行されるスクリプトを **hook** と呼びます。 AI エージェントの呼び出し、 コード変更、 テスト実行、 PR 作成といった実作業は すべて hook 側で行います。 hook はサンドボックス内で動き、 同じ behavior に複数の hook が紐付いていれば並列に実行されます。

hook と `boid` 本体は、 stdin にタスクの payload、 stdout に payload patch、 というプロトコルで通信します (詳細は [hook スクリプトプロトコル](../reference/hook-contract.md))。

## kit

サンドボックスで動かす作業に必要な部品をひとまとめに束ねた配布単位が **kit** です。 1 つの kit は次のような要素を任意に同梱します:

- **hook** — 上記の executing 中に動くスクリプト
- **commands** — サンドボックス内から `boid exec` で呼べる named コマンド
- **host_commands** — サンドボックスから host に流せるコマンドの許可リスト
- **additional_bindings** — サンドボックスにマウントしたい追加パス
- **env** — サンドボックス内に設定する環境変数

ディスク上は `kit.yaml` と関連スクリプトを並べたディレクトリで、 1 度インストールすればどのプロジェクトの `kits:` からも参照できます。 公式パッケージは [boid-kits](https://github.com/novshi-tech/boid-kits) リポジトリにあり、 ファイル構造や各フィールドの詳細は [Kit 作者向け 概要](../kit-authoring/overview.md) を参照してください。

## ジョブ (job)

hook を 1 度実行した記録のこと。 job には独自の status (`running` / `success` / `failed`) と終了コードが残ります。 「タスクを観察する」とは、 実体としてはタスクに紐付くジョブの推移を見ることです。

`boid job list --task <id>` と `boid job show <id>` が主な観測コマンドです。

## セッション (session)

**セッション**は、 タスクに紐づかない対話的ジョブです。 `boid agent <harness>` で起動し、 ターミナルに PTY が attach されます。 `harness` には `claude` / `codex` / `opencode` / `shell` のいずれかを指定します。

```bash
boid agent claude   -p <project>   # Claude Code セッションを起動
boid agent shell    -p <project>   # シェルセッションを起動
boid agent claude   -p <project> --resume <session-id>   # 既存セッションに再接続
```

### タスクとセッションの違い

| | タスク | セッション |
|---|---|---|
| 起動 | `boid task create` | `boid agent <harness>` |
| 追跡 | status / payload / instructions | なし |
| 状態機械 | `pending → executing → done` | なし (running のみ) |
| 設定 | behavior (hook / kit / readonly 等) | プロジェクトのトレイトのみ継承 |
| 用途 | 自律・長時間タスク | 対話的な作業・試験的なデバッグ |

セッションはプロジェクトの `env` / `host_commands` / `additional_bindings` / `secret_namespace` を継承します。 behavior 定義は参照しません。

セッションを終了するにはエージェントを exit させるか、 `boid agent stop <job-id>` を使います。 ブラウザを閉じてもセッションプロセスは生き続け、 Web UI から再 attach できます。

## サンドボックス (sandbox)

hook を実行する隔離環境です。 実装としては Linux の mount namespace + chroot を使い、

- 読み書きできるパスは worktree (または worktree を持たないタスク — supervisor タスクや、 project トップ `worktree:` を設定していないプロジェクトの executor タスク — ではプロジェクトのルートディレクトリ) のみに絞る
- ネットワーク接続先は `cmd/start.go` の `defaultAllowedDomains` による組み込みリストと `~/.config/boid/config.yaml` の `sandbox.allowed_domains` をマージした許可リストに限定する。 kit ごとのドメイン宣言は存在せず、 許可リストはグローバルに適用される
- ホストマシンのその他のディレクトリ (ホーム、 SSH 鍵、 他プロジェクトなど) は見えなくする

という制約をかけます。 これにより、 エージェントが暴走してもタスクの作業領域から外には出られません。

ただし一部のコマンドは作業上どうしても境界の外側に到達する必要があります (例: `git push`, `gh pr merge`, `boid task update`)。 これらは **host command** として kit 側で明示的に宣言した場合に限り、 サンドボックスの外で実行することが許されます。

## worktree

project トップで `worktree: true` を宣言したプロジェクトでは、 **executor / supervisor** タスクに専用の **git worktree** が割り当てられます。 worktree は同じリポジトリの複数ブランチを別々のディレクトリとして同時にチェックアウトする git の機能で、 これを使うと変更が他のタスクと独立した別ディレクトリに閉じます。

タスク種別によって worktree の割り当て方が異なります:

| タスク種別 | HEAD branch | fork 元 | readonly |
|---|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | n/a | sup=true / exec=false |
| **child sup / child exec** | `boid/<task_id8>` | **親タスクの HEAD branch** | sup=true / exec=false |

- **root タスク** (親なし): `base_branch` が project の現 HEAD と一致する場合 (case 1) は worktree を持たず project root 上で動く。 不一致の場合 (case 2/3) は `base_branch` を HEAD とした専用 worktree が割り当てられる
- **child タスク** (親あり): 常に `boid/<task_id8>` branch の worktree を持つ。 fork 元は **親タスクの HEAD branch** であり、 直接の親のみを参照する (1 hop)
- `base_branch` は PR target として全子タスクに継承され、 `BOID_BASE_BRANCH` env で executor に渡る

hook はその worktree 内で動作し、 生成された commit が push され、 必要であれば PR が作成されます。 タスクが done になると worktree は片付けられます。 同一 project 内で同一 HEAD branch を持つタスクは直列実行 (FIFO ロック) されます。 詳細は [`project.yaml` リファレンス / HEAD branch ロック](../reference/project-yaml.md#head-branch-ロック-1-project--1-head-branch) を参照してください。

## アクション (action)

手動の状態遷移を引き起こすイベントの単位です。代表例:

- `start` — `pending` から `executing` に進める
- `reopen` — `done` のタスクを `executing` に戻し、 新しい instruction を `Task.Instructions` 配列に append する (`--message "..."`)
- `abort` — 任意の状態を強制的に `aborted` で打ち切る

`boid action send --task <id> --type <action>` で送るほか、 Web UI からも発行できます。

## daemon

`boid` の常駐サーバプロセスです。次の役割を持ちます。

- CLI と通信するための UNIX ソケット、 Web UI 用の HTTP リスナを開く
- SQLite データベースをひとり占めで保持する
- hook を順番に発火していくループ (dispatch loop) を回す
- worktree とサンドボックスを作って片付ける

`boid start` で起動し、 `boid stop` で停止します。多くのサブコマンドは、 daemon が動いていなければ自動的に起動します。

---

次: [状態機械](state-machine.md)
