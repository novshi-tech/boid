# 4. kit をセットアップする

このページでは AI エージェント (Claude Code) を呼ぶための **kit** をインストールし、 [2. プロジェクトを初期化する](02-init-project.md) で作った `demo` プロジェクトに紐付けます。 ここまでで `boid` がタスクを受け取って Claude Code を起動できる状態になります。所要時間は 5 分ほどです。

[3. Web UI をセットアップする](03-web-ui.md) まで終えている前提です。

## このページのねらい

- **kit** が何をパッケージしているかを 1 つの実例で押さえる
- `claude-code` kit をインストールする
- `project.yaml` の `task_behaviors` に kit を紐付ける

## エージェントについて

`boid` のアーキテクチャは特定の AI エージェントに依存しない設計ですが、 現時点で実用的に動作確認が取れている agent は **Claude Code** のみです。

このチュートリアル以降は Claude Code が手元にあることを前提に進めます。 `claude` CLI が PATH にあり、 Claude Code としてサインイン済みであることを確認してください (Claude Code 側のセットアップ手順は [Claude Code の公式ドキュメント](https://docs.claude.com/en/docs/claude-code/overview) を参照)。

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
| `github.com/novshi-tech/boid-kits/go-dev` | サンドボックスに `~/go` などをマウント |
| `github.com/novshi-tech/boid-kits/github-cli` | サンドボックスから `gh` を使えるようにする |

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

## まとめ

このチュートリアルで触れた要素:

- kit の中身 (hook / gate / commands / bindings / env)
- `boid kit install` でのリポジトリ取得
- `project.yaml` の `kits` と `default_instruction` で kit を behavior に紐付ける
- `boid project reload` で編集を反映

次の章では、 ここでセットアップした構成でタスクを 1 本走らせて、 CLI と Web UI から実行の様子を観察します。

---

次: [5. 最初のタスク](05-first-task.md)
