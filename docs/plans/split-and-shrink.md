# boid の分割と縮退

ステータス: **構想 (2026-10-29、nose とのディスカッションの結論)**。実装計画ではない。
各項目に着手するときに、それぞれの plan doc に書き起こす。

関連:

- api-gateway-mcp (novshi-tech/api-gateway-mcp) の `docs/concept.md`・`docs/redesign.md`・`docs/background-access.md`
- Murmur (novshi-tech/murmur) の `docs/adr/0012` (boid からの系譜)・`0014` (個人のメモ)・`0020` (チーム単位)・`0022` (Murmur 間連携、構想)
- [cross-project-issue-triage.md](cross-project-issue-triage.md)・[signal-driven-review.md](signal-driven-review.md)・[card-next-step-and-timeline.md](card-next-step-and-timeline.md) — 退役させる側
- [api-gateway.md](api-gateway.md)・[git-gateway-cutover.md](git-gateway-cutover.md) — 外に出す側

---

## 1. 方向性: 分割と縮退

boid は、自分のニーズに合う製品がないから作り始めたもので、公式から良いものが出てくればそれを使う、
という前提のものだった。その後、Claude Code のクラウドセッションの柔軟性が上がり、
もともとサンドボックスでやりたかったこと (使い捨てのコンテナ、ネットワークの制限、認証情報を持たない git、
定期実行、PR の監視、スマホからの操作) の多くが公式でできるようになった。似た領域の製品も出てきている。

とはいえ boid を丸ごと捨てるわけではない。方針は 2 つ。

- **分割**: boid のために作ったが、boid の外 (チャット・Cowork、一般社員への Claude 活用の展開) でも使いたいものを、
  独立したプロダクトとして外に出す。API ゲートウェイとタスク管理がこれにあたり、すでに始めている
- **縮退**: boid には、公式では代わりがきかないものだけを残す

## 2. 外に出すもの

### 2.1 API ゲートウェイ → api-gateway-mcp

- boid の `internal/apigateway` (と `gwtransport`) は**退役**させ、api-gateway-mcp に一本化する。
  workspace の中に閉じるメリットはあるが、認証情報の置き場・サービスの定義・OAuth クライアント・API の使い方のスキルが
  二重になるデメリットのほうが大きい。api-gateway-mcp は自分の Cloudflare アカウントへのセルフホストなので、
  預け先が自分であることは変わらない
- 手元 (LAN・localhost) からしか届かない API は今のところない。退役で穴は空かない
- **隔離はゲートウェイのインスタンス単位**で行う。2 段に分ける

  | 段 | 単位 | 例 | 切り替え方 |
  |---|---|---|---|
  | Claude のアカウント | つなぎ先がまったく違う事業 | アーバン (ビルメン) / ノブシテック (開発) | アカウントごとにメインのゲートウェイを claude.ai につなぐ |
  | boid の workspace | 同じ事業の中の顧客の環境 | 顧客プロジェクト | boid の workspace では claude.ai のコネクタを切り、顧客向けのゲートウェイにつなぎ直す |

- 顧客向けのゲートウェイは、ノブシテックの Cloudflare アカウントに顧客ごとのデプロイを並べる
  (`deployments/<名前>.jsonc`)
- スキルは MCP のツールを短い名前 (`issue_token`) で呼び、向き先は `issue_token` が返す `GW_BASE_URL` から取るので、
  MCP サーバーの登録名はインスタンスごとに違ってもよい。2 つのゲートウェイを同時につなぐと Claude がどちらの
  `issue_token` を使うか取り違えうるので、boid の workspace では claude.ai のコネクタを切っておく

#### api-gateway-mcp に足すもの

1. **MCP ではないトークン取得の口**。Claude のセッションの外で動く処理 (git の credential helper、ハーネスのない job など) が
   短命トークンを取るため
2. **git の接続先** (§2.3)
3. **boid の接続先**。`identity` の認証方式で boid の API につなぐ (§4.2)
4. **汎用の逆向きの接続 (WebSocket モード)**。手元のサービスのほうからゲートウェイにつなぎに行き、ゲートウェイの
   Durable Object が中継する。最初は Cloudflare Tunnel で始め、これは後から足す。boid 専用にしないことが条件 (§6)

### 2.2 タスク管理 → Murmur

- boid の card / triage task・signal inbox・identity 索引・`integrationpack`・workspace の scan / sweep の仕組みは**退役**させ、
  Murmur に移す。引き継ぎ先は Murmur の 0012「boid からの系譜」の表のとおり
