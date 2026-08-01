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
		// docs/plans/release-onboarding.md 穴 7/PR6 (codex round-21
		// review): `boid project add <dir>` was removed entirely (git-URL
		// registration only, PR-4) and no longer exists to guide users
		// toward — this deprecated command's own migration message must
		// point at the CURRENT flow instead: scaffold locally, push, then
		// register the pushed git URL.
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
