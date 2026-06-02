# Web ターミナル: サーバ側 vt エミュレータによる描画崩壊の根治

> Status: Phase 1 (snapshot グリッド化, boid#514) + ライブ幅修正 (SIGWINCH 転送, boid-kits#38)
> 実装完了 (2026-06-02)。単一クライアントは綺麗になる。Phase 2 (履歴リフロー / per-client 幅) は未着手。
> 調査・PoC 完了日: 2026-06-02
>
> ## 実装メモ (Phase 1)
>
> - `internal/dispatcher/runtime_local_linux.go`: `localRuntimeSession` に `cols/rows`
>   を保持 (`Start` で 80x24、`Resize` で更新)。`subscribe()` を改修し、**interactive
>   セッションのみ** `renderTerminalSnapshot()` でグリッド再構成。raw コピーとチャンネル
>   登録はロック下、vt レンダリングはロック外。
> - **非インタラクティブはレンダリングしない**: `subscribe()` はコマンドジョブのログ SSE
>   (`internal/api/job_log_sse.go`) も共有しており、複数行ログを 80x24 グリッドに潰すと
>   壊れる。`s.interactive` で分岐。
> - **drain のデッドロック対策 + data race 回避**: `x/vt` の `Emulator` は `Read`/`Close`
>   が内部 `closed` フラグで data race する (`SafeEmulator` も `Read` 非ロックなので不可)。
>   `emu.Close()` で drain を止めず、`emu.InputPipe().(*io.PipeWriter)` の write 端のみ
>   Close → reader に EOF → `<-drained` 後に `emu.Close()` する (並行 reader 不在で race-free)。
> - **CRLF 変換が必須**: `Buffer.Render()` は行区切りが裸の LF。raw モードの xterm は LF を
>   行送りのみと解釈し階段状に崩れるため、`\n`→`\r\n` 変換してから返す (PoC は cooked TTY の
>   ONLCR で隠れていた)。
> - クライアント `web/static/boid-terminal.js`: `ws.onopen` で `term.reset()`。サーバ最初の
>   output メッセージ (= グリッドダンプ) を綺麗な画面に描く。
> - テスト: `runtime_local_linux_test.go` に interactive=グリッド解決 / non-interactive=生
>   の 2 本を追加。`go test -race ./internal/dispatcher/ ./internal/api/` グリーン。

## Phase 1 後の追加調査: 真因はライブ出力の幅 (SIGWINCH 不達)

> 調査日: 2026-06-02。Phase 1 マージ後もモバイルで崩れが残った件の根本原因と修正。

Phase 1 (初期スナップショットのグリッド化) を入れてもモバイルで崩れが残った。一次証拠で
切り分けた結果、**崩れていたのはスナップショットでなくその後のライブ出力**だった。

### 一次証拠

- 実 transcript (`runtimes/<rid>/transcript.log`) の罫線 `─` は**末尾フレームも含め全部 80 桁**
  = PTY は終始 80 桁。`renderTerminalSnapshot(raw, 80, 24)` は**完全に綺麗**、`(raw, 44, 24)` は
  崩壊 (録画幅でないと崩れる、というプラン記載どおり)。→ スナップショットは正しく綺麗を返している。
- ライブ delta は生の 80 桁バイト (相対カーソル移動が大量)。これを狭い (≒44) モバイル xterm に
  流すと崩壊する。xterm は**ライブのカーソル移動出力を再折り返しできない** (再折り返しは自身の
  resize 時のみ)。
- クライアントは接続時に resize を送るが PTY は 80 のまま。`ps -o pid,pgid,sid,tty` で確認すると、
  サンドボックス内 `claude` は **`SID ≠ PTY セッション`・`tty=?` (制御端末なし)**。fd 0/1/2 は PTY
  slave を指す。

### 根本原因

サンドボックスのプロセス構造は `outer.sh(bash) → pasta → inner bash → run-agent.py → claude` で、
`run-agent.py` は `subprocess.Popen(args, start_new_session=True)` で **claude を独自セッションに**
起動している (agent-stop の `SIGUSR1` を claude に当てないための意図的な設計)。その副作用として、
PTY リサイズ時にカーネルが**前景プロセスグループ** (outer bash / pasta / run-agent.py) に送る
**`SIGWINCH` を claude が受け取れない**。よって claude は起動時の幅 (既定 80 桁) のまま描画し続け、
ライブ出力が常に 80 桁になる。Phase 1 の vt リサイズ機構自体は正常 (直接の PTY 子プロセスは
リサイズを受け取ることをテストで確認済み)。

### 修正

`boid-kits` の `claude-code/hooks/run-agent.py` に **`SIGWINCH` ハンドラを追加し、受信したら子
claude に転送**する (run-agent.py は前景 pgrp にいるのでカーネル `SIGWINCH` を受け取れる)。claude は
更新済みの PTY winsize を読み直し、クライアント幅で描き直す。`SIGUSR1` と同じ `proc_holder` パターン・
防御的 unblock を踏襲。(PR: novshi-tech/boid-kits#38)

検証: `PTY + 前景pgrp forwarder + start_new_session=True の子` のトポロジを再現し、転送なし=子は
`SIGWINCH` を一切受けない / 転送あり=子が新サイズ (`24 44`) を取得、を確認。

### 教訓・残課題

- Web 端末の崩れは「**初期描画 (snapshot)**」と「**ライブ (PTY 実幅 vs client 幅)**」を分けて診る。
  PTY 実幅は transcript の罫線幅で判る。
- Phase 1 (boid 側 snapshot グリッド化) と本修正 (kit 側 SIGWINCH 転送) は**相補的**。両方で単一
  クライアントが完全に綺麗になる。
- **複数クライアント異幅 / read-only 観測**は依然未解決 (PTY = 1 幅、後勝ち)。完全な per-client
  幅は Phase 2 の常駐エミュレータ + グリッド配信が必要。

## 背景

Web UI (xterm.js) で長時間動いている Claude Code セッションの端末を**モバイル幅で開くと
画面が盛大に崩れる**。入力枠が何重にも積み重なり、区切り線とプロンプトが散乱して読めない。

### 根本原因 (一次証拠で確定)

現状のパイプラインは「**PTY 生バイトを溜め込み → 接続時に全リプレイ**」:

- `internal/dispatcher/runtime_local_linux.go` の `readLoop()` が PTY から 4KB ずつ読み、
  `appendTranscript()` でメモリ (`transcript bytes.Buffer`) と `transcript.log` に追記しつつ
  全購読者に配信する。
- `subscribe()` は接続時に `s.transcript.Bytes()` (= 全履歴) をスナップショットとして返す。
- `internal/api/ws_attach.go` がそれを base64 で送り、ブラウザの `web/static/boid-terminal.js`
  が `term.write()` で xterm.js に流し込む。

問題は、**Claude Code の出力が「幅依存の相対カーソル移動の塊」である**こと。実際の
transcript (5.5MB) を解析すると:

- 録画幅 = **80 桁** (80 文字ちょうどの `─` 区切り線が 2884 本)
- `ESC[<n>A` (カーソル上移動) = **98574 回** (すべて 80 桁前提の行数計算)
- `ESC[2J` (画面クリア) = **0 回** / 代替画面 (`?1049`) = **未使用**

これを**幅 44 のモバイル xterm に頭から再生**すると、折り返し位置が全部ズレて
98574 個の相対移動が累積崩壊する。クリアが一度も無いので**永遠に直らない**。

### A/B で確定した事実 (実機 = playwright モバイル幅で再現)

| 表示幅 | 結果 |
|---|---|
| 44 桁 (モバイル) | 盛大に崩壊 (入力枠が 4 重、区切り線散乱) |
| 80 桁 (= 録画幅と一致) | **完全に綺麗** (全部読める) |

- これは**リサイズのバグではない**。`boid-terminal.js:171` の `term.reset()` (リサイズ時の
  TUI 残骸クリア) では直らない。最初のスナップショット描画ですでに壊れている。
- **モバイルで特にひどい**理由: PTY はデフォルト 80x24 (`runtime_local_linux.go:74-85`) で
  録画される → モバイルは約 44 で見る → ミスマッチ最大。デスクトップ (≒80) は偶然一致して
  綺麗に見えるため、これまで見逃されていた。

関連: [[project-web-terminal-vt-emulator]]

## 調査結論: tmux ではなく Go サーバ側 vt エミュレータ

「サーバ側が画面状態 (セルグリッド) を持ち、接続時に**今の画面を吐く**」のが正攻法。
全履歴の垂れ流しをやめれば崩壊は構造的に消える。実装方式は 2 案を比較した。

### なぜ tmux ではないか

- tmux 方式 (attach-PTY) も崩壊は直るが、代償が大きい:
  - **tmux サーバはホスト側必須** → サンドボックス (Exec) との PTY 所有権が二段になり、
    `generateOuterScript()` (sandbox 起動スクリプト) と `Start()` の起動経路を作り直す必要。
  - **スクロールバックが死ぬ** (tmux は alt 画面で動くため xterm ネイティブのスクロールが
    効かない。copy-mode 委譲か `capture-pane` 注入を別途実装)。
  - tmux バイナリへのハード依存、セッションの GC・命名・永続化の面倒。
- 唯一の勝ち筋は「daemon 再起動をまたぐ永続性」だが、それはグリッドを serialize すれば追える。

### なぜ go-vt で足りるか (VS Code 方式)

- **VS Code がまさにこれをやっている**: pty host に headless xterm を常駐させ全出力を食わせ、
  再接続時は SerializeAddon で「現在グリッド」を 1 発ダンプ (全バイトリプレイではない)。
- **リフロー自体はクライアント xterm.js が行う** (コアの `BufferReflow`)。崩れていたのは
  「テキストが折り返せないから」ではなく「相対カーソル移動を違う幅で再生したから」。
  クリーンなグリッドダンプ (テキスト + 色) を送れば、xterm 側が現在幅に再折り返しできる。
  → **エミュレータ側で難しいリフローを書かなくてよい。**
- ライブラリ `github.com/charmbracelet/x/vt` が現役・pure Go・MIT で、必要な API が揃う:
  - `vt.NewEmulator(w, h)` / `(*Emulator).Write([]byte)` / `.Render() string` (SGR 付き ANSI
    ダンプ) / `.String()` (プレーン) / `.Resize(w, h)` / `.SetScrollbackSize(n)`
  - 全角 (CJK, 日本語=2セル)・絵文字・結合文字の幅計算が `runewidth`/`uniseg` で正しい。

## PoC 結果 (`/tmp/vtpoc/` に実物。コミット対象外)

本物の 5.5MB transcript (録画 80 桁, カーソル上移動 98574 個, クリア 0) を `x/vt` に食わせた:

1. **核心 OK**: `Render()` で**完全に読める 1 画面**に解決された (98574 個の相対移動が正しく
   collapse)。`live-02` (綺麗な 80 桁表示) と同じ画面を **Go だけで** 再構成。日本語も正しい。
2. **性能 OK**: フル 5.5MB を **0.16 秒** (線形。1MB=0.03s)。全履歴一括リプレイすら瞬時。
   ライブの逐次投入は無問題。
3. **設計の肝が確定**: 同じ 80 桁バイトを **44 桁エミュレータに食わせると崩れる** (文字混線)。
   → **エミュレータは「録画幅 (=PTY 実幅)」で動かす**こと。客の幅で生バイト再生したら同じ崩壊。
   モバイルで綺麗に見せるのは「80 グリッドをダンプ → クライアント xterm が 44 に再折り返し」
   または「PTY を 44 にリサイズ → Claude が描き直す」(後者はライブ実証済み)。

### 実装で踏んだ落とし穴 (重要)

- エミュレータは DA1 (`ESC[c`) や XTVERSION (`ESC[>0q`) 等の**問い合わせに応答を生成し内部出力に
  書く**。これを drain しないと `Write()` が応答書き込みでデッドロックする (PoC で 5KB が
  ハングして O(n²) と誤認した原因はこれ)。**応答は破棄する** (`go io.Copy(io.Discard, emu)`)。
  boid は観測役であり、Claude との DA1 やりとりは本物 PTY 側で既に完了しているため破棄が正しい。

## 設計

### 方針: 接続時にオンデマンドでグリッドを再構成してダンプ (Phase 1 推奨)

PoC が実証したのはまさにこの形 (transcript を読む → emu に食わせる → `Render()`)。
**常駐エミュレータを持たず、`subscribe()` 時に都度組み立てる**ことで、ライブ配信路 (既存の
購読者ブロードキャスト) と並行する長寿命エミュレータの concurrency を避けられる。0.16 秒/接続
の CPU コストは接続が稀なので許容。

変更は基本 `internal/dispatcher/runtime_local_linux.go` に閉じる:

1. `localRuntimeSession` に**現在の PTY サイズ** `cols, rows int` を保持 (`Start()` で 80x24、
   `Resize()` で更新)。これが「録画幅」= エミュレータを組む幅になる。
2. `subscribe()` のスナップショット生成を差し替え:
   - 現行: `snapshot := append([]byte(nil), s.transcript.Bytes()...)`
   - 新規: ロック下で `transcript.Bytes()` のコピーと現在サイズを取得 → **ロック外で**
     `emu := vt.NewEmulator(cols, rows)`、`go io.Copy(io.Discard, emu)`、`emu.Write(copy)`、
     `snapshot := []byte(emu.Render())`、`emu.Close()`。返すのはこのクリーンなダンプ。
   - チャンネル登録 (ライブ delta) はロック下で従来どおり atomic に行う。delta はスナップショット
     確定点以降の生バイトで、クライアントはダンプを描いた後にこれを適用する。
3. ライブ delta 配信 (`appendTranscript()` の subscriber ブロードキャスト) と `transcript.log`
   書き込みは**変更しない**。既存接続クライアントは既に同期済みで、生 delta で十分。

クライアント側 (`web/static/boid-terminal.js`):
- 初期スナップショットを書く前に `term.reset()` してクリーンな画面に置く (現状はそのまま
  書き込んでいる)。ダンプは全画面ペイントなので、まっさらな端末に書くのが前提。

### 代替案: 常駐エミュレータ (VS Code 完全準拠、Phase 2 で検討可)

`localRuntimeSession` に `emu vt.Terminal` を持ち、`appendTranscript()` で毎チャンク `emu.Write`、
`Resize()` で `emu.Resize`、`subscribe()` で `emu.Render()`。幅変更履歴をより正しく追えるが、
ブロードキャストと並行する Write/Read の concurrency 対策 (`vt.SafeEmulator` または専用ロック) が
要る。オンデマンド案で崩壊が直るなら急がない。

## 改修箇所 (Phase 1)

| ファイル | 箇所 | 変更 |
|---|---|---|
| `internal/dispatcher/runtime_local_linux.go` | struct `localRuntimeSession` :31-49 | `cols, rows int` フィールド追加 |
| 〃 | `Start()` :51 付近 (setPTYSize 80x24 :74-85) | 初期 `cols=80, rows=24` を session に記録 |
| 〃 | `Resize()` :247-258 | `setPTYSize` 後に `session.cols/rows` を更新 |
| 〃 | `subscribe()` :416-430 | スナップショットを `transcript.Bytes()` から `vt` ダンプに差し替え |
| `go.mod` | — | `github.com/charmbracelet/x/vt` 追加 |
| `web/static/boid-terminal.js` | `ws.onmessage` :116-128 / reset :171 | 初期スナップショット適用前に `term.reset()` |

**変更しない**: `appendTranscript()` :398-414 (ライブ配信 + log 書き込み)、`transcript.log` の
永続化 (静的 `/log` (`internal/api/job.go:127`) と `boid job log` が読むため残す)、
`ws_attach.go` の送受信ロジック、セキュリティモデル (すべてホスト daemon 内の話で Gate 領域)。

## 段階

- **Phase 1 (崩壊の根治)**: 上記オンデマンド案。これだけで「全履歴リプレイ崩壊」が消える。
- **Phase 2 (磨き込み・任意)**:
  - 履歴のリフロー (縮小時に右端が切れる点)。`x/ansi` の `Hardwrap` や darktile の
    `resize.go` を参考に。崩壊に比べれば些細なので後回し可。
  - 静的 `/log` のブラウザ表示も最終グリッドを serialize して幅非依存にする。
  - 常駐エミュレータ化 + グリッド serialize で daemon 再起動またぎの永続化。

## 未解決・決定事項

- **複数クライアント異幅**: PTY = 1 本 = 1 幅という制約は残る (後勝ちサイズ)。崩壊はしなくなる
  (ダンプが内部整合 + xterm 再折り返し)。完全な per-client 幅は将来課題。
- **concurrency**: オンデマンド案なら drain ゴルーチンは subscribe 内で短命に閉じる。常駐案に
  する場合は `vt.SafeEmulator` を使う (調査で plain `Emulator` の Write/Read data-race 履歴の
  指摘あり。採用時に `safe_emulator.go` の状態を確認)。
- **`x/vt` のバージョン**: 実験的パッケージ群 (`charmbracelet/x`)。API 破壊的変更の可能性を念頭に。
- **応答破棄の確認**: drain で捨てた DA1/DSR 応答が Claude 側に届かないことで不都合がないか
  (本物 PTY 側で応答済みのはずだが、実装後に実セッションで要確認)。

## 参考

- ライブラリ: https://github.com/charmbracelet/x/tree/main/vt
- VS Code 方式 (`XtermSerializer`): https://github.com/microsoft/vscode/blob/main/src/vs/platform/terminal/node/ptyService.ts
- xterm.js SerializeAddon (同サイズ前提の注意) / BufferReflow (クライアント側リフロー):
  https://github.com/xtermjs/xterm.js/tree/master/addons/addon-serialize
- リフロー参考実装: https://github.com/liamg/darktile/blob/main/internal/app/darktile/termutil/resize.go
- PoC: `/tmp/vtpoc/main.go` (transcript → `vt.NewEmulator` → `Render()`、drain 込み)
