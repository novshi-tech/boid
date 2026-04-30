# 4. GitHub PR ベースの開発ワークフロー

このページでは、 GitHub PR ベースの開発ワークフローを最初から最後まで通して実行します。 AI エージェントが新しいブランチでコードを書き、 PR を作り、 CI 完了を待ち、結果を payload に書き戻し、最後に `boid` が PR を自動マージするところまでを 1 タスクで動かします。 [3. プロジェクトと拡張パッケージ (kit)](03-projects-and-kits.md) の続きです。

このチュートリアルが終わると、 `boid` の主用途の 1 つである **AI エージェントへの開発タスク委譲** の最小構成を 1 本通せたことになります。

## 前提

- [3. プロジェクトと拡張パッケージ (kit)](03-projects-and-kits.md) の手順で `boid kit install github.com/novshi-tech/boid-kits` 済み
- `claude` CLI がサインイン済みで PATH 上にある
- `gh` CLI が `gh auth login` 済み
- 「捨ててよい」 GitHub リポジトリが 1 つあり、 `git clone` 済み (例: `~/src/github.com/<you>/boid-demo-repo`)
- そのリポジトリで GitHub Actions が CI として何か走るとなお良い (無くても動作確認は可能)

## 役割分担

このチュートリアルで作る構成では、 ワークフローの大部分を **agent への instruction** が担います。 `boid` 本体は worktree の作成と最後の自動マージだけを引き受け、 commit / push / PR 作成 / CI 完了待ち / 結果の書き戻し は agent が行います。

| 担当 | 役割 |
|---|---|
| `boid` 本体 (`worktree: true`) | タスクごとに新ブランチで git worktree を作り、片付ける |
| `claude-code` kit (hook) | `executing` 状態で Claude Code エージェントを起動 |
| `github-cli` kit | サンドボックス内から `gh` を使えるようにする |
| **agent への instruction** | 編集 → commit → push → PR 作成 → CI 待ち → `verification.findings` に結果書き込み |
| `github-auto-merge` kit (gate) | `done` 状態への entry で `gh pr merge` を実行 |

instruction が大半の責務を持つことで、ワークフローの中身 — 何を確認するか、どこで止めるか、何をログに残すか — はプロジェクトごとにテキストで自由に調整できます。 PR 作成や CI 確認のための専用 kit を組まずに済むのは、この instruction 重視の構成によるものです。

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
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instructions:
      main:
        type: execution
        consumer: claude-code
        message: |
          task の title と description に書かれた内容を実装してください。
          コミット前にユニットテストの通過を確認すること。

          実装が完了したら以下を順次実行する:
          1. `git add` + `git commit` (worktree の現ブランチ上で)
          2. `git push -u origin HEAD` で current branch を origin に push
          3. 既存 PR の有無を確認:
             PR_URL=$(gh pr list --head "$(git branch --show-current)" \
                        --json url --jq '.[0].url // ""')
             空なら `gh pr create --title "<task title>" --body "<summary>"`
          4. `gh pr checks --watch --fail-fast` で CI 完了まで待機
             (CI の無いリポでは即抜ける)。失敗時は `gh run view --log-failed`
             でログを取得
          5. 結果を verification.findings に書き戻す:
             成功時:
               echo '{"verification":{"findings":[
                 {"status":"resolved","message":"CI passed: '"$PR_URL"'"}
               ]}}' | boid task update <task_id> --payload-file -
             失敗時 (status は open、 message に失敗 job の概要とログ末尾を入れる):
               echo '{"verification":{"findings":[
                 {"status":"open","message":"CI failed: <jobs>\n<tail>"}
               ]}}' | boid task update <task_id> --payload-file -
      rework:
        type: rework
        consumer: claude-code
        message: |
          verification.findings に書かれた指摘を解消してください。
          手順は main と同じ (commit → push → CI 待ち → finding 書き戻し)。
          PR は既存 PR を再利用 (`gh pr list --head` で確認)。
```

ポイント:

- **トップレベルの `kits:`** で project 全体で使う kit を読み込む (`claude-code` と `github-cli`)
- **`task_behaviors.dev`** が今回の主役
  - `worktree: true` でタスクごとに別ブランチ・別ディレクトリ
  - kit リストには `github-auto-merge` だけ。 PR 作成と CI 確認は instruction 側
- **`default_instructions.main`** が `executing` で claude-code に渡される。 commit / push / PR 作成 / CI 待ち / 結果記録 までの手順を全部ここに書く
- **`default_instructions.rework`** は `reworking` で claude-code に渡される修正用 instruction

```bash
cd ~/src/github.com/<you>/boid-demo-repo
boid project add .
```

## タスクを作って観察する

例として README に 1 行追記するタスクを作ります。

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

1. `pending → executing` (`auto_start: true` のため)
2. `executing`: `boid` が新しいブランチで worktree を作成 → claude-code hook が起動
3. agent が編集 → commit → push → `gh pr create` → `gh pr checks --watch` → finding 書き戻し
4. finding が `status: resolved` で書かれ、 `executing` から `verifying` へ自動遷移
5. `verifying`: ここには handler が紐付いていないため、未解決 finding が無いまま素通りで `done` へ
6. `done` への entry で `github-auto-merge` gate が `gh pr merge` を実行 → PR マージ完了

最終状態を確認:

```bash
boid task show <task-id>
```

PR がマージされ、 worktree が片付けられているはずです。

## CI が落ちたとき

agent が `verification.findings` に `status: open` の finding を書いた場合、上の流れの 4 以降は次のように分岐します:

- 4'. `executing` 由来の open finding が残ったままになるため、 `executing → reworking` の自動遷移が発火
- 5'. `reworking`: claude-code の rework hook が起動し、 `default_instructions.rework` を受け取る。 finding に書かれた失敗内容を読んで修正 → 同じ手順で push → CI 待ち → finding を `resolved` に書き戻し
- 6'. すべての finding が resolved になれば `reworking → verifying → done` と進み、 auto-merge

修正回数が `state_machine.rework_limit` (既定 5 回) を超えると自動 abort します。

`verifying` ↔ `reworking` の遷移条件の詳細は [状態機械](../guide/state-machine.md) を参照してください。

## なぜこの構成にするか

この章で示した構成のキモは、 **ワークフローの本体を instruction に書く** ことです。

- 専用の検証 kit を別に組まなくても、 agent に CI 結果の解釈と finding 書き戻しを指示すれば足りる
- プロジェクトごとに「どこまで自動化するか」「どんな失敗を rework に回すか」 を instruction で個別に調整できる
- `boid` 本体は状態機械の駆動と worktree / auto-merge の周辺にだけ責任を持ち、 kit と instruction の組み合わせで多様なワークフローを表現する

別の AI エージェントによるレビューを介在させたり、静的解析やセキュリティチェックを差し込んだりするといった発展形は、後続のチュートリアルで扱う予定です。

## 次に読むもの

- [概念](../guide/concepts.md) — 出てきた用語の改めての定義
- [状態機械](../guide/state-machine.md) — `verifying ↔ reworking` の遷移条件
- [Web UI](../guide/web-ui.md) — このタスクをブラウザ / スマホから観察する
- [トラブルシューティング](../guide/troubleshooting.md) — 詰まった時に
