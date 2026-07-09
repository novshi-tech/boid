# コンテナ基盤 boid 構想ドキュメント

ステータス: 構想 (draft) — 着手判断前
作成日: 2026-07-06

---

## 背景と動機

現行 boid のサンドボックスは独自実装 (rootless userns + pivot_root + 5 段 mount) で、
普通のコンテナでは再現が難しい 2 つの価値を提供している:

1. **workspace ごとのドメイン許可リスト** (egress proxy)
2. **host command** (broker 経由のホスト側実行)

一方で、この方式には構造的なコストがある:

- 仕組みが複雑で保守負担が大きい
- 「同種のエージェントを k8s で動かしたい」という要望に応えにくい
- git builtin の許可ポリシーが複雑 (後述の通り、これは方式自体に由来する)

### 複雑さの根は「ホスト共有ファイルシステム前提」

現行の複雑さの多くは、個別の実装の問題ではなく
**サンドボックスがホストのファイルシステムを共有している**という前提 1 点に由来する:

- ホストと地続きだから git をホスト側実行 (builtin) にせざるを得ない
- ホスト実行だから hooks / git config がエスケープ経路になる → `core.hooksPath=/dev/null` で無効化
- ホスト実行だから引数レベルの複雑な許可ポリシー (`-C`/`-u` 拒否等) が必要
- ホストの上に安全な檻を手作りするための userns / uid_map / mount 制御

前提を「コンテナ + 専用 volume」に変えると、これらは解決されるのではなく**問題ごと消える**。

### 部品単位代替の方針

boid は「良い製品が出るまでの繋ぎ」を自認するプロジェクトであり、
代替は製品丸ごとではなく**部品単位**で既存のものに置き換えていく
(ポリシーはデータとして boid が持ち、enforcement は差し替え可能なバックエンドに委譲する)。
本構想はその方針の最大の適用例で、サンドボックスの enforcement 層を
コンテナランタイム + 既存部品群に委譲する。

---

## 基本方針

- **信頼境界**: broker 側 (daemon / タスク管理 DB / credential / docker socket) と
  sandbox 側 (エージェント実行、credential レス) をコンテナ境界で分離する
- **粒度**: `volume = workspace`、`コンテナ = job (使い捨て)`。
  常駐コンテナ + `docker exec` 方式は採らない
  (同一 workspace の並行タスクがプロセス空間を共有し、現行の job 単位隔離より弱くなるため。
  イメージが pull 済みならコンテナ起動コストは十分小さい)
- **状態は volume に、実行はコンテナに**。job コンテナは落として作り直せばよく、
  状態復元の仕組みは不要
- **既存プロトコルの意味論は保存し transport だけ差し替える** (broker、git、docker proxy)
- **移行は strangler 方式**: sandbox backend を interface 化し、
  現行 userns backend と container backend を並走させる。
  一気切替はしない (Phase 3-a の一気切替方針は bash→go の同機能置換だったから成立した。
  今回は別バックエンドの追加であり、並走が正当)

---

## 目標アーキテクチャ

### 配置

```
[broker 側 = 信頼側]                       [sandbox 側 = 非信頼側]
  boid daemon                                job コンテナ (使い捨て)
  タスク管理 DB (SQLite)                       entrypoint = go runner (Phase 3-a)
  broker サービス (mTLS gRPC/HTTP)             ハーネス (claude/codex/opencode)
  git gateway (認証注入)                       credential レスの素の git
  docker proxy (既存 dockerproxy を再配置)      docker CLI / TestContainers
  egress proxy                                 (DOCKER_HOST → broker の proxy)
  credential 一式
  docker socket (マウント)
                 ↑
        workspace volume を共有 (job コンテナ側にマウント)
```

- **ローカル**: docker compose で「egress proxy + workspace (job)」の最小 2 コンテナ。
  第一歩では daemon は現行どおりホストプロセスのままでよい (docker socket をホストで直接握る)。
  daemon 自体のコンテナ化 (socket マウント) は後続
- **チーム共有**: k8s。daemon は Pod 作成 RBAC を持つ controller、
  job は k8s Job / Pod として生成 (operator パターン)。
  リトライ・TTL 掃除・スケジューリングは k8s に委譲

### dispatch: push 型 (runtime API 駆動)

daemon が docker / k8s API を叩いて job コンテナを生成する。
entrypoint に Phase 3-a の go runner をそのまま据え、コンテナの寿命 = job の寿命とする。

