# プロジェクト横断の課題トリアージ (メタプロジェクト + daemon inbox)

ステータス: **draft (第 12 版 2026-08-13 — done 自動落ちの前提を canonical source 制約
(決定 16) で確定し、 done からの再浮上経路を PR-5 スコープに追加。 初版 2026-07-30)**。
Phase 0 dogfood 稼働中・ Phase 1 PR-1/2/3/4 merge 済み・PR-5 (読み戻し口 + done 自動落ち +
`reopen_triaged`) 未着手。
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

第 11 版で workspace 側に残る**本文ファイル**の呼称を **note** に確定した (khi 実機の
`issues/<slug>.md` → `notes/<slug>.md`)。 daemon 側の実体名は「triage 段階の task」で既に
確定しており、 workspace 側にもう 1 つ固有名を立てると同じ二重語彙を再生産するため、
**固有名ではなく役割語**を置く: note は「その triage task の note」であり、
`task_triage.detail.content_ref` が指す先そのもの。 card を廃した 2 つの理由 (一般的すぎる /
内容でなく入れ物を指す) のうち後者にも該当しない — note は中身そのものを指す語である。
対抗候補だった dossier (append-only で経緯が積まれる実体としては最も正確だが口頭で重い) と
docket (queue との相性は良いが初見で意味が取れない) は不採用。

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

### 決定 3 (2026-08-19 改): 越えてよい範囲は開示ポリシーが決める

**当初の形 (〜2026-08-18)**: card は見出し・ 緊急度・ 出所ポインタ級の情報のみ持ち、 メール
本文や課題の詳細は workspace 内 (メタプロジェクトのファイル、 または元システム) に留まる。
深掘りは workspace のセッションを開いて中で読む。 secret と同様、 本文も「最初から持っている
場所でしか開かない」。

**改訂後**: 越えてよい範囲は **開示ポリシー (論点 e) が決める**。 khi は「本文まで」とし、
原文は task の `description` として daemon 側に載る。 深掘りのために workspace のセッションを
開く必要は無くなる。 改訂の理由と受け入れる変化は下記。

card が運べる上限は **開示ポリシー (論点 e) が決める**。 開示の粒度 (件名まで出すか、
相手ドメインのみか、 本文まで載せるか) は workspace ごとのポリシー (データ) として定義し、
enforcement は daemon 側で行う。

**2026-08-19 改**: 当初この節は上限を二重に書いていた — 「上限は論点 e の管轄」としながら、
その上に「原文そのもの (メール本文・ 添付・ 文書全文) は越えない」というハードな線を焼き込んで
いた。 後者を外し、 上限を論点 e に一本化する。 理由 (nose):

- **境界として実際に効いているのは外部接続の credential 分離**であって、 本文が daemon 側の
  DB に載るかどうかではない。 全体のバランスとして、 前者が保てていれば十分と見る
- 本文は元システム (Jira / Slack / mail) から workspace がいつでも取り直せるので、 workspace 側の
  写しは**正本ではなくキャッシュ**である。 「どちらが正本か」ではなく「どこにキャッシュするか」の
  話にすぎず、 それに設計上の層を 1 つ割く価値が薄い
- ハードな線を維持すると、 続報の原文の追記先・ slug ↔ identity の対応・ note への射影とその
  トリガ、 といった層がまるごと必要になる (論点 k の検討で表面化した)

**受け入れる変化**: daemon の Web UI (Cloudflare Tunnel 経由で外部公開しうる) が配信する内容に
本文が含まれるようになる。 保管そのもの (DB volume / workspace volume) はどちらも同じホストで
同じようにバックアップされており at rest はほぼ等価だが、 **露出面は変わる**。 とくに Slack や
mail の本文には認証情報が貼られていることがある。 khi のポリシーは「本文まで」とするが、
新しい workspace の既定は保守的側 (要約まで) に置く。

**再取得できない source がある** (2026-08-19、 Fable レビュー指摘): 上の理由の 2 つ目
(「本文は元システムから取り直せるのでキャッシュにすぎない」) は、 `source.type` が
**`head` (頭から直接 capture) と `agent`** の場合には成立しない。 外部に正本が無いためである。
note が退役すると、 これらの原文の**唯一の写しが daemon DB** になる。 **daemon volume の
バックアップを一次の耐久性とする**。 UC-4 は既に「本文は nose 発なので daemon が一時保持しても
compartment 問題は無い」としており compartment 上は矛盾しないが、 「一時保持」が「正本」に
変わる点は明示しておく (ストレージ節の原則 2 (b) の fail-open も、 この class では成立しなくなる)。

**enforcement は弱くなる**: 当初のハードな線は「本文を運ぶフィールドが存在しない」という
構造的な enforcement だった。 改訂後、 daemon は opaque なテキストから「これは要約か原文か」を
機械的に判別できない (逆輸入 3 の closed set を守る限り意味検査はできない)。 論点 e の
enforcement の実体は **workspace 側の自制 + サイズ上限**に落ちる。 daemon が機械的に効かせ
られる唯一の枠として、 `description` と action payload のサイズ上限は別途決める。

**信頼境界の残余リスクが上がる**: 本編の信頼境界節は honeypot 性を「現行の task タイトルの
daemon 集中と同レベル」と評価していたが、 改訂後の daemon DB は全 workspace の生本文を持つので
**この評価はもう成り立たない**。 デバイス認証 1 枚の突破・ 端末盗難で全 workspace の本文が
読める、 が新しい残余リスクである (受容するが、 評価としては格上げする)。

**決定 4 (prompt injection) の再導出が要る**: 改訂前は汚染原文が workspace 内に留まり、
daemon が運ぶのは triage LLM が書いた summary / spec (一段濾過済み) だった。 改訂後は汚染された
生の本文が `description` として Web UI・ 整形セッション・ 実行 task の文脈へ配送される。
決定 4 の結論 (「Go で止まる」) は成立すると考えるが、 **「本文は既定で畳んでおく」と Go の
判断材料はトレードオフ**である点を含め、 決定 4 側で再導出する (未了)。

なおテーマ文書は引き続きファイル (決定 6)。 ただし実物は `context/thema/` にあり非 git で、
`kind: theme` の card (運用開始前の机上設計) とは現時点で別物である — 統合は
capture-and-thema.md で先送り中。

**未反映箇所**: 本 doc には旧決定 3 を前提にした記述がまだ残っている — 概観 (「本文は
workspace 内に留まり、 深掘りはその workspace のセッションを開いて行う」)、 用語表 (triage 段階の
task は「本文を含まない」)、 信頼境界表 (「本文は各 workspace セッション経由」)、 スキーマ案
(summary「原文は含まない」/ `content_ref`)、 UC-1 / UC-3 の手順、 ストレージ表 (「課題・
テーマ本文 (note) — $HOME workspace volume」)、 検証シナリオ S1 (「daemon には流れない」)。
実装前の構想 doc なので全文改訂はせず、 **この節を現行とする**。 実装に入る段で棚卸しする。

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
  **2026-08-19 改**: **課題 (card) の本文はここから外れ**、 daemon の `description` に載る
  (決定 3 改)。 テーマ文書は引き続きファイルだが、 実物は `context/thema/` にあり非 git で、
  `kind: theme` の card とは現時点で別物である (統合は capture-and-thema.md で先送り中)。
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

credential と外部到達性の workspace 横断集約は行わない (compartment を壊すため、 非目的。 card 本文の扱いは決定 3 改)。
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

### 決定 14 (第 11 版): state の正は daemon 側ただ 1 つ — workspace 側の fold は退役する

Phase 0 の khi は自己完結した event sourcing を持つ (claims → `evaluate.py` → decisions →
`card_model.fold()` → frontmatter)。 daemon 統合にあたり、 この decisions / fold を daemon の
actions / `task_triage` と**並走させない**。 workspace 側の fold を退役させ、 **state の正は
daemon 側ただ 1 つ**にする。

検討した 2 案:

- **案 A (khi が正、 daemon は射影先)**: khi は今まで通り decisions を畳み、 ついでに daemon へ
  action を push する。 PR-4 が暗黙に想定していた形で、 追加実装がほぼ要らない。
- **案 B (daemon が正、 khi は claims と本文だけ)**: khi の decisions / fold を退役し、
  workspace 側は claims (source 方言) と note (本文) と evaluate の翻訳・照合規則だけを持つ。

**案 B を採用** (nose 判断、 2026-08-13)。 理由は「二重 fold は、 両者のロジックがズレたときに
**誰も気づかない**」こと。 案 A は khi 側 fold の読み手が (queue の提示面が daemon に移るため)
実質いなくなるにもかかわらず書き続ける形になり、 ズレの検知手段が無いまま乖離が進む。 これは
決定 13 の「event 追記を正・ state は導出」を 2 箇所で別々に行うことであり、 決定 13 の趣旨
そのものに反する。

この決定で card-events.md の二層 (claims = source 方言・ workspace 内 / decisions = 共通語・
daemon と共有する契約) はむしろ元の意図に近づく — **decisions が daemon の actions に置き換わる**
だけで、 層の切れ目は動かない。

帰結:

- **workspace 側から退役するもの**: `decisions/*.jsonl`、 `card_model.fold()`、
  `bootstrap_events.py` / `verify_roundtrip.py` (移行済みの遺物)、 `observe.py` の
  `boid_task_observed` (子の生死は daemon が `child_closed` を自己記録する、 論点 9)、
  `evaluate.py` の wake 日付評価と done 自動落ち (daemon の QueueSweepLoop と決定 15 へ移譲)。
- **workspace 側に残るもの**: claims、 note (本文、 append-only)、 `evaluate.py` の照合・
  方言→共通語の翻訳・却下履歴 (`(slug, verb, basis)`)・ urgency 単調性・ 呼び直し抑止。
  `project_card.py` は「decisions を畳んで frontmatter 生成」から「**daemon から読んで note を
  render**」へ役割が変わる。
- ~~**note は残す** (nose 判断): 本文は daemon 側に載らない (決定 3) ため、 card-suggest の判断
  根拠として現地に render された 1 枚が必要。~~ **2026-08-19 に取り消し** — 決定 3 の改訂で
  本文が daemon 側に載るようになり、 この判断の根拠が消えた。 note は退役候補
  (詳細は [ingestion-identity.md](ingestion-identity.md) と khi 側 memo)。
  なお「**人間も LLM も読むのは生成されたもの**」の原則は維持する — 生 JSON を LLM に読ませると
  そこで fold させることになる。 退役後は note の代わりに daemon の `description` が
  「生成された 1 枚」になり、 actions の生ログを読むのは**決定論のスクリプト**だけ、 という
  形で原則を保つ。
- **daemon 側に不足が 2 つ生じる**: 読み戻し口 (下記 PR-5 設計メモ) と working→done (決定 15)。
  案 A ではどちらも khi 側が代替していたため顕在化していなかった。

受容する代償: **daemon 障害中は workspace 側が state を確定できない**。 claims は書けるので
入力は失われず、 復旧後の再評価で追いつく (fail-forward)。 また Python 側 fold のテスト資産は
破棄する。

### 決定 15 (第 11 版): done の自動落ちは daemon の決定論 rule で行う

決定 14 で khi から done 判定が消えるため、 daemon 側に置く。 承認は要らない (逆輸入 2 で
既に「自動で落ちる」と決めていた線をそのまま実装する):

```
全子 closed  ∧  observed.source_closed  ⇒  working → done
```

- **全子 closed**: daemon が第一次情報として持つ (`task_triage.detail.children`、 `child_closed`
  は daemon の自己記録、 論点 9)。
