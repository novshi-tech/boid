# オンボーディング

boid の初回セットアップは、daemon 起動後に **雛形生成 → push → git URL 登録** の 3 手 + (任意) workspace 設定です。`default` workspace で足りる場合は workspace 設定は不要です。

daemon 自体の起動 (`go install` → `boid check` → `boid start` → `boid web pair`) は [Getting started / 1. インストール](../getting-started/01-install.md) を参照してください。このページは project / workspace 側のセットアップに絞ります。

## project 登録の 3 手

daemon は compose スタックのコンテナ内で動いており、host 側のディレクトリを直接読めません — project を登録できるのは **git remote URL から daemon 自身が clone した場合だけ**です (`docs/plans/release-onboarding.md` 穴 7)。

| 順序 | コマンド | 役割 |
|---|---|---|
| 1 | `boid project init [dir]` | `.boid/project.yaml` の雛形をローカルに書く (daemon への登録はしない) |
| 2 | push | 雛形 (+ 実コード) を git remote に push する。`project init` が実行後にこの push コマンドの雛形も印字する |
| 3 | `boid project add <git-url> --workspace=<name>` | push した URL を daemon に登録し、daemon 自身が clone する |
| 4 (任意) | `boid workspace create` / `edit` / `apply` + `boid workspace assign` | project 専用の workspace を用意（`default` で足りるなら不要） |

project を登録すると自動的に `default` workspace に割り当てられます（daemon 起動時に `default` は常に存在が保証される）。`host_commands` / `env` / `capabilities` / `allowed_domains` など実行環境をカスタマイズしたいときだけ、専用の workspace を用意してください。

## シナリオ別

### 新規 project、default workspace で十分

```bash
mkdir -p ~/src/myproject && cd ~/src/myproject
boid project init
# ... 印字された案内どおりに commit + push ...
boid project add 'https://github.com/you/myproject.git' --workspace=default
```

`--workspace` を省略すると `boid project add` は 400 (必須フラグ) で拒否されます — 省略時は `boid project init` の印字例が `--workspace=default` を焼き込むので、そのままコピペすれば `default` workspace に入ります。

### 新規 project + 専用 workspace

```bash
cd ~/src/myproject
boid project init --workspace dev
# ... 印字された案内どおりに commit + push ...
boid project add 'https://github.com/you/myproject.git' --workspace dev
```

`--workspace` は get-or-create です。`dev` workspace が存在しなければ空の workspace を自動作成してから project を紐付けます。中身（`host_commands` / `env` 等）を詰めたい場合は後述の「workspace を作る/編集する」を続けて行ってください。

### 既存 project (すでに `.boid/project.yaml` あり) を登録する

push 済みであれば `project init` の手順は不要、`project add` だけで済みます:

```bash
boid project add 'https://github.com/you/existing-repo.git' --workspace dev
```

`.boid/project.yaml` がまだ無い既存リポジトリの場合は、そのリポジトリのルートで `boid project init` を実行 → 既存コードごと commit・push → 上記の `project add` という順になります。

### 既存 workspace に project を追加するだけ

```bash
boid project add 'https://github.com/you/another.git' --workspace dev
# dev がすでに存在するので中身はそのまま、project の紐付けだけが変わる
```

### 新規マシンでの一連の流れ

```bash
# 1つ目の project (workspace を新規作成しつつ登録)
cd ~/src/myproject
boid project init --workspace dev
# ... push ...
boid project add 'https://github.com/you/myproject.git' --workspace dev

# workspace の中身を整える (host_commands / env などをまとめて書く場合)
boid workspace edit dev --from-file dev-workspace.yaml

# 2つ目以降の project は同じ workspace に追加するだけ
boid project add 'https://github.com/you/another.git' --workspace dev
```

## workspace を作る/編集する

`default` workspace だけで足りない場合、workspace の中身は次のいずれかの方法で用意します。

