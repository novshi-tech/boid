---
name: boid-kit-init
description: >
  このマシンで利用可能なツール群をスキャンして kit.yaml を生成する。
  「boid kit init を実行して」「kit を初期化して」「kit.yaml を生成して」
  「ローカル環境の kit を作りたい」「Node.js / Go / Docker / gh を kit に登録して」
  など、ホスト環境のスキャンと kit.yaml 生成が必要なときに使用する。
  project は見ない — project とのマッチングは boid-workspace-configure の責務。
---

# boid-kit-init — ホスト環境スキャン & kit.yaml 生成スキル

**役割**: いまこのマシンで利用できるツール群を収拾して、`~/.local/share/boid/kits/<name>/kit.yaml`
を生成する。project は見ない（project とのマッチングは `/boid-workspace-configure` の責務）。

---

## secret-free 規約 (最重要)

kit.yaml に生のシークレット値を書いてはならない。

- `host_commands[*].env` の値は `secret:<key>` 参照のみ（例: `GH_TOKEN: "secret:"`）
- 生の API キー・トークン・パスワード・高エントロピー文字列は **絶対に書かない**
- 生値を書いた場合、後段 scan (`orchestrator.ScanSecretsFile`) が検知して rollback + exit 1 になる

---

## 全体フロー

```
1. スキャン       — PATH binary + $HOME 配下ディレクトリをチェック
2. 検出結果確認   — 何が見つかったかユーザに提示、生成する kit を合意
3. 衝突確認       — 既存 kit がある場合は上書き可否をユーザに確認
4. 雛形読み込み   — templates/<name>.yaml.tmpl を Read
5. 変数置換       — 検出した実値で {{変数名}} を置換
6. kit.yaml 書き込み — ~/.local/share/boid/kits/<name>/kit.yaml に書く
7. 結果サマリ     — 生成した kit 一覧を出力
```

---

## Step 1: スキャン

### 1.1 PATH binary の確認

```bash
which volta 2>/dev/null
which node 2>/dev/null
which npm 2>/dev/null
which nvm 2>/dev/null
which go 2>/dev/null
which gh 2>/dev/null
which docker 2>/dev/null
which podman 2>/dev/null
which git 2>/dev/null
which dotnet 2>/dev/null
```

### 1.2 $HOME 配下の標準ディレクトリチェック

```bash
# volta
ls "$HOME/.volta/bin/" 2>/dev/null | head -5
echo "VOLTA_HOME=${VOLTA_HOME:-}"

# nvm
ls "$HOME/.nvm/versions/node/" 2>/dev/null | head -5

# go
go version 2>/dev/null
echo "GOPATH=${GOPATH:-$(go env GOPATH 2>/dev/null)}"
echo "GOROOT=${GOROOT:-$(go env GOROOT 2>/dev/null)}"

# docker socket
ls /var/run/docker.sock 2>/dev/null
ls "${XDG_RUNTIME_DIR}/docker.sock" 2>/dev/null
ls "${XDG_RUNTIME_DIR}/cetusguard/docker.sock" 2>/dev/null

# dotnet
dotnet --version 2>/dev/null
echo "DOTNET_ROOT=${DOTNET_ROOT:-}"
ls /usr/lib/dotnet 2>/dev/null | head -3
ls /usr/share/dotnet 2>/dev/null | head -3
ls "$HOME/.dotnet/" 2>/dev/null | head -3
```

### 1.3 検出ヒューリスティック

| ツール | 検出シグナル | 生成する kit |
|---|---|---|
| volta | `which volta` 成功 **または** `$HOME/.volta/bin/node` が存在 | `node` (volta variant) |
| nvm | `$HOME/.nvm/versions/node/` に 1 件以上 **かつ** `volta` なし | `node` (nvm variant) — 雛形なし時は手書き案内 |
| system node | `which node` 成功 **かつ** volta/nvm なし | `node` (system variant) — 雛形なし時は手書き案内 |
| go | `which go` 成功 | `go-dev` |
| gh | `which gh` 成功 | `github-cli` |
| docker socket | `/var/run/docker.sock` または `$XDG_RUNTIME_DIR/cetusguard/docker.sock` が存在 | `docker` |
| podman | `which podman` 成功 | `docker` (podman variant) — `github-cli` 雛形のコメント参照 |
| dotnet | `which dotnet` 成功 | `dotnet-dev` |

