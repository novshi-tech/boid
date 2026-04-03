# TUI Phase 1 設計・実装計画

## 背景と課題

### tmux 依存の排除後の可視性ギャップ
直近のリファクタリングで tmux 依存を排除し、ジョブに PTY を直接割り当てる方式に移行した。
これにより実行基盤はシンプルになったが、複数ジョブの同時可視化手段が失われた。

### 現状の attach の利便性
`boid attach <job-id>` でジョブに接続できるが：
- ジョブ ID を事前に調べる必要がある（`boid job list --task <id>`）
- 複数ジョブ間の切り替えが煩雑（detach → list → attach の繰り返し）
- タスクの状態遷移が見えない

### TUI の位置づけ
CLI と Web UI の中間層として、ターミナル内で完結するインタラクティブ UI を提供する。
将来的な Web UI（モバイル対応）は別フェーズとして残す。

## 設計方針

- bubbletea + lipgloss で TUI を構築する
- Phase 1 では VT エミュレータを組み込まない
- attach 時は `tea.Exec` で bubbletea を一時停止し、raw terminal に切り替える
- 既存の `internal/client` パッケージをそのまま利用する
- タスク/ジョブ一覧の更新は `tea.Tick` によるポーリング（2-3 秒間隔）

## 画面構成

### メイン画面

```
┌─ Tasks ──────────────────────────────────────────────┐
│ ▸ [executing] タスクA のタイトル          2 jobs      │
│   [done]      タスクB のタイトル          1 job       │
│   [pending]   タスクC のタイトル          0 jobs      │
└──────────────────────────────────────────────────────┘
┌─ Jobs (タスクA) ─────────────────────────────────────┐
│ ▸ [running]   job-abc123  agent       interactive     │
│   [completed] job-def456  hook        exit:0          │
└──────────────────────────────────────────────────────┘
```

上段にタスク一覧、下段に選択中タスクのジョブ一覧を表示する。
フォーカスは上段・下段のどちらかにあり、キー操作でフォーカスを移動する。

### attach 画面

ジョブ選択 → Enter で bubbletea を一時停止し、既存の attach フローに入る。
Ctrl+] で detach すると bubbletea に復帰し、メイン画面に戻る。

## キーバインド

| キー | コンテキスト | 動作 |
|------|-------------|------|
| `j` / `↓` | リスト | カーソル下移動 |
| `k` / `↑` | リスト | カーソル上移動 |
| `Tab` | メイン画面 | タスク一覧 ↔ ジョブ一覧のフォーカス切替 |
| `Enter` | ジョブ一覧 | 選択ジョブに attach（interactive のみ） |
| `Enter` | タスク一覧 | ジョブ一覧にフォーカス移動 |
| `Esc` | ジョブ一覧 | タスク一覧にフォーカス戻す |
| `r` | どこでも | リスト再取得 |
| `q` | メイン画面 | TUI 終了 |
| `Ctrl+]` | attach 中 | detach → メイン画面に復帰 |

## パッケージ構成

```
cmd/
└── tui.go              # cobra コマンド登録、bubbletea プログラム起動

internal/tui/
├── app.go              # トップレベル Model（フォーカス管理、ティック制御）
├── task_list.go        # タスク一覧コンポーネント
├── job_list.go         # ジョブ一覧コンポーネント
├── attach.go           # tea.ExecProcess ラッパー（attach + detach）
└── style.go            # lipgloss スタイル定義
```

## データフロー

### 起動時
```
cmd/tui.go
  → client.NewUnixClient(DefaultSocketPath())
  → tui.NewApp(client)
  → tea.NewProgram(app, tea.WithAltScreen()).Run()
```

### ポーリング
```
tea.Tick(3s)
  → Msg: tickMsg
  → Update: GET /api/tasks → tasksUpdatedMsg
  → Update: GET /api/tasks/{selected}/detail → detailUpdatedMsg
  → View: 再描画
```

### attach フロー
```
Enter on interactive job
  → tea.Exec(attachCmd{client, jobID})
     ├── makeRawInput(os.Stdin)
     ├── sendResize()
     ├── SIGWINCH goroutine
     ├── client.AttachJob(jobID, detachReader{stdin}, stdout)
     └── detach (Ctrl+]) or process exit
  → tea.Program resumes
  → 自動リフレッシュで最新状態を反映
```

## コンポーネント設計

### app.go — トップレベル Model

```go
type focus int
const (
    focusTasks focus = iota
    focusJobs
)

type App struct {
    client   *client.Client
    focus    focus
    tasks    taskList      // タスク一覧サブモデル
    jobs     jobList       // ジョブ一覧サブモデル
    width    int
    height   int
}
```

