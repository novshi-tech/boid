# workspace home セットアップガイド

workspace ごとの永続 `$HOME` (workspace home) の作り方と、初回セットアップの手順です。
背景の設計は [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) を参照してください。

## workspace home とは

各 workspace には `boid-ws-home-<installID8>-<slug>` という専用の永続 **docker named volume**
があり、その workspace に属するプロジェクトの job (hook / exec / session いずれも) は
サンドボックス内の `$HOME` としてこの volume を read-write でマウントします。

> **2026-07-27 に変わりました**: 以前は `~/.local/share/boid/homes/<slug>/` という
> ホスト側ディレクトリでした。 container backend ではこのディレクトリが
> `$XDG_RUNTIME_DIR` (通常 tmpfs) 配下に解決されるため、**ホストを再起動すると
> 認証情報も toolchain も消えていました**。 named volume に移したことでこれが直っています。
> 旧ディレクトリは削除されず、そのまま残ります (移行 CLI が読むため)。

- **永続する**: 同じ workspace の job であれば、前の job が `$HOME` に書いたファイル
  (認証情報、パッケージキャッシュ、インストール済みツール等) は次の job でもそのまま見える
- **`$HOME/.boid` も例外ではない**: 他の `$HOME` 配下と同じく永続する。 Phase 6 PR8 以前は
  ここだけ job ごとの tmpfs で隔離されていたが、隔離の対象だったファイル経路
  (`$HOME/.boid/output/payload_patch.json`) 自体が撤廃されたため overlay も撤去済み
  (詳細は [`docs/ja/reference/hook-contract.md`](../reference/hook-contract.md) 参照)
- **volume の上には何も重ならない**: 以前は組み込み skill が
  `~/.claude/skills/<name>` に read-only で 1 本ずつ bind されていたが、現在は
  bind mount は 1 本も無い。組み込み skill も公式 Integration Pack
  (jira-cloud/bitbucket-cloud/slack) の skill も runner image に焼き込まれており
  (`/opt/boid/skills/<name>` と `/opt/boid/integrations/...`)、home の中には
  **symlink だけ**が置かれる (詳細は下の「非 embedded skill のコピーについて」参照)。
  job container が見るのは volume そのものだけ
- **workspace をまたいでは共有されない**: workspace A の `$HOME` と workspace B の `$HOME` は
  別 volume。ホストの実 `$HOME` とも共有されない (`boid` daemon 自身の `$HOME` とも別)
- **中身を直接見たいとき**: `docker volume inspect boid-ws-home-<installID8>-<slug>` の
  `Mountpoint` が実体のパス。 rootless podman では所有者がホスト側 subuid に写像されて
  いるので、読み書きには `podman unshare` が要ります

workspace を明示的に割り当てていないプロジェクトは `default` workspace の home を使います。

## init.sh の書き方

workspace に `init.sh` を置いておくと、その workspace への **最初の dispatch 時に自動実行**されます。
claude CLI のインストールなど、workspace home 側に一度だけセットアップしておきたい作業に使います。

### 置き場所と編集方法

`init.sh` は **daemon が保持**します。 実体は daemon の config ディレクトリ
(`~/.config/boid/workspaces/<slug>/init.sh`、`$XDG_CONFIG_HOME` が設定されていれば
そちら) ですが、これは **daemon 自身のファイルシステム上の**パスです —
container デプロイでは daemon の state volume の中なので、**ホスト側の
`~/.config/boid/` を編集しても反映されません**。

編集は CLI 経由で行います:

```bash
# 現在の内容を見る (ファイルに落とすなら -o init.sh)
boid workspace get-init-script <slug>

# ローカルのファイルから登録する (- で stdin)
boid workspace set-init-script <slug> -f init.sh

# $EDITOR で直接編集する (保存時に適用)
boid workspace edit-init-script <slug>

# 削除して「init.sh 無し」の状態に戻す
boid workspace unset-init-script <slug>
```

`set-init-script` / `edit-init-script` / `unset-init-script` は書き込み前に現在の
リビジョン (ETag) を確認するので、読んでから書くまでの間に他の経路で内容が変わっていれば
上書きせずにエラーになります (`--force` で無効化)。

> **`docs/examples/workspace-home-init.sh` を更新しても既存 workspace は変わりません。**
> あれは**リファレンス実装**であり、daemon が実行するのは登録済みのコピーです。
> boid 本体で例が改善されても、既に運用している workspace には
> `boid workspace get-init-script <slug>` で現物を取り出し、差分を当ててから
> `boid workspace set-init-script <slug> -f <file>` で再登録する必要があります。

### Node / pnpm を使う workspace

リファレンス実装の `install_node` は volta の shim (`node` / `npm` / `npx` /
`pnpm` / `yarn`) を `$HOME/.local/bin` へ張ります。PATH に入っているのは
`$HOME/.local/bin` であって `$HOME/.volta/bin` ではないため、この張り替えが無いと
`sh: pnpm: not found` になります。

pnpm を使う場合は、それに加えて **workspace の env に `VOLTA_FEATURE_PNPM=1`**
が要ります。volta 2.x の pnpm サポートは experimental で、このフラグが無いと
shim が `Could not find executable "pnpm"` で落ちます。

```bash
# workspace の env に足す (export → 編集 → apply)
boid workspace export <slug> > ws.yaml
# spec.env に VOLTA_FEATURE_PNPM: "1" を追記してから
boid workspace apply -f ws.yaml
```

また、`package.json` の `volta.node` などで image に無いバージョンを pin して
いる場合、volta は `nodejs.org` からツールチェーンを取得します。このドメインは
組み込みの allowed_domains floor に含まれています。

