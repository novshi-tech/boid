---
name: boid-sandbox-configure
description: >
  このマシンで利用可能なツール群をスキャンして kit.yaml を生成する (kit-init
  モード)、または workspace に紐付け済みの project 群をスキャンして必要な
  kit を workspace.yaml の kits: に追加する (workspace-configure モード)。
  起動時の環境変数 BOID_WORKSPACE_SLUG の有無でどちらのモードかを判別する
  (未設定 → kit-init、設定済み → workspace-configure)。
  「boid kit init を実行して」「kit を初期化して」「kit.yaml を生成して」
  「ローカル環境の kit を作りたい」「Node.js / Go / Docker / gh を kit に登録して」
  「az を kit にして」「atl コマンドを登録して」「playwright-cli の kit を作って」
  「boid workspace configure を実行して」「workspace の kit を設定して」
  「workspace と project をマッチングして」「どの kit が必要か調べて」
  「workspace.yaml に kit を追加して」
  など、ホスト環境のスキャン・kit.yaml 生成・workspace kit 構成が必要な
  ときに使用する。
---

# boid-sandbox-configure — kit.yaml 生成 & workspace kit 構成スキル

**役割**: このスキルは 2 つのモードを持つ、単一の SKILL.md に統合されたスキル。
起動時の環境変数 `BOID_WORKSPACE_SLUG` の有無でどちらのモードで動くかを判別する。

| モード | 判別条件 | やること | 書ける先 |
|---|---|---|---|
| **kit-init** | `BOID_WORKSPACE_SLUG` 未設定 | ホスト環境をスキャンして `~/.local/share/boid/kits/<name>/kit.yaml` を生成する。project は見ない | kits dir のみ (rw)。project bind 無し |
| **workspace-configure** | `BOID_WORKSPACE_SLUG` 設定済み | 紐付け済み project 群をスキャンし、必要な kit を `workspace.yaml` の `kits:` に追加する。kit 自体の生成は行わない | workspace dir のみ (rw)。project は ro bind |

まず最初にモードを判別する:

```bash
if [ -z "${BOID_WORKSPACE_SLUG:-}" ]; then
  echo "mode: kit-init"
else
  echo "mode: workspace-configure (slug=$BOID_WORKSPACE_SLUG)"
fi
```

**この 2 モードは物理的に異なるサンドボックスで実行される** — SKILL.md は 1 本に
統合されているが、起動元コマンド (`boid kit init` / `boid workspace configure`) は
今も 2 本のまま、それぞれ現在通りの書き込み先・bind 設定を保持する。これは意図的な
セキュリティ判断:

- kit-init は host_commands.path に任意のホストパスを書けて host 実行権を **鋳造**
  できる。project は読まない (信頼できるホストツールのみスキャン)。
- workspace-configure は project の中身 (package.json / go.mod / hook script) を
  読む、つまり **untrusted な project 入力を能動的に読む面**。kit を鋳造する力は無い
  (WorkspaceMeta に host_commands フィールドが無い)。

「鋳造能力」と「untrusted な project 入力を読む面」を同一サンドボックスに同居させない
のがこの分離の目的。統合するのは UX (SKILL.md 1本化) だけで、サンドボックスの
権限分離はそのまま。

以降、`# kit-init モード` と `# workspace-configure モード` を該当するモードのみ
読んで実行する。

---

# kit-init モード

`BOID_WORKSPACE_SLUG` 未設定のときに実行する。

**役割**: いまこのマシンで利用できるツール群を収拾して、`~/.local/share/boid/kits/<name>/kit.yaml`
を生成する。project は見ない（project とのマッチングは同スキルの workspace-configure モードの責務）。

---

## secret-free 規約 (最重要)

kit.yaml に生のシークレット値を書いてはならない。

- `host_commands[*].env` の値は `secret:<key>` 参照のみ（例: `GH_TOKEN: "secret:"`）
- 生の API キー・トークン・パスワード・高エントロピー文字列は **絶対に書かない**
- 生値を書いた場合、後段 scan (`orchestrator.ScanSecretsFile`) が検知して rollback + exit 1 になる