- `Init()`: 初回データ取得コマンドを返す
- `Update()`: フォーカスに応じてキーイベントを子コンポーネントに委譲
- `View()`: 上下分割で tasks.View() + jobs.View() を結合

### task_list.go — タスク一覧

```go
type taskList struct {
    tasks    []api.TaskSummary   // or orchestrator.Task
    cursor   int
    selected string              // 選択中タスク ID
}
```

- タスクのステータスに応じた色分け表示
- 選択変更時にジョブ一覧の再取得を発火

### job_list.go — ジョブ一覧

```go
type jobList struct {
    jobs     []api.Job
    cursor   int
    taskID   string             // 現在表示中のタスク ID
}
```

- interactive ジョブは強調表示
- running ジョブには attach 可能マーカー

### attach.go — tea.ExecProcess ラッパー

```go
type attachCmd struct {
    client *client.Client
    jobID  string
}

func (a attachCmd) Run() error {
    // cmd/attach.go の RunE と同等のロジック
    // makeRawInput, sendResize, SIGWINCH, detachReader
}
```

`tea.Exec` に渡す `tea.ExecCommand` インターフェースを実装。
既存の `cmd/attach.go` からロジックを `internal/tui/attach.go` に抽出し、
`cmd/attach.go` はそれを呼ぶ薄いラッパーにリファクタする。

### style.go — スタイル定義

```go
var (
    titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(...)
    selectedStyle = lipgloss.NewStyle().Background(...)
    statusStyles  = map[string]lipgloss.Style{
        "running":   ...,
        "completed": ...,
        "failed":    ...,
        "pending":   ...,
    }
)
```

## 依存ライブラリ

| ライブラリ | 用途 |
|-----------|------|
| `github.com/charmbracelet/bubbletea` | TUI フレームワーク |
| `github.com/charmbracelet/lipgloss` | スタイリング |
| `github.com/charmbracelet/bubbles` | リストなど汎用コンポーネント（必要に応じて） |

VT パーサー（`charmbracelet/x/vt` 等）は Phase 1 では不要。

## 既存コードへの影響

### リファクタ対象: cmd/attach.go

attach のコアロジック（raw mode, resize, SIGWINCH, detachReader）を
`internal/tui/attach.go` または共有パッケージに抽出する。

```
Before:
  cmd/attach.go → client.AttachJob()

After:
  internal/attach/attach.go   # コアロジック（raw mode, resize, detach）
  cmd/attach.go                # CLI エントリポイント（薄いラッパー）
  internal/tui/attach.go       # tea.ExecProcess 実装（コアロジックを呼ぶ）
```

### 新規 API エンドポイント
Phase 1 では不要。既存の REST API で十分。

## 実装ステップ

### Step 1: attach ロジックの抽出
- `cmd/attach.go` から raw mode, resize, SIGWINCH, detachReader を共有パッケージに移動
- `cmd/attach.go` は共有パッケージを呼ぶだけにする
- 既存テスト（あれば）が通ることを確認

### Step 2: bubbletea 依存の追加とスケルトン
- `go get` で bubbletea, lipgloss を追加
- `cmd/tui.go` にコマンド登録
- `internal/tui/app.go` に最小の Model（空画面 + q で終了）
- 起動確認

### Step 3: タスク一覧の実装
- `internal/tui/task_list.go` を実装
- client 経由で `GET /api/tasks` を呼び、一覧表示
- カーソル移動、ステータス色分け
- `tea.Tick` によるポーリング

### Step 4: ジョブ一覧の実装
- `internal/tui/job_list.go` を実装
- タスク選択時に `GET /api/tasks/{id}/detail` でジョブ取得
- Tab でフォーカス切替
- interactive マーカー表示

### Step 5: attach 統合
- `internal/tui/attach.go` を実装
- `tea.Exec` でジョブに attach
- Ctrl+] で bubbletea に復帰
- 復帰時に自動リフレッシュ

### Step 6: スタイリングと仕上げ
- lipgloss でレイアウト調整
- ウィンドウリサイズ対応（`tea.WindowSizeMsg`）
- エラー表示（サーバー未起動、ジョブ消失など）

## 将来の拡張（Phase 1 スコープ外）

- **Phase 2**: アクション発火 UI、タスク作成、ステータス遷移のリアルタイム更新
- **Phase 3**: VT エミュレータ組み込みによるマルチペイン表示（`charmbracelet/x/vt`）
- **Web UI**: xterm.js + WebSocket による端末再現（モバイル対応）
- **SSE/WebSocket**: ポーリングからプッシュ通知への移行
