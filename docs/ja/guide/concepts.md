# 概念

このページは語彙リファレンスです。他のドキュメントを読む前に一読してください。以降のドキュメントはここで定義した用語を前提にします。

## タスク (task)

`boid` が依頼から完了まで追跡する作業の単位。各タスクは以下を持ちます。

- **status** — 状態機械を遷移します: `pending → executing → verifying → reworking → done`、終端の失敗状態として `aborted`。詳細は [状態機械](state-machine.md) を参照
- **payload** — タスクの状態を持ち回る JSON ドキュメント。指示、生成された成果物、検証結果などを保持
- **behavior** — `dev` や `plan` などのラベル。どの kit / handler が関与するかを選択
- 所属する **プロジェクト**

タスクは `boid task create` で作成し、 `boid task list` / `boid task show` / `boid task watch`、TUI、Web UI で観察します。

## プロジェクト (project)

`.boid/project.yaml` を持つディレクトリ。 `project.yaml` は次を宣言します。

- `id` と `name`
- 1 つ以上の **task_behaviors** (各 behavior に対し transition モードと kit のリストを束ねる)
- (任意) kit レベルの設定

プロジェクトは `boid project add <path>` で登録します。プロジェクトは複数持てます。タスクは必ずいずれか 1 つに属します。

## behavior

プロジェクトの `task_behaviors` マップ内の名前付きスロット。タスク作成時に behavior を選ぶと、 `boid` はその behavior に紐付いた kit を読み込み、状態遷移に応じて handler を発火します。

代表的な transition モード:

- **one-shot** — 1 回実行 → verify → done。「これ 1 つやる」型
- **feedback-loop** — execute → verify → finding 解消まで rework サイクルを回す。レビューが必要な変更向け

## payload と trait

タスクの payload は JSON ドキュメントです。トップレベルのキーを **trait** と呼び、それぞれ意味が定義されています。

| Trait | 書く主体 | 駆動するもの |
|---|---|---|
| `instructions` | タスク作成者 / kit | handler の作業内容 |
| `artifact` | 実行 hook | 「executing 完了」の合図、 `verifying` への遷移 |
| `tasks` | plan hook | `artifact` と対称、 plan 系 behavior が使う |
| `verification.findings` | reviewer hook / gate | `reworking` への遷移と `verifying` への復帰 |
| `lifecycle` | core | rework_count、executed フラグなどの computed state |

handler は **payload patch** (JSON merge 指示) を出力して payload を更新します。core と kit の対話を構造化・リプレイ可能に保つための仕組みです。

## hook, gate, kit

いたる所で出てくる 3 概念です。

- **Hook** — 状態の中で **サンドボックス内** で実行されるスクリプト。実作業 (AI エージェントの呼び出し、コード変更、テスト実行) を行う。同じ状態に複数の hook を並列実行できます
- **Gate** — 状態遷移時 (entry / exit) に **host で** 発火するスクリプト。環境側の作業 (PR 作成、 `gh pr merge`、サービス再起動) を行う。並列実行され、サンドボックスロックは取りません
- **Kit** — `kit.yaml` と hook / gate スクリプト、付随アセットを束ねたディレクトリ。再利用可能なパッケージで、一度インストールすればどのプロジェクトからでも参照可能。公式 kit は [boid-kits](https://github.com/novshi-tech/boid-kits) にあります

hook / gate は stdin (タスク payload) と stdout (payload patch) で `boid` と通信します。

## ジョブ (job)

hook や gate の 1 回の実行のこと。job は独自の status (`running` / `success` / `failed`) と終了コードを持ちます。タスクを watch するとは、内部的にはジョブの推移を見ることです。

`boid job list --task <id>` と `boid job show <id>` が主な観測コマンドです。

## サンドボックス (sandbox)

hook が動く Linux mount namespace + chroot 環境。読み書きは worktree (worktree なし behavior ではプロジェクトルート) に限定され、ネットワークは宣言済みドメインのみ、host の filesystem は隠蔽されます。 hook は明示的に許可しない限り、ホームディレクトリ・ ssh 鍵・他プロジェクトに触れません。

ごく少数の **host command** (kit 単位で宣言) はこの境界を越えられます (例: `git push`, `gh pr merge`, `boid task update`)。それ以外は内部に閉じます。

## worktree

git 変更を伴う behavior (典型的には `feedback-loop`) では、 `boid` は専用の git worktree を新ブランチで作成します。hook はその worktree 内で動き、生成された commit が push され、 PR が作成されます。 PR がマージされると worktree は破棄されます。複数の dev タスクを並列に動かしても干渉しないための仕組みです。

## verification finding

`verification.findings` 内の 1 オブジェクト。レビュアーが直してほしい点を記録します。 finding は `state` (発生した状態: `executing` / `verifying` / `reworking`)、 `status` (`open` / `resolved`)、 (任意) `severity` (`fatal` は即 abort)、自由形式の message を持ちます。 `verifying → reworking` の遷移と、 rework ループからの脱出を駆動するのが finding です。

## アクション (action)

手動遷移を引き起こす user / system イベント。例: `start` (`pending → executing` 移行)、 `done` (任意状態を done に強制)、 `abort`。 `boid action send --task <id> --type <action>` で送るほか、 TUI / Web UI からも発行できます。

## daemon

長時間動く `boid` サーバ。 UNIX socket (Web UI 用に HTTP socket も) を listen し、 SQLite を保持し、 hook / gate を発火する dispatch ループを回し、 worktree とサンドボックスを管理します。 `boid start` で起動し、 `boid stop` で停止。多くの CLI は daemon が動いていなければ自動起動します。

---

次: [状態機械](state-machine.md)