`boid workspace export` した yaml には `spec.init_script` として **init.sh の内容も
含まれます**。 `boid workspace apply -f <file>` で復元でき、これが workspace ごと
別 install へ移す経路です。 `spec.init_script: ""` (明示的な空文字列) は
「この workspace に init.sh は無い」の意味で、apply 時に既存の init.sh を削除します。
キー自体が無い場合は現在の init.sh に触れません。

空の init.sh は保存できません — 空の内容を書くと「init.sh 無し」として削除されます
(空のスクリプトと未設定は dispatch から見て同じ状態なので、2 つの表現を持ちません)。

`init.sh` の上限は **128 KiB** で、これは init.sh を保存する**すべての経路**に効きます
(専用エンドポイントでも、apply する文書の `spec.init_script` でも同じ)。 daemon は hash と
heredoc への埋め込みのために内容を全部メモリに載せるので上限自体が必要で、その値は
「受理した script は `workspace export` → `workspace apply` で必ず戻せる」ことから
逆算しています — export は script を yaml 文書に埋めるので膨らみ、`apply` はその文書を
1 MiB の body 上限で読むためです。 手書きの init.sh は数 KB (リファレンス実装で 5 KB 未満)
なので実害はありません。

この保証のもう半分は export 側にあります: export した文書が `apply` の 1 MiB を超える
workspace があると、**`boid workspace export` は該当 workspace 名を挙げて失敗します** —
復元できないファイルを黙って書き出すよりはエラーの方がましだからです。 script の取り分は
上の上限で抑えられているので、ここに引っかかるのは workspace の**メタデータ**が巨大な場合
(数百 KB の `env` 値など) に限られます。 メタデータを減らすか、`--all` をやめて
残りの workspace を `boid workspace export <slug>` で個別に export してください。

export が失敗するもう 1 つのケースが **daemon の版ずれ**です。 応答の文書に
`spec.init_script` が無い (= その daemon が PR9 以前で init.sh を知らない) 場合、
CLI は該当 workspace 名を挙げて失敗し、**ファイルを書きません**。 その文書は yaml として
妥当で apply も通ってしまいますが、復元されるのは metadata だけで **init.sh は戻らず**、
次の dispatch で harness が入らない workspace になるためです。 daemon を upgrade してから
export し直してください。 なお `boid workspace apply` の側は `spec.init_script` の無い
文書を従来どおり受理します — 手書きで `env` だけを直す文書に init.sh 全文を書かせない
ためで、そちらでは「キーが無い = 触らない」が正しい意味です。

API を直接叩く場合、body の `Content-Type` は **未指定でも構いません**。 拒否されるのは
yaml / json / xml / tar のような構造化データの type だけです (それらが来る場合、送り先を
間違えている — workspace の yaml 文書は `PUT /api/workspaces/{slug}`、boid.dev/v1 envelope は
`POST /api/workspaces/apply` です)。

`init.sh` を持たない workspace は「**スクリプト実行**は不要」として素通しされます。
ただし後述の準備ステップ (prep) は `init.sh` の有無に関わらず必ず 1 回実行されます。

### 実行契約

- **実行タイミング**: そのworkspace への最初の dispatch 時、**`init.sh` の内容が
  変わった時** (sha256 ハッシュで比較)、**workspace home 自体が初期化後に
  空になった / 別物に入れ替わった時** (次項)、および **boid が `init.sh` を実行する
  環境そのものを変えた時** (下記「初期化環境が変わったとき」)。完了マーカーは
  `~/.local/share/boid/homes-meta/<slug>.init.json` — workspace home ではなく **daemon 自身の
  データ領域** (`boid.db` と同じディレクトリ) に書かれる。 job が `$HOME` として
  マウントされるディレクトリの外なので、 **job が自分の `$HOME` 経由で触ることはできない**
- **マーカーは単独では信用せず、実体と突合する**: home volume を作るときに
  `boid.workspace_home_id` という label にランダムな識別トークンを載せ、同じ値を
  マーカーにも記録する。 初期化を skip するのは **script hash とこのトークンの
  両方が一致したとき**だけ。 したがって「マーカーだけ残って volume が消えた」状態では、
  次の dispatch は空の `$HOME` で agent を走らせるのではなく `init.sh` を再実行する。
  検出できるのは **volume の削除・再作成** (`docker volume rm`、reap の誤爆、
  workspace remove の半完了) であって、**volume が残ったまま中身だけ入れ替わったケースは
  検出できない** — volume の中身は daemon からは読めず (読むには container を起こすしかなく、
  それを毎 dispatch やると完了マーカーが存在する意味が無くなる)、検出できるのは
  daemon が engine に訊ける範囲だけだからです。
  なお **job による意図的な細工**は以前から防げていません (home 自体が job の
  書き込み領域である以上、構造的に防げない)。 その場合の影響も「同じ workspace の
  次の job が壊れた home で走る」ことに留まります (job は元々自分の home の中身を
  自由に消せる)。
  **boid が作った覚えのない volume** (label の無い、手で作られた `boid-ws-home-*`) を
  見つけた場合は、初期化を繰り返しても label を後から付けられない (Engine API に
  volume label の更新は無い) ため、boid は **dispatch をエラーで止めて報告します**
