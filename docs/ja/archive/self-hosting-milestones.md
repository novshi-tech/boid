# boid Self-Hosting Milestones

## Goal

`boid` 自身の開発を `boid` 上でセルフホストできる状態まで、
段階的に機能を揃える。

関連 design note:

- [`docs/job-runtime-presenter-separation.md`](/home/nosen/src/github.com/novshi-tech/boid/docs/job-runtime-presenter-separation.md)

ここでいうセルフホストは、
単に sandbox でコマンドが動くことではなく、
以下の閉ループが成立することを指す。

- タスク作成
- 実行
- 状態観測
- 外部連携
- 再実行 / 再作業
- 完了判定

各マイルストーン完了後に、
次の優先順位を必ず見直す。

## Current Confirmed Status

コードベース上で確認できる現状は以下。

- sandbox 実行基盤はある
- orchestrator / dispatcher / sandbox の責務分離は概ね整理済み
- `hook` / `gate` 分離は実装済み
- `worktree` 管理は実装済み
- `feedback-loop` の状態機械は実装済み
- job API には一覧 / 取得 / 完了がある
- 一方で CLI は `job done` のみで、呼び出し元から job 状態を見にくい
- Web UI では task detail から jobs を見られるが、CLI 主体の自己ホスト運用にはまだ弱い
- black-box E2E harness と fixture kit / fake host command 基盤は追加済み
- `project-smoke` / `host-command-smoke` / `readonly-hook-gate` / `writable-chain` / `rework-cycle` / `feedback-loop-full` はローカルで通過済み
- `e2e/run.sh` は引数なしで全 scenario を順次実行できる
- GitHub Actions 向けに unit + black-box E2E workflow は追加済み
- ただし hosted runner 上の初回実行結果はまだ未確認

このため、直近の不足は概念設計よりも、
観測性と実運用フローの完成度にある。

## Milestone Design Principles

### 1. Core と External Kit を分離する

`git push`、`go install .`、`systemctl --user restart boid` のような
環境依存処理は product core に持ち込まない。

これらは外部 kit の契約として実現する。

現時点の前提:

- `local kits` はすでに `github.com/novshi-tech/boid-kits` に移植済み
- boid 本体に残っている local kit 実装 / 同等責務は削除対象
- 今後の gate 契約は `boid-kits` 側へ寄せる

core 側が担う責務:

- task / job の実行管理
- payload / artifact / verification の管理
- gate 起動契約
- 状態遷移

external kit 側が担う責務:

- push
- build / install
- service restart
- PR 作成
- merge
- CI / review の外部 API 連携

### 2. 先に観測可能性を作る

自己ホストでは「動いたか分からない」が最悪の失敗になる。

そのため、
新しい task type を増やすより先に、
task / job / action / payload の観測手段を揃える。

### 3. plan / triage は最後に乗せる

`plan` と `triage` は上位機能であり、
下位の one-shot / feedback-loop が安定していない状態で作っても
デバッグ対象が増えるだけになる。

## Milestones

## M0. Runtime Stabilization And Observability

### Outcome

「boid が何を実行していて、どこで止まっているか」が分かる。

### Why First

現状でも dispatch 基盤はかなりあるが、
自己ホストに必要なのは実行能力より観測能力である。
これがないと one-shot も feedback-loop も運用できない。

### Scope

- black-box E2E を継続実行できる状態を保つ
- job 観測 CLI を追加する
- task 観測 CLI / Web を強化する
- 実行結果の追跡単位を明確にする

現在ここで完了済みのもの:

- `boid start` の E2E 向けパラメータ化
- isolated temp root 上で `boid` を起動する black-box harness
- fixture kit と fake host command を使った black-box scenario 群
- hook/gate 並列実行、rework、自動遷移、feedback-loop 全サイクルの E2E
- `e2e/run.sh` 引数なし実行による全 scenario 一括実行
- GitHub Actions 用 black-box E2E workflow 追加

ここからの残り:

- product CLI の job 観測導線
- task 観測 CLI / Web の強化
- hosted runner 上での初回 CI 実行結果確認

### Concrete Tasks

- `boid job list --task <id>`
- `boid job show <job-id>`
- `boid job watch <job-id>`
- `boid task watch <task-id>` または同等の監視導線
- job に対して `handler_id`, `role`, `status`, `exit_code`, `updated_at` を見やすく出す
- task detail から action / job / payload の対応を追えるようにする
- GitHub Actions 上で black-box E2E の実行実績を確認する

### Completion Criteria

- one-shot 実行後に、呼び出し元から task / job の成否を確認できる
- job が詰まっている場合に、どの handler で止まっているか分かる
- hook/gate の主要実行経路が E2E で守られている
- black-box E2E を CI で継続実行できる

## M1. One-Shot Hook Execution On Orchestrator

### Outcome

boid 上で単発の開発タスクを自動実行し、
artifact または tasks を payload に残して完了できる。

### Scope

- `one-shot` behavior を自己ホストの最小単位として成立させる
- hook 実行から payload persist までを安定化する
- 呼び出し元が task 完了を待てるようにする

### Concrete Tasks

- one-shot task の作成テンプレート整備
- `start` -> `executing` -> `done` の観測可能な実行パスを固める
- artifact を返す hook の標準出力契約を整理する
- 必要なら `boid exec` から task 起動を包む薄い導線を作る

### Completion Criteria

- boid 自身の小さな実装タスクを `one-shot` で完了できる
- 完了時に artifact または tasks が payload に残る
- 実行結果を CLI から追跡できる