- **Murmur とゲートウェイはペアで並べる** (Murmur の 0020: 1 対 1)。顧客ごとにゲートウェイを並べるなら、Murmur も顧客ごとに並べる。
  boid の workspace で動いている顧客向けの triage (Slack・Jira・Bitbucket の収集) は、その顧客の Murmur に置き換わる
- **横断の俯瞰は持たない**。boid を使って分かったのは、結局 workspace でフィルタして、それぞれの workspace のタスクを
  見ていたこと。大事なのは複数のデータソースからの Ingest と判断で、人はそれぞれの workspace の「モード」でリストを
  眺めたい。横断は Murmur を 1 つずつ巡回する
- 用語を分ける
  - **個人のメモのデータソース** (Murmur の 0014。各 Murmur の中の、読者が本人だけのリスト): やる (Murmur に実装済み)。
    1 つの Murmur = boid の 1 workspace の分しか持たない
  - **自分専用の Murmur** (Murmur の 0022・0020 の 4。「1 人のチーム」として別にデプロイし、チームの Murmur から要約を集める):
    セキュリティの境界が微妙すぎるので、やらない方向。Murmur の ADR への反映は Murmur の側で行う
- **開発のシグナル** (PR のコメント・CI・レビューの依頼・担当課題の変化) は、取り込むなら Murmur に統合する
  (GitHub / Bitbucket のコネクタを足し、PR も Jira の課題と同じく 1 本のスレッドにする)。boid では取り込まない。
  Murmur の計画 (0016 の次の段階) に載せる候補

### 2.3 git gateway → api-gateway-mcp

git の smart HTTP は普通の HTTPS (`GET .../info/refs`、`POST .../git-upload-pack`・`git-receive-pack`) なので、
api-gateway-mcp の素通しの中継 (`proxy.ts`。ボディをストリームのまま流し、ヘッダーを差し替える) にそのまま乗る見込み。

```
git -c http.extraHeader="Authorization: Bearer $GW_TOKEN" \
    clone https://<ゲートウェイ>/api/github-git/owner/repo.git
```

`github-git`・`bitbucket-git` のような接続先を足し、上流には Basic 認証を注入する。

boid の `internal/gitgateway` の機能の行き先:

| 機能 | 行き先 |
|---|---|
| 資格情報の注入 | api-gateway-mcp |
| repo ごと・操作ごとの許可 (job token ごとに fetch のみ / push も) | **持たない**。上流の token の絞り方に任せる (下記) |
| push の観測 | 要らない。task がなくなり、成果は job の結果で返す |
| リリースの検証 | 要らない。オーケストレータが forge の API で確かめる |
| 対話のプロンプトの回避 (資格情報がないとき 502 で落とす) | `GIT_TERMINAL_PROMPT=0` |

- **git の許可は上流の token の絞り方に任せる**。api-gateway-mcp の「メソッドやパスによる認可はしない。権限を絞りたければ
  上流側で絞った認証情報を登録する」(`concept.md`) に合わせる。GitHub の fine-grained PAT・Bitbucket の repository access token は
  repo ごと・読み取りのみに絞れ、ゲートウェイには 1 サービスに複数の認証情報がある。readonly の job には読み取り専用の認証情報で
  トークンを発行する。boid にもゲートウェイにも repo ごとの許可は持たない
- 気をつけること
  - push のサイズ: Workers のリクエストのボディの上限 (プランにより 100MB〜)
  - トークンの寿命 (15 分): 長い job の途中で切れるので、credential helper が MCP ではない口から取り直す
  - clone はレスポンスのストリームなので、サイズの制限はない
- 得るもの: **GitHub に縛られないことが boid の外に出る**。Claude Code のクラウドセッションからも、ゲートウェイ経由で
  顧客の Bitbucket を clone・push できる (ネットワークの設定でゲートウェイのドメインを許可する)。Cowork のサンドボックスに
  git が入っているかは未確認

## 3. boid に残すもの

**workspace・sandbox・job の 3 つだけ**を残す。基本は「workspace (モード) に入り、隔離された環境でエージェントを起動するだけ」で、
足りないものは Murmur に出していく。その境界の見極めがこの構想の要。

### 3.1 残す価値

| | 価値 | 支える部品 |
|---|---|---|
| G1 | workspace 単位のハーネス設定の分離 (Claude Code 等の設定・MCP・スキル・つなぐゲートウェイ) | workspace の定義、workspace home の volume、`profiles` |
| G2 | 状態の永続化。特にメモリ (クラウドセッションではメモリが使えない) | workspace home の volume (`~/.claude/projects/.../memory`・セッションの jsonl)、`runtime-transcripts` |
| G3 | マルチハーネス | `adapters` |
| G4 | プログラマティックなエージェントの起動 | job の API、`dispatcher`、`sandbox` (container backend) |

