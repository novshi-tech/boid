package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// runWorkspaceExport exports one workspace (the <slug> positional argument)
// or every workspace (--all) as a boid.dev/v1 Workspace yaml document
// (docs/plans/volume-only-daemon.md §論点g). Unlike every other workspace
// subcommand, this deliberately does not go through renderOutput: the whole
// point is to emit yaml (to stdout, or -o/--output <path>), not a
// json/yaml/plain-text rendering of a structured response object.
func runWorkspaceExport(cmd *cobra.Command, args []string) error {
	if workspaceExportAll {
		if len(args) != 0 {
			return fmt.Errorf("workspace export --all takes no <slug> argument")
		}
	} else {
		if len(args) != 1 {
			return fmt.Errorf("workspace export requires exactly one <slug> argument, or --all")
		}
		if err := orchestrator.ValidWorkspaceSlug(args[0]); err != nil {
			return err
		}
	}

	c := client.FromContext(cmd.Context())
	stderr := cmd.ErrOrStderr()

	var slugs []string
	if workspaceExportAll {
		var summaries []*orchestrator.WorkspaceSummary
		if err := c.Do("GET", "/api/workspaces", nil, &summaries); err != nil {
			return fmt.Errorf("list workspaces: %w", err)
		}
		for _, s := range summaries {
			slugs = append(slugs, s.ID)
		}
		sort.Strings(slugs)
	} else {
		slugs = []string{args[0]}
	}

	docs := make([][]byte, 0, len(slugs))
	for _, slug := range slugs {
		doc, err := exportWorkspaceEnvelopeYAML(c, stderr, slug)
		if err != nil {
			return fmt.Errorf("export workspace %q: %w", slug, err)
		}
		docs = append(docs, doc)
	}
	output := bytes.Join(docs, []byte("---\n"))

	if workspaceExportOutput != "" {
		if err := os.WriteFile(workspaceExportOutput, output, 0o644); err != nil {
			return fmt.Errorf("write -o/--output: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "exported %d workspace(s) to %s\n", len(slugs), workspaceExportOutput)
		return nil
	}

	_, err := cmd.OutOrStdout().Write(output)
	return err
}

// exportWorkspaceEnvelopeYAML builds and marshals one workspace's
// WorkspaceEnvelope by composing two existing endpoints — GET
// /api/workspaces/{slug} (meta) and GET /api/projects?workspace_id={slug}
// (assigned projects) — client-side; no new HTTP endpoint was needed
// (docs/plans/volume-only-daemon.md §論点g's RPC note: "Export can be a
// client-side operation ... If the existing endpoints are insufficient ...
// add a new endpoint" — GET /api/projects?workspace_id already lists a
// workspace's projects, same data `boid workspace show` uses).
func exportWorkspaceEnvelopeYAML(c *client.Client, stderr io.Writer, slug string) ([]byte, error) {
	var detail api.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &detail); err != nil {
		return nil, err
	}

	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+slug, nil, &projects); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	sort.Slice(projects, func(i, j int) bool {
		return projectExportName(projects[i]) < projectExportName(projects[j])
	})

	if detail.Meta != nil {
		warnEnvHostPaths(stderr, detail.Meta.Env)
	}

	envProjects := make([]orchestrator.WorkspaceEnvelopeProject, 0, len(projects))
	for _, p := range projects {
		name := projectExportName(p)
		ep := orchestrator.WorkspaceEnvelopeProject{Name: name}
		if p.UpstreamURL != "" {
			ep.URL = p.UpstreamURL
		} else {
			fmt.Fprintf(stderr, "warning: project %q in workspace %q has no known git remote URL yet (pre-PR-2) — url omitted from export, will be filled in once `boid project add`/`reload` captures it\n", name, slug)
		}
		envProjects = append(envProjects, ep)
	}

	envelope := orchestrator.NewWorkspaceEnvelopeFromMeta(slug, detail.Meta, envProjects)
	return yaml.Marshal(envelope)
}

// projectExportName picks the spec.projects[].name value for p: project.
// Meta.Name (the project.yaml `name:` field) when known — the same field
// `boid project`'s and `boid workspace apply`'s own ref resolution
// (ResolveProjectRef: id > Meta.Name exact > Meta.Name substring) matches
// against, so an exported name round-trips straight back through `apply`
// unchanged. Falls back to the work_dir basename only when Meta was never
// hydrated (Meta.Name empty — should not normally happen once a project is
// registered, but export must not crash over it).
func projectExportName(p *orchestrator.Project) string {
	if p.Meta.Name != "" {
		return p.Meta.Name
	}
	return filepath.Base(p.WorkDir)
}

// hostPathPattern matches an env value that looks like it references a host
// filesystem path (docs/plans/volume-only-daemon.md §論点g「env の host path
// 依存」) — an absolute path under /home, /opt, /mnt, or /root, either at the
// start of the value or after a ':' (PATH-style) or '=' separator. This is a
// heuristic advisory check, not a validator: it deliberately also flags
// container-internal paths that happen to share a prefix (e.g.
// "/home/boid/go") — the warning tells the operator to double check, export
// still proceeds either way.
var hostPathPattern = regexp.MustCompile(`(^|[:=])(/home|/opt|/mnt|/root)/`)

// warnEnvHostPaths prints one stderr warning line per env entry whose value
// looks like a host filesystem path, in sorted key order for deterministic
// output.
func warnEnvHostPaths(stderr io.Writer, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := env[k]
		if hostPathPattern.MatchString(v) {
			fmt.Fprintf(stderr, "warning: env value %s=%s looks like a host filesystem path — not valid in container. Consider providing via a kit image layer.\n", k, v)
		}
	}
}