## M2. External Gate Kit Contract

### Outcome

環境依存の処理を `github.com/novshi-tech/boid-kits` に閉じ込めた形で
gate を実行できる。

### Why Before Worktree

worktree 化より先に、
「gate が何を受け取り、何を返すか」を固定しないと
後続の PR 作成、push、deploy の責務がぶれる。

### Scope

- external gate の入出力契約を決める
- host command の最小権限化
- gate の filesystem 非依存方針を固定する

### Concrete Tasks

- `git push`
- `go install .`
- `systemctl --user restart boid`

上記は `boid-kits` 側の gate として実装する。

また、以下もこの段階で決める。

- gate は payload/artifact を入力にする
- gate は project/worktree を直接触らない
- gate から必要な host command だけを許可する
- 成功 / 失敗を verification or artifact にどう反映するかを統一する

### Completion Criteria

- ローカル環境固有の処理が core から分離されている
- `boid` 本体が local kit 実装を持たない
- gate 実行契約が worktree なしでも安定している
- `git push` / `go install .` / restart を `boid-kits` 経由で実行できる

## M3. One-Shot Worktree Execution

### Outcome

boid 上で変更を worktree に隔離して実装し、
PR を作成し、レビュー後にマージまで進められる。

### Scope

- worktree 付き behavior を one-shot で成立させる
- PR 作成 gate を `boid-kits` 側へ寄せる
- merge の責務分離を決める

### Constraints

- worktree 内では `go install` と service restart をしてはいけない
- それらは merge 後の local gate に寄せる

### Concrete Tasks

- worktree behavior の標準定義を作る
- branch naming / cleanup ポリシーを固定する
- PR 作成 gate を追加する
- レビュー通過後に merge を起動する local gate を追加する
- 完了時の worktree cleanup を E2E で守る

### Completion Criteria

- boid 自身の変更を worktree で実装できる
- PR を作成できる
- merge 後に本体 repo 側へ反映できる
- worktree が適切に cleanup される

## M4. Feedback-Loop Worktree Execution

### Outcome

PR ベースの修正ループを boid が自律的に回せる。

### Scope

- `executing -> verifying -> reworking -> verifying -> done`
  を実運用で閉じる（旧モデルでいう feedback-loop フルサイクルに相当）
- verification を追記して rework できるようにする
- 外部レビュー / CI の結果を payload へ反映する

### Concrete Tasks

- gate で PR 作成
- CI 成功待ち
- CI 失敗時は verification を追記して `reworking` に戻す
- review 取得
- 修正指摘があれば verification を追記して `reworking` に戻す
- resolved なら `done`

### Important Note

状態機械自体はすでにある。
不足しているのは、
外部システムの状態を verification として取り込む導線である。

### Completion Criteria

- CI 失敗で自動 rework できる
- review 指摘で自動 rework できる
- すべて resolved なら `done` まで進む

## M5. Plan Task Execution

### Outcome

対話で計画した内容を boid task に落とし込み、
完了時に実行用 task 群を自動生成できる。

### Scope

- readonly な planning behavior を定義する
- plan task の成果物を `tasks` trait に統一する
- `done` 時に下位 task を生成する

### Concrete Tasks

- `plan` behavior を readonly task として定義する
- plan hook が task list を `tasks` trait に書く
- `tasks` trait から feedback-loop / one-shot task を生成する executor を作る
- task 生成時に behavior 選択ルールを持たせる

### Completion Criteria

- 対話の結果から plan task を作れる
- plan task 完了時に実行 task 群が生成される
- 少なくとも boid 自身の中規模開発を task 分解できる

## M6. Triage Task

### Outcome

簡単な依頼文から、
実行方式を自動で選べる。

### Scope

- complexity 判定
- ambiguity 判定
- task routing

### Routing Rule

- 曖昧さが小さい + 小規模: `one-shot`
- 曖昧さが小さい + 実装ループあり: `feedback-loop`
- 曖昧さが大きい: `plan`

### Concrete Tasks

- triage input schema を定義する
- complexity / ambiguity の判定基準を決める
- 自動的に plan / feedback-loop / one-shot を選ぶ
- triage 結果を task として永続化する

### Completion Criteria

- 簡単な依頼をその場で task 化して実行できる
- 曖昧な依頼は plan に回せる
- boid 自身の今後の開発依頼を triage から開始できる

## Recommended Near-Term Backlog

今すぐ着手順を絞るなら以下。

1. hosted runner 上で black-box E2E workflow の初回実行結果を確認する
2. job/task 観測 CLI を足す
3. `boid-kits` の gate 契約を決める
4. one-shot hook を boid 自身の小タスクで回す
5. worktree + PR 作成を成立させる
6. CI / review を verification に取り込む

## Suggested Definition Of Done Per Milestone

各マイルストーンの完了条件は、
「機能がある」ではなく、
「boid 自身の開発で実際に 1 回使える」とする。

具体的には毎回以下を要求する。

- boid 自身の変更に対して実際に使う
- 主要経路を E2E で固定する
- オペレータが CLI から状態を追える
- 次のマイルストーンに必要な contract を文書化する

## What Not To Do Yet

現時点では以下を後回しにするのが妥当。

- triage を先に作る
- plan task を先に高度化する
- local 環境依存処理を core に埋め込む
- 一時しのぎで boid 本体に local kit を再導入する
- worktree 前に CI/review 自動化を深掘りする

順序を守らないと、
失敗箇所の切り分けが難しくなる。
