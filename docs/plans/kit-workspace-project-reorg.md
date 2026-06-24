# kit / workspace / project 構成の再編

ステータス: ドラフト (レビュー反映済 / 2026-06-24)

## 背景と動機

現状の boid では、再利用可能であるべき `project.yaml` と `kit.yaml` に環境固有情報が混入している。

- `project.yaml` は git にコミットされる。にもかかわらず `host_commands` / `additional_bindings` / `env` / `kits` / `secret_namespace` を持ち、これらの多くはマシン固有。同僚と共有したとき環境差で壊れる。
- `project.yaml` で `volta` kit を指定すると volta の使用が強制される。プロジェクトとしては「node が使えれば十分」なのに、node の供給手段 (volta / nvm / system) という**環境判断をプロジェクトに焼き付けて**しまう。

根本原因は、project が「何をするか (task_behavior)」と「この環境でどう動かすか (kit / env / binding)」を 1 ファイルに混ぜていること。前者は portable、後者は machine-local。これを分離する。

設計方針は「環境固有の結合を*消す*」のではなく、「**commit される側 (project) から uncommitted な machine-local 側 (workspace / kit) へ追い出す**」こと。結合先が workspace に移る時点で、git に環境情報が乗らなくなり、volta 強制も消える (workspace.yaml が volta を名指しするのは「強制」ではなく、そのマシンの持ち主の選択であるため)。capability の provides/requires 抽象は導入しない (実装軽量化を優先)。

## 再編後の責務

| レイヤ | 所在 | git | 持つもの |
|---|---|---|---|
| **project** | `.boid/project.yaml` | ✅ commit | id, name, task_behaviors, worktree / base_branch / fork_point |
| **workspace** | machine-local `workspace.yaml` | ❌ | 有効 kit の名指し列挙, env (plain のみ), secret namespace 設定, docker capability |
| **kit** | machine-local (`boid init workspace` で生成) | ❌ | host_commands (パス込み), additional_bindings, env。再利用前提を捨てる |
| **config.yaml** | `~/.config/boid/` | ❌ | gc, web, notify, sandbox.allowed_domains (daemon 全体・据え置き) |
| **secret** | DB (AES-GCM) | ❌ | namespace 単位 = workspace。host_command にのみ `secret:` で注入 (堅め) |
| **project↔workspace 紐付け** | DB `project_workspaces` (流用) | ❌ | project_id → workspace_id (N:1) |

## 各レイヤ詳細

### project (`.boid/project.yaml`)

git commit 対象。環境固有の語彙を一切持たない。

- **残す**: `id`, `name`, `task_behaviors` (readonly / traits / default_instruction / hooks), `worktree`, `base_branch`, `fork_point`
  - worktree / base_branch / fork_point は VCS 構造の話で移植可能なため project 残留。
- **削除 (→ 移行先)**:
  - `kits` → workspace
  - `env` → workspace
  - `host_commands` → kit
  - `additional_bindings` → kit
  - `secret_namespace` → workspace (namespace = workspace_id に統一)
  - `capabilities.docker` → workspace

task_behavior の hook が `gh` / `node` 等を暗黙に要求しても、project はそれを宣言しない (capability 契約を入れないため)。供給は workspace / kit の責務。要求と供給のミスマッチは runtime で顕在化する (事前検出・診断強化は別テーマ → スコープ外 C)。

### workspace (machine-local `workspace.yaml`)

git 管理外。今回の再編の中核。既存の workspace は DB のグルーピング (`project_workspaces`) だけを持つガワだったが、ここに設定の実体を載せる。

- **場所**: `~/.config/boid/workspaces/<workspace_id>.yaml` (workspace_id は `boid workspace assign` で使う既存の人間可読 slug。UUID 採番ではない)
- **持つもの**:
  - `kits`: 有効 kit の名指し列挙。kit を直接名指しする (capability 抽象なし)。machine-local なので volta / nvm の選択はここで完結する。
  - `env`: plain な環境変数 (非 secret)
  - secret 設定: namespace = workspace_id。**どの secret をどの host_command に渡すか**を宣言する。env への自動展開はしない (堅め)。
  - docker capability