- **source 終了**: workspace 側の知識なので、 khi が `attrs_set` の `observed` で共通語化して
  届ける (「source は終了しているか」の水準まで。 Jira の statusCategory 等のチャネル固有表現は
  workspace に留める — 逆輸入 3 の境界原則)。
- 評価は決定論のみで、 daemon が見るのは children と card のフィールドだけ (決定 12 を満たす)。
  評価契機は既存の QueueSweepLoop と `child_closed` 記録直後の 2 点。

**自動の範囲を広げていくこと自体が目標**である (nose、 2026-08-13): 「終わった」の判定が人間に
戻ってくると、 queue への信頼 (成功の定義) がその分だけ落ちる。 dropped だけは引き続き nose の
判断でしか入らない (終わった件と捨てた件の区別が計測に必要、 状態機械節)。

この rule が全 triage task で成立することは、 決定 16 (canonical source 制約) が前提として
保証する。 制約が無いと `source_closed` を報告できない source (mail / slack / head) の task が
永久に working に滞留する — 第 12 版でその穴を塞いだ経緯は決定 16 を参照。

### 決定 16 (第 12 版): triaged 以降は canonical source を必ず持つ

**問題**: 決定 15 の `source_closed` を報告できる source は限られる。 jira (statusCategory) と
bitbucket PR (merged/declined) には終了状態があるが、 **mail・ slack・ head-capture・ agent には
無い** — メールの既読化は「処理した」であって「案件が終わった」ではないし、 Slack スレッドにも
発話にも終了の概念が無い。 このまま決定 15 を実装すると、 Jira/PR 由来でない起票が全部 working
に溜まり続ける。 queue には出ないので目立たないが、 (1) done と dropped の区別が死んで成功の
定義が計測できない、 (2) 30 日 GC が done/aborted 対象なので永久にゴミが残る、 (3) 自動の範囲を
広げるという目標に正面から反する。

**決定**: 状態を 3 値化したり猶予期間を設けたりするのではなく、 **「起票するなら
`source_closed` を報告できる source を必ず作る」を契約にする** (nose 案、 2026-08-13)。 khi なら
必ず Jira issue を立てる。 他 workspace でも GitHub issue 等、 相当するものは大抵ある。 これに
より決定 15 の rule は**一切変更せずに全 triage task で成立する**。

- **不変条件の境界は captured → triaged** (起票の確定)。 captured (head-capture 直後、 まだ形に
  なっていない) は対象外。 `kind: theme` は lifecycle に乗らない常駐なので最初から対象外。
- **daemon 側の判定はチャネル知識ゼロで済む**: 「canonical source を持つか」は
  **`observed.source_closed` キーが存在するか**で判定できる。 どの source type が終了概念を
  持つかという知識は workspace 側に留まったまま (逆輸入 3 の境界原則を守る)。
- **enforcement は daemon の reject ではなく watchdog** (queue 節 7)。 起票時に弾くと、
  ingestion が壊れたときに起票そのものが落ちる = 取りこぼし方向に壊れる。 「canonical source を
  持たない triaged task がある」を案内として queue に出す形にすれば fail-open。
- **`ref` は canonical source のキーにする**。 `ref` は dedup キー (論点 7) で後から変えられない
  ため、 起票の順序を「signal 到着 → 起票判断 → **canonical source を作る** → `task create
  --ref <issue key>`」に固定する。 mail の message-id や slack の thread_ts は claims 側の索引に
  残るので、 同一案件の束ね (論点 3、 workspace 管轄) の能力は落ちない。

受容する副作用:

- ingestion に **canonical source の起票 write scope** が要る。 決定 11 で source 書き戻しの
  write scope は既に議論済みだが、 「読んで既読にする」から「読んで issue を立てる」へ一段重く
  なる。
- **ノイズ判定が重くなる**。 起票 = Jira issue を立てる、 になるため、 決定 10 の「判断に迷う
  ものは落とさず低 urgency で起票」と緊張関係に立つ。 Phase 0 の観察で運用が回らなければ論点 d
  で再考する。
- 一方で **案件がチームから見える**ようになるのは明確な利得。 boid の中だけで完結していた案件が
  Jira / GitHub issue として存在するようになる (khi は顧客プロジェクトのため実運用上の価値が
  大きい)。

### 決定 17 (第 12 版): done からの再浮上経路 `reopen_triaged` を持つ

canonical source があっても、 **Jira issue の再オープンやメールのフォローアップは普通に起きる**。
決定 15 で done に落ちた triage task を triaged に戻す経路が要る。

実測 (2026-08-13):

- **「done を探しに行く」経路は既に成立している**。 `FindTaskByRef`
  (`internal/orchestrator/store.go:572`) に status の絞り込みが無いため、 done / dropped の task も
  ref で引ける。 khi がフォローアップを照合して `task create --ref <key>` を投げれば既存 task が
  返る。 **足りないのは見つけた後に押せる action だけ**。
- `attrs_set` / `child_added` / `child_specced` の `FromStatus` は captured/triaged/parked/ready/
  working の明示列挙 (`machine.go`、 論点 6-3) なので、 **done の task には何も押せない**。
- 既存の `reopen` は `done → executing` の rule を持つ (通常 task 用)。 triage task にこれを
  押すと **executing に飛んでメタプロジェクトの task が実行されようとする** — verb を流用しては
  ならない。

したがって **`reopen_triaged` (done → triaged) を別 verb として足す**。 命名は既存の
`wake_triaged` / `wake_ready` のパターンに合わせる。 `Manual: true` とし、 khi (Jira 再オープン
の観測から) と nose の双方が押せる形にする。 誤検知で蘇っても **queue に出てくる方向** =
fail-open なので許容する。

この経路があることで、 決定 15 の「落としすぎ」がリスクでなくなる — 戻せるなら積極的に自動で
落としてよい、 という向きに fail-open の向きが変わる。

**残る危険: `reopen` の誤爆**。 verb を分けても、 既存の `reopen` (done → executing) の rule は
そのまま残る。 done の triage task と done の通常 task は **status だけでは区別できない**ため、
状態機械のレイヤでは防げない (`machine.go` は sidecar の存在を知らない)。 Web UI の
`AvailableActions` には done の task に `reopen` ボタンが出るので、 **nose が誤って押すと
メタプロジェクトの task が executing に飛ぶ**。

**実装時の設計変更 (PR-5b、 2026-08-13)**: 当初は「`task_triage` 行を持つ task への `reopen` を
reject し `reopen_triaged` を案内する」形を想定していたが、 実装時に **routing に変更**した。
`reopen_triaged` を `Manual: false` (内部専用) にし、 **`reopen` を唯一の外向き verb のまま
残して、 `ApplyAction` が sidecar 行の有無で行き先を解決する**。 理由は 2 つ:

1. **Wake の先例と同型**。 `Wake()` が `ParkedFrom` から `wake_triaged` / `wake_ready` を内部
   解決するのは「どの caller も行き先を間違えられないようにする」ためで、 ここも同じ構造
   (status だけでは区別できない選択を caller に委ねない)。 reject 方式は「正しい verb を選べ」と
   caller に要求する形であり、 Web UI の reopen ボタンを押す nose には選びようがない。
2. **`reopen_triaged` が Manual:true だと `AvailableActions` に載る**。 通常の done task にも
   意味のない 2 つ目の reopen ボタンが生える (実装中に既存テストが検出)。 Manual:false なら
   ボタンにも `action send` にも出ず、 内部解決専用に閉じる。

これにより HTTP / Web UI / brokered `task.reopen` / CLI の全 caller が追加配線ゼロで正しく
振る舞う。 `dropped → triaged` の `reopen` は triage 語彙の一部 (誤破棄からの復帰) なので
routing 対象外 — 対象は通常 task rule が発火する done / aborted のみ。

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
8. **canonical source 欠落の検知** (第 12 版、 決定 16): `observed.source_closed` を持たない
   triaged 以降の triage task を案内として queue に出す。 起票時に daemon で reject しないのは、
   ingestion が壊れたときに起票そのものが落ちる (取りこぼし方向に壊れる) ため — 検知は
   fail-open な watchdog 側に置く。

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
   保持しても compartment 問題は無い。 **2026-08-19 改**: 決定 3 の改訂と note の退役により、
   この「一時保持」は**正本**になる (head-capture は外部に取り直す先が無いため)。 耐久性は
   daemon volume のバックアップが担う。 グローバル層が project カタログから workspace を推定し、
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
| 課題・ テーマ本文 (note、 `notes/` `themes/`) | **$HOME workspace volume** (第 11 版で実態に修正、 下記) | volume backup (restic、 2026-07-31〜) | backup 間隔分 |
| triage ルール・ 指示・ project.yaml | 同上 | 同上 | 同上 |
| 日次 digest (会計サマリ) | 同上 (小さく意味のある diff) | 同上 | 監査履歴のみ |
| card の運用 state (parked / wake / queue) | daemon DB | daemon volume | 全 card が再浮上する (fail-open、 silent loss にならない) |
| triage 詳細 log | $HOME workspace volume (rolling 30 日) | volume | 監査精度の低下のみ |
| source cursor (Slack ts / Jira since) | $HOME workspace volume | volume | 再走査分の再処理 (dedup で card は update になり無害) |
| 信号の生データ (メール本文キャッシュ等) | $HOME volume (揮発) | 不要 — source から再取得可能 | 無し |

原則:

1. **失って困る唯一の写しを、 backup されていない場所に置かない。** 一次情報は「再取得可能な
   source」「git」「backup 対象の volume」のいずれかに必ず載る。

   第 11 版で「volume・daemon DB に置かない」から上記へ緩和した。 khi 実機は note (本文) を
   git repo ではなく **$HOME workspace volume** に置いており (`$HOME/.local/state/
   boid-ingest/`)、 これは事故ではなく意図的な選択である (nose、 2026-08-13):
   (a) note は巡回のたびに追記されるため毎回の commit & push が想像以上に煩雑、
   (b) メタプロジェクト repo は**顧客 bitbucket 上にありチームと共有される** — タスク処理
   ワークフロー (スクリプト・ スキル・ 指示) をチームで共有できることを重く見た一方、 個人宛
   signal である note 本体は共有する性質のデータではない。 耐久性の根拠は restic による
   volume backup (2026-07-31〜稼働) に置き換わる。

   したがって repo (git) に載るのは **ワークフローそのもの** (scripts / skills / docs /
   project.yaml) であり、 **note と claims は volume** という切り分けになる。 原則 3 の
   credential 境界の議論は前者にのみ適用される。
2. **daemon 側 state の消失時の退行**。 第 11 版 (決定 14) で daemon が state の唯一の正に
   なったため、 「frontmatter に耐久写しを持ち workspace からの再 push で再構築する」という
   第 4 版以来の fail-open は**成立しなくなった** — 再 push すれば workspace 側 fold が復活し、
   決定 14 が消そうとした二重 fold そのものになる。 代わりの担保は 2 段:
   (a) daemon volume の backup からの復元が一次手段、
   (b) render 済みの note が volume に残るため、 復元できなくても**課題そのものは失われない**
   (state だけが失われ、 人手で triage し直す)。 「隠していた項目が出てくる方向にしか壊れない」
   という fail-open の向き自体は (b) で保たれる。
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

### 逆輸入 3: suggestion / observed — ただしチャネル固有の知識は境界の内側に留める

- **suggestion**: エージェントの推奨 1 つ (verb: go / shape / manual / park / drop / wake +
  action / reason / basis)。 verb は nose の応答語彙と揃えてあり、 queue の問いに対する
  「記入済みの答え」になる — Phase 1 の Web UI queue はボタンを選択済みで見せられ、 verb で
  並び分けできる。 daemon は verb をフィールドとして読むだけなので決定 12 に反しない。
  保存された記録ではなく**一次情報からの導出 (派生) フィールド**で、 巡回のたびに組み立て
  直される — 鮮度が価値。 state を動かす提案 (park / drop / wake) は証拠つきで card-suggest
  だけが書き、 採用は nose。 却下履歴は (card, verb, basis) で照合され同じ根拠の再提案は
  機械的に捨てられる。