- **初期化環境が変わったとき**: 完了マーカーには「どの実行環境で初期化したか」を表す世代
  番号も記録される。 boid のバージョンアップで `init.sh` の実行環境が変わると
  (直近の例: ホスト上の daemon プロセスによる直接実行 → 使い捨て container 内での実行。
  これに伴い `$HOME` の値が job から見えるパスに変わった)、 マーカーの世代が古くなるので
  **その workspace で `init.sh` が 1 回だけ再実行される**。 toolchain 自体は既に home に
  あるので、冪等な `init.sh` なら短時間で終わる。 再実行が必要なのは主に、
  インストーラが `$HOME` 配下に**絶対パスを焼き込んだもの** (symlink の向き先、shebang、
  wrapper script) を新しいパスに合わせ直すため。 なお boid のバージョンが上がっただけでは
  再実行されない — 実行環境が変わったときだけ
- **同時実行の直列化**: 同じ workspace への複数 job が同時に初回 dispatch されても、
  `init.sh` の実行は 1 回だけ。同一 daemon プロセス内は flock で直列化し、
  **プロセスを跨ぐ分は決定的な container 名** (`boid-ws-init-<installID8>-<slug>`) の
  名前衝突で排他する (flock はプロセスが死ぬと解放されるが、init container は
  daemon の死を生き延びるため)。既存の init container がまだ動いていれば、
  次の dispatch はそれの完了を待ってから進む
- **実行環境**: **boid が起こす使い捨て container の中**で `bash` により実行される
  (trusted)。ホスト側での直接実行はしない。
  - image は **default の boid runner image**。 workspace の `container_image` override は
    ここでは使わない — 準備に要るのは bash と coreutils と `init.sh` が叩くものだけで、
    override を尊重すると override 側の失敗 (pull できない image、`boid.runner_protocol`
    label が古い等) が home の準備経路に載ってしまう。 job 1 本が動かないのではなく
    workspace が home そのものを用意できなくなる
  - workspace home がその container に mount され、`$HOME` はその **mount 先の path**
    (= job が sandbox 内で見る `$HOME` と同じ path) になる。
    したがって `$HOME` 配下に絶対 path を焼き込むツール (wrapper script、shebang、
    symlink 等) を入れても sandbox 内で壊れない
  - network は **engine の既定 bridge**。job 用の workspace network (`internal: true`、
    egress は allowlist proxy 経由のみ) ではないので、toolchain のダウンロードが通る。
    裏を返すと **init.sh は無制限の egress を持つ** — これは trusted 境界の一部として
    意図的にそうしている (script を選ぶのも container を起こすのも boid であり、
    sandbox 化された job の中で走るわけではない)
  - shebang 行は無視される。**boid は `init.sh` を直接 exec せず、hash した bytes を
    container 内の一時ファイルに書き出してから実行する**
    (実行内容とマーカー hash の同一性を保証するため)。そのため以下の制約がある:
    - `$0` は元の `init.sh` パスではなく container 内の一時ファイル path (`/tmp/...`) になる。
      `dirname "$0"` から `~/.config/boid/workspaces/<slug>/` 配下の補助ファイルを
      参照する記述は動かない
    - script 自身の配置場所 (`~/.config/boid/workspaces/<slug>/`) に依存する `source ./foo` や
      `$PWD` 依存のような書き方は避ける
    - 補助ファイルが必要ならすべて `init.sh` に inline するか、workspace home にすでにある
      ものを参照する
    - 一時ファイルは実行後に削除される
  - cwd は `$BOID_WORKSPACE_HOME` (workspace home) に設定される
  - **stdin は `/dev/null`**。`read` や `cat` で標準入力から読もうとしても何も来ない
  - 渡る環境変数は **次の 3 つだけ**:
    - `HOME` — workspace home (以降のインストールはここに着地させる)
    - `BOID_WORKSPACE_SLUG` — workspace の slug
    - `BOID_WORKSPACE_HOME` — `HOME` と同じ値

    `PATH` は image のものが適用される。**ホストの `PATH` / `USER` / `LOGNAME` /
    `LANG` / `LC_ALL` / `TERM` は渡らない** (旧仕様では渡っていた)。必要なら
    script 内で自分で設定すること。ホストにだけ入れてあるコマンドには到達できないので、
    image に無いツールは `init.sh` の中でインストールする
- **準備ステップ (prep) は必ず走る**: 同じ container の中で、あなたの script より前に、
  boid 自身が skill 探索ルート (`~/.claude` / `~/.claude/skills` / `~/.agents` /
  `~/.agents/skills`) を作成し、その下に各 skill の symlink を張る。
  `init.sh` を持たない workspace でもこの container は 1 回起動する。上記の識別トークンはここでは書かれない —
  volume の作成時に volume 自身の label に載るので、home の中にはトークンは存在しない
- **失敗時は dispatch も失敗する**: `init.sh` が非ゼロ終了すると、その dispatch は
  「黙って初期化なしで走る」のではなく明示的にエラーとして fail する
  (job は `failed`、task は `aborted` になる)。エラーメッセージには
  **どの段階で失敗したか** (`prelude` / `script-setup` / `init.sh`) と
  終了コードと出力の tail が含まれる。`init.sh` の終了コードはそのまま伝播する
- **成功時も出力が残る (ただし `log.level` が既定の `info` のときに限る)**:
  0 で終了した場合も、daemon のログに `workspace home init completed` として
  **出力の末尾 2000 バイト**が `output_tail` に記録される (それより前が
  切られた場合は `[boid: omitted ... bytes]` の注記が付く)。`init.sh` は
  「成功したのに home が壊れている」形で失敗しうる —— インストーラが
  警告だけ出して終了 0 を返す、といったケース —— ので、
  その調査の最初の手がかりはこのログになる。tail だけでは足りない場合、
  `config.yaml` で `log.level: debug` にすると、保持している全量
  (最大 1MiB) が別行 `workspace home init full output` として追加で出る
  ([設定リファレンス](../reference/config-yaml.md) 参照)。
  逆に `log.level: warn` / `error` にすると、この `output_tail` の行は
  (他の INFO ログと同様に) **一切出なくなる** — 静音運用にすると、この
  init 成功時の記録そのものが失われることに注意する