- sandbox 側に常駐 agent や SSE 受信チャネルは**不要**
- 実行中 job の停止 = container kill。reopen = 新コンテナ生成
- pull 型 (常駐 client agent が指令をポーリング/ストリーム受信) は
  「daemon から runtime API に届かない環境」(別マシンのリモートランナー、NAT 越え) 向けの
  将来拡張として位置づける (GitHub Actions self-hosted runner 型)。初期アーキテクチャの前提にしない

### broker 通信: transport swap

現行 broker プロトコルは**sandbox 発で broker に接続する**方向であり、
この向きはネットワーク越しでもそのまま成立する。
UNIX socket を mTLS の gRPC/HTTP エンドポイントに差し替えるだけで、
shim / task ask blocking RPC / notify の意味論は無傷。
ローカル compose では従来どおり socket マウントでもよい。

### boid CLI のリモート接続: Web UI と同格のクライアントへ

broker 側がコンテナ / 別ホストに移ると、CLI が前提にしてきた
「daemon と同一ホスト + UNIX ソケット」が消え、`boid agent` / `boid attach` 等の
CLI 操作経路が断絶する。解決は **CLI を Web UI と同格の
「ペアリング済みデバイス (TCP + device auth)」に寄せる**こと (2026-07-07 方針)。
既存資産で大半が済む:

- CLI は全コマンドが `internal/client` の薄い HTTP クライアント経由で、
  UNIX ソケット依存は `DialContext` 1 点 → transport swap 可能
- daemon は既に全 API surface を TCP で serve している
  (`internal/server/server.go` の tcpHandler = router + transport-aware device auth)。
  Web UI の Cloudflare Tunnel 運用で本番実績あり
- 対話 attach は既存 WebSocket endpoint (`GET /api/jobs/{id}/attach/ws`,
  `internal/api/ws_attach.go`: input/resize/output/exit + device auth + revocation) を共用する。
  現行 CLI の独自 `Upgrade: boid-attach` は tunnel / 中間 proxy の透過性が
  保証されないため、リモート経路は WS に一本化する
  (ローカルの UNIX ソケット経路は従来どおりでよい)

**terminal の再現は不要 (リモートエコーモデル)**: CLI は本物の端末エミュレータ内で
動くため、xterm.js 相当の実装は要らない。入力は不透明なバイト列として転送するだけで、
クライアントが解釈するキーは detach (Ctrl+], 0x1d) のみ。エコー・行編集・描画は
全てサーバ側 PTY とハーネスの仕事 (SSH クライアントと同一構造)。
入力側の実装はローカル attach (`cmd/attach.go`) の鏡写しで、新規なのはフレーミングだけ:

- raw mode 化 (`makeRawInput` を流用。行バッファ / ローカルエコー / シグナル化を止める)
- stdin 読み取り → base64 `input` メッセージ送信 (現行 `io.Copy(conn, stdin)` の差し替え)
- SIGWINCH → `resize` メッセージ (ローカル版の別 HTTP POST よりむしろ簡潔になる)
- detach キー検出 (`detachReader`) と終了時 `term.Restore` は現行のままクライアント側

web terminal vt-emulator Phase 1 の接続時グリッドスナップショットはサーバ側の
仕組みなので、リモート CLI の途中 attach もそのまま恩恵を受ける (追加作業なし)。

設計判断が要る点:

- **コマンドの二分類**: `boid start` / `stop` / `init` / `gc` 等の
  daemon 生殺与奪・ローカル資源管理系は本質的にローカル専用。
  task / project / attach / agent / observe / exec 等はリモート可。
  分類を明文化し、リモート接続時のローカル専用コマンドは明示エラーにする
- **ローカルパス引数の契約**: `--output-file` 等ホストのパスを渡す系は境界を越えない。
  host command contract と同じ語彙 (コマンド毎の明示 + shim 早期拒否 + 代替案内) で締める。
  コンテナモデルでは project = リモート git URL が前提になるため、
  最大のローカルパス依存 (ローカルパスでの project 登録) は消える方向
- **credential UX**: 既存ペアリング機構 (`boid web pair`) をそのまま使い、
  `boid login <url>` でコードを入力して device token を `~/.config/boid` 等に保存、
  接続先は `BOID_HOST` 的な設定で切り替える。新しい認証機構は作らない

### git: gateway 方式 (builtin 廃止)

git を「ローカル操作」と「transport」に分離する。
credential が必要なのは fetch / push の transport だけで、
commit / branch / diff / checkout / worktree は credential も通信も不要。

- sandbox 内に **credential レスの素の git** を置き、ローカル操作は全てそこで完結
- remote URL は broker 側の **git gateway** (認証注入リバースプロキシ) に向ける
  (例: `http://broker.local/novshi-tech/boid.git`)。gateway が上流 (GitHub 等) への
  リクエストに token を注入する