---

## host_commands と additional_bindings の使い分け (設計原則)

| 仕組み | 用途 | 使うべき場面 |
|---|---|---|
| `additional_bindings` + `env.PATH` | サンドボックス内で **直接実行** されるローカルツールチェイン | 言語ランタイム (node / go / python / dotnet)、 パッケージマネージャ (npm / uv / cargo) |
| `host_commands` | サンドボックス境界を越えて **ホスト側で実行** されるコマンド | ホストの credential が必要 (`gh`)、 ホスト daemon との通信 (`docker`)、 ホストの特権が必要 (`systemctl`) |

判定の指針: 「サンドボックス内で動かしたい」 ものか 「**ホストでしか動かせない (もしくは動かしたくない)**」 ものか。

- node / npm / npx / pnpm / yarn → サンドボックス内 → `additional_bindings` + PATH
- go → サンドボックス内 → `additional_bindings` + PATH
- uv / pip / python → サンドボックス内 → `additional_bindings` + PATH
- dotnet → サンドボックス内 → `additional_bindings`
- gh → ホストの GitHub credential が必要 → `host_commands` (`secret:` 参照付き)
- docker → **kit は作らない**。 workspace.yaml の `capabilities.docker: {}` で boid ネイティブ proxy を有効化する

ローカルなツールチェインを `host_commands` に登録すると、 サブコマンド毎にホスト側へ broker dispatch される。 これは設計意図と逆 (ローカル実行は sandbox の egress allowlist で十分制御できる) であり、 host_commands の趣旨である **「選択的に通したい外部境界」** を曖昧にしてしまう。

新しいテンプレを追加するときはまずこの表で判定すること。

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
which docker 2>/dev/null  # 検出時の案内のみ。 kit は生成しない (capabilities.docker を案内)
which podman 2>/dev/null  # 同上
which git 2>/dev/null
which dotnet 2>/dev/null
which uv 2>/dev/null
which python3 2>/dev/null
which python 2>/dev/null
```

### 1.2 $HOME 配下の標準ディレクトリチェック

```bash
# volta (node + pnpm / yarn のチェックも兼ねる)
ls "$HOME/.volta/bin/" 2>/dev/null | head -10
echo "VOLTA_HOME=${VOLTA_HOME:-}"
ls "${VOLTA_HOME:-$HOME/.volta}/bin/pnpm" 2>/dev/null
ls "${VOLTA_HOME:-$HOME/.volta}/bin/yarn" 2>/dev/null

# nvm
ls "$HOME/.nvm/versions/node/" 2>/dev/null | head -5

# go
go version 2>/dev/null
echo "GOPATH=${GOPATH:-$(go env GOPATH 2>/dev/null)}"
echo "GOROOT=${GOROOT:-$(go env GOROOT 2>/dev/null)}"
# パッケージキャッシュ (永続 bind 対象)。 dispatch 跨ぎで効かせるため rw bind する。
echo "GOMODCACHE=$(go env GOMODCACHE 2>/dev/null)"
echo "GOCACHE=$(go env GOCACHE 2>/dev/null)"

# docker / podman socket (検出のみ。 kit は生成しない → capabilities.docker を案内する)
ls /var/run/docker.sock 2>/dev/null
ls "${XDG_RUNTIME_DIR}/docker.sock" 2>/dev/null
ls "${XDG_RUNTIME_DIR}/podman/podman.sock" 2>/dev/null

# dotnet
dotnet --version 2>/dev/null
echo "DOTNET_ROOT=${DOTNET_ROOT:-}"
ls /usr/lib/dotnet 2>/dev/null | head -3
ls /usr/share/dotnet 2>/dev/null | head -3
ls "$HOME/.dotnet/" 2>/dev/null | head -3

