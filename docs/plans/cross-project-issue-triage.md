# プロジェクト横断の課題トリアージ (メタプロジェクト + daemon inbox)

ステータス: **draft (第 6 版 2026-08-10 — Phase 1 実測チェックリスト完了・決定 7 を task 統合で
確定・「card」の固有名を廃止。 初版 2026-07-30)**。 Phase 0 dogfood 稼働中・Phase 1 未実装。
発端: nose の構想メモ (音声書き起こし、 2026-07-30) と同日の設計ディスカッション。
関連: [workspace-default-project.md](workspace-default-project.md) (workspace デフォルト project 定義)、
[volume-only-daemon.md](volume-only-daemon.md) §論点a/b (project の git URL 化・ bare repo・
workspace export/import)、 [home-workspace-volume.md](home-workspace-volume.md) ($HOME workspace
volume)、 [web-sessions.md](web-sessions.md) (Web UI セッション)。

用語注: 設計討議中は境界を越える最小単位を「封筒 (envelope)」と呼んでいたが、 コード上
`WorkspaceEnvelopeSpec` (workspace export YAML) が既に envelope の名を使っているため、 第 2〜5 版
では **card** と呼んでいた。 第 6 版で決定 7 が task 統合で確定したのに伴い、 card という固有名は
**廃止**する (一般的すぎる語であり、 内容でなく入れ物を指す語でもあるため — nose 指摘)。 以後は
**triage 段階の task** (短く triage task) と呼び、 固有フィールドの sidecar テーブル名は
`task_triage` とする。 第 5 版以前から残る本文中の「card」は「triage 段階の task」と読み替える。
なお代替候補だった issue は「Jira issue を ingest する」という本システムの文脈と確実に衝突する
ため不採用。

---

## 背景と課題

AI コーディングエージェントにより 1 プロジェクト内の生産性は 10〜20 倍になったが、 その結果
ボトルネックは実行からプロジェクト横断の注意配分に移った。 nose は現在 3 テーマ
(ビルメンテナンス会社の経営 / ソフトウェア開発 / 顧客プロジェクト) にまたがる 10 数個の
プロジェクトを並行して進めており、 以下の問題がある:

1. **次に着手可能なタスクと優先度の横断把握が困難**。 どのプロジェクトで何が待ちで何が
   着手可能かを知るには、 自分から各所を見に行くしかない。
2. **アクショナブルになる前の層に置き場が無い**。 従来のタスク管理システムは actionable な
   action item だけを扱う。 その手前のふわっとした課題感・ テーマ・ 経営課題を保持し、 時間を
   見つけて考えを進める、 という活動を支える仕組みが無い。
3. **今フォーカスが当たっているものに注意が引きずられる**。 全体を見渡して「本当に今やるべき
   こと」を選べていない。

情報源は分散している: メール (複数アカウント)、 Slack 等のコミュニケーションチャンネル、 Jira 等の
タスク管理システム (複数インスタンス)、 そして nose 自身の頭の中。

---

## 成功の定義

> **提示された課題に応えていくだけで仕事が進む。**

自分から見に行く負担を無くす、 完全 push 型。 成否は仕組みの賢さよりも **queue への信頼**で決まる:

- **網羅性**: 「見に行かなくても取りこぼしていない」と信じられること。
- **文脈同梱**: 1 項目 = 1 判断。 開いた瞬間に判断できるだけの文脈が同梱されていること。
- **Go 一発**: 実行可能なものは Go と言うだけで実行される。 実行可能でないものは対話
  (整形セッション) で実行可能な状態まで持っていける。
- **後回しの信頼**: スキップ・後回し (park) が 1 操作で済み、 指定した再浮上条件で必ず
  queue に戻ってくると信じられること。 queue は「今応えるべきもの」だけを見せる (決定 9)。
- **ふわっと層の前進**: 定期のテーマセッション (インタビュー形式) で課題感・ テーマについて
  考えを進められる。

---

## 全体像

```
┌────────────── workspace A (compartment) ───────────────┐
│ ingestion job (定期):                                   │
│   mail / Jira / Slack を workspace の secret で読む     │
│   → workspace 内で triage、 ノイズは card 化しない      │
│   → 起票分のみ card を push、 source 側は処理済み化     │
│ メタプロジェクト (普通の boid project):                 │
│   課題・テーマの本文ファイル、 triage 系の指示          │
│ 整形・テーマ・深掘りセッション (workspace 内の job)     │
└───────────────┬────────────────────────────────────────┘
                │ boid inbox push (brokered builtin、 card のみ)
                ▼
┌────────────── daemon (trusted core) ───────────────────┐
│ daemon inbox = card store (task DB と同じ信頼位置)      │
│   card: 見出し・緊急度・出所ポインタ・状態              │
│   workspace 印は daemon が押す (自己申告させない)       │
│   queue は store の上の提示 view (「今」の card だけ)   │
└───────┬─────────────────────────────▲──────────────────┘
        │ Web UI queue / notify        │ Go (= task create)
        ▼                             │ 整形・深掘り (workspace 内へ dispatch)
┌────── nose (認証済みデバイス) ───────┴──────────────────┐
│ 応える: Go / 整形 / 深掘り / 後回し (park) / 破棄       │
└────────────────────────────────────────────────────────┘
```

- workspace → daemon は card の一方通行 push のみ。 workspace の job は inbox を読めない。
- daemon → workspace の影響はすべて nose の操作 (Go / 整形開始) を経由する。
- 本文は workspace 内に留まり、 深掘りはその workspace のセッションを開いて行う。
- inbox は store、 queue は view。 後回しされた card は store に留まり、 再浮上条件で queue に
  戻る (決定 9)。

---

## 用語

| 用語 | 意味 |
|---|---|
| triage 段階の task (旧称 card) | 境界を越える最小単位。 見出し・緊急度・出所ポインタ・状態のみで、 本文を含まない。 第 6 版で task 統合が確定し固有名を廃止 — 実体はメタプロジェクト所属の task が pre-execution 状態にあるもの。 固有フィールドは sidecar テーブル `task_triage` に持つ (実測 c) |
| daemon inbox | daemon 側の card store と状態遷移の管理主体。 queue とは区別する (決定 9) |
| queue | inbox の上の提示 view。 優先度と再浮上条件から「今応えるべき card」だけを並べたもの。 nose が応答する対象 |
| 後回し (park) | 応答の一種。 card を queue から外し、 再浮上条件 (日時 / 事象 / someday) 付きで store に留める |
| メタプロジェクト | workspace ごとに 1 つ置く普通の boid project。 課題・テーマの本文ファイルと ingestion / triage 指示の置き場 |
| ingestion | workspace 内で定期実行される情報収集 job。 mail / Jira / Slack を読み、 workspace 内 triage を経て起票分だけ card を起こし、 source 側を処理済み化する (決定 10/11) |
| 整形セッション | card を「Go 可能」まで持っていく対話セッション。 対象 workspace 内で走る |
| テーマセッション | ふわっとした課題感・ テーマを前進させる定期のインタビュー形式セッション |
| Go | nose による実行承認 = triage task を **ready にする判断** (逆輸入 2)。 task 化そのものは ready からの機械処理であり、 Go を経ずに自動発生しない点は不変 |
| ready | Go 済み・dispatch 待ち。 specced な子の spec から機械的に task を起こしてよい状態。 到着時点で ready のこともある (UC-1) |

---

## 設計決定

### 決定 1: 集約 store は daemon 側に置く (daemon inbox)

プロジェクト横断の集約 store は、 どこかの workspace 内ではなく daemon に置く。

daemon は既に全 workspace の task を 1 つの DB に持ち、 Web UI は 1 つのデバイス認証でそれを
見せている。 workspace 同士は互いの task を読めず、 読めるのは daemon と認証済みの nose だけ。
この信頼モデルは境界違反とは見なされていない。 daemon inbox はこれと**同型**であり、 新しい
信頼モデルを導入しない。

却下した代替:

- **workspace 内の共有プロジェクトに集約**: 「他 workspace の情報を共通の場所に露出させる」
  問題が本当に発生するのはこの形だけ。 当初グローバル集約を断念した原因は、 集約場所を
  workspace 的な場所と暗黙に仮定していたことにある。