### script 作者が守ること

- **冪等であること**: 完了マーカーの破損や `init.sh` の再実行に耐えるようにする
  (「既にインストール済みならスキップ」を必ず自分でチェックする)
- **対話操作はしない**: `claude login` のような対話認証は `init.sh` の中ではできない。
  認証は下記の「初回ログイン」の手順で行う
- 中身は自由。ツールチェーンの設置 (claude CLI / go / volta / codex / opencode 等)、
  設定ファイルの配置など、boid は中身に関知しない

### 具体例

```bash
#!/bin/bash
set -euo pipefail

# claude CLI インストール (冪等: 既にあればスキップ)
if ! command -v claude &>/dev/null; then
  curl -fsSL https://claude.ai/install.sh | bash
fi
```

go / volta 経由の node / codex / opencode のインストールなど、より多くのツールチェーンを
入れたい場合も同じパターン (「既にあればスキップ」を各ツールごとに書く) を繰り返すだけです。

**リファレンス実装**: [`docs/examples/workspace-home-init.sh`](../../examples/workspace-home-init.sh)
に go / volta / node (lts) / claude / codex / opencode を全部セットアップする実装例が
あります (`GO_VERSION` 等を env で override 可能、RETURN trap で temp dir を掃除、
`command -v` による冪等性チェック付き)。 workspace の init.sh の雛形として
カスタマイズし、`boid workspace set-init-script <slug> -f docs/examples/workspace-home-init.sh`
で登録してください。

#### 非 embedded skill のコピーについて

boid 組み込みの skill (`/boid-task` 等) と公式 Integration Pack
(jira-cloud / bitbucket-cloud / slack) が宣言する skill
(`jira-api` / `bitbucket-api` / `slack-api`) は、どちらも runner image に
焼き込まれていて、workspace home init のときに **symlink** が張られます。
`init.sh` で扱う必要はありません (workspace home の中には実体を置きません)。

symlink の張り先は skill 1 つにつき 2 箇所です。

| 張り先 | 読むハーネス |
|---|---|
| `~/.claude/skills/<name>` | Claude Code、opencode |
| `~/.agents/skills/<name>` | codex、opencode |

2 箇所に張るのは、3 つのハーネスが共通して読むディレクトリが存在しないためです。
Claude Code は `~/.claude/skills` しか見ず、codex は `~/.agents/skills` しか
見ません (opencode は両方見ます)。 `~/.agents/skills` はベンダー横断の規約
なので、将来ハーネスを足すときはこちらで拾える見込みが高いです。

> **⚠️ 旧バージョンへのロールバックについて**
>
> skill が symlink になった home は、**image 焼き込み以前のビルドへ戻すと init が
> 失敗するようになります**。 旧ビルドの prep は `.claude/skills/<name>` を
> ディレクトリとして `mkdir -p` しますが、旧 image に `/opt/boid/skills` は無いので
> symlink が壊れた状態になり、**壊れた symlink に対する `mkdir -p` は失敗する**ためです
> (`mkdir: cannot create directory: File exists`)。
> その workspace の dispatch は以後すべて失敗し、完了マーカーを消しても直りません。
>
> 復旧するには、ホスト側から home volume 内の symlink を削除してください。
>
> ```bash
> VOL=$(docker volume inspect -f '{{.Mountpoint}}' boid-ws-home-<installID8>-<slug>)
> # rootless podman なら podman unshare 経由で
> rm -rf "$VOL/.claude/skills" "$VOL/.agents"
> ```
>
> 前方 (新しい方へ) の移行は自動です。 再 init が `rm -rf` してから `ln -sfn` するので、
> 旧 bind target のディレクトリは symlink に置き換わります。

> **これらの名前を下記の手動コピー手順で置くと、次の workspace home init で
> その手動コピーは symlink に置き換わって消えます** (boid が「stale な
> ディレクトリを新しい symlink で上書きする」動作を意図的にしているため —
> 詳細は `internal/dispatcher/skills_overlay.go` の `skillLinks` を参照)。
> 手動コピーが必要なのは、**公式 Pack が存在しないサービス**の独自 skill だけです。
> なお symlink は `<root>/<name>` という 1 エントリなので、同じディレクトリに
> 並べた手動コピーの skill を隠すことはありません。

一方、公式 Pack が無いサービスのホスト側にだけ置いてある独自 skill
(`~/.claude/skills/<name>/`) は `init.sh` からはコピーできません —
`init.sh` は boid が起こす使い捨て container の中で実行されるので、
ホストのファイルシステムにそもそも到達できないためです
(mount されているのは workspace home だけです)。

この種の skill を workspace で使いたい場合は、workspace セットアップ時に
**人間が手動で** ホストの skill をコピーしてください (公式 Pack の名前
`jira-api` / `bitbucket-api` / `slack-api` とは重ならない名前にすること):

```bash
# workspace home は named volume なので、init.sh の中で行うのが確実です。
# ホスト側から直接入れる場合は volume の Mountpoint を経由します:
VOL=$(docker volume inspect -f '{{.Mountpoint}}' boid-ws-home-<installID8>-<slug>)
mkdir -p "$VOL/.claude/skills"
cp -r ~/.claude/skills/my-custom-skill "$VOL/.claude/skills/"
```

