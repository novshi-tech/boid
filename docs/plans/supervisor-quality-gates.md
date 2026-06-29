# supervisor quality gates (アイデアメモ)

落ち着いて取り組むまでの暫定メモ。 詳細設計は未着手。

## なぜ書いたか (背景)

2026-06-29 に判明した「workspace kit の `additional_bindings` が claude/codex/opencode harness session で無音で死ぬ」 退行 (PR #674 + #675 で修正) の振り返りから派生したアイデア。

退行の根因は **Phase 3-c の `feat: codex / opencode adapter を追加` (d464581) で「claude の挙動は既存と同等」 と claim しつつ、 実装では `expandedBindings` を `harnessBindings` で **置換** していた点**。 この置換は claude-code kit (boid-kits 由来) の binding には同等だったが、 **project.yaml 内の他 kit の `additional_bindings` を巻き添えで殺した**。 当時 reviewer は「同等」 claim を額面通り受け取って通し、 動作確認は 1-turn smoke だけだった。

つまり今の品質ガードは **「変更者が claim する範囲」 を超えた挙動変化を catch できない**。

## アイデア候補 (効果軸 × コスト軸)

### 高効果・低コスト

#### G1. supervisor に強制レビューゲート

子タスク (executor) の done payload を merge する前に **必ず `/code-review max` を走らせ、 critical/high finding があれば NO-GO**。 現状の supervisor は基本「子が done → PR を merge」 のフロー ([[boid-canonical-task-behaviors]])。 ここに independent reviewer を 1 段挟む。

実装スケッチ:
- supervisor skill 側で gate logic を追加 (skill 改修)
- もしくは task hook 経路で /code-review を呼んで finding を payload に詰める (より generic)

懸念:
- review 自体のコスト (token + 待ち時間)
- false positive 多発で gate が形骸化するリスク → effort=medium 既定で、 上げ下げ可能に
- 「review の review」 が要るか (= 加速度的にコスト増える) は要検討

#### G2. アーキ図系 memory の事前整備 + supervisor が参照

今回 [[workspace-kit-bindings-2-tier-wiring]] は事後に書いた。 事前にあれば「2 段配線」 を意識して d464581 の review もできた。 主要配線パス (kit / workspace / dispatch / hook / hydration) を memory に図化し、 supervisor が planning 段階で「触る配線図」 を declare するルーチンを入れる。

実装スケッチ:
- 重要配線の memory entry を 5-10 本書き起こす
- supervisor skill に「変更計画時、 触る配線図 memory を列挙してから start する」 を skill 化

懸念:
- memory 自体が腐る (配線が変わったら更新が要る) → 配線変更を PR で入れる時は対応 memory も同じ PR で更新するルールを明文化
- 「触る図を列挙する」 ステップを誰が enforce するか → supervisor skill の definition に組み込む

### 中効果・中コスト

#### G3. 「互換性 claim」 の semantic check

「同等」「互換」「Phase N の前提」 等の claim が PR 説明 / コミットメッセージに含まれるとき、 reviewer skill (G1) が **「何を同等と言っているか」 + 「cover されない経路はあるか」** を必ず明示要求する。 d464581 をこの skill で review してれば「他 kit の additional_bindings はどう扱われるか」 を必ず質問されてた。

実装スケッチ:
- /code-review の system prompt に「互換性 claim を含む場合は cover 範囲明示を必須化」 を追加
- もしくは独立 skill `claim-audit` として切り出し

#### G4. cross-feature e2e の matrix 化

[[boid-e2e-from-sandbox-uses-ci]] にあるとおり requires-sandbox e2e は CI のみ。 そこに「ある機能で作った成果物が、 別の機能で読まれる」 系の cross-feature シナリオを matrix で揃える。 今回なら kit init で kit を作る → `boid agent` / `boid exec` / `boid task` 各 harness で binding が見えること、 の 3 列 matrix を 1 シナリオで。

実装スケッチ:
- e2e/scenarios/cross-feature-kit-binding/ を追加
- fake harness で matrix 化 (重い実 LLM は走らせない)

### 低効果 (今回の bug には効かない)

#### G5. golangci-lint 等の標準静的解析

errcheck / gosimple / unused / ineffassign 等。 やるべきだけど今回の「意図的な分岐 + 間違った claim」 は catch できない。

### 高コスト (現時点で見送り)

#### G6. mutation testing

既存 test が「逆方向の振る舞い」 を catch するか自動検証。 hot path だけに絞っても運用負担大。

## 暫定推奨

落ち着いて取り組む時の優先順序案 (要再評価):

1. **G1 + G2 を同時に着手** が一番効くと推測。 単独だとそれぞれ穴がある:
   - G1 単独 → reviewer が背景文脈なしに判断し、 「同等 claim」 を再び額面通り受け取る危険
   - G2 単独 → 変更時の参照が規律依存でガード機構として弱い
   - 組み合わせ → 変更時 supervisor が「触る配線図」 を declare → reviewer はその図に書かれた経路全部の整合性を assert として確認

2. **G3 は G1 の skill 内に統合**。 独立 skill にすると review が 2 段になりコスト増。

3. **G4 は G1+G2 が動き出してから**。 投資効果は確実にあるけど急がない。

4. **G5 は別軸の話**。 並行で進めて可。

5. **G6 は当面見送り**。

## 未決事項

- G1 の gate を skill 側に置くか task hook に置くか (= 全 task に強制するか supervisor のみ強制するか)
- G2 の「触る配線図」 を declare する形式 (memory ref のリスト? plan doc に明示?)
- G1 の reviewer モデル / effort のデフォルト (max は token 重い、 high くらいで足りるかも)
- false positive 対策 (= NO-GO の override 経路をどう設計するか)
- 既存タスクへの影響 (G1 を strict にしたら現在動いてる task hook 経路の breaking change 度合い)
- doc としてどこまで詳細化してから着手するか (この doc の延長か別 plan doc を立てるか)

## 関連 memory

- [[workspace-kit-bindings-2-tier-wiring]] — 今回の退行のアーキ的整理 (G2 が事前に揃ってればこのスタイルで主要配線を網羅したい)
- [[boid-canonical-task-behaviors]] — supervisor / executor の責務分離 (G1 を挟む場所の context)
- [[boid-e2e-from-sandbox-uses-ci]] — e2e の現状制約 (G4 の実装条件)
- [[evidence-first-infra-suspicion]] — 「primary evidence で判断」 の習慣化が今回の振り返りの根底にある
- [[evaluation-no-anchor-stated-criteria]] — reviewer の検証バイアス (claim を額面通り受け取る) は評価バイアスの近縁