# uv
uv --version 2>/dev/null
echo "UV_CACHE_DIR=${UV_CACHE_DIR:-$HOME/.cache/uv}"
echo "UV_DATA_DIR=${UV_DATA_DIR:-$HOME/.local/share/uv}"
ls "$HOME/.local/bin/uv" 2>/dev/null
ls "$HOME/.cargo/bin/uv" 2>/dev/null

# python
python3 --version 2>/dev/null
python --version 2>/dev/null
```

### 1.3 検出ヒューリスティック

| ツール | 検出シグナル | 生成する kit |
|---|---|---|
| volta | `which volta` 成功 **または** `$HOME/.volta/bin/node` が存在 | `node` (volta variant) |
| nvm | `$HOME/.nvm/versions/node/` に 1 件以上 **かつ** `volta` なし | `node` (nvm variant) — 雛形なし時は手書き案内 |
| system node | `which node` 成功 **かつ** volta/nvm なし | `node` (system variant) — 雛形なし時は手書き案内 |
| go | `which go` 成功 | `go-dev` |
| gh | `which gh` 成功 | `github-cli` |
| docker / podman | `/var/run/docker.sock` / `$XDG_RUNTIME_DIR/docker.sock` / `$XDG_RUNTIME_DIR/podman/podman.sock` のいずれかが存在、 もしくは `which docker` / `which podman` 成功 | **kit は生成しない**。 Step 2 で「workspace.yaml に `capabilities.docker: {}` を書く」 案内を出す |
| dotnet | `which dotnet` 成功 | `dotnet-dev` |
| uv | `which uv` 成功 **または** `$HOME/.local/bin/uv` / `$HOME/.cargo/bin/uv` が存在 | `python` (uv variant — 推奨) |
| system python | `which python3` または `which python` 成功 **かつ** `uv` なし | `python` (system variant) — 雛形コメント参照 |
| pyenv | `$HOME/.pyenv/versions/` に 1 件以上 **かつ** `uv` なし | `python` (pyenv variant) — 雛形コメント参照 |
| conda / mamba | `$CONDA_PREFIX` が設定済 **かつ** `uv` なし | `python` (conda variant) — 雛形コメント参照 |

---

## Step 2: 検出結果の提示と合意

スキャン結果をユーザに見せ、どの kit を生成するか確認する。

```
検出結果:
  ✓ volta   → node kit (volta variant)  を生成します
  ✓ go      → go-dev kit を生成します
  ✓ gh      → github-cli kit を生成します
  ℹ docker  → kit は生成しません。
              workspace.yaml に `capabilities.docker: {}` を追記すると
              boid ネイティブ proxy 経由で sandbox から docker daemon に
              アクセスできます (DOCKER_HOST / CONTAINER_HOST /
              TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE は自動 inject)。