## 初回ログイン

`init.sh` はツールのインストールまでしか行いません。claude / codex / opencode の
認証 (ログイン) は対話操作が必要なため、`init.sh` の中では完結できません。

workspace の home がまだ空の状態 (init.sh 実行直後など) で、一度だけ対話セッションを
起動してログインしてください:

```bash
boid agent claude -p <project-ref>
```

セッション内でハーネスの通常のログインフロー (ブラウザ認証など) をそのまま行えば、
認証情報はそのセッションの `$HOME` — つまり workspace home — に書き込まれ、
以降そのworkspace の job では認証済みの状態が永続します。

**ホストの `~/.claude.json` はコピーしません。** workspace ごとに独立してまっさらな
状態からログインするのが意図した契約です (workspace 間でホストの認証状態を共有しない)。

## 旧 workspace home (ホスト側ディレクトリ) の移行

named volume 化 (2026-07-27) より前の install には、workspace ごとの認証情報と
toolchain が `~/.local/share/boid/homes/<slug>/` に残っています。
volume 側は空のまま始まるので、中身を移すには移行コマンドを 1 回実行します:

```bash
boid workspace import-home <slug>                        # ~/.local/share/boid/homes/<slug> から
boid workspace import-home <slug> --from /path/to/backup # backup から復元する場合
boid workspace import-home <slug> --dry-run              # 何がどれだけ移るかだけ見る
boid workspace import-home <slug> --yes                  # 確認プロンプトを飛ばす (--force も同じ)
```

```
$ boid workspace import-home khi --dry-run
dry-run: would import /home/nosen/.local/share/boid/homes/khi into workspace "khi"'s HOME volume
  38412 files, 4102 dirs, 517 symlinks (1204 hard links), 4.3 GB
  nothing was sent and nothing was destroyed; re-run without --dry-run to migrate
```

### 動作

- CLI が `--from` のディレクトリを **読んで tar に固め、daemon にストリーム**します。
  daemon 側が volume を作り直し、使い捨て container に tar を流し込みます
- **bind mount は使いません**。rootless podman では container 内 uid とホスト uid が
  別物なので、bind すると `0600` の `.credentials.json` が読めません
  (volume-only pivot の発端と同じ罠)。tar なら「ホストのユーザが自分のファイルを読む →
  データとして渡る → container 側の uid で作り直される」ので mapping を跨げます
- **mode は維持されます** (`0600` は `0600` のまま)。所有者は移りません
  (job container と同じ uid で展開されます)
- **hard link は link のまま**送られます (node/volta 系 toolchain は hard link を
  多用するため、実体を二重に送らない)
- socket / FIFO / device ノードは復元できないので**スキップされ、名前が表示されます**
- **`--from` 自体が symlink の場合はリンク先を辿ります** (`homes/team-a ->
  /mnt/backup/team-a` のような、別ディスクに置いた home を指す普通の形)。 辿った先は
  確認プロンプトにも `--dry-run` の出力にも `typed (-> resolved)` の形で出るので、
  何を読んでいるかは実行前に分かります。 **home の中の symlink は辿りません** —
  そちらは home 自身の構造なので symlink のまま送られます (上記)

### 安全性

- **`--from` は読むだけです。** 削除も変更もしません。失敗しても元のディレクトリは
  そのままなので、何度でもやり直せます
- **その workspace で job が走っていると拒否されます。** engine は使用中の volume の
  削除を 409 で返し、その時点では**まだ何も壊れていません**:
  ```
  Error: import workspace home: workspace home "khi": refusing to migrate while a job is
  running in it — the engine is holding the volume "boid-ws-home-a1b2c3d4-khi" for a live
  container, and nothing has been changed. Wait for the job to finish (`boid job list`) and re-run
  ```
- **job が「起動しかけ」でも拒否されます。** engine が volume を使用中と見なすのは
  container が作られてからで、それより前 —— daemon が home volume を解決してから
  container を作るまでの間 —— は engine からは空いているように見えます。 その間に
  移行を通すと、job container は volume を**名前で** mount するので、展開中の volume を
  掴んだ job が起動してしまいます。 daemon は自前でこの区間を記録していて、同じく
  409 で断ります (`the workspace HOME volume is busy in this daemon`)。 これも
  **何も壊れる前**の拒否です
- **逆に、移行中に来た dispatch は失敗せず待ちます。** 移行は端末の前にいる人の操作
  なので断っても再実行 1 回で済みますが、dispatch は hook 評価から来るため断ると
  **job の失敗**として task に残ります。 待ち時間は移行の所要時間で頭打ちです
- **移行対象が 0 件なら拒否されます。** 空の tar を送ると、daemon は「body を読む前に
  home volume を破棄する」順序のせいで**空の home で置き換えて 200 を返します** ——
  認証情報も toolchain も消えたまま「imported」と表示される、この機能で一番まずい
  結果です。 そこで **CLI 側 (1 バイトも送る前・確認プロンプトより前)** と
  **daemon 側 (何も壊す前)** の両方で断ります。 原因は `--from` の typo、未マウントの
  backup、空のディレクトリなど様々ですが、結果は同じなので**結果の側で**断っています。
  意図して home を空にしたい場合は `boid workspace remove <slug>` して作り直してください
- **既存の home volume は破棄されます。** undo はありません。`--yes` を付けない限り
  確認プロンプトが出ます
