# 5. 最初のタスク

このページでは Claude Code エージェントに小さな質問を 1 つ依頼します。 `boid` の本来の用途 — タスクを 1 行作って投げると、 サンドボックスの中で AI が動き、 結果が記録される — を最短で 1 周します。所要時間は 5 分ほどです。

[4. kit をセットアップする](04-kits.md) まで終えている前提です。

## このページのねらい

- 小さな質問タスクを 1 本作って `auto_start: true` で即実行する
- CLI (`boid task watch`) と Web UI の両方で進行を観察する
- `payload.artifact` に書き込まれた結果を確認する

## タスクを 1 本走らせる

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

### CLI で観察する

別ターミナルで状態を流し見ます。

```bash
boid task watch <task-id>
```

しばらくすると hook ジョブの中で claude が動き、 [4. kit をセットアップする](04-kits.md) で書いた指示に従って `boid task update` で artifact が書き込まれ、 hook が正常終了すると自動遷移で `executing → done` に進むはずです。

### Web UI で観察する

[3. Web UI をセットアップする](03-web-ui.md) で開いたブラウザの `http://localhost:8080` を再読み込みすると、 作成したタスクが一覧に乗っているはずです。 行をクリックするとタスク詳細・payload・ジョブ一覧がライブで更新されます。

## 結果を確認する

タスクが `done` になったら最終結果を見ます。

```bash
boid task show <task-id>
```

`payload.artifact.answer` に回答が入っていれば成功です。

ジョブのログ (claude の出力) は次で見られます。

```bash
boid job list --task <task-id>
boid job show <job-id>
```

Web UI からも各ジョブのログを開けます。

## まとめ

このチュートリアルで触れた要素:

- `auto_start: true` で `pending` をスキップして即実行
- 状態機械が `executing → done` を自動遷移するきっかけ (hook の正常終了 + artifact trait)
- 同じタスクを CLI と Web UI のどちらからも追跡できる

ここまでで Getting started のチュートリアルは一通り終了です。 さらに踏み込んだ構成例は [ワークフロー](../../workflows.md) を、 個々のフィールドや CLI の詳細は [リファレンス](../reference/project-yaml.md) を参照してください。

## 後片付け

```bash
boid task delete <task-id>
boid project remove demo
rm -rf ~/boid-demo
boid kit remove github.com/novshi-tech/boid-kits
```
