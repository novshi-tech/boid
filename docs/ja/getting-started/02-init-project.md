# 2. プロジェクトを初期化する

> **お知らせ**: 旧 `boid init` ウィザードは廃止されました。
> 新しいセットアップフローは 3 段のコマンドに分かれています。
> 詳しくは [オンボーディング](../guide/onboarding.md) を参照してください。

このページでは新しい 3 段のフローでプロジェクトを立ち上げます。
[1. インストール](01-install.md) を完了している前提です。

## このページのねらい

- `boid kit init` でマシンの kit カタログを生成する
- `boid project init` で新規プロジェクトを作成する
- `boid workspace configure` で workspace 設定を生成する

## エージェントについて

`boid` のアーキテクチャは特定の AI エージェントに依存しない設計ですが、
現時点で実用的に動作確認が取れている agent は **Claude Code** のみです。
このチュートリアル以降は Claude Code が手元にあることを前提に進めます。
`claude` CLI が PATH にあり、Claude Code としてサインイン済みであることを確認してください
(Claude Code 側のセットアップ手順は [Claude Code の公式ドキュメント](https://docs.claude.com/en/docs/claude-code/overview) を参照)。

## ステップ 1: kit カタログを生成する

```bash
boid kit init
```

このマシンで使える kit カタログを生成します。
インストール済みの kit 一覧は `boid kit list` で確認できます。

## ステップ 2a: 新規プロジェクトを作成する

```bash
mkdir -p ~/boid-demo
boid project init ~/boid-demo --workspace dev
```

`--workspace dev` は workspace slug です。省略すると workspace への関連付けが後回しになります。

既存リポジトリにプロジェクトを作成する場合も同様です:

```bash
boid project init ~/src/myrepo --workspace dev
```

## ステップ 2b: 既存プロジェクトを登録する (既存 project.yaml がある場合)

`.boid/project.yaml` が既にある場合は `project add` を使います:

```bash
boid project add ~/src/myrepo --workspace dev
```

## ステップ 3: workspace を設定する

```bash
boid workspace configure dev
```

workspace 設定 (有効化する kit / env / host_commands など) を生成します。

## 生成された project.yaml を眺める

```bash
cat ~/boid-demo/.boid/project.yaml
```

おおむね次のような内容になっています:

```yaml
id: <uuid>
name: boid-demo
task_behaviors:
  dev:
    default_instruction:
      agent: claude-code
      message: |
        Implement what the task describes, commit on the current branch, and exit.
```

- **`task_behaviors`** — タスクの動作を定義する (詳細は [概念 / behavior](../guide/concepts.md#behavior))

登録済みプロジェクトの一覧 / 詳細は次で確認できます:

```bash
boid project list
boid project show boid-demo
```

## まとめ

このチュートリアルで触れた要素:

- **`boid kit init`** でマシンの kit カタログを生成
- **`boid project init`** で `.boid/project.yaml` を生成 + daemon 登録
- **`boid workspace configure`** で workspace 設定を生成
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