- **途中で失敗した場合**は「一度も dispatch していない workspace」と同じ状態になります —
  途中まで展開された volume は**削除され**、完了マーカーも消えているので、次の dispatch が
  空から init.sh で作り直します。移行元は無傷なので、そのまま再実行するのが復旧手順です。
  volume を消すのは、途中まで展開された home が残ると `command -v claude` のような
  在庫確認で早期 return する冪等な `init.sh` が「もう入っている」と誤判定し、
  完了マーカーまで書いて**壊れた状態が固定される**からです
  (実体が未転送でも `.local/bin/claude` だけ届いていれば確認は通ってしまう)
- **その削除自体に失敗した場合**は、展開エラーと一緒に報告されます。 案内どおり
  `docker volume rm <volume>` を手で実行してから移行をやり直すのが最短ですが、
  放置しても次の daemon 起動時に下記の掃除が引き取ります
- **daemon が移行中に強制終了した場合** (SIGKILL / OOM kill / `docker compose down -t 0` /
  電源断) も、途中まで展開された volume は残りません。 daemon は**何かを壊す前に**
  `<dataHome>/homes-meta/<slug>.migrating.json` に「移行中」の記録を書き、進行に合わせて
  その `phase` を更新します:

  | `phase` | 書かれる時点 | 次回起動 (および dispatch) の動作 |
  |---|---|---|
  | `recorded` | まだ何も壊していない時点 | 記録を消すだけ。 home には触らない |
  | `home-destroyed` | 旧 volume の削除が成功した時点 | home volume・完了マーカー・記録を削除 |
  | `home-absent` | volume が無いことを確認した時点 | 記録を消すだけ。 home には触らない |
  | `home-rebuilt` | 展開が完了した時点 | 記録を消すだけ。 home には触らない |

  `home-destroyed` は「新しい volume を作る前」に書かれ「展開が終わった後」に書き換えられる
  ので、**どの時点で落ちても安全側に倒れます** —— volume が本当に中途半端な中身を持っている
  瞬間に落ちた場合だけ削除して init から作り直し、それ以外の時点で落ちた場合は home に
  手を触れません。 `phase` が無い記録 (これより古い boid が書いたもの) は
  `home-destroyed` として扱います —— それが当時の記録の意味だからです

  `home-absent` は「移行が途中で終わり、この名前の volume が無いことまで確認できた」
  状態です (展開失敗後に途中の volume を消せた場合など)。 **記録を消す前に**書かれるのが
  肝心なところで、記録の削除は「消えたように見えるが電源断で巻き戻りうる」失敗の仕方を
  するため、削除に頼って phase を放置すると、その後に作り直された正常な home を
  次回起動が破棄しうるからです
- **起動時の掃除が失敗しても、次の dispatch が引き取ります。** 起動時に engine が
  一時的に応答しないと volume の削除に失敗しますが、その場合も記録は残り、起動は
  そのまま続きます。 `home-destroyed` の記録が残っている workspace に dispatch が来ると、
  daemon は**init.sh を走らせる前に**もう一度削除を試み、成功すれば空の home から
  作り直します (engine が復旧していれば自動で直る)。 まだ削除できない場合は、その dispatch は
  **失敗します** —— 途中の home に init.sh を走らせて完了マーカーを書いてしまうより、
  job 1 本を失敗させる方が安いためです。 エラーには volume 名と
  `docker volume rm <volume>` が出ます。 なお起動時の削除には 1 件あたり 30 秒の上限が
  あるので、接続だけ受けて応答しない engine が daemon の起動を止め続けることはありません
- **記録が書けない (fsync できない) 場合は、何も壊さずに中止します。** 記録は
  「壊す直前」に書かれるので、ここで失敗しても workspace は無傷のままです。
  daemon の state 領域が書けるか確認してから再実行してください。 `home-destroyed` への
  更新が書けない場合も同様に中止します —— 旧 volume は既に消えていますが、「未完了と
  判別できない volume」に展開を始めるよりは、home が空のまま次の dispatch に
  作り直させる方が安全なためです。 なお `home-absent` への更新が書けない場合は
  **記録を消さずに残します** —— 同じ state 領域が書けない状況では削除も
  「消えたように見えて巻き戻る」向きに失敗するためで、残った記録は次の起動・dispatch が
  (存在しない volume に対して) 無害に片付けます
- **移行成功後に記録の削除だけ失敗した場合**は、その旨とファイル名が warning に出ます。
  通常はその前に記録が `home-rebuilt` に更新されているので **home は安全**で、次の
  daemon 起動はそのファイルを消すだけです (対処不要。 ただし state 領域が書けない状態
  自体は調べる価値があります)。 warning が**次の daemon 起動でこの home が破棄される**と
  言っている場合だけ記録が `home-destroyed` のまま残っているので、再起動前にそのファイルを
  消してください

### 移行後に init.sh が 1 回走ります

これは仕様です。移行は「中身のコピー」なので、それだけでは boid 側の完了マーカー
(script hash / 世代 / skeleton / volume identity / Integration Pack skill 一覧)
が**どれも変化しません**。
何もしないと、ホスト時代の絶対パスが焼き込まれた toolchain がそのまま生き残り、
`init.sh` は二度と走りません。そこで移行コマンドは 2 つの手段を**両方**取ります:

1. **volume を作り直す** — 新しい incarnation は新しい identity を持つので、
   マーカーの `home_id` と一致しなくなる
2. **完了マーカーを削除する** — `<dataHome>/homes-meta/<slug>.init.json`

片方が失敗しても、もう片方だけで再 init に倒れます。移行済みの home に対する
冪等な `init.sh` の再実行なので、通常は短時間で終わります。

