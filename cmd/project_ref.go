package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"golang.org/x/term"
)

type resolveCandidate struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	WorkDir string `json:"work_dir"`
}

type resolveConflictResponse struct {
	Error      string             `json:"error"`
	Candidates []resolveCandidate `json:"candidates"`
}

// resolveProjectRef resolves ref to a single project via the API.
// On multiple matches, stdin is checked: if tty, interactive selection is shown;
// if not tty, an error listing candidates is returned.
func resolveProjectRef(c *client.Client, in *os.File, out io.Writer, ref string) (*orchestrator.Project, error) {
	isTTY := term.IsTerminal(int(in.Fd()))
	return resolveProjectRefIO(c, in, isTTY, out, ref)
}

// resolveProjectRefIO is the testable implementation with an explicit isTTY flag.
func resolveProjectRefIO(c *client.Client, in io.Reader, isTTY bool, out io.Writer, ref string) (*orchestrator.Project, error) {
	statusCode, body, err := c.GetRaw("/api/projects/" + ref)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}

	switch statusCode {
	case 200:
		var p orchestrator.Project
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("decode project: %w", err)
		}
		return &p, nil

	case 409:
		var cr resolveConflictResponse
		if err := json.Unmarshal(body, &cr); err != nil {
			return nil, fmt.Errorf("decode conflict response: %w", err)
		}
		if isTTY {
			return selectProjectInteractive(c, in, out, ref, cr.Candidates)
		}
		return nil, formatCandidateError(ref, cr.Candidates)

	default:
		var errResp map[string]string
		json.Unmarshal(body, &errResp) //nolint:errcheck
		if msg, ok := errResp["error"]; ok {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d", statusCode)
	}
}

// selectProjectInteractive prompts the user to pick one candidate and returns the full project.
func selectProjectInteractive(c *client.Client, in io.Reader, out io.Writer, ref string, candidates []resolveCandidate) (*orchestrator.Project, error) {
	fmt.Fprintf(out, "multiple projects match %q:\n", ref)
	for i, cand := range candidates {
		fmt.Fprintf(out, "  %d. %-20s %s\n", i+1, cand.Name, cand.WorkDir)
	}

	scanner := bufio.NewScanner(in)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(out, "Select [1]: ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			input = "1"
		}
		n, err := strconv.Atoi(input)
		if err != nil || n < 1 || n > len(candidates) {
			fmt.Fprintf(out, "invalid selection, enter 1-%d\n", len(candidates))
			continue
		}
		// Fetch the full project by exact UUID to populate all fields.
		var p orchestrator.Project
		if err := c.Do("GET", "/api/projects/"+candidates[n-1].ID, nil, &p); err != nil {
			return nil, fmt.Errorf("get selected project: %w", err)
		}
		return &p, nil
	}
	return nil, fmt.Errorf("no valid selection made")
}

// formatCandidateError returns an error that lists all candidates for non-tty output.
func formatCandidateError(ref string, candidates []resolveCandidate) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "multiple projects match %q. Specify exact id:\n", ref)
	for _, cand := range candidates {
		fmt.Fprintf(&sb, "  - %-20s %s\n", cand.Name, cand.WorkDir)
	}
	return fmt.Errorf("%s", strings.TrimRight(sb.String(), "\n"))
}