- **observed**: 機械観測の射影 (子 task の生死・source 側の終了状態)。 evaluate だけが書く。
  gone (task が DB から消えた事実) と done (完了) を区別する。 daemon 側スキーマに載せる値は
  共通語化されたもの (「source は終了しているか」等) に限り、 チャネル固有の生の表現は
  workspace 側に留める (下記の境界原則)。
- **チャネル固有の探索キー (related_jira_issues 等) は取り込まない**: Phase 0 は claims
  (source 方言・workspace 内) → decisions (共通語・daemon と共有する契約) の二段抽象に
  なっており、 **daemon は個別のコミュニケーションチャネルの知識を持たない**。 同一案件の
  束ね (thread_ts / issue_key / related_jira_issues による card 照合) はチャネルの知識その
  ものなので workspace 側 evaluate の管轄であり、 daemon の語彙には上げない。 論点 g (dedup)
  のうち「同一案件の束ね」は workspace 側で解決済みとし、 daemon 側の dedup は「同一 triage
  task への再 push は update 扱い」の水準に留める。
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

## PR-4 設計メモ (2026-08-11、 khi 統合レビュー — Fable レビュー済み、 結果は下記「Fable レビュー結果」節)

PR-4 (ingestion push の開通・[[phase1-cross-project-triage-pr1-3-complete]] 参照) 着手前に、
khi-task-collector 実機 (`~/src/bitbucket.org/Aolani-ondemand/khi-task-collector`、
`docs/card-format.md` / `docs/card-events.md` / `docs/card-dispatch.md`) と daemon 側実装
(PR-1/2/3) を突合した。 論点が複数出たため、 **実装着手前に別セッションで Fable にレビュー
してもらう**。 以下は突合で判明した事実と、 現時点の暫定結論。

### 論点 1: `TaskTriageChildSpec` に `RemoteID` が無い件 — severity 低に格下げ

khi の子 spec (`docs/card-dispatch.md`) は `remote_id` を必須にしている
(ブランチ命名規約 `feature/${remote_id}` のため)。 daemon 側
`internal/orchestrator/task_triage.go` の `TaskTriageChildSpec{Project, Behavior,
Instruction}` には `RemoteID` が無く、 PR-2 の `Dispatch()`
(`internal/api/workflow_triage.go:343`) が組む `CreateTaskRequest` にも渡らない。

**ただし** `internal/api/task_create.go:170` に既存の汎用ロジックがある:

```go
// Children inherit remote_id from their parent when they don't supply their own.
if req.RemoteID == "" && req.ParentID != "" {
    if parent, ...; parent.RemoteID != "" {
        req.RemoteID = parent.RemoteID
    }
}
```

triage task (card) 自身の `RemoteID` さえ立っていれば、 `Dispatch()` が作る子はこの既存の
親→子継承で自動的に受け取る。 なので **`TaskTriageChildSpec.RemoteID` の追加は「子だけ親と
別の remote_id にしたい (cross-track children)」稀なケース専用に格下げしてよく、 PR-4 の
必須スコープではない**。 ingestion push が card (triage task) 作成時に、 Jira など自然な
remote issue key を持つ source なら `CreateTaskRequest.RemoteID` にもその値を積む、 という
運用で基本ケースは足りる。

### 論点 2: ブランチポリシーと `RemoteID` / `source_ref` の関係

nose 指摘: 「worktree 時代はホスト共有 FS だったので worktree 分離のためのブランチポリシーが
要った。 container backend では clone が isolation の単位なので、 ローカル分離目的のブランチ
ポリシーはもう要らないのでは」— 確認したところ**前提は正しく、 既に決着済み**
(`docs/plans/branch-policy-simplification.md`、 v0.0.11 で landed)。 `boid/<id8>` per-task
branch と fork-point 概念は完全撤去済み。

ただし `BaseBranch` 自体は消えていない。 役割が「ローカル FS 分離」から「この task の commit
がどの git ブランチ (＝どの PR) に積み上がるか」という**純粋に git 側の関心**に変わっただけで、
今も必要 (同 doc: 「並列兄弟が同じ base_branch に push すると衝突するのは executor の責務」
「真に isolate したいなら子ごとに異なる base_branch を明示指定」)。

`RemoteID` の残る役目は (a) `BaseBranch` テンプレート `${TASK_REMOTE_ID}` の展開元、
(b) `FindTaskByRemote` の冪等性チェック、(c) 上記の親→子継承。 これは **source_ref
(triage task の dedup キー、下記論点3) とは役割もスコープも異なる**:
`RemoteID` は triage と無関係な boid 全体の汎用列 (supervisor/executor 等どの task でも
使う)。 source_ref は triage 専用の dedup 概念で、 type ごとに形が違う方言
(thread_ts / issue_key / message-id) を許す。 **field としての統合はしない**が、
Jira 由来の card では両者が同じ値 (issue key) を指すのが自然、 という関係に留める
(論点1 の運用がこれを実現する)。

### 論点 3: 複数 source を持つ card (`related_jira_issues`) の束ねは daemon に持ち込まない — 決着済み

khi の card は `source` (単一の主ポインタ) に加えて `related_jira_issues`
(2026-08-07 追加、 同一案件が Slack と Jira の両方から card 化されるのを束ねる補助キー) を
持てる。 これは「逆輸入3」の境界原則で**既に daemon に持ち込まないと決着済み**
(「同一案件の束ねはチャネルの知識そのものなので workspace 側 evaluate の管轄」)。
2026-08-07 に khi で「LLM に card 照合させたら同じ PR の card が2枚に分裂した」事故があり、
その反省で決定論の evaluate 側に寄せた経緯があるため、 この境界は堅持する。

### 論点 4: task_triage の更新経路 — `UpdateTaskRequest` PATCH 案は撤回、 action append に統一

当初「`task_triage` を更新する brokered op が無いので `UpdateTaskRequest`
(`internal/apiwire/task.go:23`) を拡張する」案を検討したが、 **nose 指摘で撤回**:
workspace → daemon が伝えるべきは決定13 (状態遷移はイベントソーシング) の通り本質的に
action (event) であり、 「新しい state をまるごと PATCH で上書き」という形は、 khi が
2026-08-08 に自分で経験して捨てたアンチパターン (frontmatter 直接書き換え → 複数経路が
競合 → claims/decisions 二層への移行) を daemon 側で再生産する。

代わりに **action append 経路を使う**。 これは既に end-to-end で存在する:
`BoidOpActionSend` (`internal/server/boid_executor.go:512`) → `TaskWorkflowService.
ApplyAction` → `sm.Apply()` → `CreateAction` (`actions` テーブル、 `ParkedFrom` が読むのと
同じイベントログ)。 workspace scoping も既にかかっている。 足りないのは:

1. `internal/orchestrator/machine.go` の `DefaultMachine()` Rules に khi の decisions 語彙
   (`attrs_set` / `child_added` / `child_specced` / `child_dispatched` / `child_closed`) を
   非遷移 action (`ToStatus` 空、`Manual: true`) として追加する
2. 各 action type ごとに `task_triage.detail` を更新する side-effect 関数を書く。 先例は
   PR-1 の `applyParkSideEffect` (`internal/api/store.go:65`、 park action のコミットと
   同一トランザクションで `task_triage` を upsert)。 実質、 khi の
   `scripts/card_model.py` の `fold()` を daemon 側に移植する作業になる
3. 「create」と「update」を別 API として分けない。 **最初の1件の action が事実上の
   create** (khi の `imported`/`captured` が decisions.jsonl の1行目なのと同じ形)。
   `task_create --initial-status` はその task にとっての最初の action を兼ねるだけで、
   以降は全て `boid action send --task <id> --type attrs_set ...` で押していく。 これに
   より論点2/3 で懸念した「ref ベースの dedup をどう実装するか」も消える — khi 側が
   card 作成時に daemon から返る `task_id` を自分の decisions に保持し (khi 側の decisions
   語彙にこのための新しい型が要る、 現状は未実装)、 以降はその `task_id` へ action を
   送るだけで済む

### 論点 5: dispatch 時の parent-child モデルの相違 — **第 11 版で決着** (PR-2 側に寄せる)

khi の現行 dispatch (`docs/card-dispatch.md`, `card-promote-headless --live`) は
`parent_id: "-"` で**ルートタスク**として子を作り、 card 本文を丸ごと `description` に
埋め込んで渡している (「task は card を読めない、 別プロジェクトのサンドボックスで走る」)。
一方 PR-2 の `Dispatch()` は `ParentID: taskID` を固定で入れ、 **triage task の子**として
作る (決定7: 親子 link が由来 link を兼ね、 実行 agent が親の content_ref を辿れる)。

PR-2 側の設計は khi が今手作業でやっている「本文まるごと埋め込み」workaround を不要にする
上位互換だが、 **khi 側のスキル (`card-promote-headless` / `docs/card-dispatch.md`) の
書き換えが要る**。 boid 本体の PR-4 だけでは閉じない。 このまま ingestion push だけ開通
させると、 khi が引き続き手動で `boid task create --parent-id -` を叩き続け、 PR-2 の
自動 dispatch と二重経路になるリスクがある。

**第 11 版の決着**: PR-2 側 (triage task の子として作る) に寄せ、 `card-promote-headless` を
退役する。 決定 14 で state の正が daemon になる以上、 dispatch も daemon の `Dispatch()` が
唯一の経路でなければ children の status を二重に書くことになるため、 選択の余地は実質ない。
移行順序は論点 9 の制約がそのまま効く — **specced な子の push と手動 promote の併用は二重
dispatch になるため禁止**。 具体的な順序は「PR-5 設計メモ」節の移行手順を参照。

### Fable レビュー結果 (2026-08-11、 第 9 版) — 条件付き GO

上記 5 論点を Fable がコード実物と突合してレビューし、 続けて nose との机上シナリオ検証
(PR コメント合流 / 再 Go ループ / 事象 wake / PR 系列逐次消化) で論点を追加した。 判定は
**条件付き GO** — action append 方式 (論点 4) の方向は正しく経路の実在も確認できたが、
着手前に決着すべき事項が以下の通り残る。

**裏取りできた事実** (論点 1〜3 の主張はすべてコードで確認済み):

- 親→子 remote_id 継承は `internal/api/task_create.go` に実在。 shim
  (`internal/sandbox/boid_shim.go` の `parseBoidTaskCreate`) は task spec YAML を丸ごと
  forward するため `remote_id` / `initial_status` の配管は既に通っている — PR-4 で開けるのは
  executor 側の明示 reject (`internal/server/boid_executor.go`) と workspace 検証だけ。
- `BoidOpActionSend` → `ApplyAction` → `CreateAction` の経路と workspace scoping
  (`AllowsProject`) は実在。 `sm.Apply` は `ToStatus` 空の非遷移 action と
  `FromStatus: "*"` を既にサポートする。

#### 論点 6: 論点 4 の実装注意 4 点

1. **AvailableActions 漏れ**: Manual:true + ToStatus:"" の非遷移ルールは self-loop skip
   (`ToStatus == status`) を素通りし、 attrs_set 等が該当 status の available_actions
   (= Web UI のボタン) に湧く。 AvailableActions に「ToStatus 空 = 非遷移はボタンにしない」
   skip を足す。
