# 4. フィードバックループ

このページでは、 PR ベースの開発タスクを最後まで通します。 AI エージェントがコードを書き、 GitHub に PR を作成し、 CI を待ち、結果に応じて修正を繰り返し、最後に auto-merge する、という一連のサイクルです。 [3. プロジェクトと拡張パッケージ (kit)](03-projects-and-kits.md) の続きです。

このチュートリアルが終わると、 `boid` の主用途である **AI による継続的な開発タスク自動化** の最小構成を 1 本通せたことになります。

## 前提

- [3. プロジェクトと拡張パッケージ (kit)](03-projects-and-kits.md) の手順で `boid kit install github.com/novshi-tech/boid-kits` 済み
- `claude` CLI がサインイン済みで PATH 上にある
- `gh` CLI が `gh auth login` 済み
- 「捨ててよい」 GitHub リポジトリが 1 つあり、 `git clone` 済み (例: `~/src/github.com/<you>/boid-demo-repo`)
- そのリポジトリで GitHub Actions が CI として何か走るとなお良い (無くても動作確認は可能)

## 仕掛け

`dev` という behavior を定義し、次の kit を組み合わせます。

| kit | 役割 |
|---|---|
| `claude-code` | `executing` / `reworking` で Claude Code エージェントを動かす hook |
| `github-cli` | サンドボックスから `gh` を使えるようにする |
| `github-pr-verification` | `verifying` で PR の CI 結果を取り込む gate |
| `github-auto-merge` | `done` への entry gate で `gh pr merge` を実行 |

加えて `worktree: true` を指定し、タスクごとに専用の git worktree (新ブランチ) を作って実装作業をそこに閉じ込めます。

## project.yaml を書く

`~/src/github.com/<you>/boid-demo-repo/.boid/project.yaml` を以下の内容で作成します。

```yaml
id: boid-demo
name: boid demo repo

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  dev:
    name: dev
    worktree: true
    kits:
      - github.com/novshi-tech/boid-kits/github-pr-verification
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instructions:
      main:
        type: execution
        consumer: claude-code
        message: |
          task の title と description に書かれた内容を実装してください。
          コミット → push → PR 作成 → CI 完了待ちまで行い、結果を
          verification.findings に書き込んでください。
            成功時:
              echo '{"verification":{"findings":[{"status":"resolved","message":"CI passed"}]}}' \
                | boid task update <task_id> --payload-file -
            失敗時 (status を open にして message に失敗ログ抜粋を入れる):
              echo '{"verification":{"findings":[{"status":"open","message":"CI failed: ..."}]}}' \
                | boid task update <task_id> --payload-file -
      rework:
        type: rework
        consumer: claude-code
        message: |
          verification.findings に書かれた指摘を解消してください。
          手順は main と同様。
```

`worktree: true` がカギで、 `boid` はこのタスクのために専用の git worktree を新ブランチで作り、 hook の作業場所をその worktree に閉じます。並列で別の dev タスクを走らせても干渉しません。

```bash
cd ~/src/github.com/<you>/boid-demo-repo
boid project add .
```

## タスクを作って観察する

例えば README に 1 行追記するタスクを作ります。

```bash
boid task create <<'YAML'
project_id: boid-demo
title: Add a one-line "hello from boid" to README.md
behavior: dev
auto_start: true
YAML
```

別ターミナルで状態を流し見ます。

```bash
boid task watch <task-id>
```

期待される流れ:

1. `pending → executing` (auto_start による)
2. `executing` で claude-code hook が起動。 worktree 内で README を編集 → commit → push → `gh pr create` で PR を出す
3. claude が `verification.findings` に `status: resolved` の finding を書き込む (CI が通った場合)
4. `executing → verifying` (artifact ではなく findings が書かれることで完了を表現)
5. `verifying` で `github-pr-verification` gate が PR の最終的な mergeable 状態を確認
6. 何も問題なければ `verifying → done`
7. `done` への entry で `github-auto-merge` の gate が `gh pr merge` を実行

最終状態を確認:

```bash
boid task show <task-id>
```

PR がマージされ、 worktree が片付けられているはずです。

## CI が落ちたとき (rework サイクル)

CI が失敗するように小さな変更 (テストを 1 つ壊す等) を加えてタスクを作り直すと、上の流れの 3〜5 が以下のように分岐します:

3'. claude が `verification.findings` に `status: open` の finding を書き込む
4'. `executing → verifying` (上と同じ)
5'. `verifying` の gate が finding を見て `verifying → reworking` に戻す
6'. `reworking` で claude-code の rework hook が起動 (`default_instructions.rework` が渡る)。 finding に書かれた失敗ログを読んで修正、再度 commit / push / CI 待ち
7'. CI が通れば finding が `resolved` で書き直され、 `reworking → verifying → done` と進む

`boid task watch <task-id>` でこの行ったり来たりが見えます。修正回数が `state_machine.rework_limit` (既定 5) を超えると自動 abort します。

## まとめ

このチュートリアルで通したサイクルは:

- `worktree: true` でタスクごとに別ブランチ・別ディレクトリを切り、並列実行を干渉なしに
- `claude-code` hook が executing で実装、 reworking で修正
- `github-pr-verification` gate が verifying で CI 状態を payload に取り込む
- `github-auto-merge` gate が done への entry で PR をマージ

これで `boid` の中核となる「AI による開発タスクの自走」 の最小構成を体験しました。

## 次に読むもの

- [概念](../guide/concepts.md) — 出てきた用語の改めての定義
- [状態機械](../guide/state-machine.md) — `verifying ↔ reworking` の遷移条件
- [Web UI](../guide/web-ui.md) — このタスクをブラウザ / スマホから観察するために
- [トラブルシューティング](../guide/troubleshooting.md) — 詰まった時に
