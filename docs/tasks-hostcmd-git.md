# Host Command: git 対応タスク

## 背景

git をホストコマンドとして動作させることで、サンドボックスに .ssh をマウントする必要をなくす。
現状、hostcmd パッケージの各コンポーネント（broker, shim, policy）は実装済みだが、
エンドツーエンドで接続されていない。加えてポリシー評価が甘い（全引数 `*` 許可）。

## タスク一覧

### Phase 1: エンドツーエンド接続

- [x] **1-1. main.go: argv[0] シム分岐**
  - `os.Args[0]` の basename が `boid` 以外なら `hostcmd.ShimExec` を呼ぶ
  - ブローカーソケットパスは環境変数 `BOID_BROKER_SOCKET` から取得
  - cwd を `os.Getwd()` で取得しリクエストに含める
  - **stdin の non-blocking 読み取り**: Claude Code の Bash ツールはコマンドに
    stdin パイプを繋ぐが、データを送らない場合がある。blocking read するとハング必至。
    - `os.Stdin.Stat()` で `ModeNamedPipe` をチェック
    - パイプなし → stdin は空
    - パイプあり → goroutine で読み取り + 短いタイムアウト（例: 100ms）で打ち切り
      ```
      ch := make(chan []byte, 1)
      go func() { data, _ := io.ReadAll(os.Stdin); ch <- data }()
      select {
      case data := <-ch: // stdin データあり
      case <-time.After(100 * time.Millisecond): // データなし、空として続行
      }
      ```
    - これにより「パイプはあるがデータは来ない」ケースでもハングしない
  - stdout/stderr を分離して出力、exit code で終了

- [x] **1-2. broker.go: トークン認証 + プロジェクト単位コマンド登録**
  - `Broker` にトークンレジストリ追加: `map[string]map[string]CommandDef` (token → commands)
  - `Register(token string, commands map[string]CommandDef)` でプロジェクト用コマンドセットを登録
  - `Unregister(token string)` でジョブ/シェル終了時に破棄
  - `ExecRequest` に `Token string` フィールド追加
  - `Handle` でトークン検証 → トークンに紐づくコマンドセットでポリシーチェック
  - トークン不一致 or 未登録 → 拒否

- [x] **1-3. server.go: ブローカー起動**
  - `server.New()` で `hostcmd.Broker` を生成
  - `server.Start()` で `broker.Start(ctx)` 起動
  - `server.Stop()` で `broker.Stop()`
  - ソケットパスは `{RuntimeDir}/boid-broker.sock` 等
  - `BrokerSocket() string` アクセサ追加

- [x] **1-4. wrapper.go: ブローカーソケット・トークンのマウントと環境変数**
  - `WrapperConfig` に `BrokerSocket string` と `BrokerToken string` 追加
  - `generateSetupScript`: ブローカーソケットを `$ROOT/run/boid/broker.sock` に bind mount
  - `generateInnerScript`: `export BOID_BROKER_SOCKET=/run/boid/broker.sock`
    と `export BOID_BROKER_TOKEN={token}`

- [x] **1-5. runner.go: トークン発行・登録・破棄**
  - Runner に `Broker *hostcmd.Broker` フィールド追加
  - `Execute` でジョブ起動時に UUID トークン生成
  - プロジェクトの `HostCommands` から builtin を解決し `broker.Register(token, cmds)`
  - ジョブ完了時（`job done` API）に `broker.Unregister(token)` — 要: 完了ハンドラ連携

- [x] **1-6. shell.go: トークン発行・登録**
  - shell 起動時にサーバー API 経由でトークン発行・登録
  - API エンドポイント追加: `POST /api/broker/register` → token 返却
  - WrapperConfig に BrokerSocket + BrokerToken 設定

- [x] **1-7. shim.go: cwd + トークン送信**
  - `ShimExec` で `os.Getwd()` を `ExecRequest.Cwd` に設定
  - `BOID_BROKER_TOKEN` 環境変数から Token を読み取り `ExecRequest.Token` に設定

### Phase 2: ポリシー強化

