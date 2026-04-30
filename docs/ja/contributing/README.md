# Contributing

`boid` への貢献を歓迎します。 issue でのバグ報告・機能提案、 PR でのパッチ、ドキュメントの改善、いずれもお待ちしています。

このページは外形的な手順とコーディング上のお約束をまとめています。設計の経緯は [概念](../guide/concepts.md) と [状態機械](../guide/state-machine.md)、 内部実装の話は (整備中の) アーキテクチャ章を参照してください。

## 開発環境

最低限必要なもの:

- Linux (`boid` は Linux 専用です)
- Go 1.24 以上
- `git`
- E2E を回す場合は `bash`、 `jq`、 `gh` があるとスムーズ

リポジトリを clone した後:

```bash
go test ./...        # ユニットテスト
go test -race ./...  # レースコンディション検出
go vet ./...         # 静的解析
go build ./...       # ビルド
```

`go install ./...` で開発中のバイナリを `$GOBIN` に入れて手元で動作確認できます。 daemon に変更を反映するには `boid stop && boid start` を忘れずに ([トラブルシューティング](../guide/troubleshooting.md#バグ修正をインストールしたのに変化がない) を参照)。

## コーディング規約

- **TDD**。先にテストを書き、失敗を確認してから実装する
- **外部依存は最小限**。標準ライブラリで実現できるものは追加せず、追加する場合は理由を PR で説明する
- **コミットプレフィックス**: `feat:` / `fix:` / `refactor:` / `test:` / `docs:` を使う
- **層境界を守る**: orchestrator と sandbox / dispatcher の間の依存方向は壊さない (詳細は (整備中の) アーキテクチャ章)
- **コメント**: 「何を」 ではなく「なぜ」 を書く。コードから読み取れることは書かない
- **エラーハンドリング**: 想定外の値は早期に返す。回復可能な失敗は呼び出し元が判断できるようラップする

## E2E テスト

`e2e/scenarios/` 配下に black-box シナリオが並んでいます。 全シナリオの実行:

```bash
./e2e/run.sh
```

特定シナリオだけ:

```bash
./e2e/run.sh project-smoke
```

サンドボックス内 (boid 経由) で開発中の場合、 `host_commands.run-e2e` の path match で host 側の broker に自動 dispatch されるため、サンドボックス内 Claude Code からも `./e2e/run.sh` でそのまま実行可能です (実体は host で動きます)。

新しい機能を入れたら、回帰テストとして E2E シナリオを足してください。シナリオの作り方は (整備中の) e2e ガイドを参照。

## PR を出す

1. **branch を切る**: `<topic>/<short-description>` (例: `fix/host-cmd-stdin`、 `feat/web-ui-pty`)
2. **コミットを分ける**: 1 コミットで 1 つの変更が筋が良いです。 fixup を含む雑多なコミットは squash してから送ってください
3. **PR description**: 何を / なぜ / どうやって / どうテストしたか を簡潔に。日本語で OK です
4. **CI が通ることを確認**: `go test`、 `go vet`、 (関係ある場合は) E2E が通る状態で送る
5. **review を受ける**: 指摘には反論せず即直すでも、別案を提案するでも歓迎します

`amend` や force push を含む履歴書き換えは原則しないでください。中間 commit を消したい場合は revert で対応します。

## バグ報告

issue を立てる前に、

- [トラブルシューティング](../guide/troubleshooting.md) を見て既知のパターンに該当しないか確認
- daemon log (`~/.local/state/boid/boid.log`) を覗いて関連ログを抜粋できるか確認
- 再現手順をまとめる

issue タイトルには影響を 1 行で書き、本文には:

- `boid` のバージョン (`go install` で入れた commit ハッシュなど)
- OS / ディストリビューション
- 再現手順
- 期待する挙動 / 実際の挙動
- daemon log の関連箇所抜粋

を入れてもらえると助かります。

## 機能提案

issue で「何を解決したいか」を先に共有してください。実装方針が大きい機能は、 PR を送る前にすり合わせておくと手戻りが少なく済みます。
