# boid-supervisor / boid-executor 差分棚卸し

> Status: Phase 0 成果物 (2026-06-17 作成)。
> Track B (汎用スキル統合) の入力。
> 参照: docs/plans/agent-aware-boid.md 「設計判断 3」「Phase 0」(2)。

## 調査対象

| スキル | パス |
|---|---|
| boid-supervisor | `internal/skills/data/boid-supervisor/SKILL.md` + `references/` |
| boid-executor | `internal/skills/data/boid-executor/SKILL.md` (references なし) |

---

## 1. 両スキル共通の基盤

### 1-1. コンテキスト読み込み

両スキルとも起動直後に同一の 4 ファイルを読む。

| ファイル | 内容 |
|---|---|
| `~/.boid/context/task.yaml` | タイトル・説明・ステータス・behavior |
| `~/.boid/context/instructions.yaml` | インストラクション配列。**末尾要素が active** |
| `~/.boid/context/payload.yaml` | 既存アーティファクト |
| `~/.boid/context/environment.yaml` | サンドボックス制約 (readonly / network / tools) |

- active instruction はどちらも「instructions 配列の末尾要素」として統一。
- reopen 時も同じ: 新インストラクションが末尾に追記され、それが active になる。

### 1-2. notify ライフサイクル契約

3 つの exit 分岐は完全に共通。

```bash
boid task notify "$BOID_TASK_ID" --message "..." --done  "<achievement>"  # done へ
boid task notify "$BOID_TASK_ID" --message "..." --fail  "<what broke>"   # aborted へ
boid task notify "$BOID_TASK_ID" --message "..." --ask   "<question>"     # awaiting へ
```

- どちらのスキルも「裸のアシスタントテキストで終わるな」ルールが明文化されている。
- Stop hook が廃止された後も、notify が唯一の exit ゲートとして統一。
- `boid job done` / `boid agent stop` を直接呼ばないルールも共通。

### 1-3. Q&A パターン (asking owner)

```bash
boid task notify "$BOID_TASK_ID" --message "<short>" --ask "<body>"
```

- supervisor: `BOID_USER_ANSWER` で resume → ユーザの返答に基づいて続行。
- executor: 同上。
- 両スキルとも「notify --ask 後は即止まれ。sentinel も exit も不要」。
- `BOID_USER_ANSWER` と `BOID_QUESTION_ID` の使い方 (canonical branching) も同様。

### 1-4. Progress Reporting

```bash
boid task notify "$BOID_TASK_ID" --progress "<note>"
```

- 状態遷移なし・フック無し・タイムラインエントリのみ。
- 長時間タスクの経過通知として、両スキルで同じ API を使う。

### 1-5. Aborting vs Failing の区別

両スキルとも同じ 2 分類:

| 分岐 | コマンド | 使いどころ |
|---|---|---|
| 自己報告失敗 | `notify --fail` | **デフォルト**。structured report を書いてから失敗を申告 |
| 強制終了 | `boid action send --type abort` | サンドボックス起動失敗等、報告できる情報が皆無の場合 |

### 1-6. 共通ルール

- reopen 時: 末尾の新インストラクションが active、先行要素はコンテキスト専用。
- `instructions` フィールドには書かない (read-only delivery)。
- `boid task update --payload-file -` でアーティファクトを書き、その後 notify する順序は共通。

---

## 2. supervisor 固有

### 2-1. 計画立案 (Planning)

- タスク受領後、子タスクへの分解と順序を決定する。
- 解釈余地がある場合は `notify --ask` で承認を得てから実行。
  - ただし、子が 1 本で振る舞いが自明な場合はスキップ可。
- 計画テンプレ: 子タスク表 + リスク & 前提 + 決定選択肢 A/B/C の 3 ブロック固定。

### 2-2. 子タスク create / monitor / integrate

**create:**

```bash
boid task create <<YAML | awk '{print $3}'
title: <required>
behavior: executor
ref: <stable-slug>          # 必須: resume 時の冪等キー
description: <instruction>
auto_start: true
YAML
```

