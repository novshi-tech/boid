package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var projectLocalCmd = &cobra.Command{
	Use:   "local",
	Short: "Manage .boid/project.local.yaml",
}

var projectLocalInitForce bool

var projectLocalInitCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Create an empty .boid/project.local.yaml",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProjectLocalInit,
}

var projectLocalShowCmd = &cobra.Command{
	Use:   "show [dir]",
	Short: "Print .boid/project.local.yaml",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProjectLocalShow,
}

var projectLocalSetEnvCmd = &cobra.Command{
	Use:   "set-env <key> <value> [dir]",
	Short: "Set an env override in project.local.yaml",
	Args:  cobra.RangeArgs(2, 3),
	RunE:  runProjectLocalSetEnv,
}

var projectLocalUnsetEnvCmd = &cobra.Command{
	Use:   "unset-env <key> [dir]",
	Short: "Remove an env override from project.local.yaml",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runProjectLocalUnsetEnv,
}

var projectLocalBindingMode string

var projectLocalAddBindingCmd = &cobra.Command{
	Use:   "add-binding <path> [dir]",
	Short: "Add or update an additional binding in project.local.yaml",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runProjectLocalAddBinding,
}

var projectLocalRemoveBindingCmd = &cobra.Command{
	Use:   "remove-binding <path> [dir]",
	Short: "Remove an additional binding from project.local.yaml",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runProjectLocalRemoveBinding,
}

func init() {
	// project local commands operate on local files only and do not require
	// a running boid server.
	projectLocalCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}

	projectLocalInitCmd.Flags().BoolVar(&projectLocalInitForce, "force", false, "overwrite existing project.local.yaml")
	projectLocalAddBindingCmd.Flags().StringVar(&projectLocalBindingMode, "mode", "ro", "binding mode: ro or rw")

	projectLocalCmd.AddCommand(
		projectLocalInitCmd,
		projectLocalShowCmd,
		projectLocalSetEnvCmd,
		projectLocalUnsetEnvCmd,
		projectLocalAddBindingCmd,
		projectLocalRemoveBindingCmd,
	)
	projectCmd.AddCommand(projectLocalCmd)
}

func runProjectLocalInit(cmd *cobra.Command, args []string) error {
	projectDir, err := resolveProjectRoot(optionalDirArg(args))
	if err != nil {
		return err
	}

	path := projectspec.ProjectLocalPath(projectDir)
	if !projectLocalInitForce {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	if err := projectspec.WriteProjectLocalMeta(projectDir, projectspec.NewProjectLocalMeta()); err != nil {
		return err
	}
	fmt.Printf("initialized: %s\n", path)
	return nil
}

func runProjectLocalShow(cmd *cobra.Command, args []string) error {
	projectDir, err := resolveProjectRoot(optionalDirArg(args))
	if err != nil {
		return err
	}

	meta, err := projectspec.ReadProjectLocalMeta(projectDir)
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("%s not found", projectspec.ProjectLocalPath(projectDir))
	}

	data, err := projectspec.MarshalProjectLocalMeta(meta)
	if err != nil {
		return err
	}
	fmt.Print(string(data))
	return nil
}

func runProjectLocalSetEnv(cmd *cobra.Command, args []string) error {
	projectDir, err := resolveProjectRoot(optionalDirArg(args[2:]))
	if err != nil {
		return err
	}
	meta, err := loadProjectLocalEditable(projectDir)
	if err != nil {
		return err
	}

	if meta.Env == nil {
		meta.Env = make(map[string]string)
	}
	meta.Env[args[0]] = args[1]

	if err := projectspec.WriteProjectLocalMeta(projectDir, meta); err != nil {
		return err
	}
	fmt.Printf("set env: %s\n", args[0])
	return nil
}

func runProjectLocalUnsetEnv(cmd *cobra.Command, args []string) error {
	projectDir, err := resolveProjectRoot(optionalDirArg(args[1:]))
	if err != nil {
		return err
	}
	meta, err := loadProjectLocalEditable(projectDir)
	if err != nil {
		return err
	}

	delete(meta.Env, args[0])
	if len(meta.Env) == 0 {
		meta.Env = nil
	}

	if err := projectspec.WriteProjectLocalMeta(projectDir, meta); err != nil {
		return err
	}
	fmt.Printf("unset env: %s\n", args[0])
	return nil
}

func runProjectLocalAddBinding(cmd *cobra.Command, args []string) error {
	projectDir, err := resolveProjectRoot(optionalDirArg(args[1:]))
	if err != nil {
		return err
	}
	meta, err := loadProjectLocalEditable(projectDir)
	if err != nil {
		return err
	}

	source, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve binding path: %w", err)
	}
	binding := projectspec.BindMount{Source: source, Mode: projectLocalBindingMode}
	meta.AdditionalBindings = upsertBinding(meta.AdditionalBindings, binding)

	if err := projectspec.WriteProjectLocalMeta(projectDir, meta); err != nil {
		return err
	}
	fmt.Printf("set binding: %s (%s)\n", source, projectLocalBindingMode)
	return nil
}

func runProjectLocalRemoveBinding(cmd *cobra.Command, args []string) error {
	projectDir, err := resolveProjectRoot(optionalDirArg(args[1:]))
	if err != nil {
		return err
	}
	meta, err := loadProjectLocalEditable(projectDir)
	if err != nil {
		return err
	}

	source, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve binding path: %w", err)
	}
	meta.AdditionalBindings = removeBinding(meta.AdditionalBindings, source)
	if len(meta.AdditionalBindings) == 0 {
		meta.AdditionalBindings = nil
	}

	if err := projectspec.WriteProjectLocalMeta(projectDir, meta); err != nil {
		return err
	}
	fmt.Printf("removed binding: %s\n", source)
	return nil
}

func resolveProjectRoot(start string) (string, error) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		start = cwd
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	dir := abs
	if !info.IsDir() {
		dir = filepath.Dir(abs)
	}

	original := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".boid", "project.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("project root not found from %s", original)
		}
		dir = parent
	}
}

func loadProjectLocalEditable(projectDir string) (*projectspec.ProjectLocalMeta, error) {
	meta, err := projectspec.ReadProjectLocalMeta(projectDir)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		meta = projectspec.NewProjectLocalMeta()
	}
	return meta, nil
}

func optionalDirArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func upsertBinding(bindings []projectspec.BindMount, binding projectspec.BindMount) []projectspec.BindMount {
	for i := range bindings {
		if bindings[i].Source == binding.Source {
			bindings[i] = binding
			return bindings
		}
	}
	return append(bindings, binding)
}

func removeBinding(bindings []projectspec.BindMount, source string) []projectspec.BindMount {
	if len(bindings) == 0 {
		return nil
	}

	result := bindings[:0]
	for _, binding := range bindings {
		if binding.Source != source {
			result = append(result, binding)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
