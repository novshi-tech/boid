# Signal envelope の机上検証 — field 棚卸しと schema v0

2026-08-26 実施。`docs/plans/signal-driven-review.md` §10.2 の机上検証の結果。
envelope schema v0 をここで確定し、本体 doc の §5.2 を更新した。

## 1. 方法

コードを書かず、現行 khi の実装と本番データを読んだ。

- **生産側**: khi の `khi/domain/signal.py` (Signal 型)、`khi/adapters/{slack,jira,bitbucket}.py`
  (source ごとに何を埋めるか)
- **検知側**: `khi/domain/{record,screen}.py`・`khi/app/detect.py` (機構がどの field を使うか)
- **判断側**: `khi/app/trigger.py` の `instruction()` (sweep task に何を渡すか) と
  `.claude/skills/khi-sweep/SKILL.md` (判断が何を参照するか)
- **実例**: 本番の sweep task 82 本から代表 4 本の instruction を採取し、envelope へ手書き変換

## 2. 最重要の発見: 現行 khi の Signal は envelope 案よりさらに軽い

現行本番の Signal 型は 6 field しかない — `source`・`event_key`・`identity` (単数)・
`at`・`author` (任意)・`url` (任意)。**title も preview も body も hints も無い**。
設計原則が「中身は持たない。判断が読み直す」であり、Jira adapter は fields=`updated`
だけを要求し、Slack adapter はスレッド展開を一切しない。

つまり r1〜r3 の envelope 案にあった `title`/`preview`/`hints` は**現行実績の無い追加物**で、
逆に現行が持つ `author` (self-authored 篩の材料) は**envelope 案に欠けていた**。

## 3. 実例の手書き変換

### 3.1 Slack メンション新規候補 (本番 sweep `c4f98634`、2026-08-26)

現行 instruction の 1 行:
`新規候補 identity slack-thread:1787711028.007819 — signals: slack:C043YAL7G15:1787711028.007819 — 入口: https://khi.slack.com/archives/C043YAL7G15/p1787711028007819`

```yaml
id: slack:C043YAL7G15:1787711028.007819
occurred_at: 2026-08-26T02:23:48.007819Z
source: {pack: slack, connector: mentions, service: slack-api}
identity: slack-thread:1787711028.007819
url: https://khi.slack.com/archives/C043YAL7G15/p1787711028007819
```

### 3.2 Jira 課題更新の新規候補 (本番 sweep `f47c7667`、2026-08-26)

```yaml
id: jira:ROOKPF-309:issue:2026-08-26T14:21:09.500+0900
occurred_at: 2026-08-26T05:21:09.500Z
source: {pack: jira-cloud, connector: assigned-issues, service: jira-api}
identity: jira:ROOKPF-309
url: https://<site>.atlassian.net/browse/ROOKPF-309
```

### 3.3 Bitbucket コメントが Jira identity へ合流 (本番 sweep `698dec45`、2026-08-24)

現行実例: `task cba7c559 (identity jira:ROOKPF-299) — signals: bitbucket:rook-tools:3:comment:846434397`

```yaml
id: bitbucket:rook-tools:3:comment:846434397
occurred_at: 2026-08-24T21:30:00Z   # comment の created_on
source: {pack: bitbucket-cloud, connector: pr-comments, service: bitbucket-api}
identity: jira:ROOKPF-299           # ← bitbucket connector が PR title/branch の Jira key を正規表現で抽出
url: https://bitbucket.org/AoLani-ondemand/rook-tools/pull-requests/3
author: self | <display name> | null
```

**pack を越えた identity が本番の実例にある。** Bitbucket connector が `jira:<KEY>` の
identity を出すことで、PR のコメントと Jira 課題が同じ件に機械的に合流している
(同じ巡で `jira:ROOKPF-304` に issue 更新 + PR コメント 2 件が合流した実例も採取)。

### 3.4 mail 想定 (低 S/N、仮想例 — 現行に mail source は無い)

```yaml
id: mail:INBOX:<message-id@example.com>
occurred_at: 2026-08-26T03:00:00Z
source: {pack: gmail, connector: inbox, service: gmail-main}
identity: mail-thread:<gmail-thread-id>
url: https://mail.google.com/mail/u/0/#inbox/<hex-id>
author: notification@example-saas.com
title: "【お知らせ】メンテナンスのご案内"
```

篩は workspace の scan script が `author`/`title` のパターンで ack して落とす。
本文を運ばなくても mail の篩は差出人と件名で成立する (人間の inbox 処理と同じ)。

## 4. 棚卸し表

「使用実績」は現行 khi の本番実装での使われ方。