- **store を持たない federated 表示** (表示のたび各 workspace から読む): queue と notify を
  出すには結局 materialize が必要で、 store の節約にならない。

### 決定 2: workspace → daemon は一方通行 push、 read は policy で不許可

card の投入は brokered builtin (`boid inbox push`、 仮称) で行う。 broker は TokenContext で
job の所属 workspace を知っているため、 enforcement は既存 policy table の流儀そのまま。

- 出所 workspace の印は **daemon 側で押す** (push 側の自己申告は使わない)。
- workspace の job には inbox の **read 系 op を許可しない**。 push は append 相当のみ。
- これにより「ある workspace の agent が inbox 越しに他 workspace の情報を読む」経路が
  構造的に存在しない。

なお daemon が dispatch payload として**自 workspace 印の card** を job に手渡すのは可
(自 workspace 由来の情報であり、 露出が増えないため)。 整形セッション (UC-3) やテーマ
セッションの棚卸し (UC-5) はこの経路を使う。

### 決定 3: 境界を越えるのは card 粒度、 本文は現地

card は見出し・ 緊急度・ 出所ポインタ級の情報のみ持つ。 メール本文や課題の詳細は workspace 内
(メタプロジェクトのファイル、 または元システム) に留まる。 深掘りしたくなったら、 その
workspace のセッションを開いて中で読む。 secret と同様、 本文も「最初から持っている場所でしか
開かない」。

card が運べる上限は、 triage が書いた**要約 (summary)** と**実行仕様 (task_spec)** まで
(UC-1 / UC-3)。 原文そのもの (メール本文・ 添付・ 文書全文) は越えない。 要約にどこまで
書いてよいかは開示ポリシー (論点 e) の管轄。

開示の粒度 (件名まで出すか、 相手ドメインのみか等) は将来 workspace ごとのポリシー (データ)
として定義し、 enforcement は daemon 側で行う (論点 e)。

### 決定 4: 戻り方向の影響は Go ゲート経由のみ (prompt injection を前提にする)

mail / Jira / Slack の本文は untrusted input であり、 ingestion agent は prompt injection に
晒される前提で設計する。 汚染の blast radius を「変な card・ 変な提案が queue に並ぶ」までに
限定する:

- inbox とその表示・ 提案系 (グローバル層) は workspace secret を一切持たない。
- グローバル層が workspace に対してできることは「task の提案」まで。 実行への遷移は必ず
  nose の Go を経由する。
- 従って汚染 card が届いても、 最悪は誤った提案が並ぶだけで、 Go で止まる。 他 workspace の
  内容にはどの段階でも到達しない。

### 決定 5: routing は 2 段 — workspace は構造上自明、 project は workspace 内で確定

「project の内容を見ずに各 project への dispatch ができるか」という懸念は、 routing を 2 段に
分けると解消する:

1. **workspace 所属**: ingestion は各 workspace の中で走るので、 card の workspace 所属は
   構造上決まっている (決定 2 の daemon 押印そのもの)。 判断は不要。
2. **workspace 内のどの project か**: card 段階では hint に留め、 整形セッションが対象
   workspace の**中で**確定する。 必要なら project を checkout して読む — それはその workspace
   の job の正当な権限であり、 グローバル層が project 本文を読む必要は無い。 routing の材料と
   してグローバル層が見るのは project カタログ (daemon が in-memory に持つ project meta) まで。

例外は nose の頭から直接 capture される課題で、 これのみ workspace 不定。 カタログからの推定 +
nose の一発確認で振り分ける (論点 c)。

### 決定 6: メタプロジェクトは普通の boid project、 内容はファイル・状態は daemon

boid 組み込みの特別なプロジェクト種別は作らない。 役割を分担する:

- **本文 (課題・ テーマ文書) はファイル**: メタプロジェクト (git repo) 内の frontmatter 付き
  markdown。 データモデルは初期に必ず荒れるので、 スキーマ変更耐性の高い形式で始める。
  「ポリシーはデータ、 enforcement は実装」という boid の方針にも合う。
- **状態と queue は daemon inbox**: 成功の定義の応答面は Web UI であり、 UI 操作のたびに
  git commit を挟むのはまどろっこしい。 状態遷移 (triaged → ready → dispatched...) は daemon の
  管轄にする。 card はファイル本文への `content_ref` を持つ。

Web UI から project 内ファイルを閲覧する汎用機能 (メタプロジェクトに限らず有用。 daemon は
project の bare repo を持っているため読み経路の素材はある) は、 将来の別件として扱う。

### 決定 7 (v4 改): 構造化は task モデルへの最小拡張を第一候補とする

v3 までは「task の隣に issue エンティティ」を推していたが、 nose の指摘 (凝集度・ task 側から
見た文脈 pointer の価値) を受けて方向を変える。 **第一候補は task モデルの最小拡張**:

- card は**メタプロジェクト所属の task** として持つ。 task 状態機械の上流に pre-execution
  状態 (captured / triaged / parked / ready) を足す。
- **Go = 既存の cross-project 子 task 生成**。 対象 project に実行 task を作り、 親子 link が
  由来 link (`task_ref`) を兼ねる。 card 側は子の終端で done に落ちる。
- queue は task 一覧の**決定論 filter view** (決定 12)。 Web UI の task 表示・ brokered
  `boid task create` 経路・ 親子機構をそのまま再利用でき、 専用 inbox テーブルと push op は
  ほぼ不要になる。
- 実行 task から親 (card) の content_ref が辿れるため、 実行 agent が検討経過を文脈として
  読める。

v3 の 3 論拠はこの形でこう変わる: (1) project_id 必須 → メタプロジェクトが home になり解消。
(3) 終端 GC 30 日 → 「耐久は git・ DB は運用」の原則 (ストレージ節) で許容。 (2) 状態機械の
consumer 監査 → **唯一残る実測課題** (安易に状態を混ぜない、 という警戒自体は維持)。

Phase 1 設計時の実測チェックリスト — **2026-08-10 に (a)(b)(c) すべて完了** (結果の詳細は
「Phase 1 実測結果」節):

- (a) task 状態の全 consumer が pre-execution 状態を安全に無視できるか → **概ね Yes**。
  panic 経路ゼロ、 hook 評価・自動遷移・dispatcher sweep は executing 明示ガードで安全。
  要対処は「SQL の `NOT IN ('done','aborted')` 誤包含」と「pending 限定ガードの機能不全」の
  2 系統 11 箇所に集約 (実測結果節のチェックリスト)。
- (b) sandbox からの task read 系 op の workspace scoping → **成立** (broker + executor の
  二重検査)。 欠落していた `task_get` / `task.reopen` の 2 op は **PR #927 で修正済み
  (2026-08-10 merged)**。
- (c) 追加フィールドの置き場 → **sidecar テーブル `task_triage`** で確定。 tasks への列追加は
  `orchestrator.Task` が DTO を兼ねるため全 API / CLI / Web の JSON に自動露出してしまう。

判定: **task モデル統合で確定**。 命名は固有モデル名を置かず「triage 段階の task」で通す
(用語注参照)。

### 決定 8: スコープは workspace 単位から、 横断は card メタデータのレベル

full content の workspace 横断集約は行わない (secret の compartment を壊すため、 非目的)。
ただし daemon inbox が daemon 側にあるため、 card メタデータ (件数・ 見出し) レベルの横断
queue は**最初から自然に成立する**。 「全体を見渡す」要求へはこのレベルで応える。

### 決定 9: inbox は store、 queue はその上の提示 view — 「後回し (park)」を第一級にする

inbox と queue を同一視しない。 実運用では「いったんスキップして後回しにする」card が多数を
占めるため、 ingestion から投入されたものが並ぶ場所と、 nose が応える場所は別物になる:

- **inbox (store)** は triage 済みの全 card を保持する (後回し中・ someday を含む)。
- **queue (view)** は優先度と再浮上条件から「今応えるべき card」だけを並べた提示面。 nose が
  応答するのは queue に対してであり、 成功の定義 (応えるだけで進む) は queue に適用される。
