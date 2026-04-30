# 2. 最初のタスク

このページでは AI エージェントを動かす前に、まずは `boid` のタスクライフサイクルだけを観察します。プロジェクトを 1 つ登録し、タスクを作って、状態が `pending → executing → verifying → done` と進む様子を CLI から手で確認します。所要時間は 5 分ほどです。

[1. インストール](01-install.md) を完了している前提です。

## ねらい

`boid` の主役は AI エージェントですが、その前にエージェントを動かす器であるタスクと状態機械の動きを掴んでおきます。 AI を呼び出さない分、何が起きているかが見えやすく、後続のチュートリアルで kit を入れたときも「いま状態機械はどこを動いているのか」が分かるようになります。

## プロジェクトを用意する

任意のディレクトリを作業場所にします。

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
```

`.boid/project.yaml` でこのディレクトリを `boid` のプロジェクトとして宣言します。

```bash
mkdir .boid
cat > .boid/project.yaml <<'YAML'
id: demo
name: Demo
task_behaviors:
  hello:
    name: Hello
    readonly: true
YAML
```

最小構成です。

- `id: demo` — `boid` 内でこのプロジェクトを識別する名前
- `task_behaviors.hello` — 「タスクの種類」が 1 つだけ。 hook も gate も紐付いていません
- `readonly: true` — このタスクが動くサンドボックスを書き込み禁止にします (今回はそもそも実行スクリプトが無いので影響しませんが、 副作用が無いことを宣言する意味で付けています)

`boid` に登録します。

```bash
boid project add .
```

成功すると "project added: demo" のような行が出ます。 `boid project list` で確認できます。

## タスクを作る

`boid task create` は YAML を標準入力で受け取ります。

```bash
boid task create <<'YAML'
project_id: demo
title: First task
behavior: hello
YAML
```

出力に表示される ID をメモしておいてください。以降の手順では `<task-id>` と書きます。

タスクを作っただけでは作業は始まりません。状態は `pending` です。

```bash
boid task list
boid task show <task-id>
```

`status: pending` と表示されているはずです。

## 状態を進める

`pending` から `executing` に進めるには `start` アクションを送ります。

```bash
boid action send --task <task-id> --type start
```

`task status: executing` と返ってきます。

ただし、この `hello` behavior には hook が紐付いていないため、 `executing` に入っても何も実行されません。 `boid task show <task-id>` で見ても `payload: {}` のままです。

ここで、本来なら hook が書き込むはずの完了シグナル (`artifact` trait) を、手で書き込んでみます。 [概念](../guide/concepts.md#payload-と-trait) のとおり、 payload に `artifact` が現れると `boid` は「executing 完了」とみなして自動で `verifying` に進めます。

```bash
echo '{"artifact":{"hello":"world"}}' \
  | boid task update <task-id> --payload-file -
```

戻ってきた status を見てください。

```bash
boid task show <task-id>
```

`status: done` になっています。流れとしては:

1. `artifact` が書き込まれた → `executing → verifying`
2. `verifying` で動く検証 handler が無い → そのまま `done` に通過

## 履歴を確認する

`boid` は状態遷移と payload の更新をすべて記録しています。 watch コマンドで時系列に流せます。

```bash
boid task show <task-id>
```

このタスクで実行されたジョブを見るには:

```bash
boid job list --task <task-id>
```

今回は hook を 1 つも紐付けていないので空です。後続のチュートリアルで kit を入れると、ここに各 handler のジョブが並びます。

## まとめ

このチュートリアルで触れた要素:

- **プロジェクト** を `boid project add` で登録した
- **behavior** を 1 つ宣言したが、 handler は紐付けなかった
- **アクション** (`start`) で `pending → executing` を手動遷移させた
- **payload patch** (`artifact`) を手で書き、 `executing → verifying → done` の自動遷移を観察した

実運用ではこれらは AI エージェントを呼ぶ hook が自動で行います。次のチュートリアルでは kit を導入して、 `boid` に実作業をさせます。

## 後片付け

このチュートリアルでできた状態を消したい場合:

```bash
boid task delete <task-id>
boid project remove demo
rm -rf ~/boid-demo
```

---

次: [3. プロジェクトと拡張パッケージ (kit)](03-projects-and-kits.md)
