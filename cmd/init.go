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
		msg := `boid init は廃止されました。 次の手順で初期化してください:

  1) boid workspace apply -f workspace.yaml    (behavior と実行環境を設定)
  2) boid project add <git-url> --workspace=<slug>  (repository を登録)

project.yaml は読み込まれません。workspace envelope に設定をまとめてください。

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