- 応答の語彙に **後回し (park)** を加える: 1 操作で card を parked にし、 再浮上条件
  (日時 / 事象 / someday) を付ける。 条件を満たした card は queue に戻る。 someday の card は
  テーマセッションの定期棚卸しで見直す。 棚卸し自体が放置されるリスクは watchdog が検知する
  (queue 節 7)。
- 役割分担: per-card の triage (起票判断・ 緊急度の初期値) は workspace 側 (決定 10)。
  queue の順位付け・ 再浮上の評価はグローバル層の view 責務 (論点 d)。

### 決定 10: ノイズは workspace 内 triage で落とす — inbox に全量は集約しない

mail / Slack はノイズが多い (アクション不要のプロモーションメール、 雑談・ 単なる通知)。
これらは inbox に転送しない:

- ingestion は workspace 内で triage まで行い、 **起票に値するものだけ** card にする。 明らかな
  ノイズは card 化しない。 この結果、 mail / Jira / Slack 経路の card は triaged 状態で inbox に
  到着する (captured は主に head-capture 用)。
- silent drop は網羅性への信頼を壊しうるので、 **落とした側の会計**で担保する: triage log
  (何を読み、 何を起票し、 何をなぜ落としたか) を workspace 側に残し、 日次 digest
  (「120 件走査 / 4 件起票 / 116 件ノイズ判定」 + log 参照) として nose が監査できるようにする。
- 判断に迷うものは落とさず、 低 urgency で起票する。 低 urgency の card は queue に出ない
  (決定 9) ため、 迷い起票が queue を汚すことはない。 silent drop は確信がある場合のみ。
- ノイズ判定のルール (差出人・ チャンネル・ パターン) は workspace 側のデータとして持ち、
  nose の応答傾向 (破棄・ 後回し) から育てる (論点 d)。

### 決定 11: ingestion は source 側の処理済み状態を書き戻す

「読み取ったら既読にしたい」を ingestion の契約にする。 boid が「見に行く」を代替する以上、
source 側の未読状態も boid が管理しないと、 メールクライアントに未読が溜まり続けて二重管理に
なる:

- **mail**: 起票 or ノイズ判定が済んだものを既読化する。 順序は「本文保存 → card push の ack →
  既読化」 (先に既読化すると、 push 失敗時にどちらの注意系からも消えるため)。 label 併用等の
  詳細は論点 h。
- **Slack**: 既読相当の API が実用的でないため cursor (最終処理 ts) 方式。
- **Jira**: updated-since cursor + 処理済み集合。
- いずれも workspace 内の credential で行う。 追加で必要になるのは source 書き戻し分の
  write scope のみ。

### 決定 12: daemon は判断しない — queue 評価は決定論ロジックのみ

daemon 側でエージェントを実行する機構は現状無く、 追加は変更が大きい割に筋が悪い。 評価専用の
workspace を設ける案は「workspace → daemon は push のみ」の原則 (決定 2) を壊す。 さらに積極的
な理由として、 queue 評価に LLM を挟むと**説明可能性が死ぬ**: 「なぜこの card が今ここに出て
いるか」を機械的に一文で説明できることは、 queue への信頼 (成功の定義) の一部である。

- 知能の所在は端に置く: urgency / summary / task_spec は workspace 内 triage (agent) が書き、
  修正は nose の応答が与える。 daemon が見るのは**時計・ task DB・ card のフィールドだけ**。
- 評価ルールの本体は「queue の決定論的評価」節に明示する (この計画の心臓部)。

### 決定 13: 状態遷移はイベントソーシングで持つ

card の lifecycle は遷移が多く (park / wake / 昇格 / 棚卸し / 学習)、 会計 (UC-6)・ 説明可能性
(決定 12)・ 誤操作からの復旧のすべてが「誰がいつ何をしたか」を要求する。 状態遷移は **event の
追記を正**とし、 現在 state は導出 (cache) とする。

boid には actions / timeline の既存機構があるため、 greenfield の導入ではなく**その徹底**として
設計する (Phase 1 実測: 現行の遷移記録がどこまで event log として使えるか)。 task 統合
(決定 7 改) と併せると、 card の遷移 event は task の action 履歴に自然に合流する。

---

## 信頼境界

| principal | 読める | 書ける |
|---|---|---|
| workspace の job (ingestion / 整形 / 通常 task) | 自 workspace の本文・ secret | card push (append のみ)、 自 workspace 内のファイル |
| daemon | inbox 全体、 project カタログ | inbox の状態、 task create (Go 起点) |
| nose (認証済みデバイス、 Web UI) | inbox 全体 (card 粒度)。 本文は各 workspace セッション経由 | Go / 整形開始 / 破棄 / capture |
| グローバル表示・提案系 | inbox (card) + project カタログのみ。 workspace secret 無し | 提案のみ (Go を経ずに task を作らない) |

injection シナリオ: 悪意あるメール → workspace A の ingestion agent が汚染 card を push →
queue に変な項目・ 変な提案が並ぶ → **Go で止まる**。 workspace B の内容にはどの段階でも
到達しない。

残余リスク: card だけでも 1 箇所に集まれば honeypot 性はある。 これは現行の task タイトルの
daemon 集中と同レベルであり、 device pairing + web_secret の既存認証を門番として**受容する**。

---

## データモデル (draft — Phase 0 で荒れる前提)

### 状態機械 (第 6 版で Phase 0 実運用に追従 — 逆輸入 2)

```
captured ──▶ triaged ──▶ shaping ──▶ ready ──▶ working ──▶ done
              │   ▲         (Go)       │  ▲    (全子終端 ∧ source 終了で自動)
              ▼   │                    └──┘ 残課題を子に起こして再 Go
            parked
```

- captured: capture 直後、 triage 前。 主に head-capture 経由 (mail / Jira / Slack 経路は
  workspace 内 triage を通るため triaged で到着する、 決定 10)。 Phase 0 では未使用。
- triaged: 課題として受理。 優先度・ hint 付与済み。 queue の対象。
- parked: 後回し中。 queue に出ない。 再浮上条件を満たすと戻る。 遷移元は queue に出る状態
  (triaged / ready)、 復帰は元の状態へ。
- shaping: 整形セッション進行中 (Phase 2)。 Phase 0 では未使用。
- ready: **Go 済み・dispatch 待ち** (Go = ready にする判断、 逆輸入 2)。 specced な子の spec
  から機械的に task を起こしてよい。 ready に滞留しないのは正常で、 滞留は dispatch 機構の
  不調のサイン。
- working (旧 dispatched): 着手中。 boid task 進行中 (dispatched な子あり) と、 nose が手動
  対応中 (Jira 操作・返信など、 子は open のみ) の両方を含む。
- done: 全子 closed ∧ source 終了で**自動で落ちる** (承認不要)。 dropped と区別する —
  「応えるだけで進むか」は終わった件と捨てた件を区別できないと測れない。
- dropped: 破棄。 必ず nose の判断でしか入らない。 どの状態からも入れる。
- kind が theme の card は常駐で、 この lifecycle に乗らずテーマセッションの起動単位になる
  (状態でなく kind で分ける)。

### triage task スキーマ案 (旧 card スキーマ案)

以下は**確定スキーマではなく Phase 0 で洗うフィールド候補**。 frontmatter の位置づけは
ストレージ節の原則 2 (git 側に残す耐久写し + Phase 0 の検証場) であり、 モデリングの本体は
Phase 0 の運用で安定させてから決定 7 (改) の形 (tasks + `task_triage` sidecar、 実測 c) に
落とす。