上記 3 個を生成してよいですか?
```

**docker / podman の扱い**: 検出はするが kit は作らない。 これは設計判断:

- boid ネイティブプロキシ (`capabilities.docker: {}`) が **ホスト daemon 通信の正規経路**。 daemon が起動・ ボディ検査・ id スコープ検査・ ryuk 無効化まで面倒を見る
- docker kit (socket 直 bind) はリクエストボディを検査できず、 `HostConfig.Privileged` / `HostConfig.Binds` 等の危険設定を素通しする。 kit 経由 = 攻撃面が広い
- workspace は machine-local なので、 マシン固有の docker 構成 (rootless/ rootful / podman) も workspace.yaml の `capabilities.docker: {}` 一行で扱える
- 詳細は `docs/ja/guide/docker-proxy-migration.md` 参照

---

## Step 2.5: ad-hoc 個別コマンドの kit 化 (リクエスト時のみ)

ユーザが特定のコマンドを名指しで kit 化してほしいと言ってきたとき
(例: 「`az` を kit にして」「`atl` コマンドの kit を作って」「`playwright-cli` 登録して」)、
Step 1 の自動スキャンに該当が無くてもこの Step で 1 個ずつ kit を組み立てる。
**ユーザ要求が無いときは Step 2.5 を勝手に走らせない** (PATH 全体スキャンはノイズに
なるので、 アドホック起動のみ)。

### 2.5.1 既知テンプレ該当チェック

まず Step 1 の検出ヒューリスティック表を見て、 要求されたコマンドが既知テンプレで
覆えるかチェックする:

- 該当する (例: ユーザが「`node` を kit にして」と言った) → 既知パスへ流す
  (Step 4 で `templates/<name>.yaml.tmpl` を使う)
- 該当しない (例: `az` / `atl` / `azcopy` / `freee` / `msgraph` / `playwright-cli`
  / `terraform` 等) → 以下の ad-hoc フローへ

汎用性が高そうなコマンドは「既知テンプレに足すべきかも」 と一言添えるのは可
(ただし本セッションでテンプレ追加までやらない — それは boid 本体側の改修)。

### 2.5.2 binary 確認

```bash
which <command>
<command> --version 2>/dev/null || <command> version 2>/dev/null || true
```

binary が無ければユーザに「`<command>` が PATH 上に見当たりません。 インストール
してから再実行してください」 と返してこの Step を中断する。

### 2.5.3 host_commands vs additional_bindings の判定

上の「host_commands と additional_bindings の使い分け」 表に従って判定する。
迷ったら直接ユーザに聞く:

```
`az` をどう扱いますか?
  A. host_commands  ホスト側で実行・ ホストの credential (~/.azure/ 等) を使う
                    Azure CLI のようなクラウド認証系はだいたいこちら
  B. additional_bindings  サンドボックス内に bind して直接実行
                          静的に動くツールチェイン (playwright-cli の中身等) はこちら
```

判定の典型例:

| コマンド | 経路 | 理由 |
|---|---|---|
| az | host_commands | `~/.azure/` の auth が必要、 ホスト credential |
| azcopy | host_commands | Azure auth に依存 |
| atl | host_commands | ホストの atl 設定 (~/.atl など) を使う |
| terraform | host_commands or additional_bindings | provider credential を使うなら host、 static なら bindings |
| playwright-cli | additional_bindings | Chromium バイナリ等を bind すれば動く (host 特権不要) |
| freee / msgraph | host_commands | ホストの OAuth token を使う |

### 2.5.4 対話で yaml フィールドを詰める

**host_commands 経路** の対話項目:

- **kit name** (default: binary 名と同じ。 既存と衝突するなら別名)
- **path** (default: `which <command>` の結果)
- **allow パターン** (default: `["*"]` ではなく **使うサブコマンドのホワイトリスト** を推奨)
  - わからない場合は `<command> --help` の出力を見せて 3-5 個を選んでもらう
  - 後で追加が必要になったら kit.yaml を手編集する旨を案内
- **必要な env** (default: なし)
  - 環境変数が必要なら **必ず `secret:<key>` 参照のみ**
  - 生値を入力されたら警告して `secret:<key>` に置換するよう案内
  - 不要なら `env:` セクション自体を省く

**additional_bindings 経路** の対話項目:

- **kit name** (default: binary 名)
- **bind 元 path** (default: which 結果の親ディレクトリ、 または `$HOME/.<tool>` 等)
- **PATH env への追加** (default: bind 元と同じ path)
- **mode** (default: 省略 = readonly。 書き込みが必要なときだけ `mode: rw`)
- **必要な env** (なし or `secret:<key>` のみ)

### 2.5.5 ad-hoc kit.yaml の組み立て例

**host_commands 例** (`az` を ad-hoc 登録):

```yaml
meta:
  name: azure-cli
  description: Azure CLI (az) をホスト経由で提供する
  category: utility
  generated_at: "YYYY-MM-DD"
  generated_by: boid-sandbox-configure

host_commands:
  az:
    path: /usr/bin/az
    allow:
      - account
      - login
      - storage
      - vm
    # env が必要な場合のみ。 生値は絶対書かない。
    # env:
    #   AZURE_CLIENT_ID: "secret:"
