package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ensureHostGateWorktree returns a usable worktree path for a host gate.
// existingWorktreePath() returns "" when the record's cleaned_at is set
// (typical after a task aborts and CleanupForTask drops the worktree). Replay
// scenarios commonly hit that state, so we recreate from the recorded base
// branch rather than failing — matching the semantic of allocateWorktree but
// without requiring spec.Visibility.ProjectDir (which gates leave empty).
func (r *Runner) ensureHostGateWorktree(spec *orchestrator.JobSpec, currentPath string) (string, error) {
	if currentPath != "" {
		return currentPath, nil
	}
	if r.Worktrees == nil {
		return "", fmt.Errorf("host gate requires a worktree manager")
	}
	if spec.TaskID == "" {
		return "", fmt.Errorf("host gate requires a task id")
	}

	existing, err := r.Worktrees.Get(spec.TaskID)
	if err != nil {
		return "", fmt.Errorf("lookup worktree: %w", err)
	}
	if existing == nil {
		return "", fmt.Errorf("no worktree record for task %q (host gates need a prior worktree)", spec.TaskID)
	}

	_, projectWorkDir, perr := r.resolveProjectRuntime(spec.ProjectID)
	if perr != nil {
		return "", fmt.Errorf("resolve project runtime: %w", perr)
	}
	if projectWorkDir == "" {
		return "", fmt.Errorf("project %q has no work dir; cannot recreate worktree", spec.ProjectID)
	}

	w, err := r.Worktrees.Recreate(projectWorkDir, spec.TaskID)
	if err != nil {
		return "", fmt.Errorf("recreate worktree for host gate: %w", err)
	}
	return w.Path, nil
}

// dispatchHostGate runs a gate directly on the host with cwd set to the
// worktree root and no broker or sandbox layered between it and the host.
// All gate jobs use this path; there is no sandboxed gate execution path.
func (r *Runner) dispatchHostGate(
	ctx context.Context,
	job *Job,
	spec *orchestrator.JobSpec,
	worktreeRoot string,
	cleanup orchestrator.CleanupFunc,
) (string, error) {
	if r.Runtime == nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("job runtime is required")
	}
	if len(spec.Argv) == 0 {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("host gate spec is missing argv")
	}
	if worktreeRoot == "" {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("host gate requires a worktree (task must have worktree=true)")
	}
	if r.BoidBinary == "" {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("host gate requires boid binary path to call `boid job done`")
	}

	wrapperPath, outputPath, err := writeHostGateWrapper(job.ID, worktreeRoot, spec, r.BoidBinary)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("write host gate wrapper: %w", err)
	}

	handle, err := r.Runtime.Start(ctx, RuntimeStartSpec{
		JobID:     job.ID,
		TaskID:    job.TaskID,
		ProjectID: job.ProjectID,
		HandlerID: job.HandlerID,
		Role:      job.Role,
		Command:   "bash " + wrapperPath,
		// host gates never receive interactive stdin or a TTY: their entire
		// input is the static taskJSON payload baked into the wrapper.
		Interactive: false,
		TTY:         false,
	})
	if err != nil {
		removeHostGateArtifacts(wrapperPath, outputPath)
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("start runtime: %w", err)
	}

	job.RuntimeID = handle.ID
	job.Interactive = handle.Interactive
	job.TTY = handle.TTY
	if err := UpdateJob(r.DB, job); err != nil {
		_ = r.Runtime.Stop(context.Background(), handle.ID)
		removeHostGateArtifacts(wrapperPath, outputPath)
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("persist job runtime metadata: %w", err)
	}

	r.trackTaskRuntime(job.TaskID, handle.ID)
	go r.watchRuntime(job.ID, handle.ID)
	go r.cleanupHostGateAfterWait(handle.ID, wrapperPath, outputPath, cleanup)
	slog.Info("host gate started", "job_id", job.ID, "runtime_id", handle.ID, "script", spec.Argv[0])
	return job.ID, nil
}