| 方法 | コマンド / 経路 |
|---|---|
| CLI: 新規作成 | `boid workspace create <slug> [--from-file <yaml>]`（`--from-file` 省略時は空の workspace） |
| CLI: 既存を丸ごと置き換え | `boid workspace edit <slug> --from-file <yaml>` |
| CLI: export した envelope 文書を適用 | `boid workspace apply -f <yaml>`（`boid workspace export` の出力を適用。`boid workspace import` は廃止済み） |
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

ここでの `host_commands` は**参照名**のリストであって定義そのものではありません — `gh` を参照する前に何が必要かは後述の [host_commands を定義する](#host_commands-を定義する-daemon-側の集約レジストリ) を参照してください。未定義の名前を参照すると `workspace create`/`edit`/`apply` は `400 unknown host_commands reference(s): ...` を返します。

既存 workspace の中身を確認するには `boid workspace show <slug>`、そのまま yaml として取り出すには `boid workspace export <slug>` を使います。

## ツールチェーンの自動インストールと agent サインイン

workspace は永続的な `$HOME` (workspace home、`boid_state` とは別の named volume) を持ちます。npm / claude CLI / codex CLI などのツールチェーンは `additional_bindings` (撤去済み) ではなく、workspace の `init.sh` に書いて自動インストールします:

```bash
boid workspace set-init-script dev -f init.sh
```

`init.sh` は、その workspace への最初の dispatch (最初にタスク/hook/exec が実行されるタイミング) で自動実行されます。詳細は [workspace home ガイド](workspace-home.md) を参照してください。

**`init.sh` が自動化できないのは agent 自身のサインインだけです** — claude / codex の認証は対話が必要なため、一度だけ手動でセッションに入る必要があります:

```bash
boid agent claude -p <project>
```

これは新規ユーザが最も迷いやすい箇所です。workspace ごとに 1 回、対話セッションで `/login` 等を済ませれば、以降の同じ workspace のタスクではこのステップは不要になります (`boid start` 直後にも同じ案内が表示されます)。

## host_commands を定義する (daemon 側の集約レジストリ)

workspace の `host_commands: [name, ...]` は、その workspace の sandbox が呼べる host command の**名前だけ**を列挙するものであり、コマンドそのものの定義ではありません。実際の定義 (バイナリの `path`、`allow`/`deny`/`reject` ルール、`env`) は全 workspace 共通の 1 ファイル `~/.config/boid/host_commands.yaml` に置かれています。**注意:** compose daemon では、この `~/.config/boid` は daemon コンテナ内の named volume (`build/container/compose.yml` 上の宣言名は `boid_state`、host 側の実際の volume 名は compose プロジェクト名を接頭辞にした `boid_boid_state`) 上のパスです — host 側の同名パスを編集しても daemon には届きません。編集は daemon 上で行うか (`docker exec`/`podman exec`)、`boid host-commands` API 経由の間接編集手段が増えるまでは直接編集する必要があります。

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
`boid project migrate <dir>` (`--apply` 無しなら dry-run) で変換内容を確認できます。詳細は `docs/ja/guide/migration.md` を参照してください。

**注意:** `--apply` 単体は 2026-08 (release-onboarding PR5) 以降 **拒否されます** — `boid project migrate <dir> --apply` は `--legacy-bare-metal` を併用しない限りエラーで終了します (`cmd/project_migrate.go` の `guardApply`)。`--apply --legacy-bare-metal` 自体も compose daemon 向けの安全な自動反映ではなく、**bare-metal daemon (compose を使わない旧来の単体プロセス) を直接操作する場合限定**の移行経路です — host 側の boid.db を直接開く (または `client.DefaultSocketPath()` の bare-metal socket に到達できればそちらへ) ため、compose daemon 環境ではどちらのケースにも該当せず使うべきではありません。compose daemon 下での自動移行手段は現時点でまだ確立していません — 詳細と理由は [移行ガイド](migration.md) を参照してください。