加えて、**顧客の環境をアカウントのメインから隔離して走らせる場所**であることが、公式では代わりがきかない。
クラウドセッションにはアカウントの claude.ai のコネクタがそのまま付いてくるため。

### 3.2 job

- job = 「workspace × ハーネス × 指示 × (任意で) repo とブランチ」で 1 回走らせたもの。状態は `running / success / failed` だけ
- **task・状態機械・behavior・hook・trait・payload・action・trigger は退役**させる。behavior は「どの指示とどのハーネスで起動するか」の
  プリセットに縮み、workspace の設定に置く
- **ask は「書いて終わる」**。ハーネスを待たせず、質問を結果に書いて job を終わらせる。ハーネスに求めるのは「結果を書ける」「終われる」だけになり、
  ハーネスごとの機能 (RPC の中でブロックする、後から再開する) に頼らないので、マルチハーネスが強くなる
- **結果の約束**: job は決まった場所に JSON (結果の要約・質問・成果物 (PR の URL など)) を書いて終わる。boid はそれを job の結果として保存し、
  API で返す。今の `artifact` の trait はここに縮んで残る
- 続きは同じ workspace で新しい job を起こす。文脈は、オーケストレータが渡す指示、push 済みのブランチ、G2 で永続化したメモリから拾う
- **親子タスクは boid で表さない**。card で分かったのは、behavior や親子関係が「タスクの実行 → judge → 次のタスクの提案」のループで
  相当程度代用できること。そのループをオーケストレータ (§4) に磨いていく
- 生の対話 (Claude の Remote Control で起動してスマホから触る、通知を受ける) はハーネスの機能に委ねてよい。ループはハーネスに委ねない

### 3.3 プロジェクトの概念

git の許可が上流の token に移り、task がなくなると、プロジェクトに残る役目は「job を始めるときに何を clone するか」だけになる。

- **プロジェクトのエンティティは持たない**。job を起動するときに repo の URL とブランチを渡す (クラウドセッションの `source_url` と同じ形)
- workspace にはデフォルトの repo の一覧をデータとして置ける
- clone 先は `/workspace/<repo 名>` に固定する。G2 のメモリが `~/.claude/projects/-workspace-<repo 名>/` に入るので、
  パスが揺れると job ごとにメモリが分かれる

### 3.4 部品ごとの仕分け

| 部品 | 扱い |
|---|---|
| workspace の定義 (つなぐゲートウェイ・env・egress・`$HOME`・ハーネスの設定) | 残す (核) |
| container backend・network 分離・reap・GC | 残す |
| job・transcript・attach | 残す |
| adapters | 残す (G3) |
| HTTP API / CLI | job の起動・一覧・状態・結果・ログ・停止・終わるまで待つ、に縮める |
| `orchestrator` (状態機械・behavior・hook・trait・payload・action・trigger) | 退役 |
| card・signal・`integrationpack` | 退役 (Murmur へ) |
| `apigateway`・`gwtransport` | 退役 (api-gateway-mcp へ) |
| `gitgateway`・プロジェクト | 退役 (api-gateway-mcp へ。§2.3・§3.3) |
| host command | 退役。git はゲートウェイの git、API は api-gateway-mcp、task の更新は task ごとなくなる |
| Web UI | job の一覧・ログ・attach だけに絞る。task・card の画面は消す |
| `timeline`・`notify` | 要検討 (§7) |

`api`・`orchestrator`・`dispatcher` で約 5 万行 (テストを除く) あるうち、かなりの部分が task の状態機械まわりなので、
残す 3 つに絞ると半分以下になる見込み。

## 4. オーケストレータ

### 4.1 役割

Murmur から見ると、エージェントは全部スタンドで、スタンドは自分の代理。スタンドが動いている間も、ask が来ても、
**ボールはずっと自分にある**。だから Murmur にエージェントの身元は要らない。

その代わりに、**オーケストレータ役のスタンドを 1 人置く**。

- job を起動・監視し、結果を judge し、次の一手を考え、必要なら人に質問を返す
- boid の job は Murmur を知らない。オーケストレータが人と Murmur と boid の間に入って中継する
- オーケストレータは実行しないので、コーディングエージェントである必要がない。Cowork でも ChatGPT でもよい
- 新しいプロダクトではなく、**スキル 1 つ** (Murmur のスキルの隣に boid のスキルを足す)
- 起こし方は、基本はタスクごとに起こしてそのタスクを見張らせる。定期実行で、実行できそうなタスクを見つけて回す形もありうる