---

## Step 2: 検出結果の提示と合意

スキャン結果をユーザに見せ、どの kit を生成するか確認する。

```
検出結果:
  ✓ volta   → node kit (volta variant)  を生成します
  ✓ go      → go-dev kit を生成します
  ✓ gh      → github-cli kit を生成します
  ✗ docker  → socket が見つかりません (スキップ)

上記 3 個を生成してよいですか?
```

---

## Step 3: 衝突確認 (既存 kit がある場合)

対象の kit dir (`~/.local/share/boid/kits/<name>/`) が既に存在する場合:

```bash
# 生成日時を確認
grep "generated_at" ~/.local/share/boid/kits/<name>/kit.yaml 2>/dev/null
```

`generated_at` フィールドが見つかれば、その日付でユーザに確認する:

```
2025-10-15 に生成された node kit があります。上書きしますか?
  A. 上書きする
  B. スキップする (既存を保持)
```

`generated_at` がなければ (手書き kit の場合) より慎重に確認する:

```
手動で作成された node kit があります。上書きすると変更が失われます。続けますか?
  A. 上書きする
  B. スキップする (既存を保持)
```

---

## Step 4: 雛形の読み込みと変数置換

### 雛形ファイルの場所

```
~/.claude/skills/boid-kit-init/templates/<name>.yaml.tmpl
```

Read ツールで読み込む:

```
Read("~/.claude/skills/boid-kit-init/templates/node.yaml.tmpl")
Read("~/.claude/skills/boid-kit-init/templates/go-dev.yaml.tmpl")
Read("~/.claude/skills/boid-kit-init/templates/github-cli.yaml.tmpl")
Read("~/.claude/skills/boid-kit-init/templates/docker.yaml.tmpl")
Read("~/.claude/skills/boid-kit-init/templates/dotnet-dev.yaml.tmpl")
```

### 変数置換

雛形内の `{{変数名}}` を検出した実値で置換する (Go の text/template engine は使わない — テキスト置換のみ)。

各雛形ファイルの先頭コメントに置換変数の一覧と取得方法が書いてある。

### 置換後の確認

`secret:` 参照が正しく使われているか確認する。生のシークレット値が混入していないか必ずチェックする。

---

## Step 5: kit.yaml の書き込み

```bash
# ディレクトリ作成 (親 dir は RW bind 済)
mkdir -p ~/.local/share/boid/kits/<name>

# kit.yaml 書き込み (Write ツール使用)
```

Write ツールで `~/.local/share/boid/kits/<name>/kit.yaml` に書く。

`meta` セクションに生成情報を付ける:

```yaml
meta:
  name: <kit 名>
  description: <説明>
  category: <language|vcs|ci|agent|workflow|utility>
  generated_at: "YYYY-MM-DD"
  generated_by: boid-kit-init
```

---

## Step 6: 結果サマリ

```
[生成完了]
  ~/.local/share/boid/kits/node/kit.yaml
  ~/.local/share/boid/kits/go-dev/kit.yaml
  ~/.local/share/boid/kits/github-cli/kit.yaml

[スキップ]
  docker  (socket が見つかりません)

次のステップ:
  - workspace と project を紐付けるには: boid workspace configure <slug>
  - 生成した kit を確認: boid kit list
```

---

## Step 7: legacy-* kit の整理 (任意)

過去の `boid project migrate` は project.yaml の `host_commands` / `additional_bindings` を
そのまま吸い出した `legacy-*` kit を `~/.local/share/boid/kits/` に残す。 これらは
内容が分かりにくいため、 生成した正規 kit と見比べて整理を提案する。

### 7.1 列挙

```bash
ls ~/.local/share/boid/kits/ | grep '^legacy-' || true
```

該当なしならスキップ。

### 7.2 各 legacy kit を分類

それぞれ `~/.local/share/boid/kits/<legacy-name>/kit.yaml` を Read し、 中身を
**今回生成した正規 kit** (`github-cli` / `docker` / `dotnet-dev` / `go-dev` / `node` / `python`) と
見比べて以下のいずれかに分類する:

| 分類 | 判定基準 | 推奨アクション |
|---|---|---|
| (a) テンプレと同等 | host_commands / additional_bindings が、 正規 kit の内容と意味的に同じ (allow パターンの微差は許容) | **削除 (replaced_by 付き)**: 正規 kit で完全に置き換え可能 |
| (b) テンプレ近似だが固有項目あり | テンプレ機能 + 独自 host_commands or bindings (例: `gh` + 独自 bind `/var/data`) | **改名**: 内容を表す名前に rename (例: `legacy-my-web-app` → `my-web-app-tools`) |
| (c) 雑多に複数機能を混載 | gh + docker + 独自 bind 等が混ざる | **そのまま** か、 分割提案を案内 |

### 7.3 ユーザに提案 + 承諾を取る

候補ごとに 1 件ずつ確認する (まとめて y/N にしない):

```
legacy-my-web-app の中身:
  host_commands.gh.allow=[pr, issue]
  additional_bindings=なし

→ 今回生成した `github-cli` kit と同等です。 削除して github-cli に
  置き換えますか? [y/N]
```

### 7.4 アクション適用

承諾を得たら kit dir を直接操作する (workspace.yaml には触らない — それは
CLI 側の post-step が機械的にやる):

**削除** (置き換え or 単純削除):
```bash
rm -rf ~/.local/share/boid/kits/legacy-my-web-app
```

**改名**:
```bash
mv ~/.local/share/boid/kits/legacy-my-web-app ~/.local/share/boid/kits/<new-name>
```

新名は `boid kit list` で重複しないことを事前確認する。 衝突したら別名を
ユーザに問い直す。

### 7.5 cleanup-result.json を書き出す

実施したアクションを `~/.local/share/boid/kits/.kit-init-cleanup-result.json` に
記録する。 これは `boid kit init` コマンド (CLI 側) がサンドボックス退場後に
読み取り、 全 workspace.yaml の `kits:` 参照を機械的に書き換えるための JSON。

書式:
```json
{
  "renamed": [
    {"from": "legacy-my-web-app", "to": "my-web-app-tools"}
  ],
  "deleted": [
    {"name": "legacy-other", "replaced_by": "github-cli"}
  ]
}
```

ルール:
- 整理を 1 件も実施しなかった場合はファイルを書き出さなくて良い (CLI 側は欠落を許容)
- 削除のみ (置き換え無し) なら `replaced_by` を省略する。 該当 workspace の `kits:` から単純に消える
- 同じ kit を rename と delete に同時に登録しない (矛盾)
- ファイル自体は CLI 側が処理完了後に削除する。 スキル側で消す必要はない

### 7.6 制約

- **workspace.yaml には触らない**。 kit init サンドボックスは workspace dir (`~/.config/boid/workspaces/`) に書き込み権限がない。 必ず cleanup-result.json 経由で CLI 側に委ねる
- 削除した kit を別 workspace が参照していた場合も、 CLI 側の post-step が全 workspace を横断して参照を整理する

---

## よくある落とし穴

### volta が見つかるが $VOLTA_HOME が未設定の場合

```bash
# $VOLTA_HOME 未設定時は $HOME/.volta を使う
VOLTA_HOME="${VOLTA_HOME:-$HOME/.volta}"
ls "$VOLTA_HOME/bin/node" 2>/dev/null || echo "volta binary が見当たりません"
```

### GH_TOKEN が環境変数に生値で入っている場合

環境変数に生の GitHub トークンがあっても、kit.yaml には **絶対に書かない**。
代わりに `secret:` 参照を使う。gh CLI は `~/.config/gh/hosts.yml` に認証情報を持っているため、
サンドボックス内では `gh auth token` または `GH_TOKEN` の `secret:` 参照で解決される。

### docker socket が cetusguard 経由の場合

boid では cetusguard proxy 経由でのアクセスを推奨する (素 socket の直結は行わない)。
`$XDG_RUNTIME_DIR/cetusguard/docker.sock` が存在すれば docker.yaml.tmpl の cetusguard 変数を使う。

---

## 関連スキル・コマンド

- `/boid-workspace-configure` — workspace と project のマッチング (kit init 後に実行)
- `boid kit list` — 生成した kit を確認
- `boid kit show <name>` — kit の詳細確認
