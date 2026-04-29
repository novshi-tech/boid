# トラブルシューティング

メンテナ自身が引っかかった代表的な事例です。一覧にないものでも、 daemon ログ (`~/.local/state/boid/boid.log`) と `boid task show <id>` の組み合わせで原因が分かることがほとんどです。

## daemon が起動しない

```text
Error: boid server already running (socket: /run/user/1000/boid.sock)
```

別の `boid` プロセスがすでに listen しています。 `kill` ではなく `boid stop` で落としてください。

`boid stop` が "no server running" と言うのにこのエラーが出る場合は socket ファイルが残っているだけなので、手動で削除します。

```bash
rm "$XDG_RUNTIME_DIR/boid.sock"
```

## バグ修正をインストールしたのに変化がない

`go install` し直したが daemon を再起動し忘れているケースです。 disk 上のバイナリは新しくても、 daemon は古いコードを mmap したまま動き続けます。

診断:

```bash
# disk 上のバイナリが置き換わっていれば /proc/<pid>/exe は (deleted) と表示される
ps -o pid,cmd -C boid
ls -l /proc/<pid>/exe
```

修正:

```bash
boid stop
boid start
```

「直したのにまだ起こる」の最大の原因はこれです。迷ったら再起動してください。

## タスクが `executing` のまま終わらない

3 つの可能性があります。

1. **hook に終了パスがない**。 prompt 待ち / interactive コマンド / 詰まったエージェントなどでブロックされていると、 dispatch ループが待ち続けます。 `boid job list --task <id>` に終わらない `running` ジョブが見えるはず。 `boid task abort <id>` でクリーンアップし、 hook スクリプトを確認してください
2. **完了シグナルとなる trait が payload に書かれていない**。 `artifact` (plan タスクなら `tasks`) が無いと executing からの自動遷移ルールが発火しません。 `boid task show <id>` で payload を確認し、 hook が期待する trait の payload patch を吐いているかチェック
3. **`verifying` 由来の open finding が残っている**。 `verifying` から戻ってきたタスクは、未解消の finding が `reworking` を留め置きます。 `boid task get <id> findings` (または `task show`) で確認

## タスクが `reworking` のまま終わらない

上と同じロジックを `reworking → verifying` 側で考えてください。 rework hook は `reworking` 由来の finding をすべて resolved にしないと verifying に戻れません。 hook が新しい finding を書き続けると、最終的に rework 上限自動 abort (`code=rework_limit_exceeded`) になります。本当にワークフロー上 5 回が足りないなら `~/.config/boid/config.yaml` の `state_machine.rework_limit` を上げますが、たいていは rework hook の問題です。

## `boid task list` が遅い / ディスクが膨れる

ローカルデータは 2 箇所に蓄積します。

| パス | 管理主体 | 自動 GC |
|---|---|---|
| `~/.local/share/boid/runtimes/<id>/` | `boid` | あり (24h ごと、 30d より古いものを削除) |
| `~/.claude/projects/-home-...-worktrees-boid-<taskid>/` | Claude Code | **なし** — 手動でクリーンアップ |

前者は自動 GC されます。後者は Claude Code 自身が書き込むもので、 `boid` は手を出しません。 `~/.claude/projects/` が肥大化しているなら手動で消します:

```bash
rm -rf ~/.claude/projects/-home-*-worktrees-boid-*
```

(他プロジェクトのエントリを巻き込まないよう注意してください。)

GC 設定は `~/.config/boid/config.yaml`:

```yaml
gc:
  enabled: true
  interval: 24h
  older_than: 720h    # 30 日
```

## hook 内で "permission denied" や "unknown command"

hook はサンドボックス内で動きます。 kit の `host_commands` で許可していないコマンドを叩こうとするとブロックされます。直し方は 2 通り:

- 不足コマンドを kit の `host_commands` に追加する (`git push` のような汎用ツール向け)
- gate に責任を移す。 gate はサンドボックスなしで host で動く (`systemctl restart` のような環境依存操作向け)

## Web UI: デバイスが何度もログアウトされる

cookie は `HttpOnly; Secure; SameSite=Lax` です。スマホブラウザが「閉じたら cookie 削除」設定だとデバイスログインが残りません。別ブラウザを使うか、当該ホストだけそのポリシーを外してください。

ペアリング後に公開 URL を変更すると、通知から飛ぶマジックリンクが古い URL を指したままになります。 `boid web set-url <new-url>` で更新してください。

## Web UI: ペアリングコードが "expired" / "invalid" になる

コードは発行 5 分後に失効、単回使用です。 `boid web pair` で再発行してください。

## `/proc/<pid>/exe` の boid に `(deleted)` が見える

disk 上のバイナリが置き換わったが daemon はまだ古いコードを動かしています。 [バグ修正をインストールしたのに変化がない](#バグ修正をインストールしたのに変化がない) を参照。

## どこを最初に見るか

- **daemon ログ**: `~/.local/state/boid/boid.log` (ローテーションあり)
- **タスク状態**: `boid task show <id>`
- **ジョブログ**: `boid job show <id>`
- **ライブ更新**: `boid task watch <id>` または Web UI のタスク詳細

バグらしき挙動なら、タスク / ジョブ ID と daemon ログの該当部分を添えて issue を立ててください。