- `ref` は必須 (省略 = ハードエラー)。同一 `(ref, parent_id)` は冪等 (get-or-create)。
- `instructions` フィールドで子の instruction を部分上書き可 (1 エントリ = per-field merge)。

**monitor:** Monitor ツールでバックグラウンド監視。foreground sleep は禁止。

```bash
# Monitor script (抜粋):
while true; do
  st=$(boid task show "$CHILD" --field status)
  [ "$st" != "$prev" ] && echo "child -> $st"
  case "$st" in done|aborted) exit 0;; esac
  sleep 30
done
```

**integrate:** `done` 受信後に active instruction の統合ステップを実行。

### 2-3. Lifecycle Accountability (supervisor as owner)

子タスクのライフサイクルを supervisor が「所有」する。

| 子ステータス | supervisorの対応選択肢 |
|---|---|
| `done` | Layer A (artifact.report) + Layer B (git) + Layer C (size/log) で検証 → accept / reopen / abort / 上位 ask |
| `aborted` | 原因調査 → reopen with hint / 新子作成 / 上位 ask |
| `awaiting` | 子の question を読んで answer / reopen / 上位 ask |

- **root supervisor のみ**ユーザ向け notify hook が発火する (parent_id == "" 条件)。
- 子の awaiting は supervisor が受け取る (ユーザには届かない)。

### 2-4. stuck 子検出

executor が notify を呼ばずに静かに終了した場合のセーフティネット。
同じ Monitor ループ内で検出する:

- silent exit: `executing` のまま job が終了 (`ljs != "running"`)。
- PTY hang: `transcript_idle_seconds` が閾値超 (600 秒 = デフォルト)。

検出後:
- 再確認1コマンド → reopen with status-check / abort / 上位 ask。
- empty/cancelled 出力は stuck の証拠にしない (ハーネスのアーティファクト)。

### 2-5. 自分の done 申告のバリデーション

daemon が検証して reject する 2 ルール (supervisor 固有のハードガード):

1. open children が残っている間は `notify --done` が reject される。
2. fabricated commit / branch hash は reject される (`reported release commit ... does not exist`)。

### 2-6. hard cap (暴走防止)

- 子タスク > 20、または計画開始から > 12 時間 → `notify --ask` で停止。
- daemon はこれを強制しない: supervisor 自身の制御フローで実装。

### 2-7. references (supervisor のみ)

- `references/builtins.md` — `boid task` / `boid job` / `boid action` のフラグ一覧。
- `references/state-machine.md` — 子タスクのステータス・遷移・supervisor の対応表。

executor にはこれに相当する references ディレクトリがない。

---

## 3. executor 固有

### 3-1. 実装ループ (Implement → Verify → Release → Report)

```
Read context → Implement (edit files) → Verify (tests/lint)
  → Release (git commit ± push/PR per instruction) → Write report → notify
```

- supervisor は実装フェーズを持たない (readonly)。
- release step の具体的な形は active instruction が規定する
  (例: `git commit` のみ、または `/dev-pr-flow` 呼び出し)。

### 3-2. structured report の書き込み (executor 側の主責任)

```bash
boid task update "$BOID_TASK_ID" --payload-file - <<EOF
artifact:
  report:
    summary: "<1-3 lines>"
    evidence:
      pr_url: "..."
      commit_sha: "..."
      worktree_branch: "..."
    verification:
      tests_passed: true
      ci_status: "green|red|pending|unknown"
      manual_checks: ["..."]
    caveats: ["..."]
    open_questions: ["..."]
EOF
```

- supervisor はこの report を Layer A (canonical source) として検証する。
- `summary` 欠落 = missing-report anomaly → supervisor が reopen する。

### 3-3. worktree 制約

- `environment.yaml` に記載されたワークツリー内のみ編集。
- **必ずコミットしてから exit** (uncommitted changes はワークツリー除去で消える)。
- 子タスクを自分では生成しない (分解は supervisor の仕事)。