- [x] **2-1. policy.go: deny-first 評価 + 引数結合マッチング**
  - `CommandDef` に `DeniedPatterns []string` と `AllowedSubcommands []string` 追加
  - `CheckPolicy` の評価順序を変更:
    1. `AllowedSubcommands` があればサブコマンド抽出 → ホワイトリストチェック
    2. `DeniedPatterns` でマッチすれば拒否（deny-first）
    3. `AllowedPatterns` でマッチすれば許可
    4. デフォルト拒否
  - 引数マッチングを `strings.Join(args, " ")` に変更

- [x] **2-2. policy.go: git サブコマンド抽出**
  - `extractGitSubcommand(args)` 関数追加
  - `-C`, `-c`, `--git-dir` 等のグローバルオプションをスキップ

- [x] **2-3. builtin.go: git 定義の充実**
  - `AllowedSubcommands`: status, log, diff, show, blame, add, commit, push, pull,
    fetch, checkout, switch, branch, merge, rebase, cherry-pick, stash, tag,
    worktree, rev-parse, symbolic-ref, for-each-ref, ls-files, ls-remote,
    shortlog, describe, config, reset, clean, rm, init, mv, restore, bisect, grep
  - `DeniedPatterns`: `push *://*`, `push *@*:*`, `fetch *://*`, `fetch *@*:*`,
    `remote add *`, `remote set-url *`, `remote remove *`, `remote rename *`,
    `config remote.*`, `-c *`, `submodule add *`
  - `Env`: `GIT_CONFIG_NOSYSTEM=1`

### Phase 3: cwd 検証

- [x] **3-1. broker.go: cwd 検証の追加**
  - `CommandDef` に `RequireCwd bool` と `AllowedCwdPrefixes []string` 追加
  - `Handle` メソッドで cwd を検証:
    - `RequireCwd` なら cwd 必須、絶対パス、ディレクトリ存在チェック
    - `AllowedCwdPrefixes` 内に収まるかチェック
  - git の `RequireCwd` を true に設定

### Phase 4: per-command env

- [x] **4-1. broker.go: コマンドごとの環境変数設定**
  - `Handle` メソッドで `def.Env` を `cmd.Env` に設定
  - ホスト環境変数を継承しつつ、コマンド定義の Env で上書き

### Phase 5: シークレット管理 + gh コマンド対応

boid server 自体にシークレットストアを持ち、ホストコマンドの環境変数に注入する。
旧実装の `pass` 連携に相当する機能を boid ネイティブで実現する。

- [x] **5-1. internal/secret/: シークレットストア実装**
  - SQLite テーブル: `secrets (id, key, value_encrypted, created_at, updated_at)`
  - 暗号化方式: ホストユーザーの鍵で AES-GCM（or age）
  - CRUD API: `secret.Store` — `Set(key, value)`, `Get(key)`, `Delete(key)`, `List()`
  - 鍵管理: サーバー起動時にマスターキーをロード（keyring or ファイル）

- [x] **5-2. cmd/secret.go: CLI コマンド**
  - `boid secret set <key>` — stdin or プロンプトから値を読み取り保存
  - `boid secret get <key>` — 復号して表示
  - `boid secret list` — キー一覧
  - `boid secret delete <key>`

- [x] **5-3. API エンドポイント**
  - `GET /api/secrets` — キー一覧（値は返さない）
  - `POST /api/secrets` — 設定
  - `DELETE /api/secrets/{key}` — 削除
  - 内部用: `GET /api/secrets/{key}/value` — ブローカーからの参照用

- [x] **5-4. hostcmd/CommandDef: シークレット参照構文**
  - `Env` の値に `secret:key` プレフィックスでシークレット参照
  - 例: `Env: {"GH_TOKEN": "secret:github/pat"}`
  - ブローカーがコマンド登録時にシークレットを解決し、メモリに保持
  - サンドボックスには一切渡さない（ホスト側ブローカーでのみ注入）

- [x] **5-5. builtin.go: gh コマンド定義**
  - `gh` の builtin 定義を追加
  - `Env: {"GH_TOKEN": "secret:github/pat"}` でPAT注入
  - `AllowedSubcommands` で安全なサブコマンドを許可
  - `DeniedPatterns` で危険操作を拒否

## 不要（設計判断により除外）

- **extra_denied_patterns**: mixin ベースの設計で不要。制約を変えたければ別の mixin を作る
- **コマンド定義マージ**: サンドボックスプロファイル機能がないため不要
