package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "(廃止) このコマンドは 2 つに分解されました",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Phase 2.5 PR6 (kit 機構退役) で `boid kit init` / `boid
		// workspace configure` が撤去されたため、 かつての 3 段オンボ
		// ーディング (kit init → project init → workspace configure) は
		// もう成立しない。 現行の 2 段は project 登録 + workspace
		// setup (yaml 直接 or CLI 経由の create/edit/import)。 workspace
		// は default が自動生成されるので、 default で足りるなら 1 段目
		// だけで足りる。
		msg := `boid init は廃止されました。 次の手順で初期化してください:

  1) boid project init [dir]                    (新規プロジェクト雛形)
     boid project add <dir>                     (既存プロジェクト登録)

  2) 必要なら workspace を用意 (default で足りるなら省略可):
     boid workspace create <slug> --from-file <yaml>    (新規作成)
     boid workspace edit   <slug> --from-file <yaml>    (更新)
     boid workspace import <yaml> [--mode replace]      (yaml から取り込み)
     boid workspace assign <project> <slug>             (project に紐付け)

詳細は docs/ja/guide/onboarding.md を参照
`
		fmt.Fprint(cmd.ErrOrStderr(), msg)
		return fmt.Errorf("boid init is deprecated")
	},
}

func init() {
	initCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	}
	rootCmd.AddCommand(initCmd)
}
