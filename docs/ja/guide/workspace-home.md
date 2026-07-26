# workspace home セットアップガイド

workspace ごとの永続 `$HOME` (workspace home) の作り方と、初回セットアップの手順です。
背景の設計は [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) を参照してください。

## workspace home とは

各 workspace には `~/.local/share/boid/homes/<slug>/` という専用の永続ディレクトリがあり、
その workspace に属するプロジェクトの job (hook / exec / session いずれも) はサンドボックス内の
`$HOME` としてこのディレクトリを read-write bind mount します。

- **永続する**: 同じ workspace の job であれば、前の job が `$HOME` に書いたファイル
  (認証情報、パッケージキャッシュ、インストール済みツール等) は次の job でもそのまま見える
- **`$HOME/.boid` だけは例外**: context/output ファイルのやり取りに使う `$HOME/.boid` は
  job ごとに新しい tmpfs が重ねてマウントされる。前の job が `$HOME/.boid` に書いたものは
  次の job には残らない (workspace home 本体とは別のライフサイクル)
- **workspace をまたいでは共有されない**: workspace A の `$HOME` と workspace B の `$HOME` は
  別ディレクトリ。ホストの実 `$HOME` とも共有されない (`boid` daemon 自身の `$HOME` とも別)

workspace を明示的に割り当てていないプロジェクトは `default` workspace の home を使います。

## init.sh の書き方

workspace に `init.sh` を置いておくと、その workspace への **最初の dispatch 時に自動実行**されます。
claude CLI のインストールなど、workspace home 側に一度だけセットアップしておきたい作業に使います。

### 置き場所

```
~/.config/boid/workspaces/<slug>/init.sh
```

(`$XDG_CONFIG_HOME` が設定されていればそちらが優先されます。`workspace.yaml` や
`host_commands.yaml` と同じ、ホスト側の config ディレクトリです — サンドボックスからは
見えず、書き換えることもできません。)

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
- **マーカーは単独では信用せず、実体と突合する**: 初期化に成功するたびに、
  workspace home 内の `.boid-workspace-home-id` にランダムな識別トークンを書き、
  同じ値をマーカーにも記録する。 初期化を skip するのは **script hash とこのトークンの
  両方が一致したとき**だけ。 したがって「マーカーだけ残って home が消えた」状態
  (home の置き場は daemon のデータ領域ほど永続とは限らない) では、次の dispatch は
  空の `$HOME` で agent を走らせるのではなく `init.sh` を再実行する。
  この突合が守るのは **workspace home が事故で消えた / 別物に置き換わったことの検出**
  (ホスト再起動でのディレクトリ消失、volume の削除、古いバックアップからの復元など) であって、
  job による意図的な細工ではない。 job はこのファイルを**読める**ので、値を控えたまま home の
  中身を消して書き戻す、という細工までは防げない — home 自体が job の書き込み領域である以上
  構造的に防げず、その場合の影響も「同じ workspace の次の job が壊れた home で走る」ことに
  留まる (job は元々自分の home の中身を自由に消せる)。 一方、このファイルを消す・壊す・
  別の種類のファイル (symlink や FIFO) に置き換えるといった操作で起きるのは
  **余分な初期化が 1 回走ること**だけで (init.sh は冪等が契約なので無害)、
  **boid 本体を止めることはできない** — boid はこのファイルを symlink として辿らず、
  通常ファイルであることと 1 KiB 以下であることを確かめてから読む
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
    使わない — override は `boid.runner_protocol` label を持つことを要求されるが、その label を
    焼いている image は現時点で存在しないため、override を尊重すると `container_image` を
    指定した workspace は home の準備そのものができなくなる
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
- **準備ステップ (prep) は必ず走る**: 同じ container の先頭と末尾で、boid 自身が
  `~/.claude` や `~/.claude/skills/<skill 名>` (組み込み skill の bind 先) の作成と、
  上記の識別トークンの書き込みを行う。`init.sh` を持たない workspace でも
  この container は 1 回起動する
