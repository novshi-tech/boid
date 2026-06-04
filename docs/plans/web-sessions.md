# Web セッション: タスクに紐づかない job の横断ビュー

> Status: 設計合意 (2026-06-04)、未実装。
> 本ドキュメントは設計議論の結論を起こしたもの。コード参照は議論時点の調査に基づくため、
> 実装時に行番号は再確認すること。

## 背景と動機

boid のタスクは「AI が自律で完了まで持っていく作業単位」であり、Q&A で必要なときだけ
ユーザを呼び出すことで「投げて忘れて並行性を上げる」ことを狙っている。対話的に見えるのは
本質ではなく、(1) 作業中の状態をユーザに見せる、(2) Claude Code の非対話モード課金枠を避ける、
の 2 点が理由。

一方コマンドは、**タスクのレイヤーをバイパスしてサンドボックス (bypass permissions) だけを
使いたい**、定型スクリプトを回したい、というニーズから生えた機能。boid 外で素の Claude Code を
回すのと違い、サンドボックス内で bypass permissions で実行できるのが価値。`boid exec` に加えて
Web UI からも起動できる。Web ターミナル (xterm.js) の描画がまともになったことで、Web UI 上で
コマンドを実行・再接続したい需要が高まっている。

### 解くべき問題

コマンド経由で起動した job はブラウザを閉じてもプロセスが生き続ける (後述) が、
**task に紐づかない job は Web UI から再発見する導線が存在しない**。一度ブラウザを閉じると、
job ID を直接知らない限り、走っているセッションに戻れない。

違和感の正体は「コマンドだけプロジェクト単位に閉じていて、横断性が無い」こと。boid のコンセプトは
プロジェクト横断でタスクを可視化することなのに、コマンド (とその job) だけプロジェクトが
「入口・住所」として振る舞い、タスクのようにプロジェクトを「フィルタ属性」として横断できない。

## 用語: セッション

UI 上、**task に紐づかない job を「セッション」と呼ぶ** (boid 用語として追加)。
`tmux ls` のメンタルモデル — 今走っていて attach し直せる対話セッション、を指す。
内部的には依然 job だが、ユーザ向け語彙としてタスクと並ぶ第二の概念に名前を与える。

## 現状 (コード上の事実)

### コマンド実行の 2 経路

- **Project Command**: `POST /projects/{id}/commands/{name}/execute`
  - handler `internal/api/web.go` (`WebHandler.PostProjectExecuteCommand`)、dispatch `internal/server/wire.go`
  - `spec.TaskID` を設定しない → job は **`task_id = NULL`** で永続化
- **Task Behavior Command**: `POST /tasks/{id}/commands/{name}/execute`
  - handler `internal/api/web.go` (`WebHandler.PostTaskExecuteCommand`)、dispatch `internal/server/wire.go`
  - `spec.TaskID = task.ID` → job は `task_id = <task_id>` で永続化

job 登録はどちらも `internal/dispatcher/runner.go` の `CreateJob(r.DB, j)`。
`jobs.task_id` は `0021_jobs_nullable_task_id.sql` で nullable 化済み。
どちらも実行後 `/jobs/{id}/terminal` にリダイレクトする。

### ブラウザを閉じてもプロセスは生存する

- `internal/dispatcher/runtime_local_linux.go` の `Start` は `context.Context` を `_` で捨てている
  → HTTP リクエストの cancel がプロセス kill に伝播しない。
- 子プロセスは `SysProcAttr{Setsid: true, ...}` で独立セッションのリーダーとして起動
  → Web サーバの handler goroutine から独立。
- 後始末 (`Wait`) は `context.Background()` で実行され、コネクション切断の影響を受けない。
- ブラウザを閉じると `unsubscribe` で出力購読の channel を閉じるだけ。プロセス本体は無傷。
- 再接続すると `subscribe()` が録画済み transcript を snapshot として復元し、続きを配信する。