```yaml
id: card_xxxxxxxx
workspace: khi            # daemon 押印。 push 側の自己申告は無視
kind: signal | issue | theme
state: captured | triaged | shaping | parked | ready | working | done | dropped
title: "..."              # 開示ポリシーに従った粒度
summary: "..."            # triage が書く 2〜4 文の判断用要約 (開示ポリシー適用、 原文は含まない)
urgency: now | today | week | someday   # 語彙は論点 d
wake: ""                  # parked 時のみ: 再浮上条件 (日時 / 事象 / someday)
source:
  type: mail | jira | slack | head | agent
  ref: "message-id / issue-key / permalink など。 本文は含まない"
related_jira_issues: []   # source.type をまたぐ同一案件の束ねキー (逆輸入 3。 論点 g の実解)
content_ref: "issues/2026-07-30-xxx.md"  # メタプロジェクト内の本文ファイル (相対 path)
project_hint: "..."       # 任意。 確定は整形セッション (決定 5)
suggestion:               # エージェントの推奨 1 つ。 一次情報からの導出フィールド (逆輸入 3)
  verb: go | shape | manual | park | drop | wake   #   nose の応答語彙と同じ
  action: "..."           #   次の一手を 1 文で
  reason: "..."
  basis: "..."            #   導出の根拠にした一次情報
observed:                 # 機械観測の射影 (子 task の生死・Jira status)。 evaluate だけが書く
  at: ...
children:                 # 派生した子のリスト。 旧 task_spec / task_ref / open_items を統合 (逆輸入 1)
  - id: ch_00
    title: "..."
    status: open | specced | dispatched | closed
    spec:                 #   specced 以降で必須: これだけで task create できる実行仕様
      project: "..."
      behavior: "..."
      instruction: "..."
    task_ref: ""          #   dispatched 以降: 起こした boid task の id
created_at: ...
updated_at: ...
```

### メタプロジェクト (workspace 側) のファイル構成案

```
meta/  (repo root)
  .boid/project.yaml      # task_behaviors: ingest / triage / shape / theme
  issues/                 # 課題本文 (frontmatter + markdown)
  themes/                 # テーマ文書 (長寿命)
  digest/                 # Phase 0 のみ: queue 代替の digest ファイル
```

信号の生データ (メール本文等) は repo に入れない。 必要なら $HOME workspace volume 側の
キャッシュに置いて揮発扱いにする (git 差分ノイズと秘匿の両面から)。

---

## queue の決定論的評価

この計画の心臓部。 daemon は以下のルールだけで queue を組み立てる (決定 12)。 入力は card の
フィールド・ 現在時刻・ task DB のみで、 エージェント判断は入らない。

1. **wake 評価** (parked の復帰): `wake.date <= now`、 または `wake.task` が終端に達した
   (task DB 参照)。 いずれも決定論で評価できる。 someday は wake を持たず、 棚卸し (UC-5) で
   のみ動く。
2. **queue 所属**: state ∈ {ready, triaged} かつ urgency ∈ {now, today, week}。 someday と
   parked は出ない。 head-capture の workspace 確認 (UC-4) とテーマセッションの案内 card は
   専用の確認セクションに出す。
3. **並び順** (全順序): urgency (now > today > week) → state (ready が先、 Go 一発で捌ける
   ものを上に) → created_at 昇順 (古いものを腐らせない) → id。
4. **notify**: now は即時。 today は朝の digest + 到着時。 week は日次 digest のみ。 時刻は
   config で決める。
5. **隠さない**: 該当する card は全件見せる (足切りしない)。 queue から見えない理由は
   parked / someday / dropped だけ。 「見えていなければ存在しない」と信じられるようにする。
6. **escalation は v1 では入れない**: 放置された today の強調表示などの aging ルールは
   Phase 0 の観察後に論点 d で決める (入れる場合も決定論の範囲で)。
7. **沈黙の検知 (watchdog)**: workspace ごとに「最終 ingestion 成功」と「最終棚卸し実施」の
   時刻を持ち、 閾値を超えたら案内 card を queue に出す (解消されるまで出し続ける)。 ingester
   の失敗・ cron 死・ 棚卸しの放置は、 task ビューの目視でなくこれで検知する (「来ないこと」に
   人間は気づけないため)。 これにより「事象 wake が永遠に来ない parked」と someday が埋もれる
   経路は、 「棚卸しに出る (UC-5)」+「棚卸しの放置は watchdog が刺す」の 2 段で塞がる。

---

## ユースケース

card が実際にどう処理されるかを 6 本の代表フローで示す。 いずれも Phase 1-2 完成形の挙動
(Phase 0 は queue / notify をメタプロジェクト内の digest ファイルで代替した縮退版)。 手順の
先頭は実行場所: **[ws]** = workspace 内の job、 **[daemon]**、 **[nose]** = 認証済みデバイスの
操作。

### UC-1: 顧客メールが task になって完了するまで (基本線)

1. [ws] ingestion job が定期起動し、 mail を workspace の credential で走査。 新着 12 通のうち
   9 通をノイズ判定 (プロモーション・ 通知)。 card 化せず triage log に記録 (決定 10)。
2. [ws] 顧客からの見積もり依頼 1 通を起票: 本文を `issues/2026-07-31-mitsumori.md` に保存 →
   summary (2〜4 文) と task_spec (project・ behavior・ 指示文) を書いて `boid inbox push`。
   実行仕様が自明なので**到着時点で ready** として起票する。 push の ack 後にメールを既読化
   (決定 11 の順序)。 迷った 2 通は urgency=someday の triaged で起票 (queue には出ない)。
3. [daemon] broker が workspace 印を押して inbox に格納。
4. [daemon] 決定論ルール (「queue の決定論的評価」節) により urgency=today が当日の queue に
   載り、 notify が nose に飛ぶ。
5. [nose] queue で card を開く。 title + summary で判断し、 **Go**。
6. [daemon] task_spec から対象 project に task create。 card は dispatched になり task_ref を
   記録。
7. [ws] task が通常の boid task として実行される。 task が終端に達すると card は done に落ちる。

### UC-2: 後回し (park) と再浮上

1. [ws] ingestion が Jira から「来月のメンテナンス計画の起案」を triaged (urgency=week) で
   起票。
2. [nose] queue で見て、 今週はやらないと判断。 **後回し**を選び、 再浮上条件に「来週月曜」
   (または「task X 完了後」の事象条件) を指定。 card は parked になり queue から消える
   (決定 9)。
3. [daemon] 条件成立を評価し、 card を triaged に戻す。 queue に再登場し、 notify。
4. 以後は UC-1 の手順 5 以降と同じ。

### UC-3: ふわっとした課題の整形 (整形セッション)

1. [ws] ingestion がメールスレッドから「〇〇の運用が回っていない気配」を検出。 実行仕様までは
   書けないので、 summary のみの triaged card で起票 (task_spec 無し)。
2. [nose] queue で見て、 内容を詰めたいと判断。 **整形**を選ぶ。
3. [daemon] 対象 workspace のメタプロジェクト文脈で**整形 session** (対話、 task 無し job) を
   起動。 payload に card (id・ title・ content_ref) を同梱 (決定 2 の自 workspace 手渡し)。
4. [ws] agent が本文ファイルと関連 project (必要なら checkout、 決定 5) を読み、 nose と対話
   (Web UI セッション / task ask)。 対象 project・ 実行内容・ 完了条件を確定する。
5. [ws] セッションの成果を本文ファイルに追記し、 `boid inbox push` で card を **ready に更新**
   (task_spec 付き。 同一 card の再 push は update、 論点 g)。
6. [nose] queue に ready で再登場した card を **Go**。 以後は UC-1 の手順 6 以降と同じ。
   整形の結果「やらない」と分かったら、 nose は破棄 (dropped) を選ぶ。

### UC-4: head-capture (頭の中の課題感)

1. [nose] 移動中にスマホから音声で「△△、 そろそろ手を打たないとまずい気がする」と capture。
2. [daemon] captured card として保持。 本文 (発話テキスト) は nose 発なので、 daemon が一時
   保持しても compartment 問題は無い。 グローバル層が project カタログから workspace を推定し、
   確認を queue に出す (決定 5 の例外)。
3. [nose] 推定が合っていれば一発確認。 違えば選び直す。
4. [daemon] 確定した workspace のメタプロジェクトに取り込み task を dispatch。 payload に
   発話テキストを同梱。
5. [ws] 取り込み task が本文を `issues/` に保存し、 card を triaged (content_ref 付き) に更新。
   この時点で本文は workspace に着地し、 daemon 側は card 粒度に戻る。
6. 以後、 明確なら UC-1、 ふわっとしていれば UC-3 (整形) か UC-5 (テーマ) の流れに乗る。

### UC-5: テーマセッション (定期インタビューと棚卸し)

1. [daemon] theme card 「ビルメン事業の採用」 (常駐) の定期条件が来て、 **案内 card** を queue
   に出す。 session を勝手に立てても nose 不在では意味がないため、 定期処理はここまで。