- **失敗時は dispatch も失敗する**: `init.sh` が非ゼロ終了すると、その dispatch は
  「黙って初期化なしで走る」のではなく明示的にエラーとして fail する
  (job は `failed`、task は `aborted` になる)。エラーメッセージには
  **どの段階で失敗したか** (`prelude` / `script-setup` / `init.sh` / `postlude`) と
  終了コードと出力の tail が含まれる。`init.sh` の終了コードはそのまま伝播する

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
`~/.config/boid/workspaces/<slug>/init.sh` にコピーしてカスタマイズしてください。

#### 非 embedded skill のコピーについて

boid 組み込みの skill (`/boid-task` 等) は dispatch のたびに daemon が
boid バイナリから展開し、`~/.claude/skills/<name>` に **read-only で 1 本ずつ
bind mount** するので `init.sh` で扱う必要はありません (workspace home の中には
実体を置きません)。 skill ごとに分けて mount するのは、下記の手動コピーで置いた
非 embedded skill を隠さないためです。

一方、bitbucket / jira のようなホスト側にだけ置いてある独自 skill
(`~/.claude/skills/<name>/`) は `init.sh` からはコピーできません —
`init.sh` は boid が起こす使い捨て container の中で実行されるので、
ホストのファイルシステムにそもそも到達できないためです
(mount されているのは workspace home だけです)。

この種の skill を workspace で使いたい場合は、workspace セットアップ時に
**人間が手動で** ホストの skill をコピーしてください:

```bash
mkdir -p ~/.local/share/boid/homes/<slug>/.claude/skills
cp -r ~/.claude/skills/bitbucket ~/.local/share/boid/homes/<slug>/.claude/skills/
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

## workspace の削除

`boid workspace remove <slug>` は workspace の定義 (DB row) に加えて home ディレクトリも
削除します。

```
$ boid workspace remove my-workspace
home size: 128.4 MB (/home/you/.local/share/boid/homes/my-workspace)
workspace remove "my-workspace" — 本当に削除しますか? [y/N]: y
workspace "my-workspace" removed (any assigned projects were re-assigned to "default").
home dir deleted: /home/you/.local/share/boid/homes/my-workspace (128.4 MB)
```

- **確認プロンプト**: home ディレクトリの有無やサイズに関わらず常に表示される
  (`--force` を付けたときのみスキップ)。`--yes` は `--force` のエイリアス
- **サイズ表示**: `apparent size` (`du --apparent-size` 相当。スパースファイルの
  実ブロック数ではなく、ファイルの見かけ上のバイト数の合計) — 厳密な block-based
  サイズではなく、あくまで目安
- **`default` workspace は削除できない**: 全プロジェクトが最終的に `default` へ
  再割り当てされる先であるため、予約済みとして保護されている

## `boid gc` の workspace home 表示

`boid gc` (および `boid gc --dry-run`) の出力には、`~/.local/share/boid/homes/` 配下に
実在する workspace home 一覧とそのサイズが表示されます:

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
- **`(orphan)` フラグ**: home ディレクトリだけが残っていて対応する **DB workspace row が
  存在しない**状態を示す (`workspace.yaml` の有無ではなく DB 側で判定)。典型的には
  過去の boid で作成した workspace が既に DB から削除されたが home ディレクトリだけ
  残ったケース
- orphan を実際に片付けたい場合は**手動で直接削除する**:
  ```bash
  rm -rf ~/.local/share/boid/homes/<slug>/
  rm -f ~/.local/share/boid/homes-meta/<slug>.init.json
  rm -f ~/.local/share/boid/homes-meta/<slug>.lock
  ```
  (古い boid が作った `~/.local/share/boid/homes/<slug>.init.json` /
  `.lock` が残っている場合もある。 現行 boid はこれを読まないので消しても消さなくてもよい)
  `boid workspace remove <slug>` は対応する DB row がないため 404 で失敗する
  (orphan の定義上、既に DB row は無いので)。 直接 rm するのが唯一の cleanup 経路
- サイズ計算に失敗した場合は `?` と表示され、合計サイズの計算にも含まれない
  (エラーとして扱わず、gc 全体は継続する)

## 関連ドキュメント

- 設計の背景・契約全文: [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md)
- 親構想: [`docs/plans/container-based-boid.md`](../../plans/container-based-boid.md)
- workspace 全般の CLI リファレンス: [`docs/ja/reference/cli.md`](../reference/cli.md)
