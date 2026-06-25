package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "(廃止) このコマンドは 3 つに分解されました",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		msg := `boid init は廃止されました。次の 3 コマンドで初期化してください:

  1) boid kit init                    (このマシンの kit カタログ生成)
  2) boid project init [dir]          (新規プロジェクト雛形)
     boid project add <dir>           (既存プロジェクト登録)
  3) boid workspace configure <slug>  (workspace 設定生成)

詳細は docs/ja/guide/onboarding.md を参照
`
		fmt.Fprint(cmd.ErrOrStderr(), msg)
		return fmt.Errorf("boid init is deprecated")
	},
}

func init() {
	initCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}
	rootCmd.AddCommand(initCmd)
}