2. [nose] 案内を tap し、 テーマ session (対話、 task 無し job) が起動する。 payload にテーマ
   文書の ref と、 自 workspace 印の someday / parked card 一覧が同梱される (決定 2 の自
   workspace 手渡し)。
3. [ws] agent がテーマ文書と最近の関連 card から議題を用意し、 nose にインタビューする
   (音声入力前提の対話)。
4. [ws] 対話の成果を反映: テーマ文書を更新、 新しく見えた課題を card として push、 棚卸しした
   someday card の昇格 (urgency 引き上げ) や破棄提案を push で反映。
5. [nose] 昇格された card は次の queue に現れる。 以後は通常フロー。

### UC-6: ノイズと会計 (信頼の担保)

1. [ws] ingestion のたびに triage log が積まれ、 日次 digest 「本日 120 件走査 / 4 件起票 /
   116 件ノイズ判定」が notify される (決定 10)。
2. [nose] 普段は数字を見るだけ。 たまに log を開いて監査する (誤ノイズ判定が無いか)。
3. [nose] 見逃しに気づいたら triage log から再起票する (論点 h)。 誤判定のパターンは triage
   ルールに反映され (論点 d)、 以後のノイズ判定が締まる。

---

## エージェントの実行場所とタイミング

エージェントが動く場所と起動契機を明示する。 daemon にエージェントは置かない (決定 12)。

| 処理 | 形態 | 起動 | 実行場所 |
|---|---|---|---|
| ingestion + triage | 使い切りの通常 boid task | host cron が定期に `boid task create` (論点 b) | workspace 内 sandbox |
| 整形セッション | session (対話、 task 無し job) | nose が queue から tap | workspace 内 sandbox |
| テーマセッション | session (対話) | 定期条件は「案内 card を queue に出す」まで。 本体は nose の tap で開始 | workspace 内 sandbox |
| 取り込み task (UC-4) | 小さな boid task | routing 確認を契機に daemon が dispatch | workspace 内 sandbox |
| queue / wake 評価・ notify | 決定論ロジックのみ (決定 12) | daemon 内 (時計 + DB) | daemon |

- **ingester の形態**: 「/loop で常駐 + host cron で洗い替え」でも動くが、 第一候補は
  **使い切り task** (cron が毎回 task create して使い捨てる)。 状態を task の文脈に持たせず、
  洗い替えという工程自体が消える。 冪等性は cursor ($HOME volume) と dedup (論点 g)・ source
  書き戻し (決定 11) が担保する。 dispatch オーバーヘッドが問題になったら常駐 + 洗い替えに
  切り替える。 ingest task の頻発で task 一覧 (特に closed ビュー) が溢れる問題は、
  **メタプロジェクト単位の filter** で逃がす: 既定で通常の task ビューからメタプロジェクトの
  task を除外し、 toggle で表示できるようにする。 behavior 名 filter より粗い単位で足りるし、
  task 統合 (決定 7 改) 後も card の表示面は queue なので整合する。 ただし ingestion の
  **失敗・沈黙の検知はこの filter (task ビュー) に頼らない** — cron 自体が死ぬと task 行が
  そもそも作られないため、 検知は staleness watchdog (queue 節 7) で行う。
- **対話系はすべて session**: 整形・ テーマは対話が本体なので、 boid task ではなく session
  として起動する。 定期起動が直接 session を立てても nose 不在では意味がないため、 「定期」は
  案内 card の発行までとし、 session は nose の tap を契機に立てる (session 起動経路の現状は
  Phase 2 設計時に実測)。

---

## ストレージと信頼性

boid の既存データは「揮発しても再構築可能」を前提にしてきたが、 課題・ テーマ群はその前提から
外れる: 唯一の写しが消えたら nose の課題意識そのものが消える。 データ種別ごとに置き場所と
耐久性を明示する。

| データ | 置き場所 | 耐久性の根拠 | 消失時の影響 |
|---|---|---|---|
| 課題・ テーマ本文 (`issues/` `themes/`) | メタプロジェクト repo (git) | job 終了ごとに upstream へ push | 直近セッション分のみ |
| triage ルール・ 指示・ project.yaml | 同上 | 同上 | 同上 |
| 日次 digest (会計サマリ) | 同上 (小さく意味のある diff) | 同上 | 監査履歴のみ |
| card の運用 state (parked / wake / queue) | daemon DB | daemon volume | 全 card が再浮上する (fail-open、 silent loss にならない) |
| triage 詳細 log | $HOME workspace volume (rolling 30 日) | volume | 監査精度の低下のみ |
| source cursor (Slack ts / Jira since) | $HOME workspace volume | volume | 再走査分の再処理 (dedup で card は update になり無害) |
| 信号の生データ (メール本文キャッシュ等) | $HOME volume (揮発) | 不要 — source から再取得可能 | 無し |

原則:

1. **失って困る唯一の写しを volume・ daemon DB に置かない。** 一次情報は「再取得可能な
   source」か「git」のどちらかに必ず載る。
2. **daemon 側 state の消失は「再浮上」に退行する** (fail-open)。 隠していた card が出てくる
   方向にしか壊れない。 本文ファイルの frontmatter に card フィールドの写しを持たせ、 daemon
   側は workspace からの再 push で再構築可能に保つ。 frontmatter の役割は「モデリングの本体」
   ではなく**この耐久写し** (+ Phase 0 の検証場)。
3. **upstream は「その workspace が既に持つ credential 境界」の内側に置く。** メタプロジェクト
   のために新しい credential を workspace に持ち込まない。 顧客 workspace に個人 GitHub の
   secret を置くと、 untrusted input を処理する場 (決定 4) に個人の全 private repo への到達性を
   持ち込むことになるため、 明確に避ける。 具体的には: 顧客 workspace は顧客側 git 上の自分用
   repo (khi は customer bitbucket の `khi-task-collector`、 登録済み)、 個人 workspace は
   個人 GitHub。 **boid-hosted bare repo は、 どの workspace でも credential ゼロで成立する
   恒久解**として優先度を上げる — ただし daemon volume backup (別トラック) 整備が前提である点
   は変わらず、 それまでは外部 git を耐久性の根拠にする。

---

## Phase 1 実測結果 (2026-08-10)