2. **payload merge 汚染**: `workflow_action.go` は全 action の payload を task.Payload に
   merge する。 attrs_set の大きい payload が task.Payload に染み出すため、 reopen 同様の
   consumed exempt にする (payload は actions テーブルと side-effect の fold にのみ流す)。
   既存の park も payload が merge されている — ついでに確認する。
3. **FromStatus は "*" にしない**: 型ごとに列挙する (attrs_set / child_added /
   child_specced は captured/triaged/parked/ready/working あたり)。 "*" だと通常タスクの
   executing/done にも押せてしまう。
4. **side-effect の実装パターン**: park の先例に従う — payload 検証は Tx 前 (400 を出す)、
   `task_triage.detail` の read-modify-write は Tx 内 `GetTaskTriage` から (`Wake` の
   doc comment にある race の教訓と同じ)。

なお daemon 側の attrs_set side-effect は **last-write-wins の純粋な畳み込み**にする。
urgency の単調性 (上げるのみ) は khi 側 evaluate が保証する分担 (fold は方針を持たない) の
まま — side-effect に policy を書き始めたら境界違反のサイン。

#### 論点 7: dedup は Ref=source_ref で daemon 側に床を作る (論点 4-3 の「dedup 消える」を修正)

「khi が task_id を保持するので dedup は消える」は、 create 応答〜 khi decisions 記録の間の
クラッシュで重複 card が湧く穴があり、 論点 g の決着 (「dedup 無しでは khi 統合が動かない」)
を暗黙に上書きしてしまう。 代わりに既存の Ref get-or-create (`task_create.go`、 現状
`ParentID != ""` 限定) を root task にも開放し、 ingestion create は **Ref=source_ref**
(jira: issue_key / slack: thread_ts / mail: message-id) で投げる。 既存 card があれば
**既存 task が返る**ので khi は冪等に再送でき、 クラッシュ窓も閉じ、 `RemoteID` とは独立
なので論点 2 の「field 統合しない」も守られる。 要確認: `idx_tasks_ref_parent` unique
index が parent 無し行をカバーするか。

daemon の床は「同じ source ref かどうか」の水準のみ (論点 g の通り)。 slack 起票 card に
後から同一案件の jira/bitbucket source が現れる cross-source の束ねは
`related_jira_issues` 索引 = workspace 側の管轄のまま (論点 3)。

#### 論点 8: working からの出口 3 本 (最重要 — PR-4 と同時に修正)

現行機械は `ready` が triaged→ready のみ、 `Dispatch()` も status==ready を要求するため、
**working の card に再 Go できない**。 これは (a) レビューコメント対応 (PR コメントは
性質上 working 中に来る — PR がある = 作業中)、 (b) 大型機能の PR 系列逐次消化 (1 周ごとに
再 Go を踏む) の両方を塞ぐ。 khi の `derive_state` (children 集約から state 導出) に対応する
出口を 3 本セットで足す:

- **working→ready**: 次の子が specced 済み (Go を押すだけの状態で queue に浮上)
- **working→triaged**: 次の子は open (spec を書く shape が要る状態で浮上)
- **working→parked**: Go 時に `wake_task_id` = dispatch した子を積んで park する運用を
  可能にする。 子の終端で QueueSweepLoop が自動再浮上させる (「前の PR が終わったら次の
  Go が目の前に出てくる」半自動化)

関連する運用制約: `Dispatch()` は **specced な子を全件一斉に task 化する**。 逐次 PR 系列
では「specced は次の 1 個だけ、 残りは open」が原則 (全部 specced にすると N 本並列で走り、
「interface 変更 PR は最後にマージ」の教訓と衝突する)。

#### 論点 9: child_dispatched / child_closed は khi に送らせない (語彙の役割分担)

`child_dispatched` は daemon の `Dispatch()` が既に自分で書く (PR-2)。 `child_closed` も
親子 link (`TaskRef`) で daemon が子の終端を直接知れるため、 khi が observe → evaluate →
action send で報告するのは冗長かつ二重書き込みの race になる。 語彙を役割で割る:

- **khi が送る**: 判断系のみ — `attrs_set` / `child_added` / `child_specced`
- **daemon が自己記録**: 機械的事実 — `child_dispatched` (Dispatch 時) / `child_closed`
  (子タスクの終端検知時)

done 自動落ちの規則「全子終端 ∧ source 終了」は両側にまたがる (子終端 = daemon の知識、
source 終了 = workspace の知識)。 素直な形は「khi が source 終了を attrs_set (observed) で
届け、 daemon が両条件成立で done に落とす条件 rule」だが**未決** (working→done 遷移自体も
未実装)。 論点 5 の移行順序もここで確定: **khi が children を specced で push し始めるのは
card-promote-headless 退役後** — それまでは旧経路 (root task 化) を続けてよいが、 specced
push と手動 promote の併用だけは二重 dispatch になるため禁止。

#### 論点 10: 事象 wake の 3 分類と brokered wake op

wake 条件は 3 種に整理できる:

1. **日時** (`wake_at`) — daemon 実装済み (QueueSweepLoop)
2. **自タスク終端** (`wake_task_id`) — daemon 実装済み。 `ShouldWake` は参照先タスクの
   消失も fail-open で起こす
3. **source 事象** (「Jira が動いたら」等の自由記述) — 決定 12 の線引き (daemon が見るのは
   時計・task DB・card のフィールドだけ) 上 daemon には構造的に持てず、 workspace 判断
   (collector/observe → evaluate → card-suggest 提案 → nose 承認) のまま

種類 3 の実行の口が現状無い: brokered op 一覧に wake が無く、 `wake_triaged` /
`wake_ready` は IsManualAction ガードで reject され (これ自体は正しい)、 `Wake()` は
HTTP/Web UI 専用。 sandbox 内の khi sweep は nose 承認済みの事象 wake を実行できない。
**brokered wake op (`Wake()` を呼ぶ薄い口、 workspace scoping は action send と同じ) を
PR-4 か直後に足す** — 種類 3 が workspace 管轄なのは設計通りなので、 この口は境界を壊さ
ない。 khi 側の移行の勘所: park 時に「wake_task_id で表現できる条件か」を判定して積めば、
種類 2 は LLM の巡回から完全に消える。

#### 論点 11: Go の自動化は「代行 Go タスク」で行う (機械に焼かない)

N 個の子タスクの Go を自動化したい場合、 状態機械側に auto-Go を焼くのではなく、 **nose の
代わりに Go するタスクを定期的に走らせる** (nose 案、 2026-08-11):

- Go は今まで通り `ready` action 1 本 — 委譲の中身は代行タスクの instruction に書かれた
  ポリシーで、 ポリシーと enforcement の分離に載る。 機械側に追加スコープは不要
- 入力は `?status=queue_next` (PR-3) — daemon が決定論で「Go 可能なもの」を順位付けし、
  代行タスクは判断だけ乗せる
- 「単純な単発タスクで nose の判断が要らなそうなものを代行 Go」に自然に一般化し、
  論点 8 の wake_task_id 自動再浮上と合成できる。 止めたければ cron タスクを止めるだけ (可逆)

**前提条件**: `orchestrator.Action` に actor (誰が押したか) のフィールドが無い。 khi の
claims 封筒の `by` が「作話しうる主体と機械の事実の線を消さない」ためにあるのと同じ理由で、
nose の Go と代行タスクの Go が actions ログ上で区別できない状態で代行を始めてはいけない。
代行 Go の稼働前に Action への actor 記録 (カラム追加 or payload 規約) を入れる。 PR-4
必須ではない。

#### 論点 12: 大型機能 (設計 doc → PR 分割 → 実装) のマッピング

- shape (設計) セッションは機械の外のまま。 設計 doc はチームの doc 置き場 (boid = repo の
  docs/plans、 khi = Notion) に置き、 card はポインタのみ持つ (content_ref と同じ境界)
- PR 分割 = children。 逐次消化は論点 8 のループで、 判断として残るのは Go と次の spec
  起こしだけ (どちらも意図的 — 決定 9 の ready-gate)。 接続部 (dispatch / 子終端検知 /
  再浮上 / queue 評価) はすべて決定論
- 代替として `behavior: drive` の親 1 本に系列全体を委任する型もある (Go 粒度のトレード
  オフ — PR ごとに承認するか、 マージ条件を instruction に書き切って一括委任するか)。
  サブエージェント分割の巨大単発タスクも同型で、 差は進捗の可視性 (children = Web UI で
  見える索引 / 単発 = セッションを覗く必要がある)
- **khi の sandbox から Notion に書く経路は未整備** (bitbucket-api と同型の API gateway
  service 追加が前提インフラ)。 PR-4 と独立の宿題

### 検証シナリオ (机上検証の成果 → 実装テストの仕様)

2026-08-11 の机上検証 4 本を、 実装時のテスト仕様とレビュー時のチェックリストとして固定
する。 運用規律: **実装 PR では、 daemon 側の決定論 step に pin するテスト名を注記する。
テストが無い step には「khi 側 / LLM 判断につきテスト不能」等の理由を明示する** (テスト
無しの出荷に理由を書く — boid-review の観点をこの節に対して適用する)。

#### S1: PR コメントの既存 card への合流

前提: BGO-214 の card が daemon task として存在 (Ref="BGO-214")、 state=working、 子が
dispatch 済み。

1. khi bitbucket adapter が新規 PR コメントを拾い、 branch 名から issue key を解決、
   `related_jira_issues` 索引で card に照合する — *khi 側、 テスト対象外*
2. khi が本文を workspace 側 card に追記する (daemon には流れない) — *khi 側*
3. khi が保持する task_id へ `boid action send --type attrs_set` — 期待: actions テーブル
   に追記、 side-effect が task_triage.detail を更新、 **task.Status は working のまま**
   (非遷移)、 **task.Payload は汚染されない** (論点 6-2)
4. khi が task_id を失っていた場合、 `task create --initial-status triaged` を
   Ref="BGO-214" で再送 — 期待: 新規 task は作られず**既存 task が返る** (論点 7)
5. attrs_set 等の非遷移 action が available_actions / Web UI ボタンに現れない (論点 6-1)

#### S2: レビュー対応タスクの起票と再 Go ループ

前提: S1 の続き。 card は working。

1. sweep で card-suggest が suggestion (verb: go) を導出し attrs_set で daemon に反映 —
   *khi 側 + S1-3 と同経路*
2. nose 承認後、 khi が `child_added` → `child_specced` を送る — 期待: detail.children に
   open → specced で積まれる
3. **working の card への再 Go**: working→ready 遷移 (論点 8) → `ready` action の
   Dispatch 自動連鎖 → specced の子だけ task 化 — 期待: 子タスクの remote_id は card から
   継承され base_branch が `feature/BGO-214` になる (= レビュー対象 PR と同じブランチに
   push される)
4. 子タスクの終端を daemon が検知し `child_closed` を自己記録する (論点 9) — 期待: khi
   からの child_closed 送信は不要
5. 残課題があれば新しい子を open で積み、 card は working→triaged で浮上する (論点 8)

#### S3: parked card の wake 3 分類

1. **日時**: park payload に wake_at → QueueSweepLoop が起こす (PR-1/3 実装済み、 既存
   テストの確認のみ)
2. **自タスク終端**: park payload に wake_task_id → 参照タスクの done/aborted で起こす。
   参照先が消えていても fail-open で起こす (実装済み)
3. **source 事象**: khi が判断し nose が承認 → **brokered wake op で起こす** (論点 10、
   新規実装) — 期待: sandbox 内から `Wake()` 相当が実行でき、 ParkedFrom に従い
   triaged/ready の正しい側へ復帰する。 `wake_triaged` / `wake_ready` の直接 action send
   は引き続き reject される

