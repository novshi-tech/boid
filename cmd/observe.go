package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func isTerminalTaskStatus(status orchestrator.TaskStatus) bool {
	return status == orchestrator.TaskStatusDone || status == orchestrator.TaskStatusAborted
}

func isTerminalJobStatus(status api.JobStatus) bool {
	return status == api.JobStatusCompleted || status == api.JobStatusFailed
}

func renderTaskDetail(detail *api.TaskDetailView) error {
	task := detail.Task
	fmt.Printf("ID:         %s\n", task.ID)
	fmt.Printf("Project:    %s\n", task.ProjectID)
	fmt.Printf("Title:      %s\n", task.Title)
	fmt.Printf("Status:     %s\n", task.Status)
	fmt.Printf("Behavior:   %s\n", task.Behavior)
	fmt.Printf("Created At: %s\n", formatTime(task.CreatedAt))
	fmt.Printf("Updated At: %s\n", formatTime(task.UpdatedAt))

	if len(task.Payload) > 0 && string(task.Payload) != "{}" {
		fmt.Println("Payload:")
		printPrettyJSON(task.Payload)
	}

	if len(detail.Actions) > 0 {
		fmt.Println("Actions:")
		for _, action := range detail.Actions {
			fmt.Printf("- %s  %-18s %s\n", formatTime(action.CreatedAt), action.Type, action.ID)
			if len(action.Payload) > 0 && string(action.Payload) != "{}" {
				printPrettyJSONIndented(action.Payload, "    ")
			}
		}
	}

	if len(detail.Jobs) > 0 {
		fmt.Println("Jobs:")
		fmt.Printf("  %-36s %-24s %-8s %-10s %-4s %-19s\n", "ID", "HANDLER", "ROLE", "STATUS", "EXIT", "UPDATED")
		for _, job := range detail.Jobs {
			fmt.Printf("  %-36s %-24s %-8s %-10s %-4s %-19s\n",
				job.ID,
				truncate(job.HandlerID, 24),
				job.Role,
				job.Status,
				formatExitCode(job.Status, job.ExitCode),
				formatTime(job.UpdatedAt),
			)
			if strings.TrimSpace(job.Output) != "" {
				fmt.Println("    output:")
				printPrettyJSONOrText(job.Output, "      ")
			}
		}
	}

	return nil
}

func renderJob(job *api.Job) {
	fmt.Printf("ID:         %s\n", job.ID)
	fmt.Printf("Task:       %s\n", job.TaskID)
	fmt.Printf("Project:    %s\n", job.ProjectID)
	fmt.Printf("Handler:    %s\n", job.HandlerID)
	fmt.Printf("Role:       %s\n", job.Role)
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

func printPrettyJSON(raw json.RawMessage) {
	printPrettyJSONIndented(raw, "  ")
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
