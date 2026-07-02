package client_test

import (
	"go/ast"
	"go/types"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestClientDoesNotDependOnBehavior は internal/client のアーキ不変条件を機械で守る:
//
//   - 型の共有は許可: api の DTO / orchestrator のドメイン型を import して
//     リクエスト/レスポンスをマーシャルするのは正当 (バケツリレー回避)。
//   - 振る舞いへの直接依存は禁止: client は必ず HTTP API を叩く。api/orchestrator の
//     パッケージレベル関数 (振る舞い) をローカルで呼んではならない。
//
// scripts/check-internal-architecture.sh は import グラフしか見えず「型 import」と
// 「関数呼び出し」を区別できない (backend 層 server/db/dispatcher/sandbox は import
// 自体を hard ban 済み)。本テストは go/types でシンボル単位に解決し、api/orchestrator
// の *types.Func 参照を落とすことで振る舞い依存禁止を機械 enforce する。
//
// なお型のメソッド (例: orchestrator.Task の getter) は共有された型の契約の一部なので
// 許可する (Recv() != nil を除外)。禁止対象はパッケージレベル関数のみ。
func TestClientDoesNotDependOnBehavior(t *testing.T) {
	const clientPkg = "github.com/novshi-tech/boid/internal/client"

	// 型の共有は許可するが、振る舞い関数の呼び出しを禁止するパッケージ。
	behaviorForbidden := map[string]bool{
		"github.com/novshi-tech/boid/internal/api":          true,
		"github.com/novshi-tech/boid/internal/orchestrator": true,
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}
	pkgs, err := packages.Load(cfg, clientPkg)
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("パッケージのロードに型エラーあり (上記参照)")
	}
	if len(pkgs) != 1 {
		t.Fatalf("expected 1 package, got %d", len(pkgs))
	}

	pkg := pkgs[0]
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			obj := pkg.TypesInfo.Uses[sel.Sel]
			fn, ok := obj.(*types.Func)
			if !ok || fn.Pkg() == nil {
				return true
			}
			if !behaviorForbidden[fn.Pkg().Path()] {
				return true
			}
			// メソッド (レシーバあり) は共有型の契約の一部として許可。
			if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
				return true
			}
			pos := pkg.Fset.Position(sel.Pos())
			t.Errorf("振る舞い依存の禁止違反: internal/client が %s.%s を呼んでいる (%s)。"+
				"client は必ず HTTP API を叩くこと。%s のパッケージ関数を直接呼んではならない (型の共有は可)。",
				fn.Pkg().Name(), fn.Name(), pos, fn.Pkg().Path())
			return true
		})
	}
}
