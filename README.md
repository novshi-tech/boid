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

> **Experimental — デフォルト無効**
>
> 現在の Web UI は TaskList / TaskDetail / JobList / JobDetail は動作するが、
> タスク作成・編集・リアルタイム更新には対応していない。
> 将来のリリースでモバイルファースト対応のフル機能 Web アプリに刷新予定。

デフォルトでは Web UI は無効になっています。有効にするには `--web` フラグを指定してサーバを起動します。

```bash
boid start --web
```

起動後は `http://localhost:8080` でアクセスできます。

## ビルド

```bash
go build ./...
go test ./...
```
