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

既存部品候補: `git http-backend` (git 同梱の smart HTTP サーバ)、
nginx 等での認証ヘッダ注入、[FINOS git-proxy](https://github.com/finos/git-proxy)
(push をポリシー審査するプロキシ)。

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
git clone --reference /mnt/mirror.git file:///mnt/mirror.git /workspace
git -C /workspace remote set-url origin http://broker.local/novshi-tech/boid.git  # gateway に向ける
```

- mirror は read-only マウント。fetch/push は clone 後に gateway 経由
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
  ホスト側から worktree パスを直接読む機能 (web UI の diff 表示・gc の worktree 掃除・
  peer branch 参照の `clone --local` 等) を git データを origin/gateway 経由で読む形に
  置換する必要がある。対象の全量棚卸しが未実施
- **git gateway の実装詳細**: smart HTTP プロトコルの転送、`report-status` の扱い
- **認証付き CLI の broker RPC 設計**: stdin / ファイル引数の転送方式、
  repo コンテキストの伝搬 (`-R` 強制 or RPC 伝搬)、許可コマンド集合と引数ポリシーの粒度
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
2. **git gateway + サンドボックス内 clone** (ワンセットで切替):
   - git builtin を廃止し、sandbox 内の素の git + gateway (認証注入) に置換
   - worktree bind mount をやめ、job ごとに sandbox 内の一時領域に
     `git clone --reference <ホスト repo>`
   - **ワンセットの理由 (セキュリティ)**: 共有 `.git` のまま git を sandbox 内実行に
     変えると、エージェントが共有 hooks / config に書いた内容をホスト側の git 実行が
     踏むエスケープ経路が開く。job 専用 `.git` を持って初めて
     「hooks が動いても檻の中」が成立する
   - これで git builtin の引数ポリシー・`core.hooksPath=/dev/null`・
     brokered git の `-C`/`-u` 拒否 (ローカルテスト赤の既知要因) が全て退役
   - 併せてホスト側 worktree 読者 (未解決論点参照) の置換が必須作業になる
3. sandbox backend の interface 化 + container backend (docker compose) を並走追加。
   契約は 1–2 で移行済みのため**純粋な enforcement 差し替え**になり、
   e2e シナリオも新契約を踏んだ状態で迎えられる
4. egress / docker proxy の broker 側再配置、k8s backend (operator パターン)
5. 将来拡張: リモートランナー (pull 型常駐 agent)

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
- **tmpfs は実装詳細で、契約は「job スコープ・ephemeral・ローカル I/O」**。
  tmpfs は working tree + ビルド生成物が RAM に乗るため、大規模 repo では
  disk-backed の一時領域 (runtime dir 等) への fallback を許容する
  (コンテナ版の実体も overlayfs = ディスクであり、tmpfs 固定は契約上の要請ではない)