#### S4: PR 系列の逐次消化 (+ 代行 Go)

前提: 設計 doc 済み、 children に PR-1..N (先頭のみ specced)。

1. Go → dispatch — 期待: **specced の 1 個だけ**が task 化される (open の子は触られない)
2. Go と同時に wake_task_id = 子タスクで park する (論点 8 の working→parked) — 期待:
   子の終端で自動再浮上して queue に載る
3. 次の子を specced にして再 Go する (S2-3 と同じ) — 期待: ループが N 周回る
4. (将来) 代行 Go タスクが queue_next を読んで `ready` action を送る (論点 11) — 期待:
   actions ログで nose の Go と区別できる (actor 記録が前提条件)

---

## PR-5 設計メモ (2026-08-13、 khi 統合の実装計画)

決定 14 / 15 と論点 5 の決着を受けた実装計画。 PR-4 までで daemon 側の**受け口**は揃って
いるが、 決定 14 (daemon が state の唯一の正) を成立させるには **読み口** と **done の自動
落ち**が足りない。 khi 側は現時点で daemon と 1 本も繋がっていない (dispatch はすべて
`card-promote-headless --live` の `parent_id: "-"` によるルートタスク化)。

### 実測: PR-4 時点で揃っているもの / 足りないもの

**揃っている** (2026-08-13 に main で確認):

| 用途 | 口 |
|---|---|
| triage task の起票 | `boid task create` の `initial_status` / `ref` (`internal/apiwire/task.go:46,54`)。 shim は task spec YAML を丸ごと forward する |
| 判断系の push | `BoidOpActionSend` で `attrs_set` / `child_added` / `child_specced` (`machine.go:393`)、 `park` / `ready` / `triage` / `drop` |
| Go と dispatch | `ready` action → `Dispatch()` 自動連鎖 (specced な子のみ task 化) |
| 機械的事実の自己記録 | `child_dispatched` (Dispatch 時) / `child_closed` (`finalizeTerminal` フック) — khi からの push は `IsManualAction` で reject される (論点 9) |
| 事象 wake | `BoidOpTaskWake` (`protocol.go:159`) |
| queue の列挙 | brokered `boid task list --status queue_next` (`boid_executor.go:108` の validate で許可済み) |
| **done の探索** | `FindTaskByRef` (`store.go:572`) に status 絞りが無く、 done / dropped も ref で引ける (決定 17)。 フォローアップ照合に追加実装は不要 |

**足りない (PR-5 のスコープ)**:

1. **`task_triage` の読み口がゼロ**。 HTTP API にも brokered op にも存在せず、 唯一の読み手は
   Web UI 内部の enrichment (`internal/api/web.go:279` の `queueTriageByTaskID`)。
   `BoidOpTaskGet` は `GetTaskField` 経由で **Task DTO のフィールドしか返さない**
   (`internal/server/boid_executor.go:252`) ため、 sidecar は sandbox から一切見えない。
   決定 14 では workspace 側が state を持たなくなるので、 **読めないと巡回が成立しない**。
2. **`working → done` の遷移が存在しない**。 `machine.go` の working からの出口は
   ready / triaged / parked の 3 本だけ (論点 8) で、 done への道が無い。 案 A なら khi の
   `evaluate.py` が done を判定していたが、 決定 14 でそれが消えるため、 **card が永久に
   working に溜まる**。 決定 15 の rule として daemon 側に実装する。
3. **`done → triaged` の再浮上経路が存在しない** (第 12 版、 決定 17)。 done の task には
   `attrs_set` すら押せない (`FromStatus` の列挙外) 一方、 既存の `reopen` を流用すると
   `done → executing` に飛んでメタプロジェクトの task が実行されてしまう。 **`reopen_triaged`
   を別 verb として追加**する (`Manual: true`、 khi と nose の双方が押せる)。
4. **canonical source 欠落の watchdog** (第 12 版、 決定 16): `observed.source_closed` を持たない
   triaged 以降の triage task を案内として queue に出す。 起票時の reject はしない。
5. **`task_triage.urgency` に書き手が存在しない** (2026-08-13、 PR-5a 実装中に発覚)。
   queue の心臓部である `ListTasks("queue_next")` は `task_triage` を INNER JOIN して
   `tt.urgency` で絞り・並べ (`internal/orchestrator/store.go`)、 rule 4 の notify
   (`notifyQueueEntryIfUrgent`) も同じ列を読む。 ところが **daemon 内のどこもこの列を書いて
   いなかった** — `attrs_set` は urgency も含めて全キーを不透明な `detail.attrs` blob に畳んで
   いたため、 queue view は恒久的に空・ notify は発火し得ない状態だった。 PR-1/3 が実列にした
   意図 (実測 c: queue 述語だから実列) と、 PR-4 が書いた fold の間に落ちた穴。

### PR-5 の分割 (2026-08-13)

- **PR-5a (実装済み)**: 読み戻し口 + 上記 5 の穴埋め。 「sidecar を読めるようにし、 かつ実際に
  埋まるようにする」という 1 テーマ。
- **PR-5b (実装済み)**: done lifecycle — `working → done` の自動落ち (決定 15)、
  `reopen` の routing (決定 17)、 canonical source の検知 (決定 16)。

PR-5a の内容:

- `api.TaskTriageView` + `GetTriage` / `ListTriage` (`internal/api/triage_read.go`)。 task 行 +
  sidecar 実列 + **actions から導出する `parked_from`** + 不透明 `detail` を 1 つに union する。
- HTTP: `GET /api/triage` (一覧、 `project_id` / `workspace_id` / `status`) と
  `GET /api/triage/{id}`。 `/api/tasks` 配下でなく独立ルートにしたのは、 一覧が collection
  endpoint を要るのと、 `{id}` ワイルドカードと同位置に静的 `triage` を置きたくないため。
- brokered: `BoidOpTaskTriageGet` / `BoidOpTaskTriageList` (`boid task triage <id>` /
  `boid task triage --list`)。 scoping は `BoidOpTaskGet` / `BoidOpTaskList` と同型
  (get は task を引いてから `AllowsProject`、 list は明示 project を検査し、 無指定時は
  `AllowedProjectIDs` を回して**決して無スコープで引かない**)。
- `attrs_set` の `urgency` / `kind` を**実列へ昇格**し、 blob には二重に持たせない (ドリフト
  防止)。 語彙は closed set で検証する — これは論点 6 が daemon から排除した policy (「urgency は
  上げるのみ」= khi の evaluate 管轄) ではなく、 **daemon が自分の SQL 述語を守る**検証。 typo が
  素通りすると card が queue から永久に落ちるのに何のエラーも出ない、 という queue の信頼を
  最も損なう形の失敗になるため。
- **triage task は生成時に sidecar 行を持つ**という不変条件を導入
  (`TaskAppService.CreateTask` が pre-execution status の task に空行を seed)。 `ListTriage` の
  「行があれば triage task」述語と PR-5b の reopen routing がこれに乗る — status だけでは判定
  できない (`done` は通常 task と共有) ため。 既存行には migration 0040 で backfill する
  (PR-1〜4 期に作られた triage task は最初の side-effect action が来るまで行を持たないため、
  遡って埋めないと不変条件が成立しない)。 backfill 対象は triage 専用 status のみで、
  `done` / `aborted` は**意図的に除外** — 通常 task と区別できず、 executor task を誤って
  triage task と印付けるほうが実害が大きい (working→done は PR-5b で初めて存在するので、
  既に done に到達した triage task は存在しない)。

PR-5b の内容:

- **決定 15**: `orchestrator.ShouldAutoDone` (純関数、 `ShouldWake` と同型) +
  `TaskWorkflowService.SweepDone`。 遷移は **`triage_done`** (`working → done`、
  `Manual: false`)。 **`done` の名前を再利用していない**のが要点 — `IsManualAction` は action
  名で判定するため、 `done` を使うと `boid action send --type done` が working から通ってしまい、
  daemon が 決定 15 の条件を評価しないまま khi が完了を主張できてしまう。 評価契機は
  QueueSweepLoop と `child_closed` 記録直後の 2 点。
- **決定 16**: `orchestrator.MissingCanonicalSourceGuidance` (純関数、 `WatchdogGuidance` と
  同型) + `SweepCanonicalSourceBreaches`。 判定は `observed.source_closed` **キーの存在**のみで、
  source type の知識は持たない (逆輸入 3)。 **スコープ縮小を明示**: queue 節 rule 7 は「案内
  card を queue に出す」だが、 guidance の提示面は現時点でどこにも存在しない (PR-3 の
  `WatchdogGuidance` 自体が caller ゼロ)。 1 種類のためだけに提示面を発明せず、 **検知を
  同じ純関数の形で実装し sweep から log 出力**するに留め、 queue 提示は rule 7 の分と併せて
  後続で入れる。
- **決定 17**: 上記の routing (`resolveReopenVariant`)。
- daemon が `detail` から読むのは `attrs.observed.source_closed` **1 キーのみ**。 これが
  逆輸入 3 の「共通語」の唯一のメンバーで、 チャネル固有表現 (Jira statusCategory・PR の
  merged/declined) は workspace 側に留まる。

### PR-5 レビューで出た指摘 (Opus、 2026-08-13)

PR-4 の codex 3 巡と同じく、 **実装時に見落としやすいクラス**として記録する。 High 2 件は
いずれも PR-4 round 1 と**同じクラス** (「daemon 側の権限/スコープ検査漏れ」) で、 3 回連続で
同じ場所を踏んでいる:

- **High × 2 — brokered list の scoping を executor 側にだけ書いた**。 `boid task list` の
  scoping は **broker 側**にあり (project ref の name→UUID 解決もそこ)、 executor 側の
  `AllowsProject` はあくまで二重化の内側だった。 これを取り違えた結果、
  (a) `--workspace-id` が無検査で通り、 `ListTasks` の WorkspaceID filter は
  `project_workspaces` を INNER JOIN する = **他 workspace の triage card を丸ごと読める**
  (決定 2 の compartment を正面から破る)、 (b) project を**名前**で渡すと UUID 空間の
  `AllowsProject` と突き合わされて常に失敗する、 の 2 つが同時に発生していた。
  **教訓: 「既存 op と同じ scoping」と書くときは、 その op の scoping が実際にどの層に
  あるかを読んでから書く**。 修正後は broker に task_list と同一の 3 段 (ref 解決 +
  AllowsProject + workspace 一致) を置き、 escape テストで pin した。
- **Medium — sidecar seed が transient error を「行が無い」と誤認**。 `GetTaskTriage` の
  エラーを `sql.ErrNoRows` で絞らずに upsert していたため、 SQLITE_BUSY 等の一時失敗時に
  既存 card の urgency/kind/wake_at と **detail 全体 (children・observed) を空で上書き**し、
  決定 15 に到達不能な状態にしうる。 `applyParkSideEffect` が同じ罠を明示的に潰していたのに
  新規コードで再発した。
- **Medium — routing が reopen の payload 処理より前で `req.Type` を書き換えていた**。
  `boid task reopen <card> -m "..."` のメッセージが instruction に入らず、 かつ
  `sideEffectConsumesPayload` にも入らないため raw payload が `task.Payload` に merge される
  (PR-4 が「a real regression」と呼んだ汚染そのもの)。
- **Medium — 昇格した urgency/kind の blob 側の古い写しが残る**。 「列に書いて blob には
  書かない」不変条件は PR-5a 以降の書き込みにしか効かず、 既存 card は blob の古い値を
  返し続ける。 migration 0040 に「列へ昇格 → blob から削除」を追加し、 実行時にも
  `StripDetailAttrs` で自己修復するようにした。
