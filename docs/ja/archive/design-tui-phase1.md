# TUI Phase 1 設計・実装計画

## 背景と課題

### tmux 依存の実行パス撤廃後の可視性ギャップ
直近のリファクタリングで tmux 依存を実行パスから外し、ジョブに PTY を直接割り当てる方式に移行した。実行基盤は簡潔になったが、複数ジョブの同時可視化手段が失われた。

### 現状の attach の利便性
`boid job attach <job-id>` でジョブに接続できるが:
- ジョブ ID を事前に調べる必要がある
- 複数ジョブ間の切り替えが煩雑（list → attach → 離脱 → list → attach）
- attach から離脱する方法が提供されていない
- タスクの状態遷移が見えない

### boid のミッションと TUI の位置づけ
boid の目標は「複数プロジェクトにまたがるタスクを並行して AI に実行させるときの、人間の負荷を下げる」ことである。AI エージェント単体ではプロジェクト切り替えや並列観測が難しい領域があり、それを補う「観測と制御の中心」として TUI を位置づける。

将来的には `boid` コマンド単体で起動するデフォルト UI を目指す。マルチプロジェクト・ワークスペース横断ビューもその視野に入る。

## 設計方針

### 表示層 tmux 委譲
PTY 表示は TUI プロセスに埋め込まず、tmux のペインまたはポップアップに委譲する。TUI はジョブを選択した時点で `tmux split-window` / `tmux display-popup` を発行し、子プロセスとして既存の `boid job attach` を起動する。

これにより:
- VT エミュレータを組み込む必要がない
- TUI プロセスが raw mode を明け渡す必要がない
- detach はペインを閉じる動作で自然に実現される（SIGHUP → attach クライアント終了 → サーバ PTY は生存）
- 並列観測が tmux ネイティブのペイン機能で得られる

過去のリファクタで撤廃したのは**実行層**の tmux 依存であり、本計画で導入するのは**表示層**の optional な依存である。TUI を使わないユーザは引き続き tmux なしで boid を使える。

### bubbletea をナビゲーション TUI に採用
制御を明け渡さないため、bubbletea の弱点（raw mode 復帰の扱いづらさ）には当たらない。ナビ TUI は離散的な状態遷移とリスト表示が中心であり、これは bubbletea の得意領域である。`bubbles/list` などの既存コンポーネントも活用できる。

### tmux 非起動時のフォールバック
`$TMUX` 環境変数が未設定の場合も TUI は起動する。一覧表示・状態観測は完全に動作し、ジョブを open する機能のみが無効化される。

これにより TUI は「観測と非対話的操作の中心」として tmux 非依存の領域を持ち、tmux 環境ではさらに「対話的 PTY 接続」が上乗せされる層構造となる。Phase 2 以降で追加されるタスク作成・アクション発火機能は tmux 非依存で動作する。

### ボトムアップで階層を広げる
Phase 1.0 は単一画面（アクティブジョブ一覧）に絞り、以降のフェーズで段階的に階層を追加する。これにより:
- 最速で出荷できる
- 各フェーズで API 層と UI 層を同時に成長させられる
- 階層追加は既存画面の上にモーダル画面を積む形で、スタック式ナビに移行できる

## Phase 1.0 スコープ

### 機能
- アクティブジョブ（`running && interactive`）の横断一覧表示
- tmux 検出時: Enter でジョブを別ペインで open
- tmux 非検出時: Enter でインラインメッセージ（"tmux 内で起動してください"）
- ポーリングによる自動リフレッシュ（2-3 秒間隔）
- 手動リフレッシュ（`r`）
- 終了（`q`）

### スコープ外
- タスク一覧・プロジェクト一覧・ワークスペース一覧（Phase 1.x / 2.x / 3.x）
- タスク作成・アクション発火（Phase 2.x）
- ジョブ詳細ペイン（Phase 1.2）
- 完了ジョブの閲覧（Phase 1.3）
- ステータスフィルタ（Phase 1.1）

## 画面構成

### メイン画面（tmux 環境）

```
┌─ boid ─ active jobs ───────────────────── [tmux] ─┐
│ ▸ ● job-abc123  write tests     [foo]     03:12  │
│   ● job-def456  refactor db     [bar]     01:45  │
│   ● job-xyz789  fix issue       [baz]     00:22  │
│                                                   │
└───────────────────────────────────────────────────┘
 enter: open   r: refresh   q: quit
```

- 各行: カーソル / ステータスドット / job-id（短縮）/ タスクタイトル / プロジェクト名 / 経過時間
- 右上に `[tmux]` バッジで動作モードを告知

### フォールバック画面（tmux 非検出）

```
┌─ boid ─ active jobs ────────────────── [no-tmux] ─┐
│ ▸ ● job-abc123  write tests     [foo]     03:12  │
│   ● job-def456  refactor db     [bar]     01:45  │
│                                                   │
│  ! to open a job, launch `boid tui` inside tmux  │
└───────────────────────────────────────────────────┘
 r: refresh   q: quit
```

