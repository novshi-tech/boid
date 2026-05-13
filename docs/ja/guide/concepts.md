# 概念

このページでは `boid` を構成する主要な概念を解説します。以降のドキュメントは、ここで定義した用語を前提として書かれています。

## タスク (task)

`boid` が依頼から完了まで追跡する作業の単位。各タスクは以下のフィールドを持ちます。

- **status** — タスクが今どの段階にあるかを表す値。 `pending → executing → done` を順に進み、失敗で終わった場合は `aborted` で終端します。各状態の意味と遷移条件は [状態機械](state-machine.md) で扱います
- **payload** — タスクが進行する過程で蓄積される JSON ドキュメント。最初の依頼内容、生成された成果物、レビューでの指摘などをキー名 (後述する trait) ごとに格納します
- **behavior** — `supervisor` (旧 `plan`) や `executor` (旧 `dev`) といったラベルで、このタスクで何の作業をするかを示します。プロジェクト側の設定でラベルごとに「どの拡張パッケージ (kit) を使うか」が紐付いており、選んだ behavior に応じて発火するスクリプトが切り替わります
- 所属する **プロジェクト**

タスクは `boid task create` で作成し、 `boid task list` / `boid task show` / `boid task watch`、TUI、Web UI で観察します。

## プロジェクト (project)

`.boid/project.yaml` を持つディレクトリのこと。 `project.yaml` には次を書きます。

- `id` (この `boid` 内でプロジェクトを一意に識別する文字列) と `name` (表示名)
- 任意の project トップ `worktree: true` フラグ — executor タスクごとに専用 git worktree を切るかどうか
- 1 つ以上の **task_behaviors** — behavior ラベル (典型的には `supervisor` と `executor`) ごとに、使う拡張パッケージ (kit) のリストと任意の `default_instruction` 雛形を束ねたもの。 サンドボックスを read-only にするか worktree を切るかは behavior 単位では設定せず、 canonical 名と project トップフラグから自動で導出されます
- (任意) 各 kit に渡す設定値

プロジェクトは `boid project add <path>` で `boid` に登録します。プロジェクトは何個でも登録でき、各タスクはいずれか 1 つに属します。

## behavior

プロジェクトの `task_behaviors` マップに並ぶ、名前付きの「タスクの種類」を表すエントリ。タスク作成時に behavior 名を選ぶと、 `boid` はその behavior に紐付いた拡張パッケージを読み込み、状態遷移に応じてその中のスクリプトを発火します。

**canonical な名前は 2 つ** に絞られています:

- **`supervisor`** (旧 alias: `plan`) — readonly な統括役。 要求を読み、 必要な子タスクを決め、 作成し、 監視し、 結果を統合する
- **`executor`** (旧 alias: `dev`) — 書き込み可能な実装役。 単一の集中したタスクを受けて成果物 (commit / PR / payload trait) を作る

旧 alias は load 時に翻訳されるので、 改名前に書かれた `project.yaml` もそのまま動作します。 新規プロジェクトは canonical 名で書いてください。

`boid` の状態機械は behavior に関わらず 1 種類だけです。タスクの動作の違いは、 behavior に紐付ける hook / gate の組み合わせと、 失敗時に `reopen` で executing に戻して新しい instruction を渡すかどうかで表現します。検証ループはハーネスではなく agent instruction 側の責務です。

## payload と trait

payload は、タスクが進む過程で情報を蓄積していく JSON ドキュメントです。 payload のトップレベルに置けるキーは事前に決まっており、各キーを **trait** と呼びます。各 trait には「誰が書いてよいか」「書かれることで何が起きるか」が定義されています。

| Trait | 書く主体 | 起こること |
|---|---|---|
| `artifact` | 実行スクリプト | 実装系タスクが残す成果物 (commit / PR URL / 変更ファイル等) を格納する自由形 |
| `lifecycle.abort` | `boid` 本体 | abort の `code` / `message` 等、履歴から自動算出される値 |

サブタスクの生成 (supervisor 系タスクの主要な仕事) は payload trait ではなく、 hook / gate から `boid task create` builtin を直接呼ぶ形で行います。 詳細は [`/boid-supervisor` SKILL](../../../internal/skills/data/boid-supervisor/SKILL.md) を参照してください。

instructions は payload の trait ではなく、 タスクの top-level フィールド (`Task.Instructions` 配列) に保持されます。 配列の最後の要素が active な指示で、 `boid task reopen <id> --message "..."` で append されます。

