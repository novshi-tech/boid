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

`.boid/project.yaml` を持つディレクトリのこと。 ポータブルで git 管理される「作業パターン」の定義であり、 machine-local な実行環境設定は一切含みません (それは後述の **workspace** の役割)。 `project.yaml` には次を書きます。

- `id` (この `boid` 内でプロジェクトを一意に識別する文字列) と `name` (表示名)
- 1 つ以上の **task_behaviors** — behavior 名をキーにして `hooks` / `default_instruction` 雛形を束ねたもの。 名前は自由 (free naming)。 `readonly` は behavior ごとに設定でき、 省略時は `true` (fail-safe)

プロジェクトは `boid project add <path>` / `boid project init <path>` で `boid` に登録します。プロジェクトは何個でも登録でき、各タスクはいずれか 1 つに属します。 登録すると自動的に `default` workspace に割り当てられます。

> **歴史的経緯**: 以前は project 自身が `kits:` / `host_commands` / `env` / `additional_bindings` / `secret_namespace` / `capabilities` を直接持っていました (`project.yaml` トップレベル、 または `.boid/project.local.yaml`)。 Phase 2.5 (workspace DB 一元化) でこれらは `project.yaml` からは reject されるようになり、 machine-local な実行環境は全て workspace 側に集約されました。 旧スキーマの `project.yaml` は `boid project migrate <dir>` で変換できます。詳細は [移行ガイド](migration.md) を参照してください。

## ワークスペース (workspace)

プロジェクトの **実行環境** です。 単なる分類ラベルではなく、 `host_commands` (参照名) / `env` / `capabilities` / `allowed_domains` / `additional_bindings` を持ち、 サンドボックスの設定に直接効きます。 machine 単位で `workspaces` テーブルに DB 管理され (Phase 2.5)、 project に割り当てて使います。 1 つのプロジェクトは最大 1 つの workspace に所属します。 `default` workspace は daemon 起動時に常に自動生成されるため、 カスタマイズが不要なら何もしなくても動きます。

- `boid workspace list` で登録済み workspace 一覧
- `boid workspace show <slug>` でその workspace の設定内容 (`host_commands`/`env`/`capabilities` 等) と割り当て済みプロジェクト・最近のタスクを表示
- `boid workspace create <slug>` / `edit <slug>` / `import <yaml>` で中身を作成・変更
- `boid workspace assign <project> <slug>` で割り当て (`boid workspace clear <project>` で `default` に戻す)。 `boid project init/add --workspace <slug>` は get-or-create (存在しない slug は空の workspace を自動作成してから割り当てる)

`host_commands` は二層構造です — workspace が持つのは参照 **名前** の `[]string` だけで、 実際の定義 (`path`/`allow`/`deny`/`env`) は daemon 側の `~/.config/boid/host_commands.yaml` に集約管理されます。 詳細は [オンボーディング](onboarding.md) を参照してください。

## behavior

プロジェクトの `task_behaviors` マップに並ぶ、 「タスクの種類」を表すエントリ。 タスク作成時に behavior 名を選ぶと、 `boid` はそのタスクの隔離レベルと、 紐付いた hook を読み込み、 `executing` 状態で発火します。

**Track A2 から任意の名前 (free naming) が使えます。** behavior 名に制限はなく、 `review` / `lint` / `release-mgr` のようにプロジェクトの文脈に合った名前をつけられます。

- `readonly` の既定値は **`true`** (fail-safe)。 サンドボックスを書き込み可能にするには `readonly: false` を明示してください
- `default_task_behavior` トップレベルキーで `boid task create` のデフォルト behavior を指定できます
- `supervisor` / `executor` は **旧 canonical 名** で、 現在は project 固有の名前への移行が推奨されており **deprecated** 扱いです (デーモン起動時に WARN ログが出ます)。 なお `plan` / `dev` は `BehaviorAliases` で `supervisor` / `executor` に展開される予約名なので、 free naming の例としては使えません

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