// writeHostGateWrapper materializes a small bash launcher that:
//   - sets the gate env vars (BOID_TASK_ID + behavior/task env merged in JobSpec.Env),
//   - cd's to the worktree,
//   - feeds taskJSON to the gate script on stdin,
//   - captures stdout to a temp file (consumed by `boid job done --output-file`),
//   - on exit, calls `boid job done` so watchRuntime sees a normal completion.
func writeHostGateWrapper(jobID, worktreeRoot string, spec *orchestrator.JobSpec, boidBin string) (wrapperPath, outputPath string, err error) {
	dir := os.TempDir()
	wrapperPath = filepath.Join(dir, fmt.Sprintf("boid-host-gate-%s.sh", jobID))
	outputPath = filepath.Join(dir, fmt.Sprintf("boid-host-gate-%s.out", jobID))

	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	env["BOID_TASK_ID"] = spec.TaskID
	env["BOID_JOB_ID"] = jobID
	if _, ok := env["HOME"]; !ok {
		if h := os.Getenv("HOME"); h != "" {
			env["HOME"] = h
		}
	}
	if _, ok := env["PATH"]; !ok {
		if p := os.Getenv("PATH"); p != "" {
			env["PATH"] = p
		}
	}

	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("set -u\n")
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "export %s=%s\n", k, hostGateShellQuote(env[k]))
	}
	fmt.Fprintf(&b, "cd %s\n", hostGateShellQuote(worktreeRoot))
	fmt.Fprintf(&b, "OUTPUT_FILE=%s\n", hostGateShellQuote(outputPath))
	fmt.Fprintf(&b, "JOB_ID=%s\n", hostGateShellQuote(jobID))
	fmt.Fprintf(&b, "BOID_BIN=%s\n", hostGateShellQuote(boidBin))
	// trap on EXIT to report completion regardless of script success/failure.
	b.WriteString(`trap '_exit=$?; "$BOID_BIN" job done "$JOB_ID" --exit-code "$_exit" --output-file "$OUTPUT_FILE" 2>/dev/null || true' EXIT` + "\n")

	scriptArgv := hostGateShellQuoteArgv(spec.Argv)
	if len(spec.PrimaryInput) > 0 {
		fmt.Fprintf(&b, "printf '%%s' %s | %s > \"$OUTPUT_FILE\"\n",
			hostGateShellQuote(string(spec.PrimaryInput)), scriptArgv)
	} else {
		fmt.Fprintf(&b, "%s > \"$OUTPUT_FILE\" < /dev/null\n", scriptArgv)
	}

	if err := os.WriteFile(wrapperPath, []byte(b.String()), 0o700); err != nil {
		return "", "", err
	}
	return wrapperPath, outputPath, nil
}

func (r *Runner) cleanupHostGateAfterWait(runtimeID, wrapperPath, outputPath string, extra orchestrator.CleanupFunc) {
	defer func() {
		if extra != nil {
			extra()
		}
	}()
	if r.Runtime == nil || runtimeID == "" {
		removeHostGateArtifacts(wrapperPath, outputPath)
		return
	}
	if _, err := r.Runtime.Wait(context.Background(), runtimeID); err != nil {
		// Wait may legitimately fail (ErrRuntimeUnsupported in tests); still
		// remove the wrapper so /tmp doesn't accumulate cruft.
		slog.Debug("host gate wait failed", "runtime_id", runtimeID, "error", err)
	}
	removeHostGateArtifacts(wrapperPath, outputPath)
}

func removeHostGateArtifacts(wrapperPath, outputPath string) {
	for _, p := range []string{wrapperPath, outputPath} {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove host gate artifact", "path", p, "error", err)
		}
	}
}

// hostGateShellQuote wraps s in single quotes so bash treats it literally.
// Matches sandbox/script.go's helper but is duplicated here to keep the host
// gate path self-contained (no dependency on sandbox internals).
func hostGateShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func hostGateShellQuoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = hostGateShellQuote(a)
	}
	return strings.Join(parts, " ")
}