これで現行 git builtin が抱える問題が同時に消える:

1. **credential 露出なし** — token は gateway が注入し、sandbox には一度も入らない
2. **egress allowlist に github.com 不要** — sandbox から見える git の宛先は gateway のみ
3. **remote 書き換えが無害** — gateway 以外のホストには egress が通らず、
   gateway は許可された repo のパスしかルーティングしない。
   「どこに push できるか」が URL ルーティングという最単純の層で決まる
4. **hooks / git config のエスケープ懸念が消滅** — git が sandbox 内で動くため、
   hooks が動いても檻の中。`core.hooksPath=/dev/null` も引数ポリシーも不要になる

#### 意味論の変化: 成果共有は origin 経由のみ / リモート repo が project の前提になる

この方式の根本の意味論変更は「**push しない限り、ローカルの変更は他のセッションに
共有されない**」こと。reopen の commit-only 保証 (移行戦略の決定事項) も、
ホスト側 worktree 読者の置換 (未解決論点) も、全てこの帰結。

そこから、**boid project として実行可能である = HTTPS でアクセスできるリモート
git リポジトリを持つ**、が前提になる (2026-07-06 nose 確認):

- **リモート無しのローカル repo は登録できなくなる** — 現状からの縮退だが、
  シンプルさに繋がる割り切りとしてポジティブに受け入れる。
  (gateway がホストのローカル repo を `git http-backend` で serve して origin に
  見せる逃げ道は技術的にはあるが、non-bare repo への push 問題等の複雑さを
  再導入するため初期スコープでは採らない。必要が生じたら緩和策として再検討)
- **SSH remote は非サポート** — gateway の認証注入は HTTP(S) + token 前提。
  HTTP↔SSH ブリッジは技術的には書けるが (pack protocol は transport 非依存)、
  既製部品の無い自作領域であり部品単位代替の方針に反するため採らない。
  主要ホスティング (GitHub / GitLab / Bitbucket) は全て HTTPS + token を
  サポートしており、実用上の制約は小さい

#### ホスト側は gateway を経由しない (2026-07-07 確認)

gateway は repo の正本を持たない pass-through であり、正本は上流 forge
(GitHub / Bitbucket) のまま。remote URL は clone ごとのローカル設定なので、
origin を gateway に向けるのは sandbox 内 clone だけでよく、
ホスト側 repo の origin は上流のまま (人間の SSH 操作も従来どおり) 変えない。
この非対称は解消すべき問題ではなく、「成果共有は origin 経由のみ」(前節) の
意味論そのもの。ホストと sandbox の同期は常に上流 forge を経由する。

この帰結として、「SSH → HTTPS」の移行の実体はホスト repo の remote 書き換えではなく
**gateway↔上流の接続設定 + token セットアップ**になる。SSH URL → HTTPS URL の正規化
(`git@github.com:owner/repo.git` → `https://github.com/owner/repo.git`) は機械的に行える。

#### gateway の実現方式: Go 標準 ReverseProxy の自作薄層 (2026-07-07 方針)

git smart HTTP は実質 2 エンドポイント (`GET /info/refs?service=...`、
`POST /git-upload-pack` / `/git-receive-pack`) の素の HTTP で、認証注入は
Authorization ヘッダ 1 個の付与に過ぎない。gateway の仕事は
「パスルーティング (許可 repo の allowlist) + 認証注入」だけの薄い層なので、
標準ライブラリ `net/http/httputil.ReverseProxy` で自作する (100–200 行想定)。

- **既製品 (nginx / Envoy) を採らない理由**: 許可 repo 集合というポリシーは
  boid が持つデータであり (部品単位代替の方針)、enforcement を外部プロセスに寄せると
  boid → 設定ファイル生成 → reload という間接層が増える。
  `ReverseProxy` 自体が枯れた標準部品であり、自作部分は配線のみ
