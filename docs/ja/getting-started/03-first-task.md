# 3. 最初のタスク

このページでは AI エージェントを動かす前に、まずは `boid` のタスクライフサイクルだけを観察します。タスクを作って、状態が `pending → executing → done` と進む様子を CLI から手で確認します。所要時間は 5 分ほどです。

[2. プロジェクトを初期化する](02-init-project.md) で `demo` プロジェクトを登録済みである前提です。

## ねらい

`boid` の主役は AI エージェントですが、その前にエージェントを動かす器であるタスクと状態機械の動きを掴んでおきます。 AI を呼び出さない分、何が起きているかが見えやすく、後続のチュートリアルで kit を入れたときも「いま状態機械はどこを動いているのか」が分かるようになります。

## タスクを作る

`boid task create` は YAML を標準入力で受け取ります。

```bash
boid task create <<'YAML'
project_id: demo
title: First task
behavior: supervisor
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

ただし、この最小構成では `supervisor` behavior に hook が紐付いていないため、 `executing` に入っても何も実行されません。 `boid task show <task-id>` で見ても `payload: {}` のままです。

ここでは本来 hook が成果物として書き込むはずの `artifact` trait を、手で書き込んでみます。 タスク自体は hook が無い (= `boid job done` の発火が無い) ため `executing` で止まったままになるので、 続けて `done` アクションで強制完了させます。

```bash
echo '{"artifact":{"hello":"world"}}' \
  | boid task update <task-id> --payload-file -

boid action send --task <task-id> --type done
```

戻ってきた status を見てください。

```bash
boid task show <task-id>
```

`status: done` になっています。実運用では hook が `artifact` を書いて正常終了 (`boid job done`) すると、 状態機械が `executing → done` を自動で遷移させます。

## 履歴を確認する

`boid` は状態遷移と payload の更新をすべて記録しています。

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

- **behavior** を 1 つ宣言したが、 handler は紐付けなかった
- **アクション** (`start` / `done`) で `pending → executing → done` を手動遷移させた
- **payload patch** (`artifact`) を手で書き、 タスクの成果物として残した

実運用ではこれらは AI エージェントを呼ぶ hook が自動で行います。次のチュートリアルでは kit を導入して、 `boid` に実作業をさせます。

## 後片付け

このチュートリアルで作ったタスクを片付けるには:

```bash
boid task delete <task-id>
```

`demo` プロジェクト自体は次のチュートリアルでも使うので残しておいてください。

---

次: [4. プロジェクトと拡張パッケージ (kit)](04-projects-and-kits.md)
