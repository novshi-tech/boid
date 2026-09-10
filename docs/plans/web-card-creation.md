# WebUI の新規作成を Card にする

2026-09-10 実装。

WebUI の新規作成は、目的・課題を Card として受け取り、所属メタプロジェクトの
judge に次の一手を考えさせる。CLI/API と Go による実行タスク作成は従来どおり。

## 作成先

- フォームは workspace と、その workspace に所属するメタプロジェクトを選ぶ。
- `card_events.command` と対応する `card_commands` を持つ project を候補にする。
  khi などの既存定義に、新しい種別フラグを追加する必要はない。
- 各 workspace に組み込みの Default を用意する。Web の作成先一覧取得時に
  未作成分を DB トランザクションで準備するため、後から追加した workspace にも対応する。
- title / description / 添付を受け取り、`initial_status: parked` の Card を作る。
  behavior / agent / model / Auto Start はフォームから外す。
- 作成記録から既存の Card イベント経路を通じて judge が起動する。

## 標準 judge

`boid-card-judge` を同梱する。description と既存の Card 文脈から判断を始め、
外部シグナル・Sweep・独自判断スキルを前提にしない。

判断に必要な情報が欠けている場合は、既存の **`boid task ask`** を使う。
judge タスクが回答を待ち、同じ判断を継続する。新しい Card 状態や suggestion の
verb は追加しない。後の judge が同じ確認を繰り返さないよう、回答の要点は Card の
summary に残す。実行は既存の spec / Go 経路に任せる。

Default の判断方針は個別に編集しない。専用の判断方針が必要なら
`boid-metaproject` スキルで別のメタプロジェクトを作り、作成先として選ぶ。

## Default の実行環境

Default は workspace ごとの安定した ID を持つ、リポジトリ不要の project 行。
定義はバイナリから復元し、judge は workspace HOME 内で走る。Git URL や YAML の
用意は不要。workspace の実行環境・権限は通常の hydration を通すが、リポジトリ用の
branch / fork point / 追加 behavior は継承しない。

workspace export には組み込み project を含めず、apply でもその所属を解除しない。
別 workspace への付け替えは不可。workspace 削除時は空の Default を削除する。
Card・タスクが残っている Default は、データを保持するため workspace 削除を拒否する。

添付は Card 作成記録より先に保存する。判断タスクの添付取得は、token に結びついた
元 Card の添付も読める。同名ファイルが回答側にあれば回答側を優先する。

## 検証・反映

DB を使う Card 作成→judge request のテスト、リポジトリなしの判断タスク作成・
dispatch plan、再起動・workspace 追加・所属変更の検証を追加。
ブラウザ検証は `e2e/web/card-create.cjs`。

判断スキルは runner image に含まれるため、利用環境への反映時は daemon だけでなく
runner image もこの変更を含む版へ更新する。実モデルでの判断品質は実運用で確認する。