プロセスが kill されるのは明示的な `Runtime.Stop()` (job abort 等)、自然終了、daemon 停止のみ。

### 既存導線とその限界

- Task Behavior Command の job は task に紐づくため `/tasks/{id}` の Jobs セクション
  (`web/templates/tasks.templ` の `TaskDetailJobsSection`) から再発見できる。
- Project Command の job は `task_id = NULL`。`/projects/{id}/commands`
  (`web/templates/projects.templ`) はコマンド定義の一覧のみで、実行 job の履歴を持たない。
  **Web UI に全 job 一覧画面が無いため、再訪時に UI から辿り着けない**。
- API レベルでは `/api/jobs` (task_id フィルタ無し) が `task_id = NULL` を含む全 job を返せる
  (`internal/api/job.go` の `ListJobsWithContext`) が、Web UI からは呼ばれていない。

## 設計

### 「定義」と「実行」を分ける

- コマンド**定義** (project.yaml の commands) はプロジェクトの持ち物のままでよい。これは正しい。
- 閉じ込めて間違っていたのはコマンド**実行 (job)** の可視化。これを横断にする。

### 新トップレベルビュー「セッション一覧」

- **走っている (running) セッションだけ**を横断表示する。プロジェクトでフィルタ可能。
  - 履歴は出さない。完了したら一覧から消える (`tmux ls` 方式)。これにより保存・ページング等の
    面倒が不要になる。
- 各エントリをクリックすると `/jobs/{id}/terminal` に再接続 (attach) する。
- **Create** → ダイアログでプロジェクトとコマンドを選んで start。
  - プロジェクト選択 → そのプロジェクトの project.yaml の commands を提示 → 起動。
  - 起動後は従来通り `/jobs/{id}/terminal` へ。

### タスク一覧との対称性

| | タスク一覧 | セッション一覧 |
|---|---|---|
| 一覧 | 自律 job | 走っている task 無し job |
| Create | 設定して start | プロジェクト + コマンドを選んで start |
| プロジェクト | 選ぶ属性 (フィルタ) | 選ぶ属性 (フィルタ) |

両者が「一覧 + Create」で左右対称になり、プロジェクトはどちらも「入口・住所」ではなく
「選ぶ属性」に降格する。これが当初の違和感 (コマンドにとってだけプロジェクトが住所) を根治する。

### 既存導線の廃止

「タスク一覧 → プロジェクトフィルタ → コマンドボタン → コマンド一覧」の導線は、
セッション一覧の Create に吸収して廃止する。

## 決着した論点

- 対象は **task 無し job だけ** (走っている全 job ではない。タスクの job はタスク一覧で見える)。
- 配置は **トップレベル独立** (微妙な導線の奥に埋めない)。
- 名前は **セッション**。
- 履歴は **不要** (running のみ)。

## 実装方針 (スケッチ)

1. **DB クエリ**: `task_id IS NULL` かつ `status = running` の job を引く。
   `internal/api/job.go` の `ListJobsWithContext` に running フィルタを足すか、専用クエリを追加。
2. **Web ルート**: トップレベルに `/sessions` (一覧)、`/sessions/new` 相当の Create フローを追加
   (`internal/api/web.go` のルーティング)。起動は既存の Project Command execute エンドポイントを
   再利用できる見込み。
3. **templ**: セッション一覧テンプレートと Create ダイアログを追加 (`web/templates/`)。
   タスク一覧の構造を踏襲して対称にする。
4. **ナビ**: グローバルナビにタスクと並べて「セッション」を追加。
5. **廃止**: project ページのコマンドボタン経由導線を整理。

## 残課題 / 未決

- セッションの Create ダイアログでのプロジェクト → コマンドの絞り込み UX の詳細。
- 走っている判定 (`status = running`) と表示更新の頻度 (ポーリング / SSE)。
- 既存コマンド導線廃止の段階的移行 (互換期間を置くか即時か)。