- **差し替え可能性は interface で担保**: push 内容の審査 (force-push 禁止等、
  receive-pack のプロトコル検査) が必要になった時点で
  [FINOS git-proxy](https://github.com/finos/git-proxy) 等への差し替えを再検討する
- `git http-backend` (git 同梱の smart HTTP サーバ) は「上流の無いローカル repo を
  serve する」逃げ道用の部品で、初期スコープ外 (意味論の変化の節を参照)
- 実装時の注意: packfile の大きな POST ボディのストリーミング転送、
  chunked encoding と `Expect: 100-continue` の透過

#### token 戦略: 個人 (user-level) token を採用 (2026-07-07 nose)

repo 単位 token は不採用 (repo ごとの発行・登録が運用に乗らない)。
token は広めに持ち、「どの repo に書けるか」の絞りは gateway の URL ルーティングが
全責務を負う。repo 単位 token を第二の allowlist として重ねる冗長な安全装置は捨て、
ポリシー = boid / enforcement = gateway に一本化する。

| forge | token | git HTTPS の username | 備考 |
|---|---|---|---|
| GitHub | fine-grained PAT | 実質任意 (慣例 `x-access-token`) | 1 token で複数/全 repo をカバー可。org repo は org 側の fine-grained PAT 許可設定が必要 (不可なら classic PAT fallback) |
| Bitbucket Cloud | スコープ付き API token (`read:repository:bitbucket` / `write:repository:bitbucket`) | `x-bitbucket-api-token-auth` | App Password は deprecated で使わない。Repository Access Token (repo 単位) は workspace 25 個上限もあり不採用 |

- gateway から見た forge 差分は「Basic 認証の username 規約 + token 種別」のみ。
  実装は共通化でき、forge 種別 (または username) を設定 1 フィールドで持てば足りる
- **失効前提の運用**: 両 forge とも token は失効前提 (Bitbucket API token は最長 1 年で
  強制失効)。期限切れによる上流 401 の検知・通知を gateway の設計に含める
  (token は broker 側の設定値 1 個なので差し替え自体は軽い)
- **顧客展開は個人アカウント直結を避ける**: 退職・権限変更で全停止するため、
  サービスアカウント (machine user) の token が定石

#### 認証付き CLI (gh / az / aws / fly 等): host command 方式で汎化

git gateway は git プロトコルしか運べないため、「push 直後に PR 作成を許可したい」
要件には REST/GraphQL の経路が別に必要になる (k8s では「ホスト側実行」という
現行 brokered gh の足場自体が消える)。これは gh に限らず、
外部サービスと通信する認証付き CLI (az / aws / fly / ...) 全般の問題。

**現行 host_commands の延長で解く**: 必要なツールを broker 側 (イメージ) に焼き込み、
sandbox 内には shim を置いて broker RPC で実行する。credential は broker 側にのみ存在し、
エージェントは知っているコマンドを透過的に叩ける (認知負荷が最小)。

不採用の代替案 (検討記録): GitHub API allowlist proxy — broker が実 token を持ち
許可 API パスのみ転送する方式。パス単位 allowlist は制御粒度で勝るが、
Bearer token のヘッダ差し替えで済むサービスにしか使えず (aws は SigV4 でリクエスト全体に
署名がかかるため単純な注入 proxy が成立しない)、サービスごとに proxy を作る羽目になり
汎化しない。gh だけ特別扱いするのはバランスが悪い (2026-07-06 nose 判断)。

host command 方式が RPC 設計に持ち込む課題:

- **ファイル引数・cwd・stdin が境界を越えない** (現行 brokered gh の `--body-file` 問題と同種)。
  別ホスト構成では全コマンドで恒常化するため、RPC に stdin / ファイル転送を設計するか、
  制約として明文化するかの判断が要る。制約とする場合は host command の設定を
  コマンド毎に持ち、非サポート引数を明示できるようにする (2026-07-06 nose)。
  ただしフラグ denylist だけでは位置引数がファイルパスの系 (`aws s3 cp <file> ...` 等) を
  捕まえられないため、shim 側の早期拒否 + 代替手段を案内するエラーメッセージ
  (例: 「`--body-file` 不可、`--body "$(cat ...)"` を使え」) までを設定の語彙に含める。
  黙って壊れるのがエージェントには一番高くつく
- **broker 側にはリポジトリ checkout が無い**。cwd の remote から repo を推定する系
  (`gh pr create` 等) は明示引数 (`-R`) の強制か、RPC でのコンテキスト伝搬が要る
- **引数レベルのポリシーが限定的に復活する**。broker 側で実 credential 付きで argv を
  実行するため、制御粒度は API パス allowlist より粗い。ただし対象は opt-in の
  認証付きコマンド集合のみで、git は gateway が別枠で受け持つため、
  現行 git builtin ほどの複雑さには戻らない

git は例外: repo の実体が sandbox 内 clone にあるため、
transport は host command でなく gateway 方式のまま (前節)。

### セッション分離: worktree の扱い

コンテナの分離と git worktree によるセッション分離は直交する問題で、
daemon とコンテナホストが同一ディスクかどうかで扱いが分かれる (2026-07-06 下調べ反映):

- **同一ホスト (ローカル)**: worktree bind mount も技術的には可能だが、
  契約先行の方針 (移行戦略参照) によりローカルも sandbox 内一時領域への
  `git clone --reference <ホスト repo>` に揃える (同一ホストなので mirror 不要、
  ホスト repo を直接 reference。一瞬・ディスクほぼゼロ)
- **別ホスト (k8s / リモートランナー)**: bind mount が使えないため mirror + clone 方式が必要

別ホスト構成で検討した方式と結論:

| 方式 | 評価 |
|---|---|
| RWX volume 上で各コンテナが直接 `git worktree add` | 複数書き手の git 排他制御 (ref lock, `O_EXCL`) がネットワーク FS のキャッシュ挙動 (TOCTOU) で信頼できない |
| worktree 作成を daemon に集約し、出来た dir だけ割当 | 安全だが RWX volume の I/O レイテンシがドライバ依存で残る |
| ZFS/Btrfs/CSI の volume clone | 安全・高速だが対応ストレージバックエンド必須 |
| **mirror + `git clone --reference`** | **採用**。mirror (RO マウント) を alternates で間借りしつつローカルディスクに独立 clone |

```bash
# セッション開始時 (job コンテナ内 or init コンテナ)
# clone 元は常に gateway URL、オブジェクトは RO マウントの mirror から alternates で間借り
git clone --reference /mnt/refs/novshi-tech-boid.git \
    http://broker.local/novshi-tech/boid.git /workspace
```

- **clone 元 URL は常に gateway、`--reference` はオブジェクト供給の最適化** (2026-07-08 決定)。
  file:// から clone して後で `remote set-url` する方式は採らない。
  gateway から clone すると refs は上流の最新を直接取るため mirror の鮮度に依存しない
  (push 直後の branch が mirror 未反映でも見える)。転送は mirror に無いオブジェクトの
  差分のみ。`--reference` を欠いても遅くなるだけで正しく動く (graceful degradation) ため、
  契約は gateway URL が正、reference は任意の最適化と位置づける。
  トレードオフとして refs 取得で毎 clone 上流 1 往復が入る (上流障害中は dispatch 不可)。
  初期はこの結合を許容し、障害時 fallback (mirror serve) は必要になってから足す
- mirror は read-only マウント。fetch/push は clone 後も gateway 経由
- **prune の注意**: `--dissociate` しない限り clone は mirror のオブジェクトを間借りしている。
  mirror 側 `gc --prune` の対象を古いもの限定 (`--prune=2.weeks.ago` 等) にする運用が基本だが、
  これは「最近 fetch されたオブジェクト」を守るだけで、mirror 側で unreachable になった
  オブジェクト (force-push で消えた履歴等) は年齢に関係なく刈られうる。
  boid の job は短命なので実害は小さい見込みだが、長寿セッションには
  `--dissociate` (file:// 経由ならネットワーク不要のままオブジェクトをローカルコピー) を保険とする
- **reopen 意味論は「commit 済みのみ保証」で確定** (2026-07-06 nose、移行戦略の決定事項参照):
  reopen = 再 clone + branch checkout。未コミットの作業は job 異常終了で失われる。
  現行 dev フローは done 前に push するため、正常系の成果は origin 経由で常に回収できる

#### mirror 更新戦略

- **更新トリガー**: 定期 fetch を常時動かし、webhook (GitHub `push` イベント、
  `X-Hub-Signature-256` 署名検証必須) が使える構成では間隔を長め、
  使えない (ローカル完結) 構成では短めにする。webhook 有無でコードパスを分岐させない。
  webhook 受信は既存 Web UI の Cloudflare Tunnel にパスを足すだけで済む
- **fetch と prune の分離**: fetch はオブジェクト追加のみで参照破壊リスクなし。
  危険なのは prune だけなので、独立した余裕あるスケジュールで運用する
- **単一書き手**: トリガー元が複数でも mirror への書き込みは単一ワーカーにシリアライズする。
  **ワーカーの置き場所に注意** — daemon とコンテナホストが別の場合、
  fetch ワーカーは mirror volume に RW アクセスできる側 (コンテナホスト側の常駐 Pod 等) で
  動かす必要がある。「単一書き手」の原則はそのままに、書き手の配置だけが daemon から分離する

### workspace peer プロジェクト: 動的 clone + 全 bare RO mount (2026-07-08 決定)

現行の「peer worktree を全て RO bind mount で見せる」はホスト FS 共有前提の産物。
新方式では peer も「見える = clone した分だけ」の意味論に揃え、
**事前 clone はせず、エージェントが必要になった時点で gateway 経由で動的 clone する**。

- mirror は job から見た役割 (自 project / peer) ではなく **project 単位**で存在する
  (peer は相対概念で、どの project も自身の job では主役)。
  よって peer 参照のための追加 mirror コストは無く、
  workspace 内全 project の bare を RO mount して reference 供給するのが自然
- **clone レシピは自 project と peer で完全に同一** (前節のコマンド)。
  違いは「誰がいつ叩くか」(自 project = runner が dispatch 時 / peer = エージェントが必要時) と
  gateway の許可粒度だけで、peer 専用機構は持たない
- **gateway の read/write 粒度**: job の自 project = fetch/push 可、
  workspace peer = fetch のみ (upload-pack のみ許可)。
  peer に書きたい場合は従来どおり cross-project 子タスクを作る
- **environment.yaml の advertise**: 現行のホスト path 列挙から
  `{project 名, clone URL, reference path}` の列挙に変え、
  エージェントの発見可能性を維持する
- **mount するのは bare (ローカルはホスト repo の `.git`) のみ・read-only**。
  working tree は mount しない。「commit されたものだけが境界を越える」を
  読み取り方向にも適用する。RO であることは hooks 書き込みエスケープ防止の前提でもある
- **mirror volume のレイアウトは isolation 境界に揃える**: 全 project の mirror を
  1 volume に平置きして丸ごと mount すると別 workspace の private repo の
  オブジェクトまで見える。mount は workspace 内 peer の分だけ、
  project 単位の subpath で行う
- **mirror 作成は project 登録時** (初回 dispatch 時ではない)。
  登録のみで job 未実行の project が peer として clone できない穴を塞ぐ
- これに伴い brokered op `GitOpCloneLocal` (peer 別ブランチ参照の `clone --local`) と
  `TokenContext.WorkspacePeers` の path 検証は退役。
  dispatcher の「workspace peer を列挙して mount + advertise する」データ配線のみ残る

### egress: L3 トポロジで強制

compose の internal ネットワークに job コンテナを置き、外への経路を egress proxy コンテナのみにする。
環境変数 (`HTTPS_PROXY`) への協調依存ではなく、ネットワークトポロジで強制される。
workspace → 許可ドメイン集合というポリシーデータは boid が持ち続け、
enforcement は差し替え可能にする:

- ローカル: 現行 ProxyManager 相当 or 既製 egress proxy (Smokescreen / Envoy dynamic forward proxy)
- k8s: Cilium `toFQDNs` ポリシー等に変換して流し込む
  (注意: toFQDNs は DNS 応答ベースのため、直 IP 接続はデフォルト拒否で別途塞ぐ)

### docker proxy: 既存実装の再配置

サンドボックスからの TestContainers / docker 利用は、
**既存の docker-native-proxy (`internal/sandbox/dockerproxy/`、Phase 1 マージ済み) を
broker 側に置く**ことで自然に解決する。
sandbox の `DOCKER_HOST` は broker 上の proxy を指し、
proxy がリクエストボディを検査して OK ならマウント済みの実 socket へ転送する。
新規開発ではなく配置換え。詳細は docs/plans/docker-native-proxy.md を参照。

### workspace コンテナ定義: devcontainer spec は採用しない

[devcontainer spec](https://containers.dev/) の採用を検討したが**見送る** (2026-07-06 下調べで確定):

- devcontainer の利便性 (`workspaceFolder` 自動導出、UID/GID 自動調整、宣言的設定) は
  「人間が VS Code で対話的に開発環境に入る」ケース向けのもので、
  オーケストレータがプログラム的にサンドボックスを生成・破棄する本用途とは前提が異なる
- `@devcontainers/cli` (`devcontainer up`/`exec`) で VS Code 非依存起動は技術的に可能だが、
  Node 製 CLI の間接層と起動オーバーヘッドが複数サンドボックス並行運用でデメリット
- セッションごとに対象リポジトリが変わる動的生成に `devcontainer.json` の静的設定は不向き
- egress 制御は規格外の機能で、`internal: true` ネットワーク等を素の docker/compose で
  直接組む方が自然
- ツール provisioning はコンテナイメージへの焼き込みで足りる (次節の kit 不要化)。
  devcontainer features に外部化する必要自体がない

**留保**: 「エージェント作業後の環境に人間が VS Code で同じ状態でアタッチしてデバッグしたい」
というニーズが将来生じた場合、人間とのインターフェース共通化という別の理由で
`devcontainer.json` 併置のメリットが復活する。現時点の要件からは独立した将来検討事項とする。

### kit 機構の行方: コンテナ backend では機構ごと不要化

kit は「サンドボックスがホストのファイルシステムを共有している」前提の産物であり
(ホスト上のツール群を binding で檻に見せ、machine-local な環境情報を commit 対象の
project から分離する仕組み)、コンテナ移行が完了すれば個別の代替ではなく**機構ごと消える**:

- ツール provisioning (kit の bindings / env / PATH) → コンテナイメージに焼き込み、
  ランタイムをコンテナ間 (イメージ) で共有する
- host_commands の path-match 系 (run-e2e 等) → broker RPC (未解決論点に既出)
- 「machine-local 情報を commit から分離する」軸 (kit/workspace/project 再編の動機) は
  「イメージ定義 + broker 側設定」という形に置き換わる

strangler 並走期間中は userns backend 用に kit を維持し、
container backend の完成をもって kit は退役する。

boid の役割は「セキュリティ外皮 (egress proxy / broker + host command RPC / git gateway /
docker proxy) + オーケストレーション」に純化していく方向は変わらない。

---

## 顧客展開との関係

顧客に類似の仕組みを導入する場合も boid 丸ごとではなく部品単位で再利用し、
enforcement は顧客が保守できる枯れた外部品 (Cilium / Envoy 等) に寄せる。
持ち込む資産は「何をどこで絞るか」というポリシー設計と配線の知見。
本構想により boid 本体がその構成に近づくため、リファレンス実装としての価値が上がる。

---

## 未解決論点

- **push 粒度ポリシー**: force-push 禁止などコマンド粒度の制御は
  git プロトコル (receive-pack) の検査が必要 (FINOS git-proxy の領域)。
  ただし最重要の「どの repo に書けるか」は URL ルーティングで済むため優先度は低い
- **uid mapping**: ローカルの bind mount volume でのファイル所有権。rootless podman 等で緩和
- **DB**: SQLite はローカルではそのまま。k8s チーム共有では PVC 上の SQLite か
  Postgres 移行かの判断が必要 (本構想のスコープ外として先送り可)
- **host_commands の path-match 系** (run-e2e 等): 認証付き CLI は broker イメージへの
  焼き込みで解が出た (前述) が、リポジトリ checkout や特権的実行環境を要する path-match 系は
  「ホスト」概念が消える k8s での扱い (専用 runner Pod?) を詰めていない
- **subscription key と ToS**: チーム共有では Claude subscription キーは利用規約上使えない見込み。
  API key / enterprise 前提
- **ハーネス周辺の細部**: session jsonl 永続化 (env strip)、embedded skills bind、
  IS_SANDBOX 等、現行 adapter が吸収している既知の癖のコンテナ環境での再検証
- **dispatch レイテンシ実測**: 「イメージ pull 済みなら十分速い」の裏取り
- **ホスト側 worktree 読者の棚卸し**: worktree bind mount 廃止 (契約先行ステップ ②) に伴い、
  ホスト側から worktree パスを直接読む機能 (web UI の diff 表示・gc の worktree 掃除等) を
  git データを origin/gateway 経由で読む形に置換する必要がある。対象の全量棚卸しが未実施
  (peer branch 参照の `clone --local` は workspace peer 節の動的 clone 方式で置換決定済み)
- **git gateway のプロトコル細部**: 実現方式は ReverseProxy 自作で決定済み (前述)。
  残りは `report-status` の扱い・`100-continue` / chunked 転送の実機検証
- **認証付き CLI の broker RPC 設計**: stdin / ファイル引数の転送方式、
  repo コンテキストの伝搬 (`-R` 強制 or RPC 伝搬)、許可コマンド集合と引数ポリシーの粒度
- **リモート CLI の細部**: ローカルパス引数を持つ CLI コマンドの全量棚卸し
  (ローカル専用 / リモート可の二分類の確定込み)、`boid login` の UX 詳細
  (token 保存場所・複数接続先の切替)
- **mirror 更新ワーカーのキュー実装**: 単一書き手のシリアライズ方式
  (ロック / 専用プロセス / ジョブキュー)
- **大規模 monorepo の検証**: clone 時間・mirror サイズ・`--dissociate` 時のディスク消費

---

## 移行戦略: 契約先行 (contract-first)

enforcement (コンテナ化) より先に、**境界の意味論 (契約) を現行 userns backend 上で
コンテナモデルに合わせる** (2026-07-06 方針確定)。コンテナ移行の最大の未知は
runtime API の操作ではなく「ホスト FS 共有をやめたときにエージェント・ハーネス・
運用フローが回るか」であり、これをロールバック容易な現行 backend 上で dogfood
してから enforcement を差し替える。各契約変更は単体でも現行 boid の複雑さを削る。

1. **host command の契約締め** (独立・最小): stdin / cwd 伝搬前提を廃止し、
   コマンド毎設定 + 非サポート引数の明示 + shim 早期拒否 (代替案内エラー付き) を導入
   (実装済み: [docs/plans/host-command-contract.md](host-command-contract.md) の PR1-5)
2. **git gateway + サンドボックス内 clone** (ワンセットで切替):
   - git builtin を廃止し、sandbox 内の素の git + gateway (認証注入) に置換
   - worktree bind mount をやめ、job ごとに sandbox 内の一時領域に
     `git clone --reference <ホスト repo>`
   - **ワンセットの理由 (セキュリティ)**: 共有 `.git` のまま git を sandbox 内実行に
     変えると、エージェントが共有 hooks / config に書いた内容をホスト側の git 実行が
     踏むエスケープ経路が開く。job 専用 `.git` を持って初めて
     「hooks が動いても檻の中」が成立する
   - これで git builtin の引数ポリシー・`core.hooksPath=/dev/null`・
     brokered git の `-C`/`-u` 拒否 (ローカルテスト赤の既知要因)・
     remote snapshot 機構 (token 登録時に remote を捕捉する allowlist)・
     peer 参照の brokered `clone --local` (workspace peer 節参照) が全て退役。
     「project → 上流 URL」は boid が明示マッピングとして持つ形になる
     (SSH URL → HTTPS URL の正規化込み)
   - 事前準備: 個人 token のセットアップ (token 戦略の節参照) と、
     リモート無し project の棚卸し (登録不可になるため該当の有無を確認)
   - 併せてホスト側 worktree 読者 (未解決論点参照) の置換が必須作業になる
3. **boid CLI のリモート接続** (独立トラック・1–2 と並行可、2026-07-07 追加):
   CLI を TCP + device auth クライアントに寄せ、リモート attach を
   WS endpoint に統一する (目標アーキテクチャの節参照)。
   現行ホスト daemon のまま実装・dogfood でき、daemon のコンテナ化・別ホスト化
   (ステップ 4 以降) の時点で CLI 経路の断絶が起きない状態を先に作る。
   これも契約先行の一例: CLI⇄daemon の契約を「同一ホスト UNIX ソケット特権」から
   「認証付きネットワーククライアント」に移しておく
4. sandbox backend の interface 化 + container backend (docker compose) を並走追加。
   契約は 1–3 で移行済みのため**純粋な enforcement 差し替え**になり、
   e2e シナリオも新契約を踏んだ状態で迎えられる
5. egress / docker proxy の broker 側再配置、k8s backend (operator パターン)
6. 将来拡張: リモートランナー (pull 型常駐 agent)

各段階で現行 backend に戻れる状態を維持する。

### 決定: clone の置き場所と reopen 意味論

ローカルでも clone は **job スコープの一時領域 (sandbox 内 tmpfs 等)** に作る
(2026-07-06 nose)。ホスト側永続ディレクトリに置けば現行の
「worktree は reopen をまたいで残る」意味論を保てるが、それはローカルだけ
コンテナ版と違う契約を作ることになり、コンテナ移行時にまとめて壊れる依存を残す。
コンテナ版は working dir を volume にすると性能劣化が激しいため
コンテナローカルディスク前提であり、契約先行の趣旨からローカルも同じ意味論に揃える。

- **reopen = 再 clone + branch checkout。保証は commit (+push) 済みのみ**。
  これは「push しない限りローカル変更は他セッションに共有されない」という
  根本の意味論変更 (git gateway 節参照) の帰結。
  未コミットの作業は job 異常終了で失われる (コンテナ版と同一の契約)。
  現行 dev フローは done 前に push するため正常系の成果は origin 経由で回収でき、
  task ask の Q&A は job が生きたまま行われるため reopen 自体を要しない
- **一時領域の実体はホスト側 runtime dir (`runtimes/<runtime_id>/` 配下) の
  bind mount を既定とする** (2026-07-08 決定)。tmpfs は既定にしない
  (working tree + ビルド生成物が RAM に乗るため)。契約は従来どおり
  「job スコープ・ephemeral・ローカル I/O」で変わらず、runtime dir なら
  既存 GC (24h 周期・30 日) にそのまま乗り、job 異常終了の消し忘れも自動回収される
  (コンテナ版の実体も overlayfs = ディスクであり、意味論は同一)
- **runtime dir 配下の clone はホストから読めるが診断専用** (runner-state.json と同格)。
  ホスト側の機能コードがこの path を読むことは禁止 — 依存を許すと
  「ホスト側 worktree 読者」を再生産し、コンテナ移行時にまとめて壊れる
- **sandbox 内の mount 先は固定の中立 path** (`/workspace` 等) にする。
  現行の Source==Target (ホスト path をそのまま sandbox 内に見せる) をやめ、
  コンテナ版と同じ path 契約を userns backend の時点で先に踏んでおく (契約先行)
