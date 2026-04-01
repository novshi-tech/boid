package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func ProjectLocalPath(dir string) string {
	return filepath.Join(dir, ".boid", projectLocalFilename)
}

func NewProjectLocalMeta() *ProjectLocalMeta {
	return &ProjectLocalMeta{Version: 1}
}

func MarshalProjectLocalMeta(meta *ProjectLocalMeta) ([]byte, error) {
	if meta == nil {
		meta = NewProjectLocalMeta()
	}

	normalized := *meta
	if normalized.Version == 0 {
		normalized.Version = 1
	}
	if err := validateProjectLocalMeta(&normalized); err != nil {
		return nil, err
	}

	data, err := yaml.Marshal(&normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", projectLocalFilename, err)
	}
	return data, nil
}

func WriteProjectLocalMeta(dir string, meta *ProjectLocalMeta) error {
	path := ProjectLocalPath(dir)
	data, err := MarshalProjectLocalMeta(meta)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir .boid: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", projectLocalFilename, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s: %w", projectLocalFilename, err)
	}
	return nil
}