- **project との紐付け**: DB の `project_workspaces` (project_id → workspace_id) を流用 (決定 A = a)。workspace.yaml は `workspace_id` をキーにした設定本体を持ち、紐付け自体は DB が持つ。編集対象 (kit / env) は yaml で触りやすく、機械的な ID 紐付けは DB に置く住み分け。紐付けは既存コマンド `boid workspace assign <project> <ws>` を流用する (新規 project 登録時は `boid project add --workspace <slug>` でも紐付く)。`project assign-workspace` のような新動詞は作らず、`workspace assign` に一本化する。

### kit (machine-local, `boid init workspace` の kit 生成フェーズで生成)

boid-kits リポジトリは**廃止** (決定 D)。kit は各マシンでローカル authored。

- **持つもの**: host_commands (パス込み), additional_bindings, env
- **置き場**: `~/.local/share/boid/kits/<name>/kit.yaml` (現状踏襲。ただし git pull 由来ではなくローカル生成)
- **専有・独立ファイル**: kit は再利用・共有を前提にしない (専有) が、物理的には workspace.yaml とは別の独立ファイルとして存在する (決定 ①)。「専有か共有か」と「inline か独立ファイルか」は直交する軸であり、本設計は「専有 + 独立ファイル」を採る。
- **旧 boid-kits の kit (go-dev / github-cli / volta 等)** は `boid init workspace` の**生成スキルに埋め込まれたリファレンス**に変化する。生成スキルが環境を検出し、リファレンスを雛形にして各マシン用の kit.yaml を生成する。

**検出シグナル (capability 契約を入れない代償)**: project は「node が要る」を宣言しない (前述のとおり capability の provides / requires を導入しないため)。よって `init workspace` の生成スキルは **project の repo 中身と task_behavior の hook を読んで必要ツールを推測する**ヒューリスティックで「足りない kit」を決める。例: `package.json` / `pnpm-lock.yaml` → node (供給手段は volta / nvm / system からマシン所有者が選ぶ)、`go.mod` → go、`*.csproj` → dotnet、hook script 内の `gh` / `docker` 呼び出し → github-cli / docker kit。検出は網羅性を保証しない (ヒューリスティック)。漏れた要求は runtime で顕在化する (事前検出はスコープ外 C)。生成スキルは差分追加なので、漏れても後から再実行して補える。

### secret (DB, AES-GCM)

- namespace = workspace_id。
- **粒度のトレードオフ (project → workspace へ格上げ)**: 旧 `secret_namespace` は project 単位で設定できたが、本設計では namespace = workspace_id に固定する。よって**同一 workspace の全 project が同じ secret namespace を共有する**。secret を分離したい場合は workspace を分ける (= 環境境界と secret 境界を一致させる)。粒度は粗くなるが、「環境を共有する project は資格情報も共有する」という素直なモデルを優先する。
- host_command にのみ `secret:` プレフィックスで注入 (現行 `broker.go` の `RegisterWithSecrets` 経路をそのまま使う)。
- **env への自動展開はしない** (堅め)。当初プロンプトにあった「namespace の secret を自動的に env 有効化」案は撤回。理由: 全 secret がエージェント本体と全子プロセスに露出すると、commit / PR / log 経由の漏洩面が広がり、自律エージェントに対する blast radius が過大になるため。workspace で定義できる `env` は plain な値に限り、secret 由来の値は必要な host_command にしか渡さない。

### config.yaml (据え置き)

`gc` / `web` / `notify` / `sandbox.allowed_domains`。daemon 全体のマシン設定。今回は変更しない。workspace 単位の egress allowlist (スコープ外 E) は保留。

## 初期化フローと init 専用 sandbox

### コマンドの役割分担

| コマンド | 役割 | エージェント |
|---|---|---|
| `boid init env` | 新規マシンのオンボーディング入口。daemon 確認 + 最初の `project add` を案内し、内部で `init workspace` を呼ぶ高レベルラッパー。kit 生成そのものは持たない (`init workspace` へ委譲) | あり |
| `boid project add <path> [--workspace <slug>]` | project 登録 (+ `--workspace` 指定時は紐付けも)。軽量・機械的。未知 slug は get-or-create。既存 project の紐付け変更は `boid workspace assign <project> <slug>` | なし |
| `boid init workspace <slug>` | **kit 生成 + workspace 設定の唯一の実体**。workspace に属する project 群をスキャンし、足りない kit を生成し workspace 設定を整える。べき等 (差分追加) | あり |