決定 7 の実測チェックリスト (a)(b)(c) をコードベース全域の調査で実施した結果。 ファイル:行は
実施時点 (main = PR #927 merge 後) のもの。

### (a) task 状態機械への pre-execution 状態追加 — 行けるが対処リストは実在する

安全側の事実: `TaskStatus` は素の文字列派生 (`internal/orchestrator/model.go:8-16`) で、 DB は
TEXT・CHECK 制約なし・exhaustive switch なし。 **未知状態で panic する箇所はゼロ**。 hook 評価
(`evaluator.go:37`)・自動遷移 (`machine.go:53-70`)・dispatcher の起動時 sweep
(`dispatcher/store.go:188,198`)・planner (`planner.go:258`) はいずれも対象状態の明示ガードで、
pre-execution 状態を勝手に触らない。 また status を直接セットする API は存在せず
(`apiwire/task.go` の Create/Update に Status フィールドが無い)、 新状態に入る経路は
**state machine への action 追加のみ** — これは決定 13 (イベントソーシング) と整合する制約。

要対処は 2 系統 11 箇所:

**誤包含系 (`NOT IN ('done','aborted')` パターン)**

1. `status=open` フィルタ (`internal/orchestrator/store.go:155-163`) が pre-execution task を
   全部拾い、 Web UI デフォルト画面 (`internal/api/web.go:275-276`) が someday で埋まる。
   **最大のブロッカー**。 open / closed に第 3 のカテゴリ (pre-execution) を足す。
2. `open_child_count` (`store.go:64`) が pre-execution な子を数え、 親の `notify --done` を
   `verifyDoneClaim` (`internal/api/task_notify.go:310-318`) が永久 409 にする。
3. 30 日 GC (`internal/orchestrator/repository.go:233`) は done/aborted のみ対象で、
   parked / someday は**永久保持**になる。 これは意図的仕様として明示する (課題を GC で silent
   drop しない、 fail-open と同方向)。 DB 肥大の増加源になる点だけ監視。

**pending 限定ガード系 (機能不全)**

4. title / project 編集 (`internal/api/task_service.go:86-99`) と instructions 編集
   (`:158-162`、 `lifecycle.go:137-140`) が pending 限定。 **triaged / ready の task を整形
   セッション (UC-3) で書き換えられない**。 pre-execution 状態を編集可能集合に加える。
5. `start` の FromStatus が pending 固定 (`machine.go:166`)。 ready からの機械 dispatch
   (ready → working、 逆輸入 2) に対応する遷移ルール追加が必須。

**その他**

6. `.badge-parked` CSS が既に job 表示用シノニムとして存在 (`web/static/style.css:249-252`、
   task awaiting 中の running job を "parked" と表示)。 task status に parked を足すと意味の
   異なる 2 概念が同名クラスになる。 どちらかを改名。
7. 新状態のバッジ色・行ストライプ色が無く無地表示になる (`style.css:239-248, 372-377`)。
   なお 0022 で消えた verifying / reworking の CSS が残存しており、 CSS は状態増減に追従して
   いない既往がある。
8. `boid task watch` が pre-execution を非終端とみなし無限待ちする (`cmd/observe.go:15-17`)。
9. Web UI の status タブが open / closed の 2 択ハードコード
   (`web/templates/components/filters.templ:38-47`)。 queue view 用の面が要る。
10. brokered `boid task list --status` が未検証の status 値を素通しする
    (`internal/server/boid_executor.go:326-344`)。 workspace scoping は効いているので読める
    のは自 workspace 分のみだが、 決定 2 の「read 系 op 不許可」を task 統合後にどう解釈する
    かを Phase 1 で明文化する (自 workspace の triage task が job から列挙できること自体は
    決定 2 の自 workspace 手渡しと同等で、 露出は増えない)。
11. `AvailableActions` (`machine.go:87-108`) は未知状態で `["abort"]` のみ返す。 新遷移に
    `Manual: true` を付ければ Web UI のボタンに自動で出る — UI 配線はここに乗せる。

### (b) workspace scoping — 成立。 欠落 2 op は修正済み

broker (`internal/sandbox/broker.go` の TokenContext 検証) + executor
(`internal/server/boid_executor.go` の `ctx.AllowsProject` 再検査) の二重層で、 task_list /
project_list / project_behaviors / job 系 / task_create の cross-project 経路まで scoping が
成立していることを確認。 分離の要は「broker token を持つ job に daemon API socket を渡さない」
strip (`internal/dispatcher/runner.go:922-929`)。

欠落していたのは `task_get` (cross-workspace read) と `task.reopen` (write) の 2 op のみで、
**PR #927 で executor 側に `AllowsProject` 検査を追加し merge 済み**。 これを放置したまま
task 統合すると、 triage task の ID がメール・Jira 経由で workspace 外に出回る値になった時点で
「他 workspace の課題見出しが読める」経路に昇格するところだった。

軽微な残課題: project ref の曖昧解決エラーが daemon 全域の一致件数を返し、 cross-workspace の
project 存在オラクルになる (論点 i)。

### (c) フィールド置き場 — sidecar `task_triage` + queue 述語だけ実列

- tasks への列追加は不採用: `orchestrator.Task` が DTO を兼ねており (変換層なし、
  `internal/api/store.go:217-275`)、 列を足すと全 API レスポンス・CLI・Web の JSON に自動露出
  する。 前例実測でフィールド 1 個 = 7〜17 ファイル。
- sidecar `task_triage` (task_id PK/FK): 生きた 1:1 sidecar の前例あり (`project_workspaces`)。
  先置き migration は 3 ファイル 41 行で挙動不変に切れる前例 (`02589114`) があり、 撤退も
  `DROP TABLE` 1 行で最安。
- 列の内訳: **urgency / wake_at は実列** (+index) — queue の決定論評価 (決定 12) の SQL 述語に
  なるため。 summary / task_spec / source / content_ref は述語にならないので **JSON 1 列**に
  まとめる (`tasks.payload` 流の `json.RawMessage` パターン)。 `tasks.payload` への相乗りは
  trait 名前空間 (`payload_merge.go:19-35`) と衝突するため不採用。
- 決定 13 の実測: `actions.type` は自由文字列で、 外部サブシステムが独自 kind を後付けした
  前例あり (`api_gateway_request`)。 全遷移が action 行として append 済みなので、 park / wake /
  promote を type にして payload に理由を載せるだけ — **追加スキーマゼロで使える**。 現在
  state は `tasks.status` 列が正であり action からの導出 cache にはなっていないが、 決定 13 の
  「徹底」はこの範囲 (event 追記を正とし、 遷移はすべて action を経由する) で満たすものとする。

---

## Phase 0 実運用からの逆輸入 (2026-08-10)

第 6 版の実測は boid 本体側だったが、 khi-task-collector (Phase 0 のメタプロジェクト) の運用は
本 doc 第 5 版時点の設計より先に進んでいる。 仕様の正は同 repo の `docs/card-format.md` /
`docs/card-events.md` (2026-08-10 時点の main を突合)。 本節はそこから doc 本体へ取り込む差分。

### 逆輸入 1: 子は複数 — task_spec / task_ref / open_items は children に統合された

Phase 0 の実測で「**1 つの triage task が複数の boid task に紐づくのが普通**」と分かり、 旧
task_spec (card に 1 つ) / task_ref (代表 1 個) / open_items (残課題) は子のリスト
**children** (open → specced → dispatched → closed) 1 本に統合された。 残課題つき完了を
正常系とし、 残課題を新しい子として還流させて次の Go につなぐ (フィードバックループ前提)。

決定 7 の「Go = 子 task 生成 (単数)」はこの形に読み替える。 task 統合との相性はむしろ良く、
dispatched な子はそのまま boid の親子 task 構造に落ちる。 Phase 1 の設計論点は「open /
specced な子 (まだ boid task でない残課題・実行仕様)」の置き場 — 子を最初から pre-execution
状態の task にするか、 `task_triage` 側の JSON に留めて dispatch 時に task 化するか。 後者なら
実測 (a) 項 2 (open_child_count が親の done を塞ぐ) を構造的に回避できるため、 現時点の
第一候補は後者。

### 逆輸入 2: dispatched → working、 Go は「ready にする判断」

- 状態 dispatched は **working** に再定義された (2026-08-06)。 boid task 進行中に加え、
  nose が手動対応中 (Jira 操作・返信など task を起こすまでもない作業) も含む。
- **Go とは「triage task を ready にする判断」**であり、 task を起こす行為そのものではない。
  ready → working (specced な子の spec からの task create) は決定論の機械処理でよい。
  決定 12 と整合する再定義で、 daemon 実装では「Go 操作 = ready 遷移 + 機械 dispatch」の
  2 段で実装する。 ready に滞留しないのは正常、 滞留は dispatch 機構の不調のサイン。
- done は「全子終端 ∧ source 終了」で**自動で落とす** (承認不要)。 dropped は必ず nose の
  判断 — 終わった件と捨てた件の区別が成功の定義の計測に必要なため。
- **state と suggestion は直交する**: state は「今動けるか・誰のコートにあるか」、
  suggestion は「動くとしたら何をするか」。

### 逆輸入 3: suggestion / observed / related_jira_issues

- **suggestion**: エージェントの推奨 1 つ (verb: go / shape / manual / park / drop / wake +
  action / reason / basis)。 verb は nose の応答語彙と揃えてあり、 queue の問いに対する
  「記入済みの答え」になる — Phase 1 の Web UI queue はボタンを選択済みで見せられ、 verb で
  並び分けできる。 daemon は verb をフィールドとして読むだけなので決定 12 に反しない。
  保存された記録ではなく**一次情報からの導出 (派生) フィールド**で、 巡回のたびに組み立て
  直される — 鮮度が価値。 state を動かす提案 (park / drop / wake) は証拠つきで card-suggest
  だけが書き、 採用は nose。 却下履歴は (card, verb, basis) で照合され同じ根拠の再提案は
  機械的に捨てられる。
- **observed**: 機械観測の射影 (子 task の生死・Jira status)。 evaluate だけが書く。
  gone (task が DB から消えた事実) と done (完了) を区別する。
- **related_jira_issues**: source.type をまたいで同一案件を束ねる補助キー。 論点 g (dedup)
  の実解が既にある形。
- **wake の構造化が Phase 1 の宿題**: Phase 0 の wake は自由記述であり、 queue 節 1 の
  決定論評価 (wake_at 実列) に載せるには日付 / 事象 (task 終端参照) への構造化が要る。

### 整合の確認: claims / decisions 二層は決定 12 / 13 の設計図

Phase 0 のイベント機構 — claims (source 方言・workspace 内・増やしてよい) / decisions
(共通語・card ごと・型を増やさない)、 evaluate (時計を持つ規則) / project (時計を持たない
純粋関数) の分離 — は決定 12 / 13 とそのまま整合する。 decisions の語彙 (captured / triaged /
attrs_set / child_* / parked / woken / done / dropped) は `actions.type` (自由文字列、 実測 c)
にそのまま流し込める。 **Phase 1 の daemon 側実装は greenfield ではなく、 この機構の移植と
して設計する** (observe → daemon の時計 + task DB 参照、 evaluate → queue の決定論的評価、
project → daemon 側の state 導出)。

---

## 段階導入

### Phase 0: 機構追加なしの dogfood (最初の検証)

最初に検証すべき仮説は storage でも UI でもなく、 **「push queue を信頼して、 応えるだけで
仕事が進むか」「triage 品質は十分か」**。 これは既存機構だけで検証できる:

- workspace は接続が揃っている 1 つから始める (khi 想定: mail / Jira / Slack の credential
  導線が整備済み)。
- メタプロジェクトは khi では **`khi-task-collector`** (customer bitbucket 上の自分用 repo、
  メタプロジェクトの原型として登録済み) をそのまま使う。 upstream の一般原則はストレージ節
  原則 3。 前提が既に揃っているため **Phase 0 は即開始できる**。
- ingester も既存原型からの改修で始める: host 側に Slack のみを対象とした 4 時間洗い替えの
  loop task 原型が既にある (コンテナ化検証のため cron は停止中)。 これを「使い切り task 化 →
  本文ファイル (`issues/`) 経由の挿入 → mail / Jira の追加 → digest 会計」の順で本設計に
  寄せていく。
- ingestion / triage を task_behavior として定義。 定期起動は host cron から
  `boid task create` (論点 b)。 ノイズ落とし・ triage 会計・ mail 既読化 (決定 10/11) まで通す。
- queue はメタプロジェクト内の digest ファイル + notify で代替 (triage 会計もここに載せる)。
  応答は既存の task 操作で行う。

exit criteria (2 週間程度): (1) nose が「見に行く」頻度が実際に下がる。 (2) 取りこぼしの発覚が
概ね無い。 (3) ノイズ判定の誤りが triage 会計の監査で許容内。 (4) card に相当する情報の形が
安定してくる。

### Phase 1: daemon inbox + Web UI queue

- storage は確定 (実測 a/b/c): **task モデル統合 + sidecar `task_triage`** (urgency / wake_at
  は実列 + index、 summary / task_spec / source / content_ref は JSON 1 列)。 前提だった
  scoping 欠落 (`task_get` / `task.reopen`) は PR #927 で修正済み。
- pre-execution 状態 (captured / triaged / parked / ready) を state machine への action 追加で
  導入し、 実測結果節 (a) の対処チェックリスト 11 項目を潰す。 特に: open フィルタの第 3
  カテゴリ化 (項 1、 メタプロジェクト既定除外 filter と統合して設計)、 pending 限定ガードの
  緩和 (項 4)、 Go 遷移ルール (項 5)。
- 状態遷移はすべて action 経由で記録する (決定 13、 実測 c — 追加スキーマ不要)。
- 子 (children) の扱い: dispatched な子は boid の親子 task 構造にそのまま落とす。 open /
  specced な子は `task_triage` 側 JSON に留めて dispatch 時に task 化する (逆輸入 1 の
  第一候補 — 実測 (a) 項 2 の回避)。
- queue 表示は suggestion.verb を「記入済みの答え」として使う (逆輸入 3)。 wake の構造化
  (日付 / 事象) もここで行う。
- queue を「queue の決定論的評価」節のルールで実装。
- Web UI: queue 表示、 Go (= task create)、 破棄、 task 一覧のメタプロジェクト既定除外
  filter。 notify 連携。
- Phase 0 の digest ファイル運用を inbox に置換する。

### Phase 2: 整形・テーマセッション

- 整形セッション: queue から起動 → 対象 workspace 内で対話 → ready card を出力。
- テーマセッション: 定期起動のインタビュー形式。 agent がテーマ文書と最近の card から議題を
  用意 → nose と対話 → テーマ文書更新 + Go 候補の抽出。
- queue → workspace セッションへの deep link。

### Phase 3: 展開と構造化

- 他 workspace への展開。 開示ポリシーの語彙 (論点 e)。
- モデル安定後: issue エンティティ化 (決定 7) の要否判断。
- Web UI の project ファイル閲覧 (汎用機能、 別 doc)。

---

## 非目的

- **workspace 横断の full content 集約** — secret compartment を壊すため恒久に行わない。
  横断は card メタデータのレベルまで (決定 8)。
- **チーム共有** — 本仕組みは personal。 workspace export にメタプロジェクトと inbox を
  含めない (論点 f)。
- **バックアップ機構そのもの** — メタプロジェクトの有無に関わらず必要な別トラック
  (workspace $HOME volume / daemon volume の backup)。 本 doc は依存として参照するのみ。
- **daemon 内 scheduler** — Phase 0-1 は host cron で足りる。 内蔵化は論点 b で将来判断。

---

## 未解決論点

- **論点 a: メタプロジェクトの upstream 置き場** — **workspace の credential 境界内で確保**
  (ストレージ節 原則 3)。 khi は customer bitbucket 上の `khi-task-collector` で確定済み。
  boid-hosted bare repo (credential ゼロの恒久解) は優先度を上げ、 daemon volume backup 整備後
  に移行する。
- **論点 b: 定期起動の機構**。 ingester の形態は使い切り task で決定 (実行場所節)。 残る論点は
  cron の置き場所のみ: host cron (Phase 0) vs daemon 内 scheduler (内蔵化)。
- **論点 c: capture UX**。 頭の中からの課題感を最速で入れる経路 (音声入力が第一級)。
  workspace 不定 routing (決定 5 の例外) の確認 UI。 Web UI quick capture か、 既存チャット
  経由か。
- **論点 d: queue の順位付けと学習**。 urgency の語彙、 並べ方、 再浮上条件の評価。 nose の
  応答傾向 (破棄・ 後回し) をノイズ判定 (決定 10) と順位付けに反映するか。 Phase 0 の観察から
  決める。
- **論点 e: 開示ポリシーの語彙**。 workspace ごとに「件名まで出す / 相手ドメインのみ」等。
  デフォルトは保守的に。
- **論点 f: export 除外**。 workspace export/apply (volume-only-daemon.md) にメタプロジェクトを
  乗せない印の付け方。
- **論点 g: card の洪水対策**。 dedup (同一 source ref の再 push は update 扱い)、 TTL、
  queue 側の既読 / 未読表示。 Phase 1 の設計時に決める。
- **論点 h: source 既読化の詳細**。 label 併用の要否、 archive まで踏み込むか、 nose 自身の
  既読との意味論衝突 (自分で先に読んだメールの扱い)、 誤ノイズ判定時の復旧 (triage log からの
  再起票)、 Slack / Jira の cursor 保存場所。
- **論点 i: project ref 解決の存在オラクル**。 ambiguous project ref のエラーが daemon 全域の
  一致件数を返すため、 sandbox から cross-workspace の project 存在が推測できる (実測 b)。
  軽微だが、 Phase 1 で triage task の流量が増える前に握りつぶすか判断する。

---

## 前提とする既存機構

| 機構 | 参照 | 備考 |
|---|---|---|
| $HOME workspace volume (workspace の全 job にマウント) | [home-workspace-volume.md](home-workspace-volume.md)、 `internal/dispatcher/workspace_home_volumes.go` | 実装済み |
| workspace デフォルト project | [workspace-default-project.md](workspace-default-project.md) | 実装済み (PR #868-874) |
| brokered builtin op + policy table | boid-add-builtin (skill)、 broker の TokenContext | 実装済み。 決定 2 の enforcement 点 |
| project meta の in-memory カタログ | `boid project reload` 系 | 実装済み。 決定 5 の routing 材料 |
| Web UI (device pairing + web_secret) / notify | CLAUDE.md「Web UI」、 `internal/notify/` | 実装済み。 信頼境界の門番 |
| 終端 task の 30 日 GC | CLAUDE.md「自動 GC」 | 決定 7 の理由 3 |
| daemon 側 project bare repo | [volume-only-daemon.md](volume-only-daemon.md) §論点a/b | 決定 6 の「Web UI ファイル閲覧」素材、 論点 a の boid-hosted upstream 素材 |

実装着手時は、 workspace-default-project.md の「現状の実測」節の流儀で、 触る箇所の実測確認を
先に行うこと (本 doc は構想段階であり、 上表は設計討議時点の認識)。

---

## 検討の経緯 (2026-07-30)

元の問いは「メタプロジェクトを普通の project にするか、 boid 組み込みにするか」の 2 択だった。
討議でこの 2 択が 4 つの独立な決定 (データの置き場 / データの形 / agent の実行場所 / UI) の
束であることを解き、 以下のように解消した:

- **export 混入・ git ノイズ懸念** → 内容と状態の分離 (決定 6)、 生データの repo 外置き、
  論点 a/f で解消。
- **「Web UI 表示には組み込みが要る」** → 状態と queue は daemon inbox が持つ (決定 6)。
  ファイル閲覧は汎用機能として将来別件。
- **daemon 集約を当初阻んだ 2 懸念**:
  - 「project の内容を見ずに dispatch できるか」 → routing の 2 段化 (決定 5) で解消。
    内容を読む工程は対象 workspace 内の整形セッションに置く。
  - 「プロジェクト横断の状態が見えてしまう」 → task DB が既にその形であり、 新規の露出では
    ない (決定 1)。 mail / Jira 等の resource access の隔離は workspace 単位で維持される。
- **却下した代替**: task の状態拡張 (決定 7 の 3 理由)、 federated 無 store 表示 (決定 1)、
  full content の横断集約 (非目的)。

### 第 2 版 (2026-07-31、 nose 初読フィードバック) での変更

- **inbox と queue の同一視をやめた**。 実運用は「後回し」が多数を占めるため、 store (inbox) と
  提示 view (queue) を分離し、 後回し (park) を応答の第一級にした (決定 9、 状態 parked)。
- **ノイズの全量集約をやめた**。 mail / Slack のノイズは workspace 内 triage で落とし、 網羅性は
  triage 会計 (log + 日次 digest) で担保する (決定 10)。
- **source 書き戻しを仕様化した**。 「読み取ったら既読にしたい」を ingestion の契約にした
  (決定 11、 論点 h)。

### 第 3 版 (2026-07-31、 ユースケース追加) での変更

ユースケース 6 本 (UC-1〜6) を追加。 起こす過程で露出したギャップを反映:

- card に **summary** (判断用要約) と **task_spec** (Go だけで task create できる実行仕様) を
  追加。 「文脈同梱」と「Go 一発」の実体はこの 2 フィールド。 card が運べる上限も決定 3 に明記
  (原文は越えない)。
- triage が実行仕様まで書ければ card は**到着時点で ready** になれる (UC-1)。
- daemon が**自 workspace 印の card を dispatch payload で手渡す**経路を決定 2 に補足
  (整形セッション UC-3・ 棚卸し UC-5 が使う)。
- head-capture の本文が workspace に着地するまでの**取り込み task** 経路を UC-4 で定義。

### 第 4 版 (2026-07-31、 nose 2 巡目: 実行モデル・ストレージ・モデル統合) での変更

- **決定 12 を追加**: queue 評価は決定論ロジックのみ (daemon にエージェントを置かない)。 ルール
  本体を「queue の決定論的評価」節として明示 (心臓部)。
- **「エージェントの実行場所とタイミング」節を追加**: ingester = 使い切り boid task (cron
  起動、 洗い替え不要)、 整形・ テーマ = session (nose の tap で起動)、 daemon = 決定論のみ。
- **「ストレージと信頼性」節を追加**: データ種別ごとの置き場所と消失時影響の表 + fail-open
  原則。 論点 a は「当面 private GitHub で確定」に更新。
- **決定 7 を改訂**: 構造化の第一候補を「隣接エンティティ」から **task モデルの最小拡張**
  (card = メタプロジェクト所属 task、 Go = 子 task 生成) に変更。 残る実測課題は task 状態機械
  の consumer 監査。 命名の解決 (統合なら固有名不要 / 別モデルなら issue・ concern) も記載。
- **決定 13 を追加**: 状態遷移はイベントソーシング (既存 actions / timeline 機構の徹底として)。
- card スキーマ案を「Phase 0 で洗う候補」に格下げし、 frontmatter の役割を耐久写しとして再定義
  (ストレージ節 原則 2)。
- (追記) ingester による task 一覧の汚染は**メタプロジェクト単位の既定除外 filter** で対処
  (behavior 名 filter 案は撤回)。 ingestion の失敗・沈黙は task ビューでなく **staleness
  watchdog** (queue 節 7) で検知する。

### 第 5 版 (2026-07-31、 nose 3 巡目) での変更

- **watchdog を棚卸しに拡張** (queue 節 7): 「事象 wake が永遠に来ない parked」と someday が
  埋もれる経路を、 「棚卸しに出る (UC-5)」+「棚卸しの放置は watchdog が刺す」の 2 段で塞いだ。
- **upstream の原則を書き換え** (ストレージ節 原則 3): 「当面 private GitHub」を撤回し、
  「workspace の既存 credential 境界内に置く」に一般化。 顧客 workspace への個人 GitHub secret
  持ち込みは、 untrusted input の場に個人全 repo への到達性を足すことになるため明確に回避。
  boid-hosted bare repo は credential ゼロの恒久解として優先度を上げた (backup 整備が前提の
  まま)。
- **Phase 0 は即開始可能**: khi は customer bitbucket の `khi-task-collector` (メタプロジェクト
  の原型) が登録済みで、 前提が揃っている。

### 第 6 版 (2026-08-10、 Phase 1 実測) での変更

- **決定 7 の実測チェックリスト (a)(b)(c) を完了し、 task モデル統合で確定**。 結果の全文は
  「Phase 1 実測結果」節 (新設)。 要旨: (a) 未知状態で panic する経路はゼロで、 リスクは
  「`NOT IN ('done','aborted')` の誤包含」と「pending 限定ガード」の 2 系統 11 箇所に集約。
  (b) brokered op の workspace scoping は成立、 欠落していた `task_get` / `task.reopen` は
  **PR #927 で修正・merge 済み**。 (c) フィールド置き場は sidecar `task_triage` + queue 述語
  (urgency / wake_at) のみ実列で確定。
- **「card」の固有名を廃止し「triage 段階の task」に統一** (nose 指摘: card は一般的すぎ、
  内容でなく入れ物を指す語)。 決定 7 (改) が予告していた「統合なら固有名不要」の実行。 代替
  候補 issue は Jira issue の ingest 文脈と衝突するため不採用。 sidecar テーブル名は
  `task_triage` (機能で名付け、 状態 triaged とも揃う)。
- 論点 i (project ref 曖昧解決の存在オラクル) を追加。 Phase 1 節を実測結果で具体化。
- **Phase 0 実運用からの逆輸入を追記** (同日 2 巡目): khi-task-collector の
  `docs/card-format.md` / `docs/card-events.md` (2026-08-10 の main) を突合し、 children 統合
  (1 card = 複数 task)・working / Go 再定義・suggestion / observed / related_jira_issues を
  取り込んだ (「Phase 0 実運用からの逆輸入」節)。 状態機械とスキーマ案も追従。 claims /
  decisions 二層が決定 12 / 13 の設計図になっていることを確認し、 Phase 1 は同機構の移植と
  して設計する方針にした。
