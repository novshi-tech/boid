package kit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/project"
)

// StageHooks creates a temporary directory containing all hook scripts
// from the project and all kits. Project scripts override kit scripts
// with the same filename.
// Returns the staging directory path and a cleanup function.
func StageHooks(projectHooksDir string, kitHooksDirs []project.KitHooksInfo, jobID string) (string, func(), error) {
	stagingDir := filepath.Join(os.TempDir(), fmt.Sprintf("boid-hooks-%s", jobID))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(stagingDir)
	}

	for _, m := range kitHooksDirs {
		if err := copyHookScripts(m.HooksDir, stagingDir); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy kit hooks from %s: %w", m.HooksDir, err)
		}
	}

	if err := copyHookScripts(projectHooksDir, stagingDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy project hooks: %w", err)
	}

	return stagingDir, cleanup, nil
}

func copyHookScripts(srcDir, dstDir string) error {
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