**ただし「走る」ことと「直る」ことは別です。** 焼き込まれた絶対パスを実際に
書き換えるのは `init.sh` 自身であって、boid ではありません。2026-07-28 の
dogfood では、走ったのに `node` / `npm` / `npx` / `codex` / `yarn` / `pnpm` が
sandbox 内で全滅しました — volta が張った shim が旧 HOME の絶対パスを指したまま
dangling しており、当時のリファレンス実装が持っていた relativize 処理は
**現** `$HOME` prefix にしか反応しなかったためです。加えて `volta install` は
既存 shim の中身を検証せず exit 0 を返すので、`init.sh` の再実行では永久に
直りませんでした。

`docs/examples/workspace-home-init.sh` はこれを踏まえて、dangling した絶対
symlink を現 `$HOME` 配下の実体へ張り直す `rehome_dangling_symlink` を持ちます。
自作の `init.sh` を使っている場合は、**移行前の HOME を指す symlink が自力で
直る作りになっているか**を確認してください。

## PR8 を経由しない移行 (手動 `docker cp` / backup 復元) — マーカーを手で消す

`boid workspace import-home` を使わずに volume の中身を入れ替えた場合
(`docker cp` で直接流し込んだ、`docker volume` の Mountpoint を `podman unshare`
経由で書き換えた、volume そのものを backup から復元した、など)、
**boid はそれを検出できません**。

これは実装漏れではなく境界そのものです。daemon は volume の**中身**を見られません
(見るには container を起こす必要があり、dispatch のたびにそれをやると完了マーカーが
存在する意味が無くなる)。マーカーが指しているのは「**入れ物の incarnation**」であって
中身ではないので、「入れ物は同じまま中身だけ入れ替わった」ことは原理的に見えません。

したがって**その操作をした運用者が、完了マーカーを手で消す**必要があります。
消さないと `init.sh` は二度と走らず、流し込んだ中身に対する初期化が行われません。

**マーカーはホストから直接は消せません。** 置き場は daemon 自身の永続領域
(`<dataHome>/homes-meta/<slug>.init.json`) で、container デプロイではそれが
`boid_state` named volume の中にあるためです (ホスト側にパスがありません)。
デプロイ形態ごとの消し方:

```bash
# (a) container デプロイ (docker compose / scripts/deploy-container.sh) — daemon 経由
docker exec boid_daemon_1 rm -f /home/boid/.local/share/boid/homes-meta/<slug>.init.json

# (b) daemon が止まっている / exec できない場合 — 使い捨て container から volume を触る
docker run --rm -v boid_boid_state:/state docker.io/library/debian:bookworm-slim \
  rm -f /state/.local/share/boid/homes-meta/<slug>.init.json

# (c) bare `boid start` (ホストプロセス直接起動) — 普通のファイル
rm -f ~/.local/share/boid/homes-meta/<slug>.init.json
```

- volume 名 (`boid_boid_state`) は compose のプロジェクト名 (`boid`) + volume 名。
  `docker volume ls` で確認できます
- container デプロイでの daemon 側パスは `XDG_DATA_HOME=/home/boid/.local/share`
  (compose の env) + `boid/homes-meta/` です
- **より確実なのは volume ごと消すことです** — `docker volume rm
  boid-ws-home-<installID8>-<slug>` してから `boid workspace import-home` を使う。
  こちらは identity ごと変わるので、マーカーの消し忘れが起きません
- 消しても失うのは `boid_version` / `completed_at` の履歴だけです。マーカーが無ければ
  次の dispatch が再 init します (`init.sh` は冪等である前提)

## workspace の削除

`boid workspace remove <slug>` は workspace の定義 (DB row) に加えて home も削除します。

```
$ boid workspace remove my-workspace
home size: 128.4 MB (volume boid-ws-home-a1b2c3d4-my-workspace)
workspace remove "my-workspace" — 本当に削除しますか? [y/N]: y
workspace "my-workspace" removed (any assigned projects were re-assigned to "default").
home volume deleted (volume boid-ws-home-a1b2c3d4-my-workspace) (128.4 MB)
```

- **確認プロンプト**: home の有無やサイズに関わらず常に表示される
  (`--force` を付けたときのみスキップ)。`--yes` は `--force` のエイリアス
- **表示される識別子は volume 名**: workspace home は named volume なので、
  `docker volume rm` に渡せる名前をそのまま表示する
- **サイズ表示**: engine の `docker system df` が報告する volume サイズで、
  `du --apparent-size` と同じ意味論 (スパースファイルは論理サイズ、
  ハードリンクは 1 回だけ、シンボリックリンクはリンク先文字列の長さ)。
  厳密な block-based サイズではなく、あくまで目安
- **`default` workspace は削除できない**: 全プロジェクトが最終的に `default` へ
  再割り当てされる先であるため、予約済みとして保護されている
- **workspace の `init.sh` も一緒に削除される**: dispatch は init.sh を slug だけで
  引くので、残しておくと**同じ名前で作り直した workspace が古い script を継承**し、
  まっさらな HOME volume に対して実行してしまう。 home volume と同じ best-effort 扱いで、
  削除に失敗しても remove 自体は成功として返り、warning に daemon 側の path が出る
  (row は既に無いので `boid workspace unset-init-script` では消せない — daemon 側で
  手で消すことになる)
- **その workspace で job が走っていると削除に失敗する**: engine は使用中の volume の
  削除を 409 で拒否し、これは `--force` 相当のフラグでも剥がせない。 その場合は
  workspace の DB row だけが消えた**部分完了**状態になり、次のような warning が出る:
  ```
  warning: home volume delete failed (volume boid-ws-home-a1b2c3d4-my-workspace): ...volume is being used by...
  ```
  DB row は既に無いので `boid workspace remove` の再実行は 404 になる。 job の終了後に
  `docker volume rm boid-ws-home-a1b2c3d4-my-workspace` を手で実行する
