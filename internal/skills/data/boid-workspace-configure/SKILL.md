---
name: boid-workspace-configure
description: >
  workspace に紐付け済みの project 群をスキャンし、必要な kit を
  workspace.yaml の kits: に追加する。
  「boid workspace configure を実行して」「workspace の kit を設定して」
  「workspace と project をマッチングして」「どの kit が必要か調べて」
  「workspace.yaml に kit を追加して」など、workspace の kit 構成が必要なときに使用。
  このスキルは `boid workspace configure <slug>` コマンドから起動され、
  環境変数 BOID_WORKSPACE_SLUG でターゲット slug が渡される。
---

# boid-workspace-configure — workspace kit 構成スキル

**役割**: workspace に紐付け済みの project 群をスキャンし、各 project が必要とする
kit を特定して `workspace.yaml` の `kits:` に追加する。kit 自体の生成は行わない
(それは `/boid-kit-init` の責務)。

---

## secret-free 規約 (最重要)

`workspace.yaml` に生のシークレット値を書いてはならない。

- `env:` の値は plain k/v のみ (例: `GOENV: production`)
- API キー・トークン・パスワード・高エントロピー文字列は **絶対に書かない**
- secret は kit 側で `secret:` 参照として完結させる
- 生値を書いた場合、後段 scan (`orchestrator.ScanSecretsFile`) が検知して rollback + exit 1 になる

---

## 全体フロー

```
1. 入力確認     — BOID_WORKSPACE_SLUG を読む
2. project 取得 — boid workspace show <slug> -o json で project 一覧を取得
3. project スキャン — 各 project の package.json / go.mod / hooks を読む
4. kit カタログ確認 — boid kit list + 各 kit.yaml を read してマッチング
5. 差分提示     — 追加が必要な kit をユーザに提示して確認
6. workspace.yaml 更新 — kits: array を追加 (既存の env / capabilities は温存)
7. 結果サマリ   — 追加した kit 一覧を出力
```

---

## Step 1: 入力確認

環境変数 `BOID_WORKSPACE_SLUG` から slug を取得する。

```bash
SLUG="${BOID_WORKSPACE_SLUG}"
if [ -z "$SLUG" ]; then
  echo "error: BOID_WORKSPACE_SLUG が未設定です"
  exit 1
fi
echo "対象 workspace: $SLUG"
```

---

## Step 2: project 一覧の取得

`boid workspace show <slug> -o json` で workspace の詳細と紐付け済み project 一覧を取得する。

```bash
boid workspace show "$SLUG" -o json
```

JSON の `projects[]` 配列から `work_dir` フィールドを抽出する。
`projects` が空の場合は「project が紐付けられていません」と案内して終了する。

```
projects が 0 件の場合:
  「workspace '$SLUG' に project が紐付けられていません。
   先に `boid workspace assign <project> $SLUG` で project を紐付けてから再実行してください。」
  → 終了
```

`warnings[]` に警告がある場合は内容をユーザに表示する (workspace.yaml が未作成など)。

---

## Step 3: project スキャン

各 project の `work_dir` を確認し、以下のファイルを Read して project の依存を把握する。

### 3.1 読み込むファイル

| ファイル | 目的 |
|---|---|
| `<work_dir>/package.json` | Node.js 依存の有無 (scripts, dependencies) |
| `<work_dir>/go.mod` | Go モジュールの有無 |
| `<work_dir>/.boid/project.yaml` | task_behaviors の hooks script パス |
| `<work_dir>/Dockerfile` | Docker 使用の有無 |
| `<work_dir>/docker-compose.yml` または `docker-compose.yaml` | Docker Compose 使用の有無 |
| `<work_dir>/pyproject.toml` または `<work_dir>/setup.py` | Python 依存の有無 |

ファイルが存在しない場合はスキップして次へ進む (エラーにしない)。

### 3.2 hooks script の確認

`project.yaml` の `task_behaviors[].hooks` に script が指定されている場合、
その script ファイルを Read してどのコマンドが使われているかを確認する。

```bash
# hook script の例
cat <work_dir>/.boid/hooks/on-executing.sh 2>/dev/null
```

### 3.3 検出ヒューリスティック

| 検出シグナル | 必要な kit |
|---|---|
| `package.json` が存在する | `node` |
| `go.mod` が存在する | `go-dev` |
| `Dockerfile` または `docker-compose.yml` が存在する | `docker` |
| hooks / scripts で `gh` コマンドを使用 | `github-cli` |
| hooks / scripts で `gh` を使用、または `.boid/project.yaml` に `gh` 参照 | `github-cli` |
| `pyproject.toml` または `setup.py` が存在する | `python` |

---

## Step 4: kit カタログの確認

### 4.1 インストール済み kit の列挙

```bash
boid kit list
```

出力は kit 名の一覧 (1 行 1 kit)。インストールされていない場合は `no kits installed` と表示される。

