# 2. プロジェクトを初期化する

> **お知らせ**: 旧 `boid init` ウィザードは廃止されました。
> 新しいセットアップフローは **雛形生成 → push → git URL 登録** + (任意) workspace 設定です。
> 詳しくは [オンボーディング](../guide/onboarding.md) を参照してください。

このページでは新しいフローでプロジェクトを立ち上げます。
[1. インストール](01-install.md) を完了している前提です。

## このページのねらい

- `boid project init` で `.boid/project.yaml` の雛形を作る
- push してから `boid project add <git-url>` で daemon に登録する
- workspace の `init.sh` に claude CLI の自動インストールを登録する
- 専用の実行環境が必要な場合に `boid workspace create` / `edit` で workspace を用意する

## エージェントについて

`boid` のアーキテクチャは特定の AI エージェントに依存しない設計ですが、
現時点で実用的に動作確認が取れている agent は **Claude Code** のみです。
このチュートリアル以降は Claude Code を前提に進めます。

**注意:** ここで言う Claude Code は、host 側にインストールされた `claude` CLI のことではありません。sandbox 内で実際に動く claude は workspace ごとに独立した volume 上の `$HOME` を持ち、host の `~/.claude` や host の PATH 上の `claude` バイナリを一切見ません。host に `claude` がインストール済みでもされていなくても、このチュートリアルの進行には影響しません。sandbox 内に claude を入れる手順は下記のステップ 4 で扱います。

## なぜ「雛形生成」と「登録」が別コマンドになったか

daemon は compose スタックのコンテナ内で動いています ([1. インストール](01-install.md))。 コンテナには host 側の `~/boid-demo` のようなディレクトリは存在しないため、host パスを渡す登録方法は成立しません。daemon が project を読み込めるのは **git remote URL から自分で clone した場合だけ**です。そのため `boid project init` は「ローカルに雛形を書いて、push して登録するまでの次の一手を印字する」ところまでしかやりません。実際の登録 (daemon への通知) は、push が終わったあとの `boid project add <git-url>` が行います。

## ステップ 1: 雛形を作る

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
boid project init
```

(`boid project init <dir>` のように対象ディレクトリを引数で指定することもできます。) プロジェクト名を対話で尋ねられたあと、`.boid/project.yaml` を書き込みます。この時点では daemon には一切登録されません。

`--workspace` を付けると、次のステップで印字される `boid project add` の例に workspace 名が焼き込まれます (実際の workspace 割当は `project add` の実行時に行われる get-or-create):

```bash
boid project init --workspace dev
```

コマンドが終わると、次のような案内が印字されます (実際のコマンドは `<git-url>` を実際の URL に置き換えて実行してください):

```
Next steps:
  1. Make sure your project's actual source code -- not just this scaffold -- is already committed and pushed to your remote.
  2. Commit the scaffold and push it to your remote (safe to run even if some of this is already done):
       cd ~/boid-demo && { git init && git add .boid/project.yaml && ... && git push '<git-url>' HEAD && ... }
  3. Register the pushed URL with the running boid daemon:
       boid project add '<git-url>' --workspace=default
```

## ステップ 2: push する

案内された通り、まだ push していない実コードがあれば先に commit・push してください。真新しいディレクトリ (このチュートリアルの `~/boid-demo` 等) であれば、案内のコマンドをそのまま実行するだけで `.boid/project.yaml` の commit + push まで完了します:

```bash
cd ~/boid-demo && { git init && git add .boid/project.yaml && (git diff --cached --quiet -- .boid/project.yaml || git commit -m 'add boid project scaffold' -- .boid/project.yaml) && git push 'https://github.com/you/boid-demo.git' HEAD; }
```

このコマンドは既存のリポジトリに対しても安全に再実行できます (`origin` 等の remote 設定には一切触れません — 常に URL を直接指定した一回限りの push です)。 push 後、pushed したブランチがリモートの default branch と異なる場合は警告が表示されます。daemon は登録時にリモートの default branch を読みに行くので、その場合は先に PR/merge してから次のステップに進んでください。

## ステップ 3: daemon に登録する

```bash
boid project add 'https://github.com/you/boid-demo.git' --workspace=default
```

`--workspace` は必須です。省略はできません (get-or-create — 未知の slug でも空の workspace を自動作成してから project を紐付けます)。`--name` を省略すると URL の最後のパス要素 (`boid-demo`) から project 名が決まります。

daemon が bare repository として clone に成功すると、project の ID とワークスペースが表示されます:

```
project registered: <uuid> (boid-demo)
  workspace: default
  bare repo: /home/boid/.local/share/boid/...
```

## 既存プロジェクトを登録する場合

すでに `.boid/project.yaml` を持つ既存リポジトリであれば、ステップ 1 (`project init`) は不要です。push 済みの URL を直接登録するだけです:

```bash
boid project add 'https://github.com/you/existing-repo.git' --workspace dev
```

`.boid/project.yaml` がまだ無い既存リポジトリの場合は、ステップ 1 の `boid project init` をそのリポジトリのルートで実行してから (既存コードと一緒に commit・push して) 同様に登録してください。

## ステップ 4: workspace に claude CLI の自動インストールを登録する

タスクを実際に走らせる前に、この project が割り当てられた workspace (`--workspace` を省略した場合は `default`) に `init.sh` を登録しておく必要があります。**これを飛ばすと [4. 最初のタスク](04-first-task.md) で claude が `CLI not found` エラーで即座に失敗します** — sandbox 内の claude は host 側の `claude` バイナリを一切見ないため、workspace の volume に自分でインストールしなければなりません。

`go install` だけで入れたユーザは、このリポジトリのチェックアウトを持っていません。最小限の `init.sh` はその場で作成できます:

```bash
cat > init.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if ! command -v claude >/dev/null 2>&1; then
  curl -fsSL https://claude.ai/install.sh | bash