- Low × 2 — machine.go の doc コメントの遷移表が `Manual` を実装と逆に書いていた /
  `child_closed` フックが通常 task の親子でも毎回余分な Tx を開いていた。

2 巡目 (High 0、 Medium 2 + Low 5):

- **Medium — `resolveReopenVariant` が「行の読み取り失敗」を「triage task ではない」と
  誤認**。 1 巡目で seed 側の同じ罠を直した直後に、 routing 側で**同じ誤りを再生産**していた
  (しかも 50 行離れた場所に「ErrNoRows 以外を no-row 扱いしてはいけない」と自分で書いた
  コメントがある状態で)。 しかもこちらは向きが悪く、 判定不能時に通常経路へ落ちる =
  **card を job として実行してしまう** = 決定 17 が防ぎたかったもの。 判定不能なら 503 で
  落とす形に修正。 **教訓: 「error != nil」を単一の分岐で扱う箇所は、 fail-open の向きが
  どちらかを毎回明示的に問う**。
- **Medium — 決定 16 の breach を毎分 log していた**。 khi が `observed.source_closed` を
  送り始めるまで**全 card が breach** なので、 出荷直後から 1 日 1,440 行が他の daemon log を
  埋め尽くす。 breach 集合が変化したときだけ出す形に修正 (解消時も 1 行出す)。
- Low × 5 — sweep が `working` を毎分 2 回舐めていた (done 判定と breach 判定を 1 パスに統合、
  `SweepTriage` に集約) / `ListTriage` が読めない行を無言で落としていた /
  executor の無フィルタ経路が `ctx.ProjectID` 空のとき daemon 全域を返しうる /
  `triage_done` が `finalizeTerminal` を迂回していた (card の入れ子が表現可能になった時点で
  child_closed の取りこぼしになる) / seed が get-then-upsert で TOCTOU を持っていた
  (`SeedTaskTriage` = `INSERT ... ON CONFLICT DO NOTHING` に置換)。

3 巡目 (High 0、 Medium 1 + Low 3。 ここで収束と判断):

- **Medium (一部 false positive) — dispatch 直後に終端した子の `child_closed` 取りこぼし**。
  指摘されたシナリオ自体は PR-4 の post-commit reconciliation で既にカバー済みだった。 ただし
  **決定 15 で blob が load-bearing になった**という指摘は正しい (`ShouldAutoDone` は blob しか
  見ないので、 blob が現実とズレた card は永久に working)。 sweep 側でも毎 tick
  `dispatched` な子の実 status を突き合わせて自己修復するようにした。
- Low × 3 — `resolveReopenVariant` が `TaskTriage == nil` のときだけ危険な向き (通常経路) に
  倒れていた (エラー時は安全側に倒すよう直した直後の、 同じ関数内の非対称) /
  `ListTriage` に既定 status が無く無指定で全 task 行を走査していた (`"triage"` =
  pre-execution ∪ working を新設して床にした) / **rule 4 の notify が khi の自然な順序で
  発火しない** — `captured → triage → attrs_set{urgency:now}` の順だと urgency 到着時点で
  card は既に queue member なので entry 検出が空振りする。 `notifyUrgencyRaised` を追加。

**運用メモ**: 1 巡目 High 2 / 2 巡目 Medium 2 / 3 巡目 Medium 1 (一部 FP) と単調に収束したので
3 巡で打ち切った ([[codex-review-loop-runaway-cutoff-policy]] の規律を Opus レビューにも適用)。
3 巡すべてで**同じ関数の同じ判断 (`GetTaskTriage` のエラーをどう扱うか) が別の場所で再発**して
いる点は記録に値する — 「error != nil を単一分岐で潰す箇所は fail-open の向きを毎回明示的に
問う」。

### PR-5 (boid 本体): 読み戻し口 + done 自動落ち

**読み戻し口の要件** — 「機能の完全性として読み戻しが完全でないのはおかしい。 まして action
から導出するフィールドが取れないのは余計おかしい」(nose、 2026-08-13)。 返すのは
task + sidecar + **action 導出分**を 1 つにまとめた形:

- `tasks.status` (state)
- `task_triage` の実列 (kind / urgency / wake_at / wake_task_id)
- `detail` (attrs / children / summary / source / content_ref / suggestion / observed)
- **`parked_from`** — `ParkedFrom()` (`task_triage.go`) が actions を舐めて導出する値。 現状は
  `Wake()` の内部でしか使われておらず、 外に出る口が無い。 決定 13 の「state は導出」を read
  API 側でも守るなら、 導出フィールドは読み口の一部でなければならない

粒度は**単発と一覧の両方**。 khi の sweep は全 note を舐めるため、 一覧が無いと N 回叩くことに
なる。 一覧は project スコープ (メタプロジェクト内の triage task 全件) を基本とし、
`queue_next` との関係は既存の `task_list` に揃える。 workspace scoping は
`BoidOpActionSend` / `BoidOpTaskGet` と同じ `ctx.AllowsProject` パターンを踏襲する
(triage task の見出しは workspace 境界を越えてはならない — 決定 2)。

**done 自動落ち** は決定 15 の rule。 評価契機は QueueSweepLoop と `child_closed` 記録直後の
2 点。 `observed.source_closed` が未設定の card は落ちない — これは決定 16 (canonical source
制約) により「起票が契約違反である」ことのサインであり、 watchdog が案内として拾う。 状態の
3 値化や猶予期間で救う設計は**採らない** (第 12 版で明示的に却下、 決定 16 の経緯を参照)。

### khi 側の改修

1. **note への改名** (用語注): `issues/` → `notes/`、 `CARD_ISSUES_DIR` 相当の env、 スキル名
   (`card-inbox` / `card-suggest` / `card-promote*`)、 docs (`card-format.md` /
   `card-events.md` / `card-dispatch.md`)。
2. **起票を daemon 経由にする**: 順序は「signal 到着 → 起票判断 → **canonical source を作る**
   (決定 16) → `boid task create --ref <canonical source のキー> --initial-status triaged`」。
   返る task_id を claims 側に記録する。 `ref` の get-or-create (論点 7、 project スコープ込み)
   があるため、 task_id を失っても冪等に再送でき、 done に落ちた task も同じ ref で引ける
   (決定 17)。 mail の message-id / slack の thread_ts は claims 側の索引に残す。
3. **判断系の push**: `attrs_set` / `child_added` / `child_specced` を `boid action send` で。
   `evaluate.py` の出力先が decisions から daemon の actions に変わる。
4. **fold の退役**: `card_model.fold()` / `decisions/*.jsonl` / `bootstrap_events.py` /
   `verify_roundtrip.py`。 `project_card.py` は「daemon から読んで note を render」へ。
5. **observe の縮退**: `boid_task_observed` を削除 (子の生死は daemon の `child_closed`)。
   source 側の観測 (Jira statusCategory 等) は残り、 決定 15 のために
   `observed.source_closed` を共通語化して `attrs_set` で届ける役割が加わる。
6. **dispatch の退役**: `card-promote-headless --live` を削除し、 Go = daemon の `ready`
   action に一本化 (論点 5)。 sweep の `boid task ask` による承認 UX はそのまま使える。

### 移行手順 (順序に制約あり)

論点 9 の「specced push と手動 promote の併用禁止」が効くため、 順序を固定する:

1. note への改名 (単独で完結、 daemon 非依存)
2. PR-5 (boid 本体) を先に出す — 読み口が無いと 4 以降が書けない
3. 起票を daemon 経由にする + `attrs_set` の push を開通。 **この段階では
   `card-promote-headless` をそのまま使う** (children はまだ khi 側が持つ)
4. `card-promote-headless` を退役し、 同時に `child_added` / `child_specced` の push と
   fold の退役を行う — **3 と 4 の間に「specced を push しつつ手動 promote も残す」状態を
   作らない**
5. 使い捨ての移行スクリプトで既存 note (25 枚超) を daemon task に流し込む。
   `bootstrap_events.py` の逆版 — decisions を読んで `task create --ref` + `attrs_set` +
   `child_added` / `child_specced` を再生する。 一度きりなので repo に残さない

### 検証シナリオへの追加

「検証シナリオ」節の S1〜S4 に加え、 PR-5 では以下を pin する:

- **S5: 読み戻しの完全性** — park した triage task を読み口から取得したとき、
  `parked_from` が actions から導出されて返ること。 `attrs_set` で書いた任意のキーが
  `detail.attrs` として往復すること。 他 workspace の triage task は取得できないこと
  (`AllowsProject` gate)
- **S6: done の自動落ち** — 全子 closed かつ `observed.source_closed` で working→done。
  片方だけでは落ちないこと。 落ちた事実が action として記録されること
- **S7: フォローアップによる再浮上** (決定 17) — done の triage task を `ref` で引けること
  (`FindTaskByRef` に status 絞りが無いことの pin)。 `reopen_triaged` で done→triaged に戻り、
  戻った後は `attrs_set` / `child_added` が再び押せること。 加えて **triage task への `reopen` が
  `reopen_triaged` に routing される**こと・ 通常 task の `reopen` は executing のままであること・
  `reopen_triaged` の直接 push が reject されること (決定 17 の「`reopen` 誤爆」対策の pin)

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
- **khi 統合** (第 11 版で追加、 PR-5): 決定 14 (state の正は daemon) / 決定 15 (done 自動
  落ち) の実装と、 khi 側の fold 退役・ 起票と判断の push 経路・ `card-promote-headless`
  退役・ note への改名・ 既存 note の移行。 詳細と順序制約は「PR-5 設計メモ」節。
  **これが完了した時点で Phase 1 の当初目的 (Phase 0 の digest 運用の置換) が実際に達成される**
  — PR-1〜4 は daemon 側の受け口を用意しただけで、 khi はまだ 1 本も繋がっていない。

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

- **workspace 横断の full content 集約** — 2026-08-19 に範囲を縮小 (決定 3 改)。 恒久に
  行わないのは **credential と、 外部システムへの到達性の集約**である。 card の本文が daemon に
  載るかどうかは開示ポリシー (論点 e) の管轄になった。 横断して**一覧・ 検索**する対象は
  引き続き card メタデータのレベルまで (決定 8)。
- **チーム共有** — 本仕組みは personal。 workspace export にメタプロジェクトと inbox を
  含めない (論点 f)。
- **バックアップ機構そのもの** — メタプロジェクトの有無に関わらず必要な別トラック
  (workspace $HOME volume / daemon volume の backup)。 本 doc は依存として参照するのみ。
- ~~**daemon 内 scheduler**~~ — **2026-08-19 に非目的から外した** (論点 k)。 トリガ
  (`project.yaml` トップレベルの `triggers`) を daemon が持ち、 workspace 側に cron は残らない
  方針になった。 実装は [ingestion-identity.md](ingestion-identity.md) の B-5。

---

## 未解決論点

- **論点 a: メタプロジェクトの upstream 置き場** — **workspace の credential 境界内で確保**
  (ストレージ節 原則 3)。 khi は customer bitbucket 上の `khi-task-collector` で確定済み。
  boid-hosted bare repo (credential ゼロの恒久解) は優先度を上げ、 daemon volume backup 整備後
  に移行する。
- **論点 b (2026-08-19 決着): 定期起動の機構**。 **内蔵側に倒れた** (論点 k)。 daemon が
  トップレベル `triggers` を読んでスケジュール・ single-flight・ 実行結果の記録を持ち、
  workspace 側に cron は残らない。 外部 API を叩く窓 (cursor と頻度) は論点 h の通り
  workspace 側。 実装は [ingestion-identity.md](ingestion-identity.md) の B-5。
- **論点 c: capture UX**。 頭の中からの課題感を最速で入れる経路 (音声入力が第一級)。
  workspace 不定 routing (決定 5 の例外) の確認 UI。 Web UI quick capture か、 既存チャット
  経由か。
