# ロードマップ

このページは `boid` の現状と、これから手を入れていく方向性をまとめたものです。 確定済みのリリーススケジュールではなく、 「いま何を優先しているか」 の可視化が目的です。

## 現状

- 開発者本人 + 関係者の数名で日常的に使い、 boid 自身の開発を boid で回している段階
- ドキュメントは公開リリースに耐えうる最小セット (Getting started 01–04、 ガイド、 reference、 contributing、 kit-authoring overview、 architecture overview) を整備済み
- Web UI は Phase 1–3 が稼働中。 Cloudflare Tunnel 経由でスマホからの操作も日常的に行えている
- 公開先: GitHub `novshi-tech/boid`

## 段階

公開のしかたは段階的に広げていきます。

### 1. 直接サポート段階 (現在)

- 利用者は本人と直接連絡を取れる範囲に限定
- セットアップで詰まったら直接サポートする前提なので、ドキュメントの隙間 (例: kit ごとの細かい設定) は許容
- 主目的: 実利用での欠陥洗い出し、ドキュメントの欠落把握、 kit 構成のテンプレ化

### 2. 自立セットアップ段階

- 直接サポートなしでもセットアップから 1 タスク完了までたどり着けるようドキュメントと既定値を整える
- `boid init` を整備して、対話的にプロジェクトのスケルトンと kit セットを生成できるようにする
- README で謳う特徴を 1 タスク以内で全部体験できるチュートリアルを追加 (既存 04 の拡張、または別ページ)
- リリースタグ + バイナリ配布の検討

### 3. Public release 段階

- GitHub リポジトリを広く案内、 issue / discussion を受け付ける
- breaking change を入れる場合は事前告知 + マイグレーションパスを示す
- バージョニング規則 (SemVer 寄り) と changelog を維持する

## 近い将来に手を入れる領域

優先度は流動的ですが、現時点で「ここを磨く」と意識している領域です。

### Web UI Phase 4 (xterm.js / live attach)

- ブラウザから interactive PTY でエージェントセッションに接続する機能
- 既存の Phase 1–3 と異なり、双方向ストリーミングが必要なので技術的に大きめのピース

### レビューエージェント込みのワークフロー

- 「複数の AI エージェントが互いの成果物をレビューして finding を書き込み合う」 構成
- これが整うと、 [4. GitHub PR ベースの開発ワークフロー](getting-started/04-dev-workflow.md) の次の段として 「フィードバックループ」 のチュートリアルが書けるようになる
- 関連する設計トピック: payload trait の役割分担、 instruction routing の `consumer` 拡張

### 言語 / フレームワーク別 kit の拡充

- 現状の公式 kit は `claude-code` / `codex` / `go-dev` / `python-uv` / `dotnet-dev` / `volta` / `docker` / `github-cli` / `github-auto-merge` / `boid-tasks` 等
- ユーザのプロジェクトに合わせて、より多くの言語ツールチェーン kit を追加 / 整備していく予定

### ドキュメント

整備中ですが、優先度高めで埋めていく対象:

- `reference/cli/*` — 各サブコマンドの個別ドキュメント (現状は `--help` で代替)
- `reference/handler-contract.md` — hook / gate のスクリプトプロトコルの完全リファレンス
- `reference/traits.md` — payload trait の網羅仕様
- `architecture/persistence.md` — SQLite schema 詳細
- `architecture/sandbox-internals.md` — namespace / chroot / bind mount / proxy の詳細
- `adr/*` — 主要設計判断の記録 (script 廃止、 host command 解禁、 kits 外部リポ化、 consumes / produces 分離など)

## 当面やらないこと

「やらない」 を明示することで、設計判断のブレを減らすために書いておきます。

- **マルチユーザ / 組織機能**。 `boid` はパーソナル前提で、複数ユーザでひとつの daemon を共有する用途には設計を寄せていません
- **クラウド SaaS の提供**。配布は OSS のソースコード + バイナリのみで、共有サーバを運営する予定はありません
- **Linux 以外への対応**。 サンドボックス実装が Linux 固有 (mount namespace, chroot, unshare) のため、 macOS / Windows へのポートは想定していません
- **エージェント側の詳細制御を core に持ち込む**。 prompt の組み立て・モデル選択・ツール許可などは agent 側 (Claude Code 等) と instruction で表現する。 core は state machine と orchestration の責任に閉じる

## フィードバック

ロードマップに対する希望、 「ここを先に整備してほしい」 等の声は GitHub の issue で歓迎します。 詳細は [Contributing](contributing/README.md) を参照してください。
