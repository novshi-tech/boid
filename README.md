# boid

汎用パーソナル AI オーケストレータ。

## インストール

```bash
go install github.com/novshi-tech/boid@latest
```

## 使い方

```bash
boid start          # サーバを起動（デーモン化）
boid task list      # タスク一覧
boid task show <id> # タスク詳細
boid stop           # サーバを停止
```

## Web UI

> **Experimental**
>
> 現在の Web UI は TaskList / TaskDetail / JobList / JobDetail は動作するが、
> タスク作成・編集・リアルタイム更新には対応していない。
> 将来のリリースでモバイルファースト対応のフル機能 Web アプリに刷新予定。

`boid start` のデフォルトで Web UI は有効です。

```bash
boid start
```

起動後は `http://localhost:8080` でアクセスできます。listen アドレスは `--http-addr` で変更可能 (例: `boid start --http-addr 127.0.0.1:5171`)。

## ビルド

```bash
go build ./...
go test ./...
```