```

**additional_bindings 例** (`playwright-cli` を ad-hoc 登録):

```yaml
meta:
  name: playwright-cli
  description: playwright-cli をサンドボックスに直接 bind
  category: utility
  generated_at: "YYYY-MM-DD"
  generated_by: boid-sandbox-configure

env:
  PATH: "/opt/playwright/bin:${PATH}"

additional_bindings:
  - source: /opt/playwright
  # 書き込みが必要なら mode: rw を追加
```

### 2.5.6 既存衝突確認 → 書き込み

組み立てた yaml は通常の Step 3 (衝突確認) → Step 5 (書き込み) → Step 6
(サマリ) の経路に合流させる。 ad-hoc 生成も `generated_by: boid-sandbox-configure` を
付け、 結果サマリには **「ad-hoc 生成」** とラベルを添えて自動スキャン分と
区別する:

```
[生成完了]
  ~/.local/share/boid/kits/node/kit.yaml      (auto)
  ~/.local/share/boid/kits/azure-cli/kit.yaml (ad-hoc)
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
~/.claude/skills/boid-sandbox-configure/templates/<name>.yaml.tmpl
```

Read ツールで読み込む:

```
Read("~/.claude/skills/boid-sandbox-configure/templates/node.yaml.tmpl")
Read("~/.claude/skills/boid-sandbox-configure/templates/go-dev.yaml.tmpl")
Read("~/.claude/skills/boid-sandbox-configure/templates/github-cli.yaml.tmpl")
Read("~/.claude/skills/boid-sandbox-configure/templates/dotnet-dev.yaml.tmpl")
Read("~/.claude/skills/boid-sandbox-configure/templates/python.yaml.tmpl")
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
  generated_by: boid-sandbox-configure
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

## Step 7: project migrate 由来の整理対象 kit の整理 (任意)

過去の `boid project migrate` は project.yaml の `host_commands` / `additional_bindings` を
そのまま吸い出した `legacy-*` kit を `~/.local/share/boid/kits/` に残す。 加えて
古い migrate ロジックは `kits:` フィールドに書かれた kit ref の末尾 segment を
そのまま kit dir 名にしていたため、 以下のような **`legacy-` 接頭辞ではない整理対象 kit** も
混在し得る:

- project の base 名 (例: `ubs-apps-msgraph`, `atl-khi-azure-devops`)
- 統合サービス名 (例: `azure-devops`)
- まれに `ValidKitName` (lowercase + 数字 + `-`、 1–64 文字) を **満たさない** 不正名
  (例: `github.com` — `.` は不正、 過去の migrate バグで漏れた)

これらは内容が分かりにくく、 生成した正規 kit と機能重複も多いので整理を提案する。

### 7.1 列挙

整理対象は **`legacy-*` だけでなく、 正規 kit と既存 integration kit を除いた残り全て** を候補に挙げる。
正規 kit は本スキルが今回 Step 3〜5 で生成したもの (`github-cli` / `dotnet-dev` / `go-dev` / `node` / `python` 等)、
ユーザが意図して育てている integration kit (例: `board` / `atl` / `playwright` 等) はユーザ確認のうえスキップする。

```bash
# legacy-* (明示的な migrate 由来)
ls ~/.local/share/boid/kits/ | grep '^legacy-' || true

# それ以外の全 kit (= 正規 kit や整備済 integration kit を除いた整理候補)
ls ~/.local/share/boid/kits/
```

該当なしならスキップ。

不正名 kit (`ValidKitName` 違反、 例えば `.` 入り) を見つけた場合は **必ず整理対象に含める**
(放置すると workspace.yaml 側の参照も読めない)。 cleanup-result.json の `deleted[].name` /
`renamed[].from` には不正名をそのまま書いて構わない (CLI 側は match 側の不正名を tolerate する)。

### 7.2 各候補 kit を分類