- `[no-tmux]` バッジで状態を表示
- Enter を押すとフッタ領域にインラインメッセージを出す（画面遷移・ポップアップなし）
- 一覧取得・状態観測は通常通り動作

## キーバインド

| キー | 動作 |
|------|------|
| `j` / `↓` | カーソル下移動 |
| `k` / `↑` | カーソル上移動 |
| `Enter` | 選択ジョブを tmux 別ペインで open（tmux 環境のみ） |
| `r` | 一覧リフレッシュ |
| `q` / `Ctrl+C` | TUI 終了 |

## tmux 委譲の仕組み

### 起動環境の検出
起動時に `$TMUX` 環境変数を確認する。存在すれば tmux モード、なければフォールバックモード。セッション中は切り替えない（ユーザが途中で tmux に入ることは想定しない）。

### open の発行
ジョブ選択時、以下のコマンドを `exec.Command` で発行する:

```go
cmd := exec.Command("tmux", "split-window", "-h",
    "-P", "-F", "#{pane_id}",
    "boid", "job", "attach", jobID)
paneID, _ := cmd.Output()
```

`-P -F '#{pane_id}'` で新しいペインの ID を取得し、TUI 側で記録する。

### detach（ペインのクローズ）
`boid job attach` から離脱する方法は、ユーザが tmux の通常操作でペインを閉じるだけ:
- `Ctrl+b x`（tmux default kill-pane）
- もしくは attach クライアントを `Ctrl+C` で終了

ペインが閉じると attach クライアントプロセスは SIGHUP を受けて終了し、UNIX ソケット接続が切れる。サーバ側の PTY・ジョブプロセスは影響を受けず継続する。

この挙動は現状の `boid job attach` ですでに成立しており、Phase 1 のために `boid job attach` 側に detach キーを実装する必要はない。

### 重複 open の防止
同じジョブを 2 回選択したとき、TUI は記録済みの pane_id を `tmux list-panes -a -F '#{pane_id}'` で検証し、まだ生きていればその既存ペインにフォーカスを移す（`tmux select-pane -t <pane_id>`）。生きていなければ新規に split-window する。

### レイアウト選択
Phase 1.0 のデフォルトは水平分割（`split-window -h`）。将来のオプション:
- `--popup`: `tmux display-popup -E` でポップアップ起動（単一ジョブ集中用）
- `--new-window`: 別ウインドウで開く

これらは Phase 1.1 以降で追加する。

## 必要な API 追加

### 横断ジョブ一覧エンドポイント
現状の `GET /api/jobs` は `task_id` が必須で、全プロジェクト横断のジョブ取得ができない。DB 層には `internal/dispatcher/store.go` の `ListJobs()` が存在するが、API 層に expose されていない。

Phase 1.0 のために以下を追加する:

```
GET /api/jobs?status=running&interactive=true
```

- `status`: `running` / `pending` / `completed` / `failed` / 未指定=全て
- `interactive`: `true` / `false` / 未指定=両方
- Phase 1.0 ではこの 2 つのフィルタのみサポート（YAGNI）。`project_id` / `workspace_id` / `task_id` フィルタは以降のフェーズで追加する。

### JobWithContext 型
レスポンスにはタスクタイトル・プロジェクト名を埋め込んで返す。TUI が複数 API を叩く必要をなくし、一覧画面をシンプルに保つ。

```go
// internal/api/job_model.go に追加
type JobWithContext struct {
    Job                // 既存 Job 型を embed
    TaskTitle   string `json:"task_title"`
    ProjectName string `json:"project_name"`
}
```

サーバ側で tasks / projects を JOIN し、まとめて返す。

### クライアント側
`internal/client` に以下を追加:

```go
type JobListFilter struct {
    Status      string
    Interactive *bool  // nil = 未指定
}

func (c *Client) ListJobs(filter JobListFilter) ([]api.JobWithContext, error)
```

## パッケージ構成

```
cmd/
└── tui.go              # cobra コマンド登録、bubbletea プログラム起動

internal/tui/
├── app.go              # トップレベル Model（ポーリング制御、tmux 検出）
├── active_jobs.go      # アクティブジョブ一覧コンポーネント
├── tmux.go             # tmux 委譲ヘルパー（split-window, list-panes, select-pane）
└── style.go            # lipgloss スタイル定義
```

Phase 1.0 の範囲は単一画面なので最小構成。Phase 1.1 以降で `task_list.go`、`project_list.go` などを追加していく。

## データフロー

### 起動
```
cmd/tui.go
  → tmux 検出 ($TMUX 環境変数)
  → client.NewUnixClient(DefaultSocketPath())
  → tui.NewApp(client, tmuxEnabled)
  → tea.NewProgram(app, tea.WithAltScreen()).Run()
```

### ポーリング
```
tea.Tick(2s)
  → tickMsg
  → Cmd: client.ListJobs({status: "running", interactive: &true})
  → jobsUpdatedMsg
  → View: 再描画
```

