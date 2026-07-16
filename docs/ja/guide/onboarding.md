# オンボーディング

boid の初回セットアップは **2 段** です。`default` workspace で足りる場合は実質 **1 段**で終わります。

## 2 つのステップ

| 順序 | コマンド | 役割 |
|---|---|---|
| 1 | `boid project init [dir]` / `boid project add <dir>` | project を daemon に登録（新規なら雛形も作成） |
| 2 (任意) | `boid workspace create` / `edit` / `import` + `boid workspace assign` | project 専用の workspace を用意（`default` で足りるなら不要） |

project を登録すると自動的に `default` workspace に割り当てられます（daemon 起動時に `default` は常に存在が保証される）。`host_commands` / `env` / `capabilities` / `allowed_domains` など実行環境をカスタマイズしたいときだけ、専用の workspace を用意してください。

## シナリオ別

### 新規 project、default workspace で十分 (1 段)

```bash
boid project init ~/src/myproject
```

`--workspace` を省略すると `default` workspace に入ります。

### 新規 project + 専用 workspace (2 段)

```bash
boid project init ~/src/myproject --workspace dev
```

`--workspace` は get-or-create です。`dev` workspace が存在しなければ空の workspace を自動作成してから project を紐付けます。中身（`host_commands` / `env` 等）を詰めたい場合は後述の「workspace を作る/編集する」を続けて行ってください。

### 既存 project を登録 (2 段)

```bash
boid project add ~/src/myproject --workspace dev
```

`boid project init` と同じく `--workspace` は get-or-create です。

### 既存 workspace に project を追加するだけ (1 段)

```bash
boid project add ~/src/another --workspace dev
# dev がすでに存在するので中身はそのまま、project の紐付けだけが変わる
```

### 新規マシンでの一連の流れ

```bash
# 1つ目の project (workspace を新規作成しつつ登録)
boid project init ~/src/myproject --workspace dev

# workspace の中身を整える (host_commands / env などをまとめて書く場合)
boid workspace edit dev --from-file dev-workspace.yaml

# 2つ目以降の project は同じ workspace に追加するだけ
boid project add ~/src/another-project --workspace dev
```

## workspace を作る/編集する

`default` workspace だけで足りない場合、workspace の中身は次のいずれかの方法で用意します。

| 方法 | コマンド / 経路 |
|---|---|
| CLI: 新規作成 | `boid workspace create <slug> [--from-file <yaml>]`（`--from-file` 省略時は空の workspace） |
| CLI: 既存を丸ごと置き換え | `boid workspace edit <slug> --from-file <yaml>` |
| CLI: yaml から取り込み | `boid workspace import <yaml> [--mode create-only\|replace]` |
| API: 直接 POST/PUT | `POST /api/workspaces` / `PUT /api/workspaces/{slug}`（body は `application/yaml`） |
| 旧経路 (残置): yaml を手で置く | `~/.config/boid/workspaces/<slug>.yaml` を直接編集 → `boid workspace assign <project> <slug>` で auto-create |

`--from-file` に渡す yaml の例:

```yaml
env:
  MY_TOKEN: "secret:my-token"
host_commands:
  - gh
allowed_domains:
  - example.com
```

ここでの `host_commands` は**参照名**のリストであって定義そのものではありません — `gh` を参照する前に何が必要かは後述の [host_commands を定義する](#host_commands-を定義する-daemon-側の集約レジストリ) を参照してください。未定義の名前を参照すると `workspace create`/`edit`/`import` は `400 unknown host_commands reference(s): ...` を返します。

既存 workspace の中身を確認するには `boid workspace show <slug>`、そのまま yaml として取り出すには `boid workspace export <slug>` を使います。

## host_commands を定義する (daemon 側の集約レジストリ)

workspace の `host_commands: [name, ...]` は、その workspace の sandbox が呼べる host command の**名前だけ**を列挙するものであり、コマンドそのものの定義ではありません。実際の定義 (バイナリの `path`、`allow`/`deny`/`reject` ルール、`env`) は全 workspace 共通の 1 ファイル `~/.config/boid/host_commands.yaml` に置かれています。

`kit init` が撤去される前は、このファイルは host をスキャンして自動生成されていました。撤去後は手で書き足します:

```yaml
host_commands:
  gh:
    path: /usr/bin/gh
    allow: [pr, issue]
  aws:
    path: /usr/local/bin/aws
```

書き足したら、稼働中の daemon に再読込させます:

```bash
boid host-commands reload
```

daemon が現在把握している名前一覧は次で確認できます:

```bash
boid host-commands list
```

コマンドの詳細は [CLI リファレンス / Host Commands](../reference/cli.md#host-commands) を参照してください。

## 各レイヤの概念

- **project**: 作業パターン（portable、git commit 対象）。`.boid/project.yaml` に記述する。
- **workspace**: 実行環境（machine 単位、workspaces テーブルで DB 管理）。`host_commands` / `env` / `capabilities` / `allowed_domains` / `additional_bindings` などを持ち、project に割り当てる。`default` workspace は常に自動生成される。

## kit 機構の退役について

旧バージョンでは `boid kit init`（マシン単位の kit カタログ生成）→ `boid project init/add` → `boid workspace configure`（LLM 対話で workspace.yaml を生成）という 3 段オンボーディングでしたが、Phase 2.5 PR6 (2026-07) で `kit init` / `workspace configure` およびその周辺コマンド (`kit list` / `kit remove`) は撤去されました。workspace の中身は上記の CLI 操作か yaml 直接編集で用意します。

## 旧 `boid init` からの移行

旧 `boid init` は廃止されました。上記のフローを使ってください。

旧スキーマの `project.yaml`（`kits` / `env` / `host_commands` / `capabilities` 等を含む）を持つ場合は
`boid project migrate <dir>`（`--apply` を付けるまでは dry-run）で自動変換できます。詳細は `docs/ja/guide/migration.md` を参照してください。
