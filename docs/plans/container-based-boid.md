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

既存部品候補: `git http-backend` (git 同梱の smart HTTP サーバ)、
nginx 等での認証ヘッダ注入、[FINOS git-proxy](https://github.com/finos/git-proxy)
(push をポリシー審査するプロキシ)。

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

### workspace コンテナ定義: devcontainer spec の採用検討

job コンテナのイメージ定義に [devcontainer spec](https://containers.dev/) の採用を検討する。
ツール provisioning を devcontainer features に委譲できれば、
現行 kit が担う仕事の一部を外部標準に置き換えられる可能性がある。
boid の役割は「セキュリティ外皮 (egress proxy / broker / git gateway / docker proxy) +
オーケストレーション」に純化していく。

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
- **host_commands の path-match 系** (run-e2e 等): broker RPC 経由で成立する見込みだが、
  「ホスト」概念が消える k8s での扱い (専用 runner Pod?) は詰めていない
- **subscription key と ToS**: チーム共有では Claude subscription キーは利用規約上使えない見込み。
  API key / enterprise 前提
- **ハーネス周辺の細部**: session jsonl 永続化 (env strip)、embedded skills bind、
  IS_SANDBOX 等、現行 adapter が吸収している既知の癖のコンテナ環境での再検証
- **dispatch レイテンシ実測**: 「イメージ pull 済みなら十分速い」の裏取り

---

## 移行戦略

1. sandbox backend の interface 化 (現行 userns backend をそのまま 1 実装に)
2. container backend (docker compose) を並走で追加、ローカルで dogfood
3. git gateway / egress / docker proxy の broker 側配置
4. k8s backend (operator パターン)
5. 将来拡張: リモートランナー (pull 型常駐 agent)

各段階で現行 backend に戻れる状態を維持する。