| envelope 案の field | 現行の対応物 | 生産側 | 検知側の使用 | 判断側の使用 | 判定 |
|---|---|---|---|---|---|
| `id` | `event_key` | 全 adapter が生成 | dedup・settled・attempts・dead letter | write の `signals` に verbatim で必須 | **必須** |
| `occurred_at` | `at` | 全 adapter | source cursor (栞) そのもの | 使わない | **必須** |
| `source` (単一文字列) | `source` | 全 adapter | 栞のキー | 使わない | **必須**。新設計では pack/connector/service の 3 分割 (栞は service instance × connector 単位、skill mount と binding の解決に要る) |
| `identities[]` (複数) | `identity` (**単数**) | 全 adapter | 候補の grouping・resolve | 新規候補の読み入口 | **必須・単数へ変更**。現行実績は常に 1 個。複数の実需が出てから配列化する |
| `resource.url` | `url` | slack/bitbucket は常時、jira は site 設定時 | Target へ運ぶだけ | 新規候補の原文入口 (無いと identity から探し直し) | **推奨 (任意)**。トップレベル `url` へ平坦化 |
| `resource.kind` / `resource.id` | 無い | — | — | 使わない (identity と url で足りている) | **削除**。resolver 導入時に再検討 |
| (無い) | `author` | bitbucket のみ (self 判定を 1 bit 化)、slack/jira は意図的に None | self-authored 篩 (フェイルクローズ) | 使わない | **追加 (任意)**。mail の篩にも必須級 |
| `title` | 無い | — | — | 使わない | **任意へ降格**。篩の材料としてのみ (mail の件名)。判断はこれに依存しない |
| `preview` | 無い | — | — | 使わない | **削除**。「中身は運ばない」は現行の実証済み原則 |
| `hints` | 無い | — | — | 使わない | **削除**。thread の解決は connector が identity へ畳む (slack の permalink → thread_ts が実例)。実需が出たら optional field を足せばよい |

## 5. 確定 schema v0 (`boid signal list --json` の 1 要素)

```yaml
id: slack:C043YAL7G15:1787711028.007819    # 必須。connector が同一 event から必ず同じ値を生成
occurred_at: 2026-08-26T02:23:48Z          # 必須。RFC3339 tz-aware。source cursor の材料
source:                                    # 必須。provenance
  pack: slack
  connector: mentions
  service: slack-api                       # service instance 名
identity: slack-thread:1787711028.007819   # 必須。機械的に導出できる帰属候補 (単数)
url: https://khi.slack.com/archives/...    # 任意 (強く推奨)。原文への入口
author: self | <生ID> | null               # 任意。篩の材料。self は「自分自身」の共有語彙
title: "..."                               # 任意。篩の材料。判断は依存しない
```

- dedup の単位は **(service instance, connector, id)** の複合。id 文字列自体に instance を
  焼き込むかは CLI 詳細設計で決める (現行 khi は instance 概念が無く素の event_key)
- 栞 (cursor) も同じ複合単位で持つ。**「栞は自分自身を越えて前進する」は connector 側の
  契約** — Jira の JQL が分精度しか持たないため、取得後に `at <= 栞` を落とすところまでが
  fetch の責務 (2026-08-23 に 13 時間の再検知ループとして実証済み)
- `identity` の語彙は **workspace 全体の共有語彙** (§3.3)。Pack contract で identity を
  pack-scope に閉じてはいけない — bitbucket connector が `jira:<KEY>` を出すのが本番実績
- `author` の `self` 正規化は connector の責務 (生 ID と自分の識別子の比較)。落とすか
  どうかは workspace の篩が決める — 現行 screen.py の分担をそのまま引き継ぐ

## 6. 副産物の発見: boid 内部シグナルの経路が未決

khi の sweep は外部 3 source に加えて **boid 自身の action 列** (子の終端、人の書き込み)
を第 4 の source として読んでいる。settled 判定・attempts・dead letter の記録も
action 列 1 本に載っており、外部シグナルの決着記録も boid 側にある。

r3 doc の全体像 (§4) は外部 → inbox の経路しか描いていない。boid 内部変化を
(a) daemon が inbox へ直接投入する、(b) 現行どおり workspace の scan script が
`boid action list` を直読みし続ける、のどちらにするかは未決として本体 doc §12 に積んだ。
初期実装は (b) を推す — 今日動いている形のままで、core の変更ゼロ。

## 7. H4 (知識の置き場所) への含意

sweep skill (325 行) を読んだ範囲では、サービス固有の調査手順は
「`slack-thread:<ts>` ならそのスレッド、`jira:<KEY>` ならその課題を gateway で読む」の
2 行程度しか無く、残りは boid の書き方 (verb 表・card の status 対応) と判断方針。
つまり **知識の分解 (H4) の主戦場は sweep skill ではなく、`boid-api-skills` 側の
reference skill を Pack へ移すこと**になる。shadow-b の設計はこの前提で組んでよい。