- **論点 d: queue の順位付けと学習**。 urgency の語彙、 並べ方、 再浮上条件の評価。 nose の
  応答傾向 (破棄・ 後回し) をノイズ判定 (決定 10) と順位付けに反映するか。 Phase 0 の観察から
  決める。
- **論点 e: 開示ポリシーの語彙**。 workspace ごとに「件名まで出す / 相手ドメインのみ /
  本文まで」等。 デフォルトは保守的に。 **2026-08-19 に決定 3 の上限がここへ一本化された**
  ため重みが増した。 併せて **enforcement の限界を正直に持つこと**: daemon は opaque な
  テキストから「要約か原文か」を機械的に判別できないので、 効かせられるのはサイズ /
  フィールド粒度まで。 意味的な粒度は workspace 側の自制に依る。
- **論点 f (2026-08-11 決着): export 除外**。 `project.yaml` に明示フラグ (例:
  `meta_project: true`) を持たせ、 workspace export/apply がこれを見てスキップする。 運用ルール
  頼みにせず仕組みで強制する。
- **論点 g (2026-08-11 決着、 2026-08-19 前方参照)**: card の洪水対策。 Phase 1 は最小限に留める:
  dedup は同一 source ref (mail message-id / jira key / slack ts) の再 push を update 扱いに
  するのみ。 **この dedup は論点 k で多対一の identity 索引 + binding のライフサイクルへ
  置き換わる**。 洪水そのものは workspace 側の篩いで止まる、 という整理も同じく論点 k。
  TTL・ queue 側の既読 / 未読表示は Phase 1 では入れない。 queue の安定を最優先し、 必要になった
  ら足す。
- **論点 h (2026-08-11 決着): source 既読化の詳細**。 既読化は **daemon 側の責務にせず、
  メタプロジェクト側 (workspace 内の ingestion task) の責務**とする — 決定 11 で既に
  workspace credential 内で行うとしていた線をそのまま踏襲。 深さは read 化のみに留め、
  label 併用・ archive までは Phase 1 では踏み込まない。 誤ノイズ判定時の復旧は UC-6 の
  triage log からの手動再起票で対応する。 Slack / Jira の cursor 保存場所は決定11通り
  $HOME workspace volume。
- **論点 i (2026-08-11 明示的に後回し): project ref 解決の存在オラクル**。 ambiguous project ref
  のエラーが daemon 全域の一致件数を返すため、 sandbox から cross-workspace の project 存在が
  推測できる (実測 b)。 リークは軽微なので Phase 1 着手前には塞がない。 triage task の流量が
  実際に増えて問題が顕在化した時点で優先度を上げる。
- **論点 j (2026-08-13 決着): done 自動落ちの前提と再浮上**。 決定 16 (canonical source 制約) と
  決定 17 (`reopen_triaged`) で決着。 経緯: 決定 15 の `source_closed` を報告できない source
  (mail / slack / head) の task が永久に working に滞留する穴を、 状態の 3 値化でも猶予期間でも
  なく「起票するなら報告できる source を必ず作る」という契約で塞いだ。
- **論点 k (2026-08-18 提起): 判断スケジューラと取り込み identity**。 決定 14 が退役させたのは
  workspace 側の `decisions` だけで `claims` + fold は残ったため、 **fold は今も 2 つある**
  (決定 14 の決め手「2 つの fold を並べるとズレたときに誰も気づかない」がまだ満たされていない)。
  原因は workspace の都合ではなく daemon 側の口の不足 2 点 — (1) 宛先未確定のイベントを
  受け取れない (action は `task_id` 必須) (2) actions の履歴を読む口が無い (`action_send` は
  あるが `action_list` が無い)。
  検討の過程で、 **「daemon は決定論」「取り込みは workspace」を同時に立てると「判断のトリガは
  daemon が出す」**という線が出てきた。 daemon を**トリガの出所**、 workspace を**判断の実装**と
  する To-Be に整理し、 `project.yaml` の**トップレベルに `triggers`** を新設する
  (`task_behaviors` は変更しない — trigger は「どう実行するか」ではなく「いつ始まるか」の
  性質なので器が違う)。 daemon が持つのは **スケジュール / single-flight / 実行結果の記録**の
  3 つだけで、 走らせるのは workspace のコマンド (`run: python3 scripts/x.py`)。
  「LLM に用があるか」の判定は workspace のスクリプトが持つ (決定論なので決定 12 と衝突しない)。
  判断の記録は payload 不透明の汎用注記 action。 外部キーは task の **identity** として多対一で持たせ、
  未着キーは専用 inbox ではなく **`captured` な triage task** として着地させる (篩いが
  workspace 側に残るので daemon 側の流量は今と変わらない)。 詳細は
  [ingestion-identity.md](ingestion-identity.md)。 決定 16 の `ref` を一般化し、 論点 g の
  dedup を置き換え、 論点 b の判断起動側を内蔵へ倒し、 決定 10 の到着状態を一部 `captured` へ
  倒す。 **2026-08-19、 同 doc の未確定 26 件を「着手前 / PR 内 / 後で」に仕分け、 着手前の
  5 件のうち 4 件を決定した** (`tasks.ref` は子 dedup 専用として残す / v1 は排他 identity のみ /
  push は差分 reconcile を続ける / `captured` → `triaged` は LLM が提案し人が押す)。 実装はまだ。
  本編に畳むかは同 doc の未確定。

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
  credential と外部到達性の横断集約 (非目的)。

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

### 第 13 版 (2026-08-18、 判断スケジューラの切り出し) での変更

Phase 1 完了後の khi 実機と daemon 側 main を再突合し、 **決定 14 が fold を 1 つに
減らしきれていない**ことを確認した (退役したのは `decisions` だけで `claims` +
`fold_claims()` は残っており、 `daemon_sync.py` は fold エンジン 2 つの間の reconcile を
している)。 原因を daemon 側の口の不足 2 点に特定し、 そこから **論点 k** を提起、 詳細は
[ingestion-identity.md](ingestion-identity.md) に分離した。

- 本編の変更は当初、 論点 k の追加のみだった (実装前のため)。 その後 **決定 3 を改訂**し、
  それに連動して **決定 14 の帰結の一部** (「note は残す」と `project_card.py` の役割)、
  **決定 6** (課題本文はファイル)、 **非目的の daemon 内 scheduler**、 **論点 b** も
  変わっている。 「決定 3 だけを変えた」ではないので、 連動分もそれぞれの節に注記した
- 同 doc は「daemon は決定論 / LLM 判断は workspace / **トリガは daemon**」の 3 原則から
  出発する。 判断の起動は `project.yaml` トップレベルの `triggers` が宣言し、 daemon は
  「いつ、 どのコマンドを走らせるか」しか知らない (走らせた先で何が起きるかは関知しない) ので、
  **判断の場を増やしても daemon は無変更**という拡張性を持つ。 論点 b (定期起動の機構) は
  内蔵側へ倒れ、 workspace 側に cron は残らない
- 決定 16 (`ref` = canonical source のキー) を「登録に使った 1 本目の identity」へ一般化し、
  論点 g の dedup 案を多対一の identity 索引 + binding のライフサイクルへ置き換える
- 決定 17 (`reopen_triaged`) に**引き金**を与える: 第 12 版は「done を探す経路は既に
  成立している、 足りないのは押せる action だけ」としたが、 誰が何を見て押すかは空いていた
- 決定 10 (mail/Jira/Slack は triaged で到着) の一部を `captured` へ倒す。 篩いは workspace の
  LLM に残るが、 起票の確定 (どの card になるか) は daemon 側の索引と判断に回る
- 併せて既存の穴を 1 件記録した: 決定 14 で判断を Web UI へ移したのに Web UI 側に回答を
  記録する経路が無く、 **Web UI で却下しても再提案の抑止が効かない**
- **決定 3 を改訂** (2026-08-19、 本編で唯一の決定変更): 「原文そのものは越えない」という
  ハードな上限を外し、 上限を論点 e の開示ポリシーに一本化した。 境界として実際に効いているのは
  外部接続の credential 分離であって本文の所在ではない、 という判断 (nose)。 これにより
  workspace 側は「原文の保持」という責務を手放し、 note への射影とその同期、 続報の追記先解決、
  slug ↔ identity の対応がまとめて不要になる。 受け入れる変化は Web UI の露出面 (詳細は決定 3 の
  節)。 非目的の「full content の横断集約」も、 credential と外部到達性の集約に範囲を縮小した

### 第 12 版 (2026-08-13、 done 自動落ちの前提を確定) での変更

第 11 版が積み残した「`observed.source_closed` を持たない triage task が永久に done に落ちない」
を決着させた (論点 j)。

- **決定 16 を新設**: triaged 以降は canonical source (`source_closed` を報告できる source) を
  必ず持つ。 `source_closed` を報告できるのは jira / bitbucket PR だけで、 mail / slack /
  head-capture / agent には終了状態の概念が無い、 という事実がこの穴の正体。 状態の 3 値化や
  猶予期間で救うのではなく、 **「起票するなら報告できる source を必ず作る」を契約にする**
  (nose 案) ことで、 決定 15 の rule を一切変えずに全 triage task で成立させる。 daemon 側の
  判定は「`observed.source_closed` キーが存在するか」だけで済み、 チャネル知識はゼロのまま
  (逆輸入 3 の境界を守る)。 enforcement は起票時 reject ではなく watchdog (queue 節 8 を新設)。
  `ref` は canonical source のキーにする。
- **決定 17 を新設**: `reopen_triaged` (done → triaged) を別 verb で追加。 実測で
  「done を**探す**経路は既に成立している」ことを確認した (`FindTaskByRef` に status 絞りが
  無く、 done / dropped も ref で引ける) — 足りないのは見つけた後に押せる action だけだった。
  併せて **`reopen` 誤爆の危険**を記録: done の triage task と done の通常 task は status だけ
  では区別できず状態機械では防げないため、 ガードは sidecar が見える `ApplyAction` に置く。
- **PR-5 のスコープを 2 項目追加** (`reopen_triaged` + `reopen` ガード、 canonical source
  watchdog)。 検証シナリオに **S7 (フォローアップによる再浮上)** を追加。

### 第 11 版 (2026-08-13、 khi 統合の方向決定) での変更

PR-4 完了後、 khi-task-collector 実機 (`~/src/bitbucket.org/Aolani-ondemand/khi-task-collector`)
と daemon 側 main を再突合し、 「khi を daemon へのタスク集約フローに統合する」方向を確定した。

- **決定 14 を新設**: state の正は daemon 側ただ 1 つ。 案 A (khi が正・daemon は射影先、
  PR-4 の暗黙の想定) と案 B (daemon が正・khi は claims と本文だけ) を比較し、 **案 B を採用**
  (nose 判断)。 決め手は「二重 fold は両者のロジックがズレたときに誰も気づかない」こと、 および
  案 A では khi 側 fold の読み手が実質いなくなること。 khi の decisions / `card_model.fold()` /
  `observe.py` の `boid_task_observed` / wake 日付評価は退役し、 claims と note と翻訳規則だけが
  残る。 代償 (daemon 障害中は state を確定できない) は受容。
- **決定 15 を新設**: done の自動落ち (全子 closed ∧ `observed.source_closed`) を daemon の
  決定論 rule で行う。 決定 14 で khi から done 判定が消えるため必須。 「自動の範囲を広げて
  いくこと自体が目標」(nose) — 終わった判定が人間に戻ると queue への信頼がその分落ちる。
- **論点 5 を決着**: PR-2 側 (triage task の子として作る) に寄せ、 `card-promote-headless` を
  退役する。 決定 14 の下では dispatch 経路も 1 本でないと children を二重に書くことになる。
