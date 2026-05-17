# 4. プロジェクトと拡張パッケージ (kit)

[3. 最初のタスク](03-first-task.md) では handler を 1 つも紐付けずに状態機械だけを動かしました。このページでは **kit** を導入し、 hook を介して実作業を `boid` に任せます。所要時間は 10 分ほどです。

## このページのねらい

- `kit` が `boid` で何を提供するか、概念ではなく実体として理解する
- `boid kit install` でリポジトリをインストールする
- `project.yaml` の `task_behaviors` に kit を紐付ける
- AI エージェント (Claude Code) を呼ぶ、最小の動作例を 1 本通す

## kit が何をパッケージしているか

ディスク上で kit は `kit.yaml` と数枚のスクリプトが入ったディレクトリです。 `kit.yaml` は次のような内容を宣言します。

- **hook** — どの状態 (`executing` など) で何のスクリプトを動かすか
- **gate** — どの状態遷移 (entry / exit) で何のスクリプトを動かすか
- **commands** — サンドボックス内で `boid exec` 経由から呼べるコマンド
- **host_commands** — サンドボックスから host 側に流せるコマンドの宣言
- **additional_bindings** — サンドボックスにマウントしたい追加パス
- **env** — サンドボックス内で設定する環境変数

つまり、 kit は「タスクのある状態で何を起動し、その実行に何を許可するか」 をまとめたパッケージです。プロジェクトは behavior に kit を紐付けることで、自前で hook を書かずに既製の動作を組み込めます。

公式の kit は [`github.com/novshi-tech/boid-kits`](https://github.com/novshi-tech/boid-kits) リポジトリにまとまっています。代表的なもの:

| kit ref | 役割 |
|---|---|
| `github.com/novshi-tech/boid-kits/claude-code` | Claude Code エージェントを hook で起動 |
| `github.com/novshi-tech/boid-kits/codex` | OpenAI Codex CLI エージェントを hook で起動 |
| `github.com/novshi-tech/boid-kits/go-dev` | サンドボックスに `~/go` などをマウント |
| `github.com/novshi-tech/boid-kits/github-cli` | サンドボックスから `gh` を使えるようにする |
| `github.com/novshi-tech/boid-kits/github-auto-merge` | executing → done の exit gate で `gh pr merge` を実行 |

## kit リポジトリをインストールする

`boid kit install` でリポジトリを `~/.local/share/boid/kits/<repo path>/` に `git clone` します。

```bash
boid kit install github.com/novshi-tech/boid-kits
```

クローン先のサブディレクトリがそれぞれ個別の kit になります。 `claude-code` を使うときの kit ref は `github.com/novshi-tech/boid-kits/claude-code` です。

インストール済み一覧:

```bash
boid kit list
```

## project.yaml に kit を書く

[2. プロジェクトを初期化する](02-init-project.md) で作った `~/boid-demo/.boid/project.yaml` を、 Claude Code エージェントを呼ぶ behavior に書き換えます。

```yaml
id: demo
name: Demo

kits:
  - github.com/novshi-tech/boid-kits/claude-code

task_behaviors:
  supervisor:
    name: Supervisor
    default_instruction:
      type: execution
      agent: claude-code
      message: |
        task の title と description に書かれた質問に答え、結果を artifact trait に書き込め:
          echo '{"artifact":{"answer":"<回答>"}}' \
            | boid task update <task_id> --payload-file -
```

ポイント:

- **トップレベルの `kits:`** には、 project 全体で使う kit を並べます。ここでは `claude-code` 1 つだけ
- **`task_behaviors.supervisor`** で canonical な readonly behavior を宣言。 readonly フラグは canonical 名から自動導出されるので明示する必要はありません (今回はファイルの編集ではなく回答だけ書ければよいため readonly で十分)
- **`default_instruction`** は `executing` 状態で claude-code エージェントに渡す指示の雛形 (単一 Instruction object)。 `agent: claude-code` で、 claude-code kit の hook が「自分宛の指示だ」と認識します

書き換えたら project を reload します。

```bash
boid project reload
```

## 動かしてみる

前提として `claude` CLI が PATH にあり、 Claude Code としてサインイン済みであることを確認してください (Claude Code 側のセットアップ手順は [Claude Code のドキュメント](https://docs.claude.com/en/docs/claude-code/overview) を参照)。

タスクを作って自動実行させます。

```bash
boid task create <<'YAML'
project_id: demo
title: Linux って一言でいうと？
behavior: supervisor
auto_start: true
YAML
```

`auto_start: true` を付けると、 `pending` を経ずに直接 `executing` に入ります。

別ターミナルで状態を流し見ます。

```bash
boid task watch <task-id>
```

しばらくすると hook ジョブの中で claude が動き、 `boid task update` で artifact が書き込まれ、 hook が正常終了すると自動遷移で `executing → done` に進むはずです。

最終結果:

```bash
boid task show <task-id>
```

`payload.artifact.answer` に回答が入っていれば成功です。

ジョブのログ (claude の出力) は次で見られます。

```bash
boid job list --task <task-id>
boid job show <job-id>
```

## まとめ

このチュートリアルで触れた要素:

- kit の中身 (hook / gate / commands / bindings / env)
- `boid kit install` でのリポジトリ取得
- `project.yaml` の `kits` と `default_instruction`
- `auto_start: true` で `pending` をスキップ

次は worktree と auto-merge を組み合わせた、 GitHub PR ベースの開発ワークフローに進みます。

## 後片付け

```bash
boid task delete <task-id>
boid project remove demo
rm -rf ~/boid-demo
```

`boid kit remove github.com/novshi-tech/boid-kits` でインストール済みリポジトリも消せますが、後続のチュートリアルで再利用するため残しておくのが便利です。

---

次: [5. GitHub PR ベースの開発ワークフロー](05-dev-workflow.md)
