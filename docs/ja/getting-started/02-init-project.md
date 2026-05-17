# 2. プロジェクトを初期化する

このページでは `boid` で扱う **プロジェクト** を 1 つ初期化します。 `boid` は「タスク」を実行単位として動きますが、 タスクは必ず何らかのプロジェクトに属します。 まずプロジェクトを 1 つ用意しておくと、 以降のチュートリアルで作るタスクが居場所を持てます。 所要時間は 2 分ほどです。

[1. インストール](01-install.md) を完了している前提です。

## プロジェクトとは

ディスク上のプロジェクトは、 任意のディレクトリの直下に `.boid/project.yaml` を置いただけのものです。 リポジトリと 1:1 で対応させるのが典型的ですが、 単なる作業ディレクトリでも構いません。

`.boid/project.yaml` は最低限、 プロジェクトの識別子 (`id`) と、 そのプロジェクトで作れるタスクの種類 (`task_behaviors`) を宣言します。 hook / gate / kit といった実作業の定義はあとから足せます。

## 作業ディレクトリを用意する

このチュートリアル専用のディレクトリを 1 つ作ります。

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
```

既存のリポジトリの直下に `.boid/project.yaml` を置く形でも問題ありません。

## `.boid/project.yaml` を書く

最小構成の `project.yaml` を作成します。

```bash
mkdir .boid
cat > .boid/project.yaml <<'YAML'
id: demo
name: Demo
task_behaviors:
  supervisor:
    name: Supervisor
YAML
```

各フィールドの意味:

- **`id: demo`** — `boid` 内でこのプロジェクトを識別する名前。 `boid task create` などで `project_id: demo` として参照する
- **`name: Demo`** — Web UI / TUI に表示する人間向けの名前
- **`task_behaviors.supervisor`** — このプロジェクトで作れる「タスクの種類」を 1 つだけ宣言。 `supervisor` は canonical な 2 つの behavior 名のうち readonly な方で、 readonly フラグは canonical 名から自動導出されるため明示する必要はありません

実運用では behavior に hook / gate / kit を紐付けて、 AI エージェントを起動したりサンドボックスを開いたりします。 ここではまだ何も紐付けず、 [4. 最初のタスク](04-first-task.md) で Claude Code を呼ぶ kit を足す形で進めます。 `boid` のアーキテクチャ自体は特定のエージェントに依存しませんが、 現時点で動作確認が取れているのは Claude Code のみです。

## プロジェクトを登録する

`boid` の daemon にプロジェクトを認識させます。

```bash
boid project add .
```

成功すると `project added: demo` のような行が出ます。 `.` は現在のディレクトリ (`~/boid-demo`) を指し、 daemon はこのパス配下の `.boid/project.yaml` を読み込んで内容を取り込みます。

登録済みプロジェクトの一覧:

```bash
boid project list
```

詳細を見る:

```bash
boid project show demo
```

`id` / `name` / `task_behaviors` などが反映されているはずです。

## `project.yaml` を編集したとき

`project.yaml` は登録時に内容が daemon にロードされます。 ファイルを編集した場合は、

```bash
boid project reload
```

で全プロジェクトを再読み込みします。 daemon の再起動は不要で、 実行中のタスクも巻き込まれません。

## ローカル上書き (`project.local.yaml`)

リポジトリにコミットしたくない設定 (個人の追加 binding や環境変数) は `.boid/project.local.yaml` で上書きできます。 雛形は

```bash
boid project local init
```

で作れます。 詳細は [`project.yaml` リファレンス](../reference/project-yaml.md) を参照してください。 本チュートリアルでは使わないので、 ここでは存在だけ覚えておけば十分です。

## まとめ

このチュートリアルで触れた要素:

- **`.boid/project.yaml`** に `id` と `task_behaviors` を宣言した
- **`boid project add`** で daemon にプロジェクトを認識させた
- **`boid project list` / `show`** で登録内容を確認した
- **`boid project reload`** で `project.yaml` の編集を反映できることを覚えた

次のチュートリアルではここで初期化したプロジェクトを使って Web UI をセットアップし、 そのあと実際にタスクを動かします。

## 後片付け (任意)

このチュートリアルだけを試したい場合の片付け:

```bash
boid project remove demo
rm -rf ~/boid-demo
```

ただし、 次のチュートリアルでも同じプロジェクトを使うので、 続けて読むなら残しておいてください。

---

次: [3. Web UI をセットアップする](03-web-ui.md)
