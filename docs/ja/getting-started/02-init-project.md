# 2. プロジェクトを初期化する

> **お知らせ**: 旧 `boid init` ウィザードは廃止されました。
> 新しいセットアップフローは project 登録 + (任意) workspace 設定の **2 段**です。
> `default` workspace で足りる場合は実質 **1 段**で終わります。
> 詳しくは [オンボーディング](../guide/onboarding.md) を参照してください。

このページでは新しいフローでプロジェクトを立ち上げます。
[1. インストール](01-install.md) を完了している前提です。

## このページのねらい

- `boid project init` で新規プロジェクトを作成する
- 専用の実行環境が必要な場合に `boid workspace create` / `edit` で workspace を用意する

## エージェントについて

`boid` のアーキテクチャは特定の AI エージェントに依存しない設計ですが、
現時点で実用的に動作確認が取れている agent は **Claude Code** のみです。
このチュートリアル以降は Claude Code が手元にあることを前提に進めます。
`claude` CLI が PATH にあり、Claude Code としてサインイン済みであることを確認してください
(Claude Code 側のセットアップ手順は [Claude Code の公式ドキュメント](https://docs.claude.com/en/docs/claude-code/overview) を参照)。

## ステップ 1: 新規プロジェクトを作成する

```bash
mkdir -p ~/boid-demo
boid project init ~/boid-demo
```

`--workspace` を省略すると `default` workspace に自動的に割り当てられます (daemon 起動時に `default` は常に存在が保証される)。`host_commands` / `env` / `allowed_domains` などを project 専用にカスタマイズしたい場合だけ、`--workspace` で専用 workspace を指定してください:

```bash
boid project init ~/boid-demo --workspace dev
```

`--workspace` は get-or-create です。`dev` workspace が存在しなければ空の workspace を自動作成してから project を紐付けます。

既存リポジトリにプロジェクトを作成する場合も同様です:

```bash
boid project init ~/src/myrepo --workspace dev
```

## ステップ 1b: 既存プロジェクトを登録する (既存 project.yaml がある場合)

`.boid/project.yaml` が既にある場合は `project add` を使います:

```bash
boid project add ~/src/myrepo --workspace dev
```

`project init` と同じく `--workspace` は get-or-create、省略時は `default` workspace です。

## (任意) ステップ 2: workspace の中身を用意する

ステップ 1 で `--workspace dev` を指定した場合、`dev` はすでに存在しています (get-or-create で空の workspace が作られ、project も紐付け済み)。 したがって中身を詰めるのは **edit** であって create ではありません:

```bash
boid workspace edit dev --from-file dev-workspace.yaml
```

(`boid workspace create dev --from-file ...` はこの時点では `409` になります — `dev` はすでに DB row を持っているため。`create` が使えるのは、まだ存在しない slug に対してだけです。例えばステップ 1 で `default` に登録して、今から**別の**新規 workspace を用意する場合はこちらを使い、続けて `boid workspace assign boid-demo <slug>` で project を紐付けてください。)

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

- **`boid project init`** で `.boid/project.yaml` を生成 + daemon 登録 (`default` workspace に自動割当)
- 専用の実行環境が必要なら `--workspace <slug>` (get-or-create) + `boid workspace create` / `edit`
- 後から yaml を編集した場合は `boid project reload` で反映

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