fi
EOF

boid workspace set-init-script default -f init.sh
```

(`--workspace dev` で登録した場合は `default` を `dev` に読み替えてください。)

登録した `init.sh` は、その workspace への**最初の** dispatch (最初のタスク/hook/exec/`boid agent` セッション) で自動的に実行されます — 冪等に書いてあるので、以降の dispatch では `command -v claude` が成功し何もしません。より本格的な例 (Go / Node / codex CLI のインストール、symlink の相対化等まで含む) は、このリポジトリの `docs/examples/workspace-home-init.sh` を参照してください (チェックアウトが無い場合は GitHub 上でファイルを直接開いて内容をコピーしても構いません)。詳細な契約は [workspace home ガイド](../guide/workspace-home.md) を参照してください。

## (任意) ステップ 5: workspace の中身を用意する

ステップ 3 で `--workspace dev` のような新規 slug を指定した場合、`dev` はすでに存在しています (get-or-create で空の workspace が作られ、project も紐付け済み)。 したがって中身を詰めるのは **edit** であって create ではありません:

```bash
boid workspace edit dev --from-file dev-workspace.yaml
```

(`boid workspace create dev --from-file ...` はこの時点では `409` になります — `dev` はすでに DB row を持っているため。`create` が使えるのは、まだ存在しない slug に対してだけです。)

`dev-workspace.yaml` の例:

```yaml
env:
  MY_TOKEN: "secret:my-token"
host_commands:
  - gh
allowed_domains:
  - example.com
```

`host_commands` はここでは **参照名**のリストであって定義そのものではありません — 各名前 (上の例では `gh`) はあらかじめ daemon 側の `~/.config/boid/host_commands.yaml` に定義されている必要があります。未定義の場合は [オンボーディング / host_commands を定義する](../guide/onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ) を参照してください。

中身の確認は `boid workspace show dev`、yaml として取り出すには `boid workspace export dev` を使います。詳細は [オンボーディング / workspace を作る・編集する](../guide/onboarding.md#workspace-を作る編集する) を参照してください。

その他のツールチェーン (Go / Node / codex CLI 等) の追加インストールも同じ `init.sh` 経由です (ステップ 4 参照)。

## 生成された project.yaml を眺める

```bash
cat ~/boid-demo/.boid/project.yaml
```

おおむね次のような内容になっています (wizard 組込みの雛形、`internal/initwizard/default_behaviors.tmpl`):

```yaml
id: <uuid>
name: boid-demo
default_task_behavior: supervisor
task_behaviors:
  executor:
    default_instruction:
      agent: claude-code
      message: |
        Implement what the task.yaml title and description ask
        for, then commit on the current branch (boid/<task_id8>,
        cut from the project's base branch) and exit. Do not
        push, do not open a PR — the parent supervisor merges
        the branch into the base branch locally.
  supervisor:
    default_instruction:
      agent: claude-code
      message: |
        Triage the request, create child executor tasks, and
        monitor them in order. Each child commits onto its
        boid/<task_id8> branch (cut from the base branch by
        boid's worktree feature). When a child reaches `done`:
          1. git checkout <base_branch>
          2. git merge --no-ff boid/<child_id8>
             -m "Merge boid/<child_id8>"
          3. Verify the merged result, then launch the next
             child.
        If a merge conflicts or the verification fails, reopen
        the child with `boid task reopen <child_id> -m "..."`.
```

- **`default_task_behavior`** — `behavior:` を省略した `boid task create` がどの `task_behaviors` エントリを使うか
- **`task_behaviors`** — タスクの動作を定義する (詳細は [概念 / behavior](../guide/concepts.md#behavior))。名前は自由 (free naming) — ここでの `supervisor` / `executor` は wizard 側のデフォルト名であって予約語ではない

登録済みプロジェクトの一覧 / 詳細は次で確認できます:

```bash
boid project list
boid project show boid-demo
```

## まとめ

このチュートリアルで触れた要素:

- **`boid project init`** で `.boid/project.yaml` の雛形を生成 (daemon への登録はしない)
- push してから **`boid project add <git-url> --workspace=<name>`** で daemon に登録 (`--workspace` は必須、get-or-create)
- **`boid workspace set-init-script`** で claude CLI の自動インストールを workspace に登録 (これをやらないと次章の最初のタスクが `CLI not found` で失敗する)
- 専用の実行環境が必要なら `boid workspace create` / `edit`
- 後から yaml を編集した場合は `boid project reload` で反映 (push した remote から daemon が再取得するには `boid project fetch <ref>`)

次の章では、ここで初期化したプロジェクトに対して Web UI のセットアップを行います。

## 後片付け (任意)

このチュートリアルだけを試したい場合の片付け:

```bash
boid project remove boid-demo
rm -rf ~/boid-demo
```

ただし、以降のチュートリアルでも同じプロジェクトを使うので、続けて読むなら残しておいてください。

---

次: [3. Web UI をセットアップする](03-web-ui.md)