### open フロー（tmux 環境）
```
Enter on active job
  → Cmd: openJobCmd(jobID)
     ├── 記録済み pane_id が生きていれば tmux select-pane -t <id>
     └── いなければ tmux split-window -h -P -F '#{pane_id}' boid job attach <id>
  → 返却された pane_id を Model に記録
  → 次の tickMsg で一覧を再取得（状態反映）
```

### open フロー（tmux 非検出）
```
Enter on active job
  → Update: tmux 非検出のためエラーメッセージを Model にセット
  → View: フッタ領域にインラインメッセージ表示
  → 数秒後にメッセージクリア
```

## 既存コードへの影響

### `boid job attach` / `cmd/attach.go`
変更なし。ペインが閉じられることで自然に終了し、サーバ PTY には影響しない現状の挙動をそのまま活用する。旧 Phase 1 計画で予定していた attach ロジックの抽出リファクタは不要。

### API 層（`internal/api/job.go`）
横断ジョブ一覧エンドポイント `GET /api/jobs`（`task_id` 無し）を追加。既存の `task_id` 必須の挙動は後方互換として残すか、統一するかは実装時に判断。

### DB 層（`internal/dispatcher/store.go`）
既存の `ListJobs()` を活用。必要に応じて `status` / `interactive` による SQL フィルタを追加する。

## 実装ステップ

### Step 1: API 層の拡張
1. `internal/dispatcher/store.go` の `ListJobs()` に status / interactive フィルタを追加
2. `internal/api/job_model.go` に `JobWithContext` 型を追加
3. `internal/api/job.go` の list ハンドラを拡張し、`task_id` 無しでも動作するように変更。tasks / projects を JOIN
4. `internal/client` に `ListJobs(filter)` メソッドを追加
5. ユニットテスト追加

### Step 2: bubbletea スケルトン
1. `go get github.com/charmbracelet/bubbletea github.com/charmbracelet/lipgloss github.com/charmbracelet/bubbles`
2. `cmd/tui.go` に cobra コマンド登録
3. `internal/tui/app.go` に最小の Model（空画面 + `q` で終了）
4. 起動確認

### Step 3: アクティブジョブ一覧
1. `internal/tui/active_jobs.go` を実装
2. `client.ListJobs()` 経由で一覧取得
3. カーソル移動、ステータス色分け、プロジェクト名の表示
4. `tea.Tick` によるポーリング

### Step 4: tmux 委譲
1. `internal/tui/tmux.go` を実装（`$TMUX` 検出、split-window、list-panes、select-pane）
2. Enter でジョブを open
3. pane_id の記録と生存確認
4. 重複 open の防止

### Step 5: フォールバック
1. tmux 非検出時の UI 分岐
2. `[no-tmux]` バッジ、Enter 時のインラインメッセージ
3. メッセージの自動クリア

### Step 6: スタイリングと仕上げ
1. lipgloss でレイアウト調整
2. ウィンドウリサイズ対応（`tea.WindowSizeMsg`）
3. エラー表示（サーバ未起動、API エラー、tmux コマンド失敗）

## 以降のフェーズのロードマップ

Phase 1.0 以降は以下のように段階的に拡張する。各フェーズは独立した変更として進められる。

| フェーズ | 追加要素 | 備考 |
|---|---|---|
| 1.1 | ステータスフィルタ（running / pending / completed / failed） | フィルタバー追加 |
| 1.2 | ジョブ詳細ペイン | 右側または下段に直近出力・メタ情報 |
| 1.3 | 完了ジョブの閲覧 | `Job.Output` またはトランスクリプトを表示 |
| 1.4 | レイアウトオプション（`--popup`、`--new-window`） | tmux 委譲先の選択 |
| 2.0 | タスク一覧階層 | スタック式ナビへ移行、タスク → ジョブの drill-down |
| 2.1 | タスク作成フォーム | `bubbles/textinput`、tmux 非依存 |
| 2.2 | アクション発火 UI | 対話的にアクション送信 |
| 3.0 | プロジェクト一覧階層 | プロジェクト → タスク → ジョブ |
| 3.1 | ワークスペース横断ビュー | 複数プロジェクトの並列観測 |
| 4.0 | `boid` デフォルト起動 | 引数無しで TUI を開くエントリポイント |

## Phase 1 スコープ外（遠い将来）

- **VT エミュレータ組み込み**: tmux 非依存の PTY 埋め込み。現計画では不要。需要が出れば再検討。
- **Web UI**: xterm.js + WebSocket によるモバイル対応。別プロジェクト扱い。
- **SSE/WebSocket**: ポーリングからプッシュ通知へ。パフォーマンス課題が顕在化してから検討。

## 依存ライブラリ

| ライブラリ | 用途 |
|-----------|------|
| `github.com/charmbracelet/bubbletea` | TUI フレームワーク |
| `github.com/charmbracelet/lipgloss` | スタイリング |
| `github.com/charmbracelet/bubbles` | リストなど汎用コンポーネント（必要に応じて） |

VT パーサー（`charmbracelet/x/vt` 等）は本計画では不要。`tmux` コマンドは実行時依存（バイナリ）であり、Go モジュールには含まれない。