スクリプト側は **payload patch** (JSON のマージ指示) を出力して payload を更新します。 daemon は受け取った patch を順に保存しており、各タスクの状態変化を後から再生してデバッグできます。

## hook, gate, kit, handler

`boid` ではタスク実行に関わるスクリプトを 2 種類に分けています — **hook** と **gate** — そして両方をまとめて呼ぶ場合は **handler** という言葉を使います。それらをパッケージ化して再利用可能な単位にしたものが **kit** です。

- **hook** — `executing` 状態にいる間に実行されるスクリプト。 AI エージェントの呼び出し、コード変更、テスト実行などの主作業はここで行います。サンドボックス内で動き、同じ behavior に複数の hook が紐付いていれば並列に実行されます。 hook は `executing` でしか走りません
- **gate** — 状態遷移の前後 (`phase: entry` / `phase: exit`) で発火するスクリプト。 PR の作成、 `gh pr merge`、サービス再起動など、ホストマシン側に作用する作業はここに置きます。サンドボックスを介さず host で動き、状態を進める前後の関所になります
- **kit** — `kit.yaml` と、 hook / gate のスクリプト、付随アセットをまとめたディレクトリ。 1 度インストールすれば、どのプロジェクトの `task_behaviors` からも参照できます。公式パッケージは [boid-kits](https://github.com/novshi-tech/boid-kits) リポジトリにあります

hook / gate と `boid` 本体は、 stdin にタスクの payload、 stdout に payload patch、というプロトコルで通信します。

## ジョブ (job)

handler を 1 度実行した記録のこと。 job には独自の status (`running` / `success` / `failed`) と終了コードが残ります。「タスクを観察する」とは、実体としてはタスクに紐付くジョブの推移を見ることです。

`boid job list --task <id>` と `boid job show <id>` が主な観測コマンドです。

## サンドボックス (sandbox)

hook を実行する隔離環境です。実装としては Linux の mount namespace + chroot を使い、

- 読み書きできるパスは worktree (または worktree を持たないタスク — supervisor タスクや、 project トップ `worktree:` を設定していないプロジェクトの executor タスク — ではプロジェクトのルートディレクトリ) のみに絞る
- ネットワーク接続先は kit が宣言したドメインに限定する
- ホストマシンのその他のディレクトリ (ホーム、 SSH 鍵、他プロジェクトなど) は見えなくする

という制約をかけます。これにより、エージェントが暴走してもタスクの作業領域から外には出られません。

ただし一部のコマンドは作業上どうしても境界の外側に到達する必要があります (例: `git push`, `gh pr merge`, `boid task update`)。これらは **host command** として kit 側で明示的に宣言した場合に限り、サンドボックスの外で実行することが許されます。

## worktree

project トップで `worktree: true` を宣言したプロジェクトでは、 **executor** タスクごとに新しいブランチで専用の **git worktree** が作成されます。worktree は同じリポジトリの複数ブランチを別々のディレクトリとして同時にチェックアウトする git の機能で、これを使うと変更が他のタスクと独立した別ディレクトリに閉じます。 hook はその worktree 内で動作し、生成された commit が push され、必要であれば PR が作成されます。 タスクが done になると worktree は片付けられます。

supervisor タスクは `worktree:` フラグに関わらず worktree を持たず、 常に readonly で project root 上を走ります。

## アクション (action)

手動の状態遷移を引き起こすイベントの単位です。代表例:

- `start` — `pending` から `executing` に進める
- `reopen` — `done` のタスクを `executing` に戻し、 新しい instruction を `Task.Instructions` 配列に append する (`--message "..."`)
- `abort` — 任意の状態を強制的に `aborted` で打ち切る

`boid action send --task <id> --type <action>` で送るほか、 TUI / Web UI からも発行できます。

## daemon

`boid` の常駐サーバプロセスです。次の役割を持ちます。

- CLI と通信するための UNIX ソケット、 Web UI 用の HTTP リスナを開く
- SQLite データベースをひとり占めで保持する
- handler を順番に発火していくループ (dispatch loop) を回す
- worktree とサンドボックスを作って片付ける

`boid start` で起動し、 `boid stop` で停止します。多くのサブコマンドは、 daemon が動いていなければ自動的に起動します。

---

次: [状態機械](state-machine.md)