### 4.2 各 kit の詳細を確認

kit ディレクトリは `~/.local/share/boid/kits/<name>/kit.yaml` にある。
`boid kit show` は未実装のため、直接 Read ツールで読む。

```
Read("~/.local/share/boid/kits/<name>/kit.yaml")
```

`meta.name` / `meta.description` / `host_commands` を確認し、
project が要求するコマンドをその kit が提供しているか判断する。

### 4.3 マッチング

Step 3 で特定した「必要な kit」と、Step 4.1 のカタログを照合する。

| 結果 | 対処 |
|---|---|
| カタログに存在する | workspace.yaml の `kits:` に追加対象としてリストアップ |
| カタログに存在しない | ユーザに「`boid kit init` の再実行」を案内 (後述) |

---

## Step 5: 差分提示と確認

現在の `workspace.yaml` の `kits:` の内容 (なければ空) と、追加しようとする kit を比較して提示する。

```
現在の workspace.yaml:
  kits: [node, go-dev]  ← 既存

追加が必要な kit:
  ✓ node        → 既にあります (スキップ)
  ✓ go-dev      → 既にあります (スキップ)
  + github-cli  → 新たに追加します
  ! python      → カタログにありません (boid kit init の再実行が必要)

上記の変更を適用してよいですか?
```

ユーザが確認したら Step 6 へ進む。変更がない場合は「変更なし」と表示して終了。

---

## Step 6: workspace.yaml の更新

### 6.1 既存内容の読み込み

```
Read("~/.config/boid/workspaces/<slug>.yaml")
```

ファイルが空または存在しない場合は新規作成として扱う。

### 6.2 更新ルール

- `kits:` 配列: 既存の kit を保持しつつ、新たに必要な kit を末尾に追加する (重複は追加しない)
- `env:` セクション: **ユーザが既に設定した値を絶対に変更しない**
- `capabilities:` セクション: **ユーザが既に設定した値を絶対に変更しない**
- 他のフィールドも温存する

### 6.3 書き込み形式

Write ツールで `~/.config/boid/workspaces/<slug>.yaml` に書く。
形式は YAML。以下の構造を保持する:

```yaml
# workspace: <slug>
kits:
  - node
  - go-dev
  - github-cli

# env: (既存のユーザ設定があればそのまま保持)
# env:
#   KEY: value

# capabilities: (既存のユーザ設定があればそのまま保持)
```

`kits:` が空になる場合でも `kits: []` を明示的に書かない (omitempty により省略される)。

### 6.4 secret-free チェック

書き込む前に `env:` の値に高エントロピー文字列・トークンらしきパターンが含まれていないか自己チェックする。
疑わしい値があれば書き込みを中止してユーザに確認する。

---

## Step 7: 結果サマリ

```
[完了] workspace: <slug>
  kits: [node, go-dev, github-cli]
  追加: github-cli

次のステップ:
  - 設定を確認: boid workspace show <slug>
  - kit が足りない場合: boid kit init を再実行
```

---

## 足りない kit のガイダンス

project が必要とするコマンドをカタログの kit が提供していない場合は以下を出力する:

```
[要対応] 以下の kit がカタログに見つかりませんでした:
  - python  (pyproject.toml が検出されました)

`boid kit init` を再実行すると Python 環境が検出され、kit が生成されます。
生成後に `boid workspace configure <slug>` を再実行してください。
```

---

## よくある落とし穴

### workspace.yaml の env: に生値を書いてしまう場合

`env:` には plain な設定値 (環境名、パス設定など) のみ書く。
API キー・トークン等は kit 側の `secret:` 参照で解決する。

```yaml
# 良い例 — workspace.yaml の env: に書いてよいもの
env:
  APP_ENV: production
  LOG_LEVEL: info

# 悪い例 — 絶対に書かない (後段 scan で rollback される)
env:
  GITHUB_TOKEN: ghp_xxxxxxxxxxxxxxxxxxxx  # NG: 生トークン
  DATABASE_URL: postgres://user:pass@host/db  # NG: 認証情報入り URL
```

### kit は存在するが host_commands が合わない場合

kit の `host_commands` が期待するバイナリパスと実際のホストパスが異なる場合がある。
`boid kit init` で再生成するか、`~/.local/share/boid/kits/<name>/kit.yaml` を直接編集する。

### boid workspace show が daemon 接続エラーになる場合

`boid workspace configure` は daemon が必要。daemon が起動していない場合は:

```bash
boid start
boid workspace configure <slug>
```

---

## 関連スキル・コマンド

- `/boid-kit-init` — ホスト環境をスキャンして kit.yaml を生成 (先に実行が必要)
- `boid workspace show <slug>` — workspace の現在の設定を確認
- `boid kit list` — インストール済みの kit を一覧
- `boid workspace assign <project> <slug>` — project を workspace に紐付け
