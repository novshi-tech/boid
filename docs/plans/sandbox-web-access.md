# サンドボックス内 Web アクセス（WebFetch / WebSearch 代替）

## 背景・問題

サンドボックスは全 egress を `HTTPS_PROXY=http://10.0.2.2:<port>`（`internal/sandbox/proxy.go`）に通し、
プロキシは allowlist 方式（`cmd/start.go: defaultAllowedDomains()` + `config.yaml: sandbox.allowed_domains`）で
リスト外を 403 で弾く。

Claude Code の WebFetch は**クライアント側実行**で標準の `HTTPS_PROXY` を尊重するようになった（最近の挙動）。
その結果、WebFetch のトラフィックがこの egress allowlist を通り、**allowlist 外ドメインが全部 403** になる。

- 検証: `pypi.org` / `files.pythonhosted.org`（allowlist）は WebFetch 成功、`example.com` / `github.com`（非 allowlist）は 403。
  WebFetch の成否が boid ローカルの allowlist と完全一致 → WebFetch がローカルプロキシ経由で動いている動かぬ証拠。
- boid 側のネットワークコードは無変更（allowlist 最終更新 2026-04-03、proxy env 注入は initial commit から）。変わったのは WebFetch 側。

### なぜ単純な「全許可」にしないか

「WebFetch を通すなら curl も同じだから全許可で良い」は誤り。
WebFetch は **GET のみ**で、外に出せるのは URL クエリに乗る数 KB だけ・ログにも残る。
curl は任意 POST ボディでメガ単位を送れる。exfiltration チャンネルとしての太さが桁違い。
最優先で避けたいのは「機密情報の不用意な外部送信」なので、**太いチャンネル（POST/大量アップロード）は
allowlist に限定したまま、細いチャンネル（GET/低帯域）だけ全ドメインに開く**のが正しい姿。

プロキシ層で WebFetch と curl を区別する手段は無い（ツール別 proxy 不可・HTTPS CONNECT の中身は不可視・
UA は TLS トンネル内で proxy から見えない）。よって区別はプロキシではなく**アプリ層（boid 仲介）**で行う。

## 方針

2 段構え。

### 案 1（即時・止血）: サンドボックスで WebFetch を無効化

WebFetch は今でもサンドボックスでは 403 で実質壊れている。これを「明示的に無効化」して、
エージェントが 403 で無駄ターンを踏まないようにする（UX 改善。セキュリティ境界は引き続き egress allowlist）。

- 機構: Claude Code の permission deny ルール。**裸のツール名 `WebFetch`（`--disallowedTools WebFetch`）で
  ツールがエージェントのコンテキストから完全に消える**（公式ドキュメント確認済み）。deny はどの設定階層でも最優先で、
  `--permission-mode bypassPermissions` 下でも効く（bypassPermissions はプロンプトを飛ばすだけで deny を無視しない）。
- 差し込み口（確定）: **boid-kits の `claude-code` kit `hooks/run-agent.py` の `build_claude_args` に
  `--disallowedTools WebFetch` を追加**する。
  - 理由: kit は `kit.yaml` でホストの `${HOME}/.claude` を **`mode: rw`** で bind mount している。
    よって boid daemon 側が `~/.claude/settings.json` を書くと**ホストの本物の設定を破壊する**ため不可。
    `/etc/claude-code/managed-settings.json`（衝突なし・最優先）は read-only rbind の `/etc` に書けず非 root では不可。
    起動 argv を握っているのは kit なので、kit 側フラグが唯一クリーンな差し込み口。
  - 効果範囲: サンドボックスの claude 起動のみ（executor / supervisor / discuss）。ホストの普段使い claude は無傷。
  - デプロイ: boid-kits 側の独立 PR（kit-first デプロイ）。boid daemon 側のコード変更は不要。
- 縮退安全性: 万一フラグが効かなくても、egress allowlist が引き続き 403 を返すだけで悪化しない。

### WebSearch は対応不要（ネイティブで動く）

WebSearch は WebFetch と違い**サーバ側実行**（検索は Anthropic 側で行われ、結果は allowlist 済みの
`api.anthropic.com` 経由で返る）。egress allowlist の影響を受けず、**サンドボックス内でもそのまま動く**
（2026-06-10 に実機で確認済み）。よって WebSearch の boid 実装・検索 API・鍵管理はすべて不要。

ただし WebSearch が返すのは「タイトル + URL + スニペット」まで。URL の**中身を読む**には WebFetch が要るが、
それは案 1 で無効化される。したがって **WebSearch（探す＝ネイティブ）→ boid fetch（読む＝案 2 で実装）** が
補完関係になる。案 2 で boid 側が新規に作るのは fetch のみ。

### 案 2（本命）: boid が WebFetch 相当（ページ読み取り）をホスト仲介で提供

fetch をホスト側で実行し、サンドボックスには結果だけ返す。boid 仲介なので GET 限定・URL ログ・
ボディ禁止を強制でき、案 1 の「GET-only 保証」を TLS を割らずにアプリ層で実現する（CA 不要・pinning 影響なし）。

#### アーキテクチャ

1. **ホスト側 fetch 実体**
   - `boid fetch <url>`: ホストで GET → HTML を markdown 変換して返す。GET 固定・ボディ不可・URL ログ・
     private/metadata レンジ拒否・任意の allowlist/denylist。
   - 実装形態の選択肢:
     - 軽い: `host_commands` に 1 エントリ追加（`run-e2e` と同じ path-match dispatch）。既存配線に乗る。
     - ちゃんと: builtin として実装（policy テーブル登録 + broker dispatch。`boid-add-builtin` スキル参照）。
   - 依存最小（CLAUDE.md 規約）: HTML→markdown は標準ライブラリ範囲か最小の手段で。

2. **サブエージェント・ラップ（トークン対策）**
   - 生ページを主エージェント（Opus）のコンテキストに流すとトークン爆食い。WebFetch が安いのは小モデル要約のため。
   - これを **サブエージェント**で再現する: 主エージェントがスキルを呼ぶ → サブエージェント（安いモデル）が
     `boid fetch` を実行し、生コンテンツを**自分のコンテキストで消費**して要約だけ主に返す。
   - boid 側で小モデル API を自前配線しなくて済む（おじさん案）。

3. **配布: 埋込スキルと同じ流れ**
   - `internal/skills/data/boid-web/`（仮）に SKILL.md を置き go:embed。
   - `boid start` 時に `internal/server/server.go` が `~/.local/share/boid/skills` へ展開（既存 `skills.DeployAll`）。
   - サンドボックスにマウント（既存スキルと同経路）。
   - WebFetch は案 1 で消えているので、エージェントは自然にこのスキル経由で web を取りに行く（CLAUDE.md / スキル説明で誘導）。

#### スコープ

- `boid fetch`（外部依存なし）+ サブエージェント・ラップ・スキル一式。WebSearch はネイティブで動くため対象外。

#### 未解決事項

- builtin か host_command か。
- HTML→markdown 変換の手段（依存最小の縛り）。
- サブエージェントに使わせる「安いモデル」の指定方法。
- 主エージェントへの誘導（WebFetch deny + スキル説明 + CLAUDE.md）。

## 利用実態（優先度判断）

- boid タスク内では web fetch はほぼ不要。
- 一方、Web UI のコマンド実行セッション（スマホから多用）での対話的な調べもの・計画立案では「まぁまぁ必要」。
- → まず案 1 で止血。案 2 は本計画として用意し、必要が顕在化したら Phase 1 から着手。
