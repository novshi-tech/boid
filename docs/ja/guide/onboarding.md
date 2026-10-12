# Workspace と repository のセットアップ

behavior、hook、trigger、signals.sources、card_commands、card_events、実行環境はすべて workspace DB から解決します。repository の project.yaml は読みません。

[登録と設定の手順](../getting-started/02-init-project.md) の envelope を参考に設定を書き、次を実行してください。

```bash
boid workspace apply -f workspace.yaml
boid project add https://github.com/you/repo.git --workspace dev
boid project show repo --explain
boid workspace export dev
```

`--workspace` は必須です。未知の slug は空の workspace として作成されるため、task 起動前に behavior を適用してください。新規 project ID は URL から導出します。名前を変える場合は `--name` を指定します。既存 ID は維持します。

設定は dispatch 時に解決します。`base_branch` は task 作成時に保存し、空なら agent の checkout が origin の default branch を使います。

`project init` と `project migrate` は撤去済みです。旧定義は workspace envelope の `spec` に移し、`boid workspace apply` で反映します。`project fetch` / `project reload` は設定を読みません。

ツールのインストールと認証は workspace の永続 HOME に保存します。[workspace home](workspace-home.md) を参照してください。