project を複数足してから `init workspace` を 1 回叩けばまとめて整合する。新規 project 追加後も再度叩けば差分だけ補う。「空の workspace を先に作る」フローは持たない (空では何を有効化すべきか決められないため)。workspace は project を足して `init workspace` を叩くことで育つ。

**kit 生成の所在を一点に固定する**: kit を生成するのは常に `init workspace` (の kit 生成フェーズ) だけ。`init env` はマシン初回のオンボーディングを案内し、内部で `init workspace` を呼ぶラッパーに徹する。両者が kit 生成を別々に主張しないことで、「kit はどこで作られるか」の答えが一意になる。

**紐付けと設定生成の間に窓がある**: `project add --workspace` は DB の `project_workspaces` 紐付けだけを書き、`workspace.yaml` は `init workspace` を叩くまで生成されない。その窓で task が走ると kit / env 無しの degraded 状態になる (落ちはしない)。実運用では `project add` → `init workspace` を続けて叩く想定。

### 2 フェーズ (基本は単一 sandbox 内で順次)

`init workspace` は内部で 2 フェーズに分かれ、フェーズごとに**書き込み先が異なる** (決定 ①)。

1. **kit 生成フェーズ**: 環境を検出 (read) し、必要な kit を独立ファイルとして生成。書き込み先 = 当該 kit ファイル (`~/.local/share/boid/kits/<name>/kit.yaml`)。
2. **workspace 設定フェーズ**: 生成した kit を workspace.yaml に有効化登録し、env を整える。書き込み先 = `workspace.yaml`。

**基本線は単一 init sandbox 内で 2 フェーズを順に実行**し、write allowlist をフェーズ境界で切り替える (kit 生成中 = 当該 kit ファイル / workspace 設定中 = `workspace.yaml`)。物理的に別 sandbox プロセスへ割るのは「フェーズ間で write allowlist が決して重ならない」ことを強制したい場合のオプションとして残すが、デフォルトは単一セッション (フェーズ間の state 受け渡しコストを避ける)。

### init 専用 sandbox プロファイル

通常タスク sandbox とは別の特別プロファイル。「検出 (read) は自由・漏出 (write/exfil) を塞ぐ」設計。

- **read**: 全ファイルシステム可。理由 = 開発ツール (volta / anyenv 系 / uv / nuget / go 等) が `$HOME` 以下に散在し、機密ディレクトリだけを除外するのが困難なため、read 範囲は絞らない (決定 ②)。
- **write**: フェーズごとの allowlist のみ (kit 生成フェーズ = 当該 kit ファイル / workspace 設定フェーズ = `workspace.yaml`)。
- **host_command**: 全無効。host_command は egress allowlist を経由しない host 直実行 (gh / systemctl 等) であり、read した機密の exfil 経路になりうるため塞ぐ (決定 ③)。
- **egress**: 通常の allowlist (HTTPS_PROXY)。sandbox 内からの直接通信はここで制限される。
- sandbox 内でのコマンド実行 (`node --version` 等) は通常通り可能。host_command 無効は sandbox 内実行をブロックしない (決定 ③)。よって検出に支障はない。

機密の exfil 防御は出口を**三段**で考える: (1) sandbox 内通信 = egress allowlist で遮断、(2) host 直実行 = host_command 無効で遮断、(3) **model context = read した内容はエージェントのコンテキストに乗り LLM プロバイダへ送られる**。(3) はハーネス自身の API 接続であり sandbox の egress proxy を通らないため、機構では遮断できない。read 範囲を全 FS に広げる以上 (3) は構造的に開いたままになる。

(1)(2) は機構で塞げるが、(3) と残る間接経路 (read した機密を `workspace.yaml` に書く) は**エージェント自身のチェック (self-policing)** に依存する。さらに守備範囲を狭めるため、**`workspace.yaml` / `kit.yaml` は通常の task sandbox には mount しない** (task には dispatcher が解決済みの env 値 / kit binding だけを spec に焼いて渡す。現行どおり)。よって万一 secret が `workspace.yaml` に混入しても、後続の task エージェントがそれを読んで commit する経路は存在せず、self-policing が効けばよい範囲は init sandbox 内に閉じる。これらは悪意エージェントには効かないが、想定脅威 (信頼されたユーザの環境セットアップ中のうっかり混入) には十分とする。

