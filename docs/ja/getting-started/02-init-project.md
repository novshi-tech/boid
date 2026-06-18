# 2. プロジェクトを初期化する

このページでは `boid init` でプロジェクトを 1 つ立ち上げます。 同じウィザード内で **kit** (拡張パッケージ) のセットアップも済ませるので、 ここを抜けると 「タスクを投げれば AI エージェントが動く」 状態のプロジェクトが出来あがります。 所要時間は 5 分ほどです。

[1. インストール](01-install.md) を完了している前提です。

## このページのねらい

- 公式 kit リポジトリをインストールする (オプション)
- `boid init` の対話ウィザードでプロジェクトを 1 つ作る
- 生成された `.boid/project.yaml` の内容を確認する

## エージェントについて

`boid` のアーキテクチャは特定の AI エージェントに依存しない設計ですが、 現時点で実用的に動作確認が取れている agent は **Claude Code** のみです。 このチュートリアル以降は Claude Code が手元にあることを前提に進めます。 `claude` CLI が PATH にあり、 Claude Code としてサインイン済みであることを確認してください (Claude Code 側のセットアップ手順は [Claude Code の公式ドキュメント](https://docs.claude.com/en/docs/claude-code/overview) を参照)。

## kit リポジトリをインストールする (オプション)

追加の hooks や host_commands が必要な場合は公式 kit リポジトリをインストールできます。

```bash
boid kit install github.com/novshi-tech/boid-kits
```

クローン先は `~/.local/share/boid/kits/github.com/novshi-tech/boid-kits/` です。 リポジトリ直下の各サブディレクトリがそれぞれ 1 つの kit になります (`claude-code` や `github-cli` など)。 kit 全体の仕組みは [Kit 作者向け 概要](../kit-authoring/overview.md) を参照してください。

インストール済みの一覧は次で確認できます:

```bash
boid kit list
```

## 作業ディレクトリを用意する

このチュートリアル専用のディレクトリを 1 つ作ります。

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
```

既存のリポジトリの直下で `boid init` する形でも問題ありません。

## `boid init` を走らせる

```bash
boid init
```

対話ウィザードが立ち上がります。 全プロンプトとも Enter で既定値が選ばれるので、 迷わなければそのまま進めて構いません。

```
Project name [boid-demo]:
Available kits (auto-detected marked with ✓):
  [✓] 1. Claude Code (github.com/novshi-tech/boid-kits/claude-code)
  [ ] 2. GitHub CLI (github.com/novshi-tech/boid-kits/github-cli) (optional)
  [ ] 3. Go development (github.com/novshi-tech/boid-kits/go-dev)
  ...
Enable/disable kits (space-separated numbers, prefix - to deselect, Enter to keep defaults):
>
Checking requirements...
  ✓ claude (/home/<you>/.local/bin/claude)

✓ Created /home/<you>/boid-demo/.boid/project.yaml
project registered: <uuid> (boid-demo)
```

順に何を聞かれているか:

1. **Project name** — Web UI 表示用の名前。 ディレクトリ名がそのまま既定値
2. **Available kits** — インストール済み kit のうち、 このマシンで動かせるものに `✓` が付いて自動選択されます (例: `claude` CLI が PATH にあれば Claude Code がオンに)。 番号を打って on/off を切り替えられます
3. **Requirements check** — 選んだ kit が必要とする host コマンドが PATH 上にあるかを確認

`task_behaviors.supervisor` / `task_behaviors.executor` の雛形は boid バイナリに内蔵されており、 kit のインストールなしで自動生成されます。

最後にウィザードが `.boid/project.yaml` を生成し、 `boid` の daemon に自動登録します。

## 生成された project.yaml を眺める

```bash
cat .boid/project.yaml
```

おおむね次のような内容になっています:

```yaml
id: <uuid>
name: boid-demo
worktree: true
kits:
  - github.com/novshi-tech/boid-kits/claude-code
task_behaviors:
  executor:
    default_instruction:
      type: execution
      agent: claude-code
      message: |
        Implement what the task.yaml title and description ask
        for, then commit on the current branch and exit. ...
  supervisor:
    default_instruction:
      type: execution
      agent: claude-code
      message: |
        Triage the request, create child executor tasks, and
        monitor them in order. ...
```

- **`worktree: true`** — executor タスクが専用の git worktree (ブランチ `boid/<task_id8>`) で動くことを示す
- **`kits:`** にウィザードで選んだ kit が並びます
- **`task_behaviors.supervisor` / `task_behaviors.executor`** が `boid` の 2 つの canonical な役割。 supervisor は readonly な統括役、 executor は書き込み可能な実装役です (詳細は [概念 / behavior](../guide/concepts.md#behavior))
- **`default_instruction`** はタスク作成時に agent に最初に渡る指示の雛形。 必要なら手で書き換えて `boid project reload` してください

登録済みプロジェクトの一覧 / 詳細は次で確認できます:

```bash
boid project list
boid project show boid-demo
```

## まとめ

このチュートリアルで触れた要素:

- **`boid kit install`** で公式 kit リポジトリをインストール
- **`boid init`** の対話ウィザードで `.boid/project.yaml` を生成 + 自動登録
- 生成された yaml の `kits:` / `task_behaviors` を確認
- 次に編集した場合は `boid project reload` で反映

次の章では、 ここで初期化したプロジェクトに対して Web UI のセットアップを行います。

## 後片付け (任意)

このチュートリアルだけを試したい場合の片付け:

```bash
boid project remove boid-demo
rm -rf ~/boid-demo
```

ただし、 以降のチュートリアルでも同じプロジェクトを使うので、 続けて読むなら残しておいてください。

---

次: [3. Web UI をセットアップする](03-web-ui.md)
