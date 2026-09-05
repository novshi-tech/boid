package orchestrator

import "context"

// Actor 値の3系統。人間が押した action と代行タスクが押した action を actions
// ログ上で区別できないと、事故ったときに「人間の判断ミスか代行タスクの誤判定か」
// が復元不能になる。
const (
	// ActorHuman は Web UI / CLI からの人間操作 (nose 本人)。
	ActorHuman = "human"
	// ActorDaemon は daemon 自身の自動処理 (dispatch loop の auto_advance、
	// dispatch_error、daemon shutdown による abort、fired event 記録、
	// 起動時の auto-reopen など) — どの task/human にも帰属しない機械的記録。
	ActorDaemon = "daemon"
)

// ActorTask はサンドボックス内から boid task/action 経由で送られた operation
// (brokered) の actor 値を返す。taskID はその operation を送った側のタスク自身の ID —
// 自己申告 (boid task notify 等) では対象タスクと同一、khi 等 workspace push や将来の
// 代行タスクでは対象タスクと異なりうる。
func ActorTask(taskID string) string {
	return "task:" + taskID
}

type actorContextKey struct{}

// WithActor は ctx に actor (この操作を駆動しているのは誰/何か) を埋め込む。
// ApplyAction → Dispatch、Wake → Dispatch のような多段呼び出しチェーンの途中で
// シグネチャを増やさずに Action.Actor をスタンプできるようにするための伝播経路。
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ActorFromContext は WithActor で設定された actor を返す。未設定なら空文字。
func ActorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(actorContextKey{}).(string)
	return actor
}
