package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// gate role は HOME=/tmp (tmpfs) なので、inner script 冒頭で
// /tmp/.boid/output/ が mkdir される仕掛けが必要。FileWrite sentinel が
// その役目を担うことを確認する。
func TestBuildSandboxSpec_GateHasOutputDirSentinel(t *testing.T) {
	spec := BuildSandboxSpec(orchestrator.JobSpec{
		Role:       orchestrator.RoleGate,
		TaskID:     "t1",
		ProjectID:  "p1",
		HandlerID:  "h1",
		ProjectDir: "/tmp/proj",
		HookScript: "gate.sh",
		BoidBinary: "/usr/local/bin/boid",
	}, SandboxBuildOptions{JobID: "job-1"})

	if !containsFileWritePath(spec.Files, "/tmp/.boid/output/.placeholder") {
		t.Errorf("gate spec missing /tmp/.boid/output sentinel; files = %+v", filePaths(spec.Files))
	}
}

// hook role は HOME=<projectDir> (tmpfs が被さる) なので、<projectDir>/.boid/
// output/ が同様に事前作成される必要がある。
func TestBuildSandboxSpec_HookHasOutputDirSentinel(t *testing.T) {
	spec := BuildSandboxSpec(orchestrator.JobSpec{
		Role:       orchestrator.RoleHook,
		TaskID:     "t1",
		ProjectID:  "p1",
		HandlerID:  "h1",
		ProjectDir: "/tmp/proj",
		HookScript: "hook.sh",
		BoidBinary: "/usr/local/bin/boid",
	}, SandboxBuildOptions{JobID: "job-2"})

	if !containsFileWritePath(spec.Files, "/tmp/proj/.boid/output/.placeholder") {
		t.Errorf("hook spec missing output sentinel; files = %+v", filePaths(spec.Files))
	}
}

func containsFileWritePath(files []sandbox.FileWrite, path string) bool {
	for _, f := range files {
		if f.Path == path {
			return true
		}
	}
	return false
}

func filePaths(files []sandbox.FileWrite) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}