### sandbox 拡張 (決定 ④ — 調査で前提が判明)

**朗報: sandbox 層は既に task / command の model から独立している** (調査済み)。`internal/sandbox` は `orchestrator` を import せず (`broker_test.go:17-22` が layer independence を明言)、`sandbox.Spec` (`internal/sandbox/spec.go:12-74`) は readonly フラグも TaskID も持たない中立な spec。`task.readonly` → `Visibility.Writable` → `Mount.ReadOnly` の翻訳は dispatcher 層 (`planner.go:93`, `sandbox_builder.go:432,446`) が担っている。よって**「密結合を解きほぐす」大リファクタは不要**。

新プロファイルの表現に必要なのは sandbox.Spec の**拡張のみ** (規模: 中、むしろ小寄り):

| 項目 | 現状 | 必要な変更 |
|---|---|---|
| read 全 FS | base dirs (`/bin` `/usr` `/etc` 等) のみマウント | `Spec` に read 範囲フラグ + `BuildPlan` (`internal/sandbox/plan.go:25-37`) に host root を ro で rbind する分岐 |
| write 特定ファイルのみ | project 全体を `Writable` フラグで一括 rw/ro | per-file write allowlist を dispatcher の翻訳に追加 (既存の `IsFile` binding + `Mode:"rw"` を活用) |
| host_command 全無効 | `spec.HostCommands` が空なら register されない | 実質**フラグ不要** — init sandbox に host_command を渡さなければ達成。明示フラグを足すなら `Spec` に 1 つ |
| egress allowlist | 既存 (`ProxyPort` + `AllowedDomains`) | 再利用 |

**base dirs の ReadOnly 欠落 (調査で確定)**: 現状 `internal/sandbox/plan.go:29-37` は base system dir (`/bin` `/sbin` `/lib` `/lib64` `/usr` `/etc`) を `MountRBind` + `Slave` で bind するが **`ReadOnly` フラグを付けていない** (現在 ro 適用なし。実害は uid_map `0:1000:1` の DAC で root 所有ファイルへの書き込みが弾かれることで抑制されている)。init sandbox の「全 FS read-only」を本物にするには、この base mount に `ReadOnly` を足す必要がある。主題とは独立だが、同じプロファイル実装で同時に直す。

task / command 側の変更は最小 (新プロファイルを選ぶ syntax のみ)。既存設計 (orchestrator → dispatcher 翻訳 → 中立 sandbox.Spec) を踏襲する。

## 実装メモ

### 埋込スキル bind (検証済み: 決定 B)

Phase 3-e で**完了済み・追加作業不要**。

- 埋込スキル (`/boid-task` / `boid-orchestrate` / `boid-web`) は `internal/skills/deploy.go:12` で embed され、`internal/server/server.go:68` の `DeployAll` が `~/.local/share/boid/skills/<name>` に展開する。
- 各 adapter の `Bindings()` (`internal/adapters/claude/bindings.go:54` 他) が `~/.local/share/boid/skills/<name>` → `~/.claude/skills/<name>` を返す。claude / codex / opencode すべてこの経路。
- boid-kits への実装依存はゼロ (残存はコメントの歴史的言及のみ)。

