# オンボーディング

boid の初回セットアップは **3 段** です。

## 3 つのコマンド

| 順序 | コマンド | 役割 |
|---|---|---|
| 1 | `boid kit init` | このマシンで使える kit カタログを生成（daemon 不要） |
| 2a | `boid project init [dir]` | 新規プロジェクト雛形作成 + daemon 登録 |
| 2b | `boid project add <dir>` | 既存プロジェクトを daemon 登録 |
| 3 | `boid workspace configure <slug>` | workspace 設定（有効化する kit を選択） |

## シナリオ別

### 新規マシン + 新規 project (3 段全部)

```bash
boid kit init                          # マシンの kit カタログ（+ default_harness 対話設定）
boid project init ~/src/myproject --workspace dev
boid workspace configure dev
```

### 新規マシン + 既存 project (3 段全部)

```bash
boid kit init
boid project add ~/src/myproject --workspace dev
boid workspace configure dev
```

### 既存マシン + 新規 project (2 段)

```bash
boid project init ~/src/newproject --workspace dev
boid workspace configure dev          # 既存 workspace なら不要なことも
```

### 既存マシン + 既存 project (2 段)

```bash
boid project add ~/src/myproject --workspace dev
boid workspace configure dev
```

### 既存 workspace に project 追加だけ (1 段)

```bash
boid project add ~/src/another --workspace dev
# kit / env 構成は既存 workspace のまま
```

## boid kit init の詳細

`boid kit init` は **daemon 未起動でも動作**します（初手オンボーディングを想定した設計）。

### default_harness の設定

初回実行時、`~/.config/boid/config.yaml` に `default_harness` が未設定の場合、冒頭で対話プロンプトが表示されます:

```
Which harness do you want to use by default? [claude/codex/opencode]: claude
```

入力した値は `~/.config/boid/config.yaml` に永続化されます。次回以降は聞かれません。

環境変数 `BOID_DEFAULT_HARNESS` でも override できます:

```bash
BOID_DEFAULT_HARNESS=codex boid kit init
```

`default_harness` の設定は [`config.yaml` リファレンス](../reference/config-yaml.md) も参照してください。

### 環境スキャンと kit 生成

harness セッションが起動し、このマシンにインストールされているツールを自動検出して `~/.local/share/boid/kits/` 配下に kit を生成します:

```
[scanning host environment...]
[detected: volta (~/.volta), gh, docker, go 1.24]
[entering interactive harness session for kit generation]
... (harness 対話) ...
[generated: node, github-cli, docker, go-dev]
[secret scan: clean]
```

生成される kit の例:

| ツール | kit 名 | 検出条件 |
|---|---|---|
| Node.js (volta) | `node` | `which volta` 成功 |
| Node.js (system) | `node` | `which node` 成功 |
| Go | `go-dev` | `which go` 成功 |
| GitHub CLI | `github-cli` | `which gh` 成功 |
| Docker | `docker` | `which docker` かつ socket 存在 |

### secret scan

kit 生成後、書き込まれた `kit.yaml` 群に対して自動的に secret scan が走ります。高エントロピー文字列や認証トークンのパターンが検出された場合は全体を rollback して終了します。

kit.yaml の `host_commands[*].env` には生の secret 値を書かないでください。secret 参照は `secret:<key>` 形式を使います。

## boid workspace configure の詳細

`boid workspace configure <slug>` は daemon が必要です（未起動の場合は自動起動します）。

**前提条件**: `default_harness` が設定済みである必要があります。未設定の場合は次のメッセージが表示されて停止します:

```
default_harness が設定されていません。先に boid kit init を実行してください。
```

### 動作

harness セッションが起動し、workspace に紐付けられた project 群をスキャンして必要な kit を `~/.config/boid/workspaces/<slug>.yaml` に書き込みます:

```
[scanning workspace 'dev' projects: boid, boid-kits]
[entering interactive harness session for workspace configuration]
... (harness 対話) ...
[written: ~/.config/boid/workspaces/dev.yaml]
[secret scan: clean]
[reloaded projects]
```

project が必要とするツールに対応する kit がカタログに存在しない場合は、案内メッセージが表示されます:

```
project が gh を要求していますが github-cli kit がカタログに見つかりません。
boid kit init を再実行してください。
```

workspace.yaml への書き込み前に既存ファイルはバックアップされます。エラー発生時は自動的に rollback されます。

## 各レイヤの概念

- **project**: 作業パターン（portable、git commit 対象）。`.boid/project.yaml` に記述する。
- **workspace**: 環境マッチング（machine-local）。どの kit / env を使うかを選ぶ `workspace.yaml`。
- **kit**: ツール供給（グローバル共有）。`host_commands` / `env` / `additional_bindings` を提供する。

## 旧 `boid init` からの移行

旧 `boid init` は廃止されました。上記 3 段を使ってください。

旧スキーマの `project.yaml` (`kits` / `env` / `host_commands` / `capabilities` 等を含む) を持つ場合は
`boid project migrate <dir>` で自動変換できます。詳細は `docs/ja/guide/migration.md` を参照してください。
