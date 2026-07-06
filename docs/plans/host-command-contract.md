# host command の契約締め (container-based-boid 移行戦略 1)

ステータス: 実装中
作成日: 2026-07-06
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略 1

---

## 背景

コンテナ基盤移行 (contract-first 方式) の第一歩。enforcement の差し替えより先に、
host command の境界契約を現行 userns backend 上でコンテナモデルに合わせて dogfood する。

コンテナモデルでは broker とサンドボックスが別ホストになり得るため、
以下の暗黙の前提が成立しなくなる:

- **stdin 伝搬**: shim が読んだ stdin を broker 側プロセスに流し込む
- **cwd 伝搬**: host command が project/worktree の checkout ディレクトリで実行される
  (cwd から repo を推定する `gh pr create` 等がこれに依存)
- **ファイルパス引数**: サンドボックス内のパスを host 側プロセスが読める
  (`gh pr create --body-file <sandbox path>` が既知の踏み抜きポイント)

## 契約 (決定事項, 2026-07-06 nose)

1. **stdin は渡らない** — shim は stdin を読まず、broker は受け取っても捨てる。
   `stdin: true` 設定は deprecated (parse は通す + 警告ログ、無効)。
   実運用の利用者はゼロ (テストのみ) で実害なし
2. **host command は repo checkout に依存しない** — cwd は中立ディレクトリ
   (`os.TempDir()`) 固定。token の worktree/project dir を cwd に使う優遇を廃止。
   path 宣言系 (run-e2e 等) も例外にしない — 「ホスト checkout 依存コマンドは
   コンテナ移行後は使えない」制約を受け入れる (e2e/run.sh は `SCRIPT_DIR` 起点で
   cwd 非依存のため現時点の実害なし)
3. **repo 文脈は env 注入で与える** — `-R` 強制ではなく、per-command `env:` に
   コンテキスト変数 `${boid:repo_slug}` (token 登録時に project の origin remote から
   `host/owner/repo` 形式で導出) を導入。gh は `GH_REPO: ${boid:repo_slug}` で
   従来どおり透過的に使える。git builtin の remote snapshot と同じ前例。
   汎用機構であり gh 特別扱いにはしない
4. **非サポート引数はコマンド毎設定で明示し、代替案内付きで拒否する** —
   `reject` ルール (glob match + reason)。shim が早期拒否 (UX)、broker が
   権威として同一ルールを enforcement (境界は broker)。黙って壊れるのが
   エージェントには一番高くつくため、reason (代替手段の案内) を必須とする

### 語彙

```yaml
host_commands:
  gh:
    allow: [pr, issue]
    reject:
      - match: "*--body-file*"    # allow/deny と同じ glob 意味論、joined args に対して
        reason: 'サンドボックスのファイルパスは host から見えない。--body "$(cat <file>)" を使う'
    env:
      GH_REPO: "${boid:repo_slug}"
```

## 実装分割

| PR | 内容 |
|---|---|
| 1 | reject 語彙 + loader/transport (両 CommandDef ミラー + 変換 seam)、stdin deprecation 警告。挙動変更なし |
| 2 | broker enforcement: stdin 全廃、reject ルール拒否、非 streaming / streaming の重複ゲートを共通 pre-exec ゲートに統合 |
| 3 | cwd 中立化 + `${boid:repo_slug}` 展開 (ペア必須: cwd を切ると gh が repo を見失う) |
| 4 | shim 早期拒否: `BOID_HOST_COMMAND_RULES` env 注入 + shim ローカル reject |
| 5 | environment.yaml への reject 表面化、notes 更新、boid-sandbox-configure gh テンプレート、ユーザ docs |

## 運用ノート

- 既存マシンの kit.yaml (gh エントリ) への reject ルール + `GH_REPO` env の追加は手動
  (テンプレートは PR5 で更新されるが、生成済み kit.yaml には反映されない)
- 移行戦略 2 (git gateway + sandbox 内 clone) 以降は container-based-boid.md を参照