**申し送り (重要)**: shell adapter (hook script 実行) だけは `Bindings()` が nil のため、まだ legacy kit binding 経路 (`internal/dispatcher/sandbox_builder.go:150-159` の `expandedBindings` + `Visibility.KitRoots`) に乗っている。kit が workspace 配下に移行する際、この経路が新しい供給元 (workspace が有効化した kit の host_commands / additional_bindings) を正しく受け取るよう配線を確認すること。落とすと hook script が sandbox に見えず task が死ぬ (PR #594 退行と同じ failure mode)。

### hook script の所在

task_behavior の hook は project に残る。ScriptPath を持つ hook (builtin / script) の実体配置先を確認する (project の `.boid/` 配下か)。agent hook は ScriptPath 不要 (Phase 3-e の fallback 合成)。

## マイグレーション (ハードカットオーバー)

同僚が既に boid を運用しているため、互換性を明示的に設計する。boid 既存の「削除キーは移行ガイダンス付きエラーにする」流儀 (`spec_loader.go` の `workspace_id` / `hooks` / `gates` 拒否) を踏襲する。

1. **project.yaml の 6 フィールドを「削除キー」化**。`ReadProjectMeta` で検出したらエラー + 移行ガイダンスを出す (例: `host_commands は workspace 配下の kit に移動しました。boid init env を実行してください`)。
2. **project.local.yaml を廃止** → workspace.yaml に集約。project.local.yaml が担っていた env / host_commands / additional_bindings / secret_namespace のローカルオーバーライドは、すべて workspace.yaml が引き継ぐ (granularity が project 単位から workspace 単位に格上げ)。
3. **移行ヘルパー**: 既存 project の env / kits / host_commands / additional_bindings を吸い上げて workspace.yaml の雛形を生成し、DB 紐付けまで行う `boid` サブコマンド (自動変換まで踏み込むか、ガイダンス出力に留めるかはオープンクエスチョン)。
4. **`boid init env`**: 新規マシンのオンボーディング入口。daemon 確認と最初の `project add` を案内し、内部で `init workspace` を呼んで kit + workspace.yaml を生成させる (生成の実体は `init workspace` の生成スキル = 決定 D)。

### 同僚影響と手順

- アップグレード後、既存 project.yaml が削除キー検出でエラーになり、起動を拒否する (黙って壊れるのではなく、明示的に止まる)。
- 救済経路: 移行ヘルパーで自動変換、または `boid init env` で workspace + kit を再生成。
- project.yaml の git diff は「6 フィールドが削除される」形になる (環境固有情報が剥がれる = 狙い通り)。

## スコープ外 (別テーマ)

- **C: セッション診断 / マルチエージェント構成の評価・最適化**。capability 要求と供給のミスマッチの事前検出を含む、エージェント実行中の問題の観測・診断・最適化。プロジェクト構成とセッション診断は独立した大きなテーマとして別途取り組む。
- **E: workspace 単位の egress allowlist**。便利だが proxy の管理が複雑化する懸念があり、見当がついてから検討。

## オープンクエスチョン

- ~~workspace の作成フロー~~ **解決済み**: `project add --workspace <slug>` が get-or-create で紐付け、`init workspace <slug>` が project 群をスキャンして kit/env を整える (上記「初期化フロー」参照)。`boid workspace create` は不要 (workspace は project を足して育てる)。
- ~~kit 生成フェーズと workspace 設定フェーズの sandbox 分割粒度~~ **解決済み**: 基本線は単一 init sandbox 内で 2 フェーズを順次実行し write allowlist を切り替える。別プロセス分割は allowlist 非重複を強制したい場合のオプション (上記「2 フェーズ」参照)。
- self-policing チェックの実装方法 (エージェントへの指示で足りるか、生成後に機密パターンを機械的に走査する後段を置くか)。
- ~~前提リファクタリング (決定 ④) の現状把握~~ **解決済み**: sandbox 層は既に独立 (上記「sandbox 拡張」参照)。密結合解きほぐしは不要、Spec 拡張のみ (中規模)。
- ~~base system dirs の ReadOnly 適用~~ **確定 (調査済み)**: `internal/sandbox/plan.go:29-37` は base dirs (`/bin` `/sbin` `/lib` `/lib64` `/usr` `/etc`) を `MountRBind` + `Slave` で bind するが `ReadOnly` を**付けていない**。実害は uid_map `0:1000:1` の DAC で抑制されているが、init sandbox の「全 FS read-only」実装時に `ReadOnly` を足す必要がある (上記「sandbox 拡張」参照)。
- 移行ヘルパーは自動変換まで踏み込むか、ガイダンス出力 (手動移行) に留めるか。
- ScriptPath を持つ hook の実体配置先 (project の `.boid/` 配下か)。
- shell adapter 経路 (`sandbox_builder.go:150-159`) の legacy kit binding を、workspace が有効化した kit の供給に繋ぐ具体配線。