### 3-4. Aborting vs Failing の executor 固有強調

supervisor よりも executor スキルのほうが `action send --type abort` の
使いどころを明確に限定している:
「起動時サンドボックス破損等、本当に何も報告できないときだけ」。

---

## 4. readonly 制約下での振る舞い差

| 項目 | supervisor (readonly=true) | executor (readonly=false) |
|---|---|---|
| ファイル編集 | 禁止。git 読み取りのみ | ワークツリー内を自由に編集 |
| git 操作 | read-only (log / diff / show 等) | commit / push を含むフルセット |
| リリース | 不要 (子が行う) | active instruction が規定 |
| worktree | サンドボックスが provisioned されるが実質 read-only | provisioned + writable |
| `boid task create` | 使用する (コア動作) | 使用しない (子を作らない) |
| Monitor ツール | 使用する (子の監視) | 使用しない |
| stuck 検出 | 実装する (Monitor 内) | 不要 |

> Note: readonly フラグは behavior 名から自動付与されているが (`behavior_resolve.go`
> の `applyCanonicalBehaviorOverrides`)、実質的な違いは `task.Readonly` フラグが
> `dispatcher/sandbox_builder.go` で参照されてサンドボックスの書き込み可否を決める
> 点のみ (agent-aware-boid.md「設計判断 1」参照)。

---

## 5. 統合構成の推奨と根拠

### 推奨: 「共通基盤 + コンテキスト依存分岐の汎用 1 本」

**構成案:**

```
汎用スキル (例: boid-task)
├── [共通] context 読み込み・notify ライフサイクル・Q&A・progress
├── [分岐] task.readonly == false かつ parent_id あり (または supervisor からの instruction)
│   → executor モード: Implement → Verify → Release → Report → notify --done/fail
└── [分岐] task.readonly == true (または instruction が "plan/orchestrate" 型)
    → supervisor モード: Plan → create children → Monitor → Integrate → notify --done/fail
```

**根拠:**

1. **共通部分が重い**: Section 1 で示した通り、notify 契約・Q&A・progress・
   aborting vs failing の区別はほぼ完全に共通。この共通基盤を 2 本維持するコストが
   高い。

2. **分岐トリガーがシンプル**: Section 6 で列挙する通り、`task.readonly` フラグと
   `parent_id` の存在だけで動作モードが機械的に決まる。behavior 名での分岐は不要。

3. **「skill 1 本 + テンプレ多数」モデルとの整合**: agent-aware-boid.md 設計判断 1 の
   「ルートテンプレ複数共存モデル」は、スキルが behavior 名から動作を分岐させないことを
   前提とする。汎用 1 本 + コンテキスト分岐はこのモデルと直接対応する。

4. **動的 instruction 生成パターンとの相性**: 子タスクへの instruction を親スキルが
   動的生成するパターン (設計判断 3「動的 instruction 生成パターンへの転換」) は、
   汎用 1 本スキルが自分自身を「親モード」で動かしている場合に子タスクを作るフローと
   自然に対応する。

**「共通基盤 + 役割別モジュール」との比較:**

| 観点 | 汎用 1 本 + 分岐 | 共通基盤 + 役割別モジュール |
|---|---|---|
| ファイル数 | 1 SKILL.md (分岐あり) | 共通 md + supervisor module + executor module |
| 分岐点の明示性 | SKILL.md 内の if/when 節で記述 | モジュール境界で自然に分離 |
| free naming との整合 | ◎ behavior 名不問 | △ モジュール選択に新たな命名規約が要る |
| 統合リスク | 分岐条件の誤りが全モードに影響 | モジュール間インタフェース設計が要る |
| 保守性 | 共通修正が 1 箇所で済む | モジュール追加時の共通部拡張が楽 |

将来的に「review」「research」等の第 3 のモードが増える場合は役割別モジュール構成が
スケールするが、現状は 2 モードの分岐で十分。汎用 1 本からはじめて、必要に応じて
モジュール分割する進め方が低リスク。

---

