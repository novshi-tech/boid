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

`init.sh` を持たない workspace は「初期化不要」として何もせず素通しされます。

### 実行契約

- **実行タイミング**: そのworkspace への最初の dispatch 時、および **`init.sh` の内容が
  変わった時** (sha256 ハッシュで比較。完了マーカーは
  `~/.local/share/boid/homes/<slug>.init.json` に書かれ、`$HOME` の外にあるためサンドボックスから
  改竄できない)
- **同時実行の直列化**: 同じ workspace への複数 job が同時に初回 dispatch されても、
  `init.sh` の実行は 1 回だけ (flock で直列化)。待つ側は完了を待ってから続行する
- **実行環境**: ホスト側 (trusted) で `/bin/bash` により実行される。
  shebang 行は無視される。 **boid は `init.sh` を直接 exec せず、hash した bytes を
  `~/.local/share/boid/homes/` 配下の一時ファイルにコピーしてから実行する** (symlink 経由の
  TOCTOU 対策と、実行内容とマーカー hash の同一性を保証するため)。
  そのため以下の制約がある:
  - `$0` は元の `init.sh` パスではなく一時ファイル path になる。 `dirname "$0"` から
    `~/.config/boid/workspaces/<slug>/` 配下の補助ファイルを参照する記述は動かない
  - script 自身の配置場所 (`~/.config/boid/workspaces/<slug>/`) に依存する `source ./foo` や
    `$PWD` 依存のような書き方は避ける
  - 補助ファイルが必要ならすべて `init.sh` に inline するか、workspace home にすでにある
    ものを参照する
  - cwd は `$BOID_WORKSPACE_HOME` (workspace home ディレクトリ) に設定される
  以下の環境変数が渡る:
  - `HOME` — workspace home ディレクトリ (以降のインストールはここに着地させる)
  - `BOID_WORKSPACE_SLUG` — workspace の slug
  - `BOID_WORKSPACE_HOME` — `HOME` と同じ値
  - 加えて `PATH` / `USER` / `LOGNAME` / `LANG` / `LC_ALL` / `TERM` はホストの値がそのまま渡る。
    それ以外のホスト環境変数 (ホストの `XDG_*` や `HOME` 等) は意図的に継承されない
- **失敗時は dispatch も失敗する**: `init.sh` が非ゼロ終了すると、その dispatch は
  「黙って初期化なしで走る」のではなく明示的にエラーとして fail する
  (job は `failed`、task は `aborted` になる)。エラーメッセージには終了コードと
  出力の tail が含まれる

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
workspace home へ自動同期するので `init.sh` で扱う必要はありません。

一方、bitbucket / jira のようなホスト側にだけ置いてある独自 skill
(`~/.claude/skills/<name>/`) は `init.sh` からはコピーできません —
`init.sh` はホスト側 (trusted) で実行されますが、その時点のホストの実 `$HOME` を
指す変数は渡っていないためです (`HOME` は既に workspace home に切り替わっています)。

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
  rm -f ~/.local/share/boid/homes/<slug>.init.json
  rm -f ~/.local/share/boid/homes/<slug>.lock
  ```
  `boid workspace remove <slug>` は対応する DB row がないため 404 で失敗する
  (orphan の定義上、既に DB row は無いので)。 直接 rm するのが唯一の cleanup 経路
- サイズ計算に失敗した場合は `?` と表示され、合計サイズの計算にも含まれない
  (エラーとして扱わず、gc 全体は継続する)

## 関連ドキュメント

- 設計の背景・契約全文: [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md)
- 親構想: [`docs/plans/container-based-boid.md`](../../plans/container-based-boid.md)
- workspace 全般の CLI リファレンス: [`docs/ja/reference/cli.md`](../reference/cli.md)