サンドボックスの実行環境の一部をまとめて配布するための単位が **kit** です。 **hook や task behavior は kit の役割ではありません** — hook は常に `project.yaml` の `task_behaviors.<name>.hooks` が権威です (kit が hook を提供したことは一度もありません)。 kit が実際に同梱できるのは次の要素だけです:

- **host_commands** — サンドボックスから host に流せるコマンドの許可リスト
- **additional_bindings** — サンドボックスにマウントしたい追加パス
- **env** — サンドボックス内に設定する環境変数

ディスク上は `kit.yaml` と関連ファイルを並べたディレクトリです。 **Phase 2.5 PR7 (2026-07) で `WorkspaceMeta.Kits` フィールド (workspace の `kits:`) はコードから完全撤去されました** — `project.yaml` に `kits:` を書く経路も既に撤去済みです。 kit ディレクトリ自体 (`~/.local/share/boid/kits/<name>/kit.yaml`) は残っていますが、 参照して読み込む経路は `boid project migrate` が生成する legacy kit (host_commands 定義を daemon 側の集約レジストリに登録する用途のみ) と `boid workspace assign` の auto-create 補助経路に限られます。 公式パッケージは [boid-kits](https://github.com/novshi-tech/boid-kits) リポジトリにあり、 ファイル構造や各フィールドの詳細は [Kit 作者向け 概要](../kit-authoring/overview.md) を参照してください。 kit 機構自体の退役の経緯は [オンボーディング / kit 機構の退役について](onboarding.md#kit-機構の退役について) と [移行ガイド / kit 機構の最終撤去](migration.md#kit-機構の最終撤去-phase-25-pr7) を参照してください。

## ジョブ (job)

hook を 1 度実行した記録のこと。 job には独自の status (`running` / `success` / `failed`) と終了コードが残ります。 「タスクを観察する」とは、 実体としてはタスクに紐付くジョブの推移を見ることです。

`boid job list --task <id>` と `boid job show <id>` が主な観測コマンドです。

## セッション (session)

**セッション**は、 タスクに紐づかない対話的ジョブです。 `boid agent <harness>` で起動し、 ターミナルに PTY が attach されます。 `harness` には `claude` / `codex` / `opencode` のいずれかを指定します。 サンドボックス内で対話シェルを開きたいだけの場合は `boid exec -p <project> -- bash` を使います (`boid agent shell` は git gateway cutover 後に退役しました)。

```bash
boid agent claude   -p <project>   # Claude Code セッションを起動
boid exec           -p <project> -- bash   # サンドボックス内でシェルを開く
boid agent claude   -p <project> --resume <session-id>   # 既存セッションに再接続
```

### タスクとセッションの違い

| | タスク | セッション |
|---|---|---|
| 起動 | `boid task create` | `boid agent <harness>` |
| 追跡 | status / payload / instructions | なし |
| 状態機械 | `pending → executing → done` | なし (running のみ) |
| 設定 | behavior (hooks / readonly 等) | workspace の設定のみ継承 |
| 用途 | 自律・長時間タスク | 対話的な作業・試験的なデバッグ |

セッションは project が割り当てられている **workspace** の `env` / `host_commands` / `additional_bindings` / `capabilities` を継承します。 secret は workspace 自身の slug をネームスペースとして解決されます。 behavior 定義は参照しません。

セッションを終了するにはエージェントを exit させるか、 `boid agent stop <job-id>` を使います。 ブラウザを閉じてもセッションプロセスは生き続け、 Web UI から再 attach できます。

## サンドボックス (sandbox)

hook を実行する隔離環境です。 実装としては Linux の mount namespace + chroot を使い、

- 読み書きできるパスは、 project が可視なジョブでは sandbox 内に git gateway 経由で clone された project のコピー ([worktree](#worktree) 参照) のみに絞る
- ネットワーク接続先は `cmd/start.go` の `defaultAllowedDomains` による組み込みリストと `~/.config/boid/config.yaml` の `sandbox.allowed_domains` をマージしたグローバル floor に、 project が割り当てられている **workspace** の `allowed_domains` を additive にマージした許可リストに限定する。 workspace はこの floor を狭められない (削れるのは追加のみ)
- ホストマシンのその他のディレクトリ (ホーム、 SSH 鍵、 他プロジェクトなど) は見えなくする

という制約をかけます。 これにより、 エージェントが暴走してもタスクの作業領域から外には出られません。

ただし一部のコマンドは作業上どうしても境界の外側に到達する必要があります (例: `git push`, `gh pr merge`, `boid task update`)。 これらは **host command** として、 project が割り当てられている workspace の `host_commands` (参照名のリスト。 実体は daemon 側の集約レジストリ `~/.config/boid/host_commands.yaml`、 または workspace が読み込む legacy kit) で明示的に宣言した場合に限り、 サンドボックスの外で実行することが許されます。

## worktree

> **歴史的経緯**: 以前は `project.yaml` の project トップ boolean フィールド `worktree: true` で child タスクに専用の isolated branch (`boid/<id8>`) を割り当てていました。 [git gateway cutover (2026-07)](../../plans/git-gateway-cutover.md) で名前上の `git worktree` は既に不要になっており、 [branch-policy-simplification Phase 1 (v0.0.11)](../../plans/branch-policy-simplification.md) で per-task branch と fork point 概念を廃止、 続く **Phase 2 (v0.0.12)** で `worktree` フィールド自体を撤去しました。

現行の挙動: project が可視なジョブは毎回 sandbox 内に project を新規 clone し、 タスク種別に関わらず `task.BaseBranch` を直接 checkout します。 host 側にはジョブ専用の worktree ディレクトリは一切作られず、 host repo の `.git` にも書き込みません — 成果は commit → push して初めて他セッションに共有されます。 既存 `project.yaml` の `worktree:` 行は BC のため silent ignore されます。

per-task `boid/<id8>` branch と fork point の概念は、 同一 `.git` を共有する worktree 時代に「child が親の未完了の作業を引き継ぐ」ために存在していました。 各 job が独立した fresh clone を持つようになったことで (clone 自体が isolation 単位)、 この仕組みは不可能 (fresh clone は origin の pushed ref しか見えないため未 push の親の変更は見えない) かつ不要 (別々の clone なら同じ branch 名を checkout しても衝突しない) になり、 廃止されました。 詳細な経緯は docs/plans/branch-policy-simplification.md を参照してください。

タスク種別による branch の割り当ては、 現在は一律です:

| タスク種別 | HEAD branch | readonly |
|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | sup=true / exec=false |
| **child sup / child exec** | `task.BaseBranch` | sup=true / exec=false |

- **root タスク** (親なし): clone した上でその `base_branch` を直接 checkout する (新規 branch は作らない)。 `base_branch` が origin にまだ存在しない場合 (case 3) は解決済みの `fork_point` からローカル作成する
- **child タスク** (親あり): root タスクと全く同じ扱い。 `base_branch` を省略すると親タスクの `base_branch` をそのまま継承するため、 明示指定がない限り親子は同じ branch を checkout する
- `base_branch` は PR target として全子タスクに継承され、 `BOID_BASE_BRANCH` env で executor に渡る

並列に走る兄弟 executor が同じ `base_branch` へ同時に push すると衝突しますが、 これは今回の変更前から変わらない executor 側の rebase/retry 契約です。 並列 child を isolate したい場合は、 子ごとに異なる `base_branch` を割り当ててください。

hook は sandbox 内の clone の中で動作し、 生成された commit が push され、 必要であれば PR が作成されます。 タスクが終了すると clone は sandbox の runtime ディレクトリごと (通常の runtime GC で) 片付けられます — worktree 専用の cleanup 処理はもうありません。 同一 project 内で同一 HEAD branch を持つタスクの直列実行 (FIFO ロック) も廃止済みで、 同じ branch を対象とする複数タスクも並行して dispatch されます。 同時に push した場合は non-fast-forward reject → fetch + merge/rebase という通常の git の作法で解決してください。

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
- サンドボックス (project が可視なら sandbox 内 clone を含む) を作って片付ける

`boid start` で起動し、 `boid stop` で停止します。多くのサブコマンドは、 daemon が動いていなければ自動的に起動します。

---

次: [状態機械](state-machine.md)
