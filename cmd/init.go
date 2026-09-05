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

  1) boid project init [dir]                    (新規プロジェクト雛形を生成し、次の手順を案内)
     git push した URL を                         (既存プロジェクトは自分で push 済みの
     boid project add <git-url> --workspace=...   git URL を直接登録)

  2) 必要なら workspace を用意 (default で足りるなら省略可):
     boid workspace create <slug> --from-file <yaml>    (新規作成)
     boid workspace edit   <slug> --from-file <yaml>    (更新)
     boid workspace apply -f <yaml>                     (export した envelope 文書を適用)
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
