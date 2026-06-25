# オンボーディング

boid の初回セットアップは **3 段** です。

## 3 つのコマンド

| 順序 | コマンド | 役割 |
|---|---|---|
| 1 | `boid kit init` | このマシンで使える kit カタログを生成 |
| 2a | `boid project init [dir]` | 新規プロジェクト雛形作成 + daemon 登録 |
| 2b | `boid project add <dir>` | 既存プロジェクトを daemon 登録 |
| 3 | `boid workspace configure <slug>` | workspace 設定 (有効化する kit を選択) |

## シナリオ別

### 新規マシン + 新規 project (3 段全部)

```bash
boid kit init                          # マシンの kit カタログ
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

## 各レイヤの概念

- **project**: 作業パターン (portable, git commit 対象)。`.boid/project.yaml` に記述する。
- **workspace**: 環境マッチング (machine-local)。どの kit / env を使うかを選ぶ `workspace.yaml`。
- **kit**: ツール供給 (グローバル共有)。`host_commands` / `env` / `additional_bindings` を提供する。

## 旧 `boid init` からの移行

旧 `boid init` は廃止されました。上記 3 段を使ってください。

旧スキーマの `project.yaml` (`kits` / `env` / `host_commands` / `capabilities` 等を含む) を持つ場合は
`boid project migrate <dir>` で自動変換できます。詳細は `docs/ja/guide/migration.md` を参照してください。
