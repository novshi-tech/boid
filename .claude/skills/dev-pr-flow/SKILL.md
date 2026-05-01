---
name: dev-pr-flow
description: >
  実装変更を ship するための統一ワークフロー: ユニットテスト確認 → commit → push → PR 作成/再利用 → CI 完了まで watch する。
  .boid/project.yaml の task_behaviors.dev.default_instruction.message と同等の手順を、boid task の外 (手動 dev / 通常の対話セッション) でも再現するためのスキル。
  Use when shipping a change in the boid repo requires the full local verify → commit → push → PR → CI cycle (e.g. 「変更を PR にして CI 通るまで見て」 「dev タスクと同じ流れで ship して」 「commit + push + PR + watch まとめてやって」).
---

# dev PR flow

実装が固まった後の「ローカル検証 → commit → push → PR 作成/再利用 → CI watch」を一本化するための手順。
boid task の `dev` behavior が agent に渡す instruction と同じ流れを、boid task 外でも辿れるようにする。

## 適用範囲

- 実装が完了している (またはユーザがこの skill の起動で「完成」と宣言した) 状態
- 現在のブランチが PR 対象 (worktree でも通常 branch でも可)
- E2E は CI に任せる前提。ローカル `./e2e/run.sh` は走らせない (特殊事情があるならユーザに確認)

## 手順

### 1. ユニットテストを通す

コミット前に必ず:

```bash
go vet ./...
go test ./...
```

`go build` はテストを見ないので単独で代替にしない。失敗したら直してから次へ進む。

### 2. commit

意図したファイルだけ明示的に stage する。`git add -A` / `git add .` は使わない (secret や生成物の混入防止)。

```bash
git add <files>
git commit -m "<type>: <subject>"
```

プレフィックスは `feat:` / `fix:` / `refactor:` / `test:` (CLAUDE.md コーディング規約)。

### 3. push

```bash
git push -u origin HEAD
```

force-push / 履歴書き換えは禁止。中間 commit を消したいなら revert で対応する。

### 4. PR 作成 or 既存 PR 再利用

```bash
PR_URL=$(gh pr list --head "$(git branch --show-current)" --json url --jq '.[0].url // ""')
```

- `$PR_URL` が空 → `gh pr create --title "<title>" --body "<body>"` で新規作成
- 空でない → 既存 PR を再利用 (再 push 済みなので CI は自動で再実行)

title / body の決め方:

- title はそのブランチの主題を 1 行で (70 字以内)
- body は要点 bullet。boid task 経由なら `task: <task_id>` と summary を含める。手動 dev なら関連 Issue/PR や背景を書く

### 5. CI watch

```bash
gh pr checks --watch --fail-fast
```

- CI が無いリポでは即 exit 0 で抜ける (それで OK)
- 失敗したら `gh run view --log-failed` で失敗 job のログを確認し、原因を直してから手順 2-5 を繰り返す
- mergeable 状態が崩れた場合は別途「conflict 発生時」セクションを参照

CI green でこの skill のスコープは終了。

## merge は含めない

boid task の dev behavior では `task.exit` gate (auto-merge) が CI green 後に PR を merge するが、このスキル単体では merge しない。merge はユーザの明示的な指示があってから `gh pr merge` を実行する。

## boid task として走る場合との差分

| 観点 | boid task (dev) | この skill 単体 |
|------|----------------|----------------|
| ブランチ | worktree を boid が用意 | 現在の任意ブランチ |
| title/body 出所 | task.yaml | 会話 / ユーザ指示 |
| CI 失敗時 | exit 非ゼロ → boid が aborted | exit 非ゼロ → 単に停止 |
| 完了後 merge | `task.exit` gate が auto-merge | 実行しない |

## conflict 発生時のリカバリ

base に merge できなくなったら rebase ではなく merge で解消する (CLAUDE.md 規約):

```bash
git fetch origin
git merge origin/main
# conflict 解消
git add <resolved>
git commit
git push origin HEAD
```

その後手順 5 (CI watch) を再実行する。force-push 不要な履歴を保つために必ず merge を使う。
