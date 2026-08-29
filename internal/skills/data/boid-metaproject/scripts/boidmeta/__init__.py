"""boidmeta — boid のメタプロジェクトが共有する検知・記録の機構。

**このパッケージは組み込みスキル `boid-metaproject` の一部として runner image へ
焼き込まれ、全 sandbox の `~/.claude/skills/boid-metaproject/scripts/` に現れる。**
メタプロジェクト側のリポジトリへコピーするものではない —— コピーすると boid の
card 状態機械や action 語彙の写しがメタプロジェクトの数だけ増え、片方だけ古くなる。
khi で実際に起きた形: 状態遷移表が 4 箇所に散り、`PAYLOAD_CONSUMING` の欠落で
`drop-child` が 2026-08-23 に本番で無言に失敗した。

## 層

- `boid_store` —— boid CLI (sandbox の shim) の薄いラッパ。**`Any` を扱ってよいのは
  ここだけ**で、ここから先は型だけを相手にする
- `inbox` —— signal inbox の読み (`list`)・申告 (`claim`)・決着 (`ack`)
- `signal` / `event_key` / `screen` / `summary` / `childid` —— 値から値を作るだけ。
  標準ライブラリしか import しない
- `detect` —— 1 巡の対象を組む篩い (identity で束ねる・落とす・上限で切る)
- `sweep` —— 1 巡の骨格 (`scripts/sweep_targets.py` の実体)
- `write` —— 記録 CLI の実体 (`scripts/write.py` の実体)

## メタプロジェクトが持つもの / 持たないもの

**持たない**: 上記すべて。boid の card 語彙・action の形・inbox の契約に依存する
機構は、boid 本体と同じリポジトリで 1 つだけ維持する。

**持つ**: `.boid/project.yaml` (trigger / signals.sources / behaviors) と、
**判断そのもの**を書いたスキル。どの signal に何を提案するかは workspace ごとに
違うし、そこが唯一の付加価値でもある。
"""