- **その workspace で `boid workspace import-home` が走っていると 409 で拒否される**:
  移行は volume を消して**同じ名前で作り直し中身を書き込む**ので、削除と重なると
  「DB row は消えたのに認証情報入りの volume だけ残る」状態になる。 その volume は
  どの dispatch も mount せず、`boid workspace remove` でも消せない (row が無いので 404)。
  拒否は **DB row を消す前**に起きるので何も壊れていない。 移行の完了を待って再実行する
  ```
  Error: workspace home "my-workspace": refusing to remove this workspace while
  `boid workspace import-home` is replacing its HOME volume ...
  ```
  逆向き (移行中に削除が先) も同じく 409 で拒否される
- **削除できたか確認が取れなかった場合も黙らず報告する**: daemon が home volume の
  行方を答えられなかったとき — engine に到達できず確認できなかった、あるいは daemon が
  volume 化以前のバージョンでホスト側ディレクトリの話をしている — CLI はその旨と
  調べ方を出す:
  ```
  warning: could not confirm the home volume was deleted: the daemon reported no home volume name (a daemon older than the volume rewiring, or one with no engine handle)
    the workspace row is gone either way; check with `docker volume ls --filter label=boid.workspace_home=my-workspace` and remove any match with `docker volume rm <name>`
  ```
  特に効いてくるのは CLI と daemon のバージョンがずれているとき (CLI はリモート daemon
  も操作できるので起こりうる)。 volume は workspace slug をそのまま label に持つので、
  この label filter はバージョンに関係なく効く。
  一度も dispatch していない workspace には home volume がそもそも無く、そのケースは
  警告を出さない — daemon 側でこの 2 つは区別できるので、本当に不明なときだけ出る

## `boid gc` の workspace home 表示

`boid gc` (および `boid gc --dry-run`) の出力には、workspace home 一覧とそのサイズが
表示されます:

```
$ boid gc
deleted: 3 tasks, 5 jobs, 5 actions, 2 runtimes, 0 sandbox tmp entries
workspace homes:
  my-workspace:            128.4 MB
  (orphan) old-workspace:  4.1 MB
  total:                   132.5 MB
```

- **これは表示のみ**: `boid gc` は workspace home を**自動削除しません**
  (`runtimes/` とは違う扱い — workspace home は永続データという設計)
- **`(orphan)` フラグ**: home volume だけが残っていて対応する **DB workspace row が
  存在しない**状態を示す (`workspace.yaml` の有無ではなく DB 側で判定)。典型的には
  過去の boid で作成した workspace が既に DB から削除されたが home volume だけ
  残ったケース、または上記の「job が走っていて削除に失敗した」部分完了状態
- **一覧に出るのはこの install が作った home だけ**: volume 名は
  `boid-ws-home-<installID8>-<slug>` で install ごとに固有なので、1 つの engine を
  複数の boid install で共有していても互いの home は出てこない。
  逆に、install ID を持つ前に作られた `boid-ws-home-noinst-<slug>` は現行 daemon が
  mount しないため一覧にも出ない — 残っている場合は `docker volume ls` で確認して手動で消す
- orphan を実際に片付けたい場合は**手動で直接削除する**:
  ```bash
  docker volume rm boid-ws-home-<installID8>-<slug>
  rm -f <dataHome>/homes-meta/<slug>.init.json
  rm -f <dataHome>/homes-meta/<slug>.lock
  ```
  (古い boid が作った `~/.local/share/boid/homes/<slug>.init.json` /
  `.lock` が残っている場合もある。 現行 boid はこれを読まないので消しても消さなくてもよい)
  `boid workspace remove <slug>` は対応する DB row がないため 404 で失敗する
  (orphan の定義上、既に DB row は無いので)。 直接 rm するのが唯一の cleanup 経路
- **engine 呼び出しは 2 回あり、degrade も 2 通りある**。 一覧の取得
  (`docker volume ls` 相当) とサイズの取得 (engine 全体を走査する
  `docker system df` 相当の 1 回の呼び出し) は別のリクエストなので、engine に到達
  できなかったときの degrade はどちらが失敗したかで変わる:
  - **サイズ取得だけ失敗した場合**: 一覧はそのまま残り、各 entry が `?` になる。
    `?` は合計サイズの計算に含まれず (不明なサイズで合計を過小申告しないため)、
    エラーとしても扱わない (gc 全体は継続する)。 サイズは volume ごとではなく 1 回の
    走査でまとめて取るので、通常は all-or-nothing — 全 entry が `?` なら
    「その 1 回が失敗した」であって「volume が個別に読めない」ではない
  - **一覧の取得自体が失敗した場合**: 表示すべき一覧が無いので、表の代わりに理由を
    出し、gc 本来の削除結果は引き続き報告する:
    ```
    deleted: 3 tasks, 5 jobs, 5 actions, 2 runtimes, 0 sandbox tmp entries
    workspace homes: listing unavailable (list workspace home volumes: ...)
    ```
    daemon 側のログに残すだけでなく理由を表示するのは、一覧を省略しただけだと
    「まだ workspace home が 1 つも無い install」と見分けがつかないため

## 関連ドキュメント

- 設計の背景・契約全文: [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md)
- 親構想: [`docs/plans/container-based-boid.md`](../../plans/container-based-boid.md)
- workspace 全般の CLI リファレンス: [`docs/ja/reference/cli.md`](../reference/cli.md)