## 6. task コンテキストからの自動判定ポイント一覧

Track B 実装時に汎用スキルが「どちらのモードで動くか」を決定するための情報源。

| 判定ポイント | 取得方法 | 判定ロジック |
|---|---|---|
| **readonly フラグ** | `environment.yaml` の `readonly` キー | `true` → supervisor モード (計画・監視) / `false` → executor モード (実装・コミット) |
| **parent_id** | `task.yaml` の `parent_id` | 空文字 → root タスク (ユーザ起点) / 非空 → child タスク (supervisor 起点) |
| **active instruction の内容** | `instructions.yaml` 末尾要素の `message` | "plan" / "orchestrate" / "supervisor" 等のキーワードが含まれる場合は監視モード寄り。ただし readonly フラグが主判定であり、instruction はヒントに留める |
| **behavior 名 (互換期間中のみ)** | `task.yaml` の `behavior` | `supervisor` → supervisor モード / `executor` → executor モード。free naming 解禁後は参照しない |
| **instructions 配列の長さ** | `instructions.yaml` | length > 1 → reopen 後の再実行。末尾のみ active、先行はコンテキスト |
| **BOID_USER_ANSWER 環境変数** | 実行時環境 | 非空 → `notify --ask` 後の resume。Q&A branching に入る |
| **artifact.children の有無** | `payload.yaml` の `artifact.children` | 存在 → resume 時の子タスク reconcile が必要 |

### 判定の優先順位

```
1. environment.yaml の readonly フラグ   ← 最高優先。サンドボックス制約の事実
2. task.yaml の behavior (互換期間中)    ← free naming 解禁後は廃止
3. active instruction のキーワード       ← ヒント。矛盾時は readonly を優先
```

### 注意点

- `readonly` フラグは daemon が behavior 名から自動付与しており (互換期間中)、
  agent が直接書き換えることはできない。したがって「`environment.yaml` の
  `readonly` を読む」は安全で信頼できる判定方法。
- 将来 Track A2 で free naming 解禁後は behavior 名からの自動付与が廃止される。
  その後は `environment.yaml` の `readonly` が唯一のモード判定根拠になる。
- `parent_id` は「誰がオーナーか」を示すが、モードそのものを決めない。
  root supervisor (parent_id == "") も child executor (parent_id != "") も存在する。
  モードは readonly で決まり、親子関係はライフサイクル契約のルーティングに影響する。

---

## 7. Track B 実装への示唆

### スキル名候補

`boid-task` (agent-aware-boid.md 候補) が適切。behavior 名に依存せず、
任意のタスクを動かす汎用スキルであることを表す。

### 実装順序

1. Section 1 の共通基盤を新スキルとして実装 (notify・Q&A・progress・aborting 分類)。
2. Section 6 の判定ロジック (readonly フラグ + parent_id) をスキル冒頭に実装。
3. executor モードを実装 (Section 3: Implement → Verify → Release → Report)。
4. supervisor モードを実装 (Section 2: Plan → create → Monitor → Integrate)。
5. stuck 検出を supervisor モードの Monitor ループに統合。
6. 既存プロジェクトでの並走検証 (旧スキルとの切り替え)。

### 注意すべき差分

- **stuck 検出**は supervisor 固有の複雑ロジック。実装誤りのリスクが高い。
  executor モードには不要なので、判定ガードで確実に分岐させること。
- **supervisor の done 申告バリデーション** (open children 禁止・commit hash 検証) は
  daemon 側ルールであり、スキル側では「wait for Monitor event から進め」という
  実装ガイドラインで対処する。
- **references** (builtins.md / state-machine.md) は supervisor モードでのみ参照。
  統合後も同ファイルを参照先として維持するか、スキル本文に内包するかは Track B 実装時に決定。
- **動的 instruction 生成パターン** (子タスクへの instruction を親スキルが動的生成) は
  新スキルのデフォルト挙動として実装する (設計判断 3 参照)。旧スキルでは
  project.yaml の `default_instruction` を子に渡す静的パターンだった。
