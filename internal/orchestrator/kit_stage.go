package orchestrator

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// StageHooks creates a temporary directory containing all hook scripts
// from the project and all kits. Kit scripts are prefixed with the kit
// consumer name (e.g. "claude-code--run-agent.sh") to avoid collisions.
// Project scripts override kit scripts with the same filename.
// Returns the staging directory path and a cleanup function.
//
// Deprecated: Use collectHookFiles in planner.go instead.
// PlanHook no longer calls StageHooks; hooks are now bind-mounted individually.
func StageHooks(projectHooksDir string, kitHooksDirs []KitHooksInfo, jobID string) (string, func(), error) {
	stagingDir := filepath.Join(os.TempDir(), fmt.Sprintf("boid-hooks-%s", jobID))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(stagingDir)
	}

	for _, info := range kitHooksDirs {
		if err := copyScriptsWithPrefix(info.HooksDir, stagingDir, info.Consumer); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy kit scripts from %s: %w", info.HooksDir, err)
		}
	}

	if err := copyScripts(projectHooksDir, stagingDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy project scripts: %w", err)
	}

	return stagingDir, cleanup, nil
}

// StageGates creates a temporary directory containing all gate scripts
// from the project and all kits. Project scripts override kit scripts
// with the same filename.
func StageGates(projectGatesDir string, kitGatesDirs []KitGatesInfo, jobID string) (string, func(), error) {
	return stageScripts("boid-gates", jobID, projectGatesDir, gateDirs(kitGatesDirs))
}

func stageScripts(prefix, jobID, projectDir string, kitDirs []string) (string, func(), error) {
	stagingDir := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", prefix, jobID))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(stagingDir)
	}

	for _, dir := range kitDirs {
		if err := copyScripts(dir, stagingDir); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy kit scripts from %s: %w", dir, err)
		}
	}

	if err := copyScripts(projectDir, stagingDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy project scripts: %w", err)
	}

	return stagingDir, cleanup, nil
}

func gateDirs(infos []KitGatesInfo) []string {
	if len(infos) == 0 {
		return nil
	}
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.GatesDir)
	}
	return out
}

// copyScriptsWithPrefix copies hook scripts from srcDir to dstDir, prefixing
// each filename with "{consumer}--" when consumer is non-empty.
func copyScriptsWithPrefix(srcDir, dstDir, consumer string) error {
	if consumer == "" {
		return copyScripts(srcDir, dstDir)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".sh" && ext != ".py" {
			continue
		}
		dstName := consumer + "--" + e.Name()
		if err := copyFile(filepath.Join(srcDir, e.Name()), filepath.Join(dstDir, dstName)); err != nil {
			return err
		}
	}
	return nil
}

func copyScripts(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".sh" && ext != ".py" {
			continue
		}
		if err := copyFile(filepath.Join(srcDir, e.Name()), filepath.Join(dstDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