```
            人
            ▲│ 質問 / Go
            │▼
   オーケストレータ (Cowork・ChatGPT・Claude Code…  ※スキルだけ)
     │ 読む・記録する          │ job の起動・状態・結果
     ▼                         ▼
   Murmur ◀── ゲートウェイ ──▶ boid (workspace・sandbox・job)
   (台帳。ボールは                  │ 顧客の workspace は
     ずっと自分)                    ▼ 顧客向けのゲートウェイへ
                               コーディングエージェント (Claude Code・Codex…)
```

card の「実行 → judge → 次の提案」は、「job → オーケストレータ → 人 → job」になる。

### 4.2 boid をゲートウェイに挿す

| 区間 | 経路 | 認証 |
|---|---|---|
| オーケストレータ → Murmur | ゲートウェイ → Service Binding (今のまま) | `identity` |
| オーケストレータ → boid | ゲートウェイ → Cloudflare Tunnel (後から WebSocket モード) | `identity` (boid がゲートウェイの JWKS で検証) |
| boid → Murmur | なし | — |

- Murmur は Service Binding 前提だが、boid は手元にあるので Service Binding は張れない。boid の job が Murmur を知らない形にしたので、
  boid と Murmur は直接つながらない
- `identity` は Service Binding がなくても `base_url` に送れるので、Tunnel の形ならゲートウェイのコードの変更は要らない
- boid の API は「sandbox の中で任意のコードを走らせる口」なので、ゲートウェイの JWT がないものは全部はじき、許可する `sub` を
  boid の設定で絞る
- WebSocket モード (boid のほうからつなぎに行く) にすれば、手元を外に公開しなくて済み、攻撃される面が減る。セキュリティの面でも
  こちらが強い
- オーケストレータがポーリングし続けなくて済むよう、boid の API に「job が終わるまで待つ」(long-poll か、終わったら知らせる) 口を用意する

## 5. この形でできるようになること

- **クラウドセッションから boid を呼べる**。Claude Code のクラウドセッションが、手元の boid で Codex を起動して PR をレビューさせる、
  のようなハーネスをまたいだクロスチェックが、オーケストレータのスキルでできる。G3 の使い道がいちばんはっきりする
- **クラウドセッションや Cowork から Bitbucket に触れる** (§2.3)
- **汎用の WebSocket モードを LAN の中のサービスへのトンネルとして使える**。普通のチャットが LAN の中のサービスを叩けるようになる

部品ごとの責務がそれぞれ 1 つになった (ゲートウェイは「届く・認証する」、Murmur は「台帳」、boid は「隔離された実行環境」、
オーケストレータは「スキル」) ので、組み合わせで使い道が広がる。方向性の筋が良いことの傍証と見ている。

## 6. 条件・リスク

- **WebSocket モードの LAN トンネルは「チャットから LAN の中に入れる口」になる**。どのホストに届いてよいか・誰が使ってよいかを
  ゲートウェイの管理者が決める (管理画面の接続先の登録の延長)。boid 専用にせず、汎用の「逆向きの接続先」として作る。
  ゲートウェイの性格 (HTTP と OAuth2 だけを知り、サービスを足してもコードを変えない) を崩さない
- **顧客の環境の結果がオーケストレータを通る**。オーケストレータはメインのアカウントのスタンドなので、顧客の workspace の job の結果は
  メインのアカウントの Claude に入る。job の結果に何を書くか (要約にとどめる) を結果の約束で決める
- **job は状態を持たない前提になる**。成果は job が終わる前に push する、を job の約束にする

## 7. 未決

1. `timeline`・`notify` を残すか。job の一覧と結果で足りるか、通知はオーケストレータ / Murmur の側に寄せるか
2. 定期実行 (今の trigger・sweep) をどこに置くか。オーケストレータの定期実行 (Cowork の scheduled task など) に寄せるか、
   boid に最小の定期起動を残すか
3. job の結果の JSON の形 (要約・質問・成果物のキー、顧客の情報をどこまで書くか)
4. 人がチャットにいないときの質問の届け先。Murmur の個人のメモのスレッドに書く (次に巡回したときに見える) か、
   オーケストレータの定期実行がまとめて通知するか
5. 移行の順序。外に出す側 (api-gateway-mcp の git・MCP ではないトークンの口・boid の接続先) を先に用意し、boid の退役はその後になる。
   退役のあいだの互換 (既存の workspace の設定・DB のマイグレーション) の扱い
6. Cowork のサンドボックスに git が入っているか (実機で確認)
7. Murmur の側への反映: 自分専用の Murmur をやらない方向 (0020 の 4・0022)、GitHub / Bitbucket のコネクタ。Murmur の ADR で決める