それぞれ `~/.local/share/boid/kits/<name>/kit.yaml` を Read し、 中身を
**今回生成した正規 kit** (`github-cli` / `dotnet-dev` / `go-dev` / `node` / `python`) と
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
- **`deleted[].name` / `renamed[].from`** (整理対象の元名) には、 `ValidKitName` に通らない
  不正名 (例: `github.com`) を書いてよい。 CLI 側は文字列等値で workspace.kits を整理するだけなので tolerate する
- **`deleted[].replaced_by` / `renamed[].to`** (workspace.kits に新規書き込む側) は必ず
  `ValidKitName` を満たす slug にする。 CLI 側が validate して reject する

### 7.6 制約

- **workspace.yaml には触らない**。 kit-init モードのサンドボックスは workspace dir (`~/.config/boid/workspaces/`) に書き込み権限がない。 必ず cleanup-result.json 経由で CLI 側に委ねる
- 削除した kit を別 workspace が参照していた場合も、 CLI 側の post-step が全 workspace を横断して参照を整理する

---

## kit-init モード: よくある落とし穴

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

### docker / podman 検出時の案内

kit-init モードは **docker kit を生成しない**。 docker 関連の sandbox 設定はすべて
boid ネイティブ proxy (workspace.yaml の `capabilities.docker: {}`) に集約する。

理由:

- ネイティブ proxy は daemon が自動起動・ リクエストボディ検査・ id スコープ検査・
  TestContainers Ryuk 無効化まで一括で面倒を見る
- 旧 docker kit (socket 直 bind) は HTTP メソッド / URL のみ照合する古い経路で、
  `HostConfig.Privileged` 等の危険設定を素通しする
- workspace は machine-local なので、 マシン固有 (rootless / rootful / podman) の
  差は workspace.yaml 1 ファイルで吸える

検出時の案内例 (Step 2 の出力に含める):

```
ℹ docker socket を検出しました (/var/run/docker.sock)。
   workspace.yaml に次を追記すると sandbox から使えるようになります:

     capabilities:
       docker: {}

   詳細: docs/ja/guide/docker-proxy-migration.md
```

過去の kit-init で生成された `docker` kit (cetusguard variant 含む) を見つけた
場合は Step 7 の整理対象として削除を提案する (`replaced_by` は空。 capabilities への
切り替えはユーザがworkspace.yaml で行う旨を案内)。

---

# workspace-configure モード

`BOID_WORKSPACE_SLUG` が設定されているときに実行する。

**役割**: workspace に紐付け済みの project 群をスキャンし、各 project が必要とする
kit を特定して `workspace.yaml` の `kits:` に追加する。kit 自体の生成は行わない
(それは同スキルの kit-init モードの責務)。

このモードのサンドボックスは **daemon ソケットを持たない** (即時自己発火 — compromise
されたエージェントが task/session を起動して鋳造 kit をその場で発火させる — の経路を
断つための最小権限化)。project 一覧は daemon API を叩くのではなく、CLI (host 側) が
起動前に取得して環境変数として注入したものを読む。

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
2. project 取得 — BOID_WORKSPACE_PROJECTS (CLI 注入済み env) から project 一覧を読む
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

**このサンドボックスに daemon ソケットは無い。** project 一覧は `boid workspace show`
のような daemon API 呼び出しではなく、CLI (host 側) が起動前に daemon から取得して
注入した環境変数 `BOID_WORKSPACE_PROJECTS` から読む。

```bash
echo "$BOID_WORKSPACE_PROJECTS"
```

出力は `[{"id": "...", "work_dir": "..."}, ...]` 形式の JSON 配列 (project が 0 件
なら `[]`)。各要素の `work_dir` を Step 3 で読む — project ディレクトリは host 側で
既に read-only bind 済みなので、そのまま Read ツールで中身を読める。

`BOID_WORKSPACE_PROJECTS` が `[]` または未設定の場合:

```
「workspace '$SLUG' に project が紐付けられていません。
 先に `boid workspace assign <project> $SLUG` で project を紐付けてから再実行してください。」
→ 終了
```

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
`boid kit list` は `~/.local/share/boid/kits/` のファイルを直接読むだけなので daemon は不要
(ProfileInit サンドボックスは host root を read-only rbind しているのでこのパスは常に見える)。

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
- `allowed_domains:` セクション: **ユーザが既に設定した値を絶対に変更しない** (詳細は 6.5)
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

# allowed_domains: (既存のユーザ設定があればそのまま保持)
# allowed_domains:
#   - .example.com
```

`kits:` が空になる場合でも `kits: []` を明示的に書かない (omitempty により省略される)。

### 6.4 workspace スキーマ (フィールド一覧)

`WorkspaceMeta` が受け付けるトップレベルキーは以下のみ。これ以外のキー
(`network:` 等) はパース時に黙って捨てられるので、間違って書かないこと。

| キー | 型 | 用途 |
|---|---|---|
| `kits` | `[]string` | このスキルの主目的 |
| `env` | `map[string]string` | 全サンドボックスへ注入する env (secret-free 規約) |
| `capabilities` | object | サンドボックス能力フラグ (例: `docker: {}`) |
| `allowed_domains` | `[]string` | workspace スコープの proxy egress 許可 (詳細は次節) |

### 6.5 allowed_domains について

workspace スコープで HTTP(S) proxy の egress 許可ドメインを追加できる。
**トップレベル**の `allowed_domains:` に書く。`network.allowed_domains` の
ような **ネスト構造は受け付けない** ので注意 (LLM が config.yaml の
`sandbox.network` の癖に引っ張られて間違いやすい)。

```yaml
# 良い例 — workspace.yaml はトップレベル
allowed_domains:
  - .cosmos.azure.com
  - api.openai.com

# 悪い例 — このネスト書きは黙って無視される
network:
  allowed_domains:
    - .cosmos.azure.com
```

意味論:
- daemon-wide の floor (config.yaml `sandbox.allowed_domains` + boid 既定)
  に **加算** される。workspace 側で floor を **削れない**。
- 重複は大文字小文字を無視して dedup される (先勝ち)。
- マッチ規則は floor と同じ:
  - `registry-1.docker.io` … 完全一致
  - `.cosmos.azure.com` … サフィックス一致 (`<sub>.cosmos.azure.com`)

このスキルの責務は **既存値の温存** であって新規追加ではない。ユーザが
明示的に「`<domain>` を allow に追加して」と頼まない限り、`allowed_domains`
を勝手に編集しない。

### 6.6 secret-free チェック

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

## workspace-configure モード: 足りない kit のガイダンス

project が必要とするコマンドをカタログの kit が提供していない場合は以下を出力する:

```
[要対応] 以下の kit がカタログに見つかりませんでした:
  - python  (pyproject.toml が検出されました)

`boid kit init` を再実行すると Python 環境が検出され、kit が生成されます。
生成後に `boid workspace configure <slug>` を再実行してください。
```

---

## workspace-configure モード: よくある落とし穴

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

### BOID_WORKSPACE_PROJECTS が空・想定より少ない場合

このサンドボックスは daemon に接続できない。project 一覧は host 側の `boid workspace
configure` コマンドが起動前に取得して注入したスナップショットであり、サンドボックス内
から再取得はできない。想定と異なる場合は、host 側で `boid workspace assign <project>
<slug>` を実行してから `boid workspace configure <slug>` を再実行するようユーザに案内する。

---

## 関連コマンド

- `boid kit init` — kit-init モードを起動 (daemon 不要)
- `boid workspace configure <slug>` — workspace-configure モードを起動 (host 側 CLI は daemon が必要。サンドボックス内は不要)
- `boid kit list` — 生成した kit を確認
- `boid kit show <name>` — kit の詳細確認 (未実装。直接 Read で代用)
- `boid workspace show <slug>` — workspace の現在の設定を確認 (host 側 CLI から。サンドボックス内スキルからは呼ばない)
- `boid workspace assign <project> <slug>` — project を workspace に紐付け
