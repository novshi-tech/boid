package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func isTerminalTaskStatus(status orchestrator.TaskStatus) bool {
	return status == orchestrator.TaskStatusDone || status == orchestrator.TaskStatusAborted
}

func isTerminalJobStatus(status api.JobStatus) bool {
	return status == api.JobStatusCompleted || status == api.JobStatusFailed
}

type taskDetailYAML struct {
	ID          string       `yaml:"id"`
	ProjectID   string       `yaml:"project_id"`
	Title       string       `yaml:"title"`
	Description string       `yaml:"description,omitempty"`
	Status      string       `yaml:"status"`
	Behavior    string       `yaml:"behavior"`
	CreatedAt   string       `yaml:"created_at"`
	UpdatedAt   string       `yaml:"updated_at"`
	Payload     any          `yaml:"payload,omitempty"`
	Actions     []actionYAML `yaml:"actions,omitempty"`
	Jobs        []jobYAML    `yaml:"jobs,omitempty"`
}

type actionYAML struct {
	ID        string `yaml:"id"`
	Type      string `yaml:"type"`
	CreatedAt string `yaml:"created_at"`
	Payload   any    `yaml:"payload,omitempty"`
}

type jobYAML struct {
	ID        string `yaml:"id"`
	HandlerID string `yaml:"handler_id"`
	Role      string `yaml:"role"`
	Status    string `yaml:"status"`
	ExitCode  *int   `yaml:"exit_code,omitempty"`
	UpdatedAt string `yaml:"updated_at"`
	Output    string `yaml:"output,omitempty"`
}

func renderTaskDetail(detail *api.TaskDetailView) error {
	task := detail.Task
	out := taskDetailYAML{
		ID:          task.ID,
		ProjectID:   task.ProjectID,
		Title:       task.Title,
		Description: task.Description,
		Status:      string(task.Status),
		Behavior:    task.Behavior,
		CreatedAt:   formatTime(task.CreatedAt),
		UpdatedAt:   formatTime(task.UpdatedAt),
	}

	if len(task.Payload) > 0 && string(task.Payload) != "{}" && string(task.Payload) != "null" {
		var p any
		if err := json.Unmarshal(task.Payload, &p); err == nil {
			out.Payload = p
		}
	}

	for _, action := range detail.Actions {
		a := actionYAML{
			ID:        action.ID,
			Type:      action.Type,
			CreatedAt: formatTime(action.CreatedAt),
		}
		if len(action.Payload) > 0 && string(action.Payload) != "{}" && string(action.Payload) != "null" {
			var p any
			if err := json.Unmarshal(action.Payload, &p); err == nil {
				a.Payload = p
			}
		}
		out.Actions = append(out.Actions, a)
	}

	for _, job := range detail.Jobs {
		j := jobYAML{
			ID:        job.ID,
			HandlerID: job.HandlerID,
			Role:      job.Role,
			Status:    string(job.Status),
			UpdatedAt: formatTime(job.UpdatedAt),
		}
		if isTerminalJobStatus(job.Status) {
			code := job.ExitCode
			j.ExitCode = &code
		}
		if strings.TrimSpace(job.Output) != "" {
			j.Output = strings.TrimSpace(job.Output)
		}
		out.Jobs = append(out.Jobs, j)
	}

	b, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal task detail: %w", err)
	}
	fmt.Print(string(b))
	return nil
}

func renderJob(job *api.Job) {
	fmt.Printf("ID:         %s\n", job.ID)
	fmt.Printf("Task:       %s\n", job.TaskID)
	fmt.Printf("Project:    %s\n", job.ProjectID)
	fmt.Printf("Handler:    %s\n", job.HandlerID)
	fmt.Printf("Role:       %s\n", job.Role)
	fmt.Printf("Runtime:    %s\n", valueOrDash(job.RuntimeID))
	fmt.Printf("Attachable: %s\n", yesNo(job.RuntimeID != "" && job.Interactive))
	fmt.Printf("TTY:        %s\n", yesNo(job.TTY))
	fmt.Printf("Status:     %s\n", job.Status)
	fmt.Printf("Exit Code:  %s\n", formatExitCode(job.Status, job.ExitCode))
	fmt.Printf("Created At: %s\n", formatTime(job.CreatedAt))
	fmt.Printf("Updated At: %s\n", formatTime(job.UpdatedAt))
	if strings.TrimSpace(job.Output) != "" {
		fmt.Println("Output:")
		printPrettyJSONOrText(job.Output, "  ")
	}
}

func renderJobList(jobs []*api.Job) {
	fmt.Printf("%-36s %-24s %-8s %-10s %-4s %-19s\n", "ID", "HANDLER", "ROLE", "STATUS", "EXIT", "UPDATED")
	for _, job := range jobs {
		fmt.Printf("%-36s %-24s %-8s %-10s %-4s %-19s\n",
			job.ID,
			truncate(job.HandlerID, 24),
			job.Role,
			job.Status,
			formatExitCode(job.Status, job.ExitCode),
			formatTime(job.UpdatedAt),
		)
	}
}

func printPrettyJSONIndented(raw json.RawMessage, indent string) {
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			fmt.Printf("%s%s\n", indent, line)
		}
		return
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			fmt.Printf("%s%s\n", indent, line)
		}
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		fmt.Printf("%s%s\n", indent, line)
	}
}

func printPrettyJSONOrText(text string, indent string) {
	raw := json.RawMessage(text)
	var out any
	if err := json.Unmarshal(raw, &out); err == nil {
		printPrettyJSONIndented(raw, indent)
		return
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Printf("%s%s\n", indent, line)
	}
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func valueOrDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func formatTime(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	return ts.Local().Format("2006-01-02 15:04:05")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func printWatchHeader(kind, id string) {
	fmt.Fprintf(os.Stdout, "== %s %s ==\n", kind, id)
}

func formatExitCode(status api.JobStatus, code int) string {
	if !isTerminalJobStatus(status) {
		return "-"
	}
	return fmt.Sprintf("%d", code)
}
