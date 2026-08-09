# ポータブル CLI クライアント (GOOS=windows / darwin)

## 目的

boid の daemon は Linux 専用のままにしつつ、**CLI クライアント経路だけを Windows / macOS でビルドできるようにする**。

直接の動機は Google OAuth。`boid secret oauth login <service>` の loopback フロー (RFC 8252 §7.3) は、CLI を走らせたマシンの `127.0.0.1` にコールバックを受ける (`cmd/secret_oauth.go`)。ブラウザが Windows 側にしかない環境では、Linux ホストで CLI を叩いてもリダイレクトが届かない。Windows 上に CLI があれば素直に解決する。

副次的に、ノート PC から自宅 daemon に `boid attach` する、といった使い方も開く。

## 前提: daemon は移植しない

移植するのは**クライアント半分だけ**。userns / pivot_root / container backend の docker-out-of-docker 配線・PTY・broker は Linux 固有のままで、1 行も触らない。

非 Linux ビルドの boid は「リモート専用 CLI」になる:

- `hostModeEnabled()` が常に false を返す (`cmd/host_other.go`) ので、`PersistentPreRunE` は `profiles.Resolve` 経路に落ちる。これは `boid login <url>` が設定する、Web UI と同じ device auth + TLS の経路。
- ローカル daemon の自動起動は fail-closed で拒否する (`internal/client/autostart_lock_other.go`)。

## 何が壊れていたか

クライアントは Linux 固有 syscall をほぼ必要としないのに、GOOS=windows でビルドできなかった。原因は syscall ではなく**コンパイル時の依存グラフ**だった:

```
internal/client → internal/api → dispatcher, db, sandbox, skills, web/templates → ...
```

`internal/client` が `internal/api` を import していたのは wire DTO (`Job`, `TaskDetailView`, `WorkspaceDetail` ...) を取るためだけ。しかし `internal/api` は daemon の handler パッケージなので、DTO 1 個の代金として daemon 世界が丸ごとクライアントのコンパイル経路に乗っていた。

## 打った手

### PR1: `internal/apiwire` の切り出し

daemon⇔client の wire 契約 (28 型 + `NormalizePublicURL`) を新パッケージへ移設。`internal/apiwire` が import してよいのは**標準ライブラリと `internal/orchestrator` だけ**。

- `internal/api` は `apiwire_aliases.go` で全シンボルを type alias として再輸出する。handler / `internal/server` / `web/templates` の呼び出しは一切変わらない。
- `internal/client` の唯一の非移植コードだった autostart の `flock(2)` を `lockAutostart` として GOOS 別に分離。

### PR2: `cmd` の隔離と main の分割

- Linux 専用の cmd ファイルに `//go:build linux` を付ける。Windows ビルドから消えるのは `start` / `stop` / `check` / `gc reap` / `fetch` / `install-skills` / `project migrate` / `runner-container` / `workspace import-home` — すべて scope=local の daemon ライフサイクル機構。
- `cmd` の DTO 参照を `apiwire` へ付け替え。
- `main.go` の shim 経路 (`sandbox.RunBoidShim` / host-command shim) を Linux 専用にし、非 Linux は `cmd.Execute()` のみの `main_other.go` を使う。
- `internal/profiles` の config ロックを GOOS 別に分離。**Windows 版は `LockFileEx` の実ロック**であって no-op ではない (`boid login` / `logout` は Windows でも同じ config.yaml を read-modify-write する)。
- `attach` のウィンドウリサイズ追従を GOOS 別に分離。Unix は SIGWINCH、Windows は 250ms ポーリングで実際に寸法が変わったときだけ RPC を送る。

## 退行させないための 4 つのゲート

移植性は「一度通した」では保てない。壊し方が `import` 1 行と軽く、しかも Linux 上の `go build ./...` では一切検知できないため、ゲートを 4 つ置いた。

| ゲート | 場所 | 捕まえるもの |
|---|---|---|
| `TestApiwireDependencies` | `internal/apiwire/deps_test.go` | apiwire 自身が余計な内部パッケージを掴む |
| `internal/client -> internal/api` の hard ban | `scripts/check-internal-architecture.sh` | client が DTO 目当てに api へ戻る |
| `TestNoAccidentallyLinuxOnlyRemoteCommands` | `cmd/portable_build_test.go` | scope=remote コマンドが黙って Linux 専用になる |
| クロスビルド | CI `unit` ジョブ | 上 3 つをすり抜けた実コンパイルの破綻 |

3 つ目には**明示的な許可リスト**がある。現在の例外は 1 件だけ:

- `workspace import-home` — scope=remote だが、CLI 側がローカルディレクトリを tar に流す処理で `internal/dispatcher` のヘルパ (`WorkspaceHomesDir` / `ResolveWorkspaceHomeSource` / `WriteWorkspaceHomeTar`) に依存している。移植するにはそれらを dispatcher から持ち上げる必要があり、この分割より大きい変更になる。daemon のホスト上で走らせる一度きりの移行コマンドなので、当面は例外のまま。

## 検証状況

- Linux: `go build ./...` / `go vet ./...` / `go test ./...` / `go test -race` (影響パッケージ) すべて green。
- クロスビルド: `GOOS=windows GOARCH=amd64` と `GOOS=darwin GOARCH=arm64` で boid バイナリのビルドが通ることを確認。
- **未検証**: 実際の Windows 機での実行。`LockFileEx` によるロックと attach のリサイズポーリングは、クロスコンパイルが通ることしか確認できていない。初回の Windows dogfood で確かめること。

## 残件

- Windows 実機での dogfood (上記「未検証」)。
- リリースワークフローへの windows/darwin バイナリの追加は未着手 — 今は「ビルドが通る」ことの担保まで。配布物に含めるかは別判断。