- **用語: workspace 側の本文を note に確定**。 daemon 側は「triage 段階の task」で確定済みの
  ため、 workspace 側には固有名を立てず役割語を置く (`notes/<slug>.md` =
  `task_triage.detail.content_ref` の指す先)。 対抗候補 dossier / docket は不採用。
- **ストレージ節を実態に合わせて修正**。 note は git repo ではなく **$HOME workspace volume**
  に置かれており、 これは意図的な選択 (毎回の commit & push が煩雑 / メタプロジェクト repo は
  顧客 bitbucket 上でチーム共有されるため、 ワークフローは共有するが個人宛 signal の本文は
  共有しない)。 原則 1 を「backup されていない場所に置かない」に緩和し、 耐久性の根拠を restic
  volume backup に置いた。 原則 2 の「frontmatter を耐久写しにして再 push で再構築」は決定 14 と
  両立しないため撤回し、 daemon volume backup + render 済み note の 2 段に置き換えた。
- **「PR-5 設計メモ」節を新設**。 実測で daemon 側の不足 2 点を特定: (1) `task_triage` の読み口が
  HTTP / brokered ともにゼロ (`BoidOpTaskGet` は Task DTO のフィールドしか返さない)、
  (2) `working → done` の遷移が存在しない。 いずれも案 A なら khi 側が代替していたため
  顕在化していなかった。 併せて khi 側改修 6 項目・ 移行手順 (specced push と手動 promote の
  併用禁止に由来する順序制約)・ 検証シナリオ S5/S6 を記載。

### 第 10 版 (2026-08-11、 PR-4 実装完了) での変更

PR-4 (ingestion push 開通 + action 語彙拡張 + working 出口 3 本 + child_closed 自己記録 +
brokered wake op) を単一 PR として実装・merge した (PR #933、 boid リポジトリ)。 論点 6〜10 の
実装状況は以下の通り (今後のセッションが diff から再導出しなくて済むよう記録):

- **論点 6 (実装注意 4 点)**: 全て反映。 `AvailableActions` は `ToStatus == ""` の非遷移
  ルールを skip するよう修正 (6-1)。 `attrs_set`/`child_added`/`child_specced` の payload は
  `task.Payload` へ merge せず side-effect にのみ流す「consumed」扱いにし、 ついでに確認した
  `park` の同種バグ (wake_at/wake_task_id payload が task.Payload にも漏れていた) も同時に
  修正 (6-2)。 `FromStatus` は `captured`/`triaged`/`parked`/`ready`/`working` の明示列挙、
  `"*"` は使っていない (6-3)。 side-effect は `applyParkSideEffect` に倣い、 payload 検証は
  Tx 前・ `task_triage.detail` の read-modify-write は Tx 内 `GetTaskTriage` から行う (6-4)。
- **論点 7 (dedup)**: `FindTaskByRef` の `ParentID != ""` 限定を撤廃し root task にも開放。
  ただし実装レビュー (codex round 1) で **「daemon-global で workspace 非スコープ」という
  Blocker が発覚** — 全 workspace の root task が `parent_id=""` を共有するため、 別
  workspace が同じ source_ref を使うと衝突し、 後発の create が先発 workspace の task を
  誤って受け取ってしまう欠陥があった。 修正として `FindTaskByRef`/`CreateTask` の get-or-create
  を **project_id もスコープに含める**よう変更し、 migration 0037 で unique index を
  `idx_tasks_ref_parent(ref, parent_id)` → `idx_tasks_ref_parent_project(ref, parent_id,
  project_id)` に置換した (旧 index は同 migration 内で DROP)。 併せて UUID 形の
  source_ref (id 直接一致にフォールバックする既存の後方互換分岐) が resend で非冪等になる
  Major も codex round 2 で発覚・修正 (id 一致が scope 外/not-found の場合は ref カラムの
  scoped query にフォールバックするよう変更)。
- **論点 8 (working からの出口 3 本、最重要)**: 実装済み。 `ready`/`triage`/`park` の 3 verb を
  `working` からの Manual 遷移として追加 (既存 pre-execution 語彙を再利用、 新規 verb は
  追加していない)。 `ready` action の Dispatch 自動連鎖は fromStatus に関わらず
  `newTask.Status == ready` だけを見ているため working→ready でも自動的に効く。
- **論点 9 (child_dispatched/child_closed の書き手分離)**: `child_dispatched` は
  `Dispatch()` が (元々の想定通り) 自己記録していたが、 実装時の調査で **実際には action row を
  書いていなかった** ことが判明したため、 `Dispatch()` の既存 Tx 内で `child_dispatched`
  action を追加するようにした。 `child_closed` は新規実装: `TaskWorkflowService.
  finalizeTerminal` (全終端遷移経路の単一合流点) にフックし、 子タスクが done/aborted に
  なった際、 親の `task_triage.detail.children` を照合して該当 child を closed にマークし、
  親側に `child_closed` action を追記する。 両 action とも machine.go に `Manual:false` の
  ダミー rule のみ登録し (`sm.Apply` からは呼ばない、 直接 `tx.CreateAction`)、
  `IsManualAction` が false を返すため `ApplyAction`/`BoidOpActionSend` 経由での khi push は
  自動的に reject される。 実装レビューで **child の生成/auto-start が Dispatch の Tx commit
  より先に走るため、 極端に速く終端した子の child_closed が永久に取りこぼされ得る** という
  Major が発覚し、 `Dispatch()` の Tx commit 直後に `newlyDispatched` を再照合し、 既に
  終端していれば `recordChildClosedOnParent` を直接呼ぶ補完ステップを追加して解消した。
- **論点 10 (brokered wake op)**: `BoidOpTaskWake` (`boid task wake <task-id>`) を新設。
  `api.WorkflowService.Wake` の薄いラッパーで、 workspace scoping は `BoidOpActionSend` と
  同じ `ctx.AllowsProject` パターン。 `wake_triaged`/`wake_ready` の直接 action send は
  `IsManualAction` ガードにより引き続き reject される (bypass ではない)。

**スコープ外 (計画通り、着手せず)**: 論点 5 (khi 側 `card-promote-headless` 移行、
khi-task-collector リポジトリの作業)、 論点 11 (代行 Go タスク — actor 記録が前提条件で
PR-4 必須ではないと明記済み)、 論点 12 (大型機能マッピング、 Notion gateway 宿題)、
`meta_project` フラグ (2026-08-11 nose 決定で別件扱い)。

**PR 分割判断**: 単一 PR (PR #933) として完走。 スコープが大きかったが、 codex レビュー
3 巡 (round 1: Blocker 2 件・ Major 3 件・ Minor 1 件、 round 2: Major 2 件・ Minor 1 件、
round 3: 0 件で「Ready to merge」) を経て収束したため分割は不要と判断した。 round 1 の
Blocker はいずれも「daemon 側の権限/スコープ検査漏れ」系 (child_specced の project field が
workspace 認可を経ずに Dispatch へ流れる経路、 root task dedup の workspace 非スコープ) —
実装時に見落としやすいクラスとして次回以降の類似実装時に注意する。

### 第 9 版 (2026-08-11、 Fable レビュー + 机上シナリオ検証) での変更

- **「Fable レビュー結果」節を新設** (条件付き GO)。 論点 1〜3 はコード実物で裏取り済み
  (shim の YAML 丸ごと forward により remote_id / initial_status の配管は既存)。 追加論点:
  論点 4 の実装注意 4 点 (論点 6)、 dedup の Ref=source_ref 案 (論点 7、 「khi の task_id
  保持で dedup 消える」を修正)、 **working からの出口 3 本** (論点 8、 PR-4 と同時修正 —
  レビュー対応と PR 系列消化の両方を塞ぐ最重要)、 child_dispatched / child_closed の
  書き手分離と論点 5 の移行順序 (論点 9)、 事象 wake 3 分類と brokered wake op (論点 10)、
  代行 Go タスク + Action の actor 記録 (論点 11、 nose 案)、 大型機能のマッピングと
  Notion gateway 宿題 (論点 12)。
- **「検証シナリオ」節を新設**。 机上検証 4 本 (S1: PR コメント合流 / S2: 再 Go ループ /
  S3: wake 3 分類 / S4: PR 系列逐次消化) を実装テストの仕様 + レビュー時チェックリストと
  して固定。 実装 PR は決定論 step に pin するテスト名を注記し、 テスト無し step は理由を
  明示する規律を明記。

### 第 8 版 (2026-08-11、 PR-4 着手前レビュー — khi 実運用モデルとの突合) での変更

- **「PR-4 設計メモ」節を新設**。 khi-task-collector 実機 (`docs/card-format.md` /
  `docs/card-events.md` / `docs/card-dispatch.md`) と daemon 側 PR-1/2/3 実装を突合し、
  5 つの論点を整理: (1) `TaskTriageChildSpec.RemoteID` 不足は既存の親→子継承で severity 低に
  格下げ、 (2) ブランチポリシー (worktree 分離目的) は既に撤去済みだが `BaseBranch` 自体は
  git 側の関心として存続、 `RemoteID` と `source_ref` は field 統合しない、 (3)
  `related_jira_issues` 的な複数 source 束ねは daemon に持ち込まない方針を再確認、 (4)
  `UpdateTaskRequest` PATCH 案を撤回し、 既存の `BoidOpActionSend` → `ApplyAction` →
  `actions` テーブル経路 (action append) に統一する方針に転換、 (5) dispatch 時の
  parent-child モデル (khi の root task 化 vs PR-2 の子 task 化) は**未決着**として明記。
- **次アクション**: 別セッションで Fable にこの節をレビューしてもらってから実装着手。

### 第 7 版 (2026-08-11、 Phase 1 実装可否の確認 + 未解決論点の決着) での変更

- **Phase 1 実装可否をコードベース実測で再確認**。 第 6 版の「Phase 1 実測結果」節の主張
  (TaskStatus は素の string 型で panic 経路ゼロ・ `store.go` の `NOT IN ('done','aborted')`
  誤包含・ `machine.go` の `start` FromStatus 固定・ `task_service.go` の pending 限定編集
  ガード・ `project_workspaces` sidecar 前例) を該当ファイルの現状と突合し、 いずれもズレが
  無いことを確認した。 `task_triage` テーブルは未着手のまま (migration は `0034` まで)。
  ブロッカー無し、 着手可能と判定。
- **論点 f/g/h を決着、 論点 i は明示的に後回しと決定** (詳細は各論点の記述)。 要旨:
  export 除外は `project.yaml` の明示フラグ、 洪水対策は Phase 1 では dedup のみの最小限、
  既読化は daemon でなくメタプロジェクト側 (ingestion task) の責務で read 化のみ、
  project ref 解決オラクルは軽微リークとして Phase 1 着手前には塞がない。
- 論点 a〜e は Phase 1 実装を直接ブロックしないため今回は据え置き (a は既に決着済み、
  b/c/d/e は Phase 0 観察後 or Phase 2 以降の判断で足りる)。

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
  (1 card = 複数 task)・working / Go 再定義・suggestion / observed を取り込んだ
  (「Phase 0 実運用からの逆輸入」節)。 状態機械とスキーマ案も追従。 claims / decisions 二層が
  決定 12 / 13 の設計図になっていることを確認し、 Phase 1 は同機構の移植として設計する方針に
  した。 なお related_jira_issues 等のチャネル固有の探索キーは、 一度取り込みかけて **nose の
  指摘で撤回** — claims (方言) / decisions (共通語) の二段抽象を守り、 daemon はチャネルの
  知識を持たない (逆輸入 3 の境界原則)。
