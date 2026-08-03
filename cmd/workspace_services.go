package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// workspaceServicesCmd groups the API gateway service-enablement CLI vocab
// docs/plans/api-gateway.md §論点5 confirms: "boid workspace services
// add/remove/list <ws> <service> 系。allowed_domains の既存語彙に揃える" —
// mirroring the add/remove/list shape (rather than a single --from-file
// whole-document replace, the way allowed_domains/extra_repos are edited
// today) since granular single-service enable/disable is the expected
// common case for this field specifically.
var workspaceServicesCmd = &cobra.Command{
	Use:   "services",
	Short: "Manage a workspace's enabled API gateway services (docs/plans/api-gateway.md)",
}

var workspaceServicesAddCmd = &cobra.Command{
	Use:   "add <slug> <service> [service...]",
	Short: "Enable one or more API gateway services for a workspace (GET + POST /api/workspaces/apply)",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runWorkspaceServicesAdd,
}

var workspaceServicesRemoveCmd = &cobra.Command{
	Use:   "remove <slug> <service> [service...]",
	Short: "Disable one or more API gateway services for a workspace (GET + POST /api/workspaces/apply)",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runWorkspaceServicesRemove,
}

var workspaceServicesListCmd = &cobra.Command{
	Use:   "list <slug>",
	Short: "List a workspace's own enabled API gateway services (GET /api/workspaces/{slug})",
	Long: `List the API gateway services THIS workspace enables on top of the daemon-wide
services_floor (config.yaml) — mirrors the workspace's own allowed_domains list, not the
effective (floor ∪ workspace) set a job actually dispatches with. The floor itself is not
exposed by this command (allowed_domains has no equivalent "show effective" surface either).`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceServicesList,
}

func init() {
	for _, c := range []*cobra.Command{workspaceServicesAddCmd, workspaceServicesRemoveCmd, workspaceServicesListCmd} {
		c.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	}
	workspaceServicesCmd.AddCommand(workspaceServicesAddCmd, workspaceServicesRemoveCmd, workspaceServicesListCmd)
	workspaceCmd.AddCommand(workspaceServicesCmd)
}

// runWorkspaceServicesAdd enables the named services for slug, in addition
// to whatever it already has enabled (deduplicated).
func runWorkspaceServicesAdd(cmd *cobra.Command, args []string) error {
	return mutateWorkspaceServices(cmd, args[0], args[1:], addServiceNames)
}

// runWorkspaceServicesRemove disables the named services for slug. Removing
// a name that isn't currently enabled is a no-op, not an error — mirrors
// the general "idempotent set edit" shape the sibling allowed_domains/
// extra_repos fields already have via whole-document replace.
func runWorkspaceServicesRemove(cmd *cobra.Command, args []string) error {
	return mutateWorkspaceServices(cmd, args[0], args[1:], removeServiceNames)
}

// runWorkspaceServicesList shows slug's own Services list (GET /api/workspaces/{slug}).
func runWorkspaceServicesList(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())
	var detail api.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &detail); err != nil {
		return fmt.Errorf("show workspace: %w", err)
	}
	var services []string
	if detail.Meta != nil {
		services = detail.Meta.Services
	}

	return renderOutput(cmd, services, func() error {
		out := cmd.OutOrStdout()
		if len(services) == 0 {
			fmt.Fprintln(out, "no services enabled for this workspace (config.yaml services_floor entries, if any, still apply)")
			return nil
		}
		for _, s := range services {
			fmt.Fprintln(out, s)
		}
		return nil
	})
}

// addServiceNames returns current with each of names appended, deduplicated
// (case-sensitive — service names are config.yaml map keys, not domains;
// see orchestrator.ResolveEnabledServices's own doc comment for the same
// distinction at resolution time). Order is preserved: existing entries
// first, then newly-added ones in the order given.
func addServiceNames(current, names []string) []string {
	seen := make(map[string]bool, len(current)+len(names))
	out := make([]string, 0, len(current)+len(names))
	for _, s := range current {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range names {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// removeServiceNames returns current with every entry in names dropped,
// preserving the relative order of what remains.
func removeServiceNames(current, names []string) []string {
	remove := make(map[string]bool, len(names))
	for _, s := range names {
		remove[s] = true
	}
	out := make([]string, 0, len(current))
	for _, s := range current {
		if !remove[s] {
			out = append(out, s)
		}
	}
	return out
}

// mutateWorkspaceServices implements the shared GET-then-apply shape both
// add and remove need.
//
// This deliberately goes through POST /api/workspaces/apply (the envelope
// path, `boid workspace apply`'s own endpoint) rather than a GET-full-meta +
// PUT /api/workspaces/{slug} (the create/edit path `boid workspace edit`
// uses): PUT decodes through workspaceMetaStrict
// (internal/orchestrator/workspace_meta_strict.go), which — by that type's
// own documented "PR3 の deliberate scope boundary" — has NO field for
// task_behaviors / base_branch / fork_point / default_task_behavior. A
// GET-then-PUT round trip of the FULL meta therefore either 400s outright
// ("field task_behaviors not found in type orchestrator.workspaceMetaStrict",
// confirmed empirically) or, had the strict decoder tolerated the extra
// keys, would have silently WIPED those four fields on every services
// add/remove — UpdateIfRevisionMatches performs an unconditional whole-row
// column write from whatever workspaceMetaStrict decoded, and that type has
// no field to carry them through. Neither outcome is acceptable for a
// command whose entire contract is "touch only the services list".
//
// The apply endpoint's envelope decode (decodeWorkspaceEnvelopeSpec) is
// presence-gated per spec.* key (WorkspaceEnvelopeApply.FieldsPresent) and
// DOES support task_behaviors/base_branch/fork_point/default_task_behavior.
// Submitting a document whose spec carries ONLY the "services" key — built
// here as a bare map, NOT by marshaling a orchestrator.WorkspaceEnvelopeSpec
// struct literal, since that type's fields deliberately carry no `omitempty`
// (missing-vs-empty is load-bearing there) and would emit every other field
// as an explicit empty value, clearing them exactly the way the PUT path
// would have — means every other field (host_commands, env, task_behaviors,
// projects, init_script, ...) is left completely untouched server-side.
func mutateWorkspaceServices(cmd *cobra.Command, slug string, names []string, transform func(current, names []string) []string) error {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())

	var current api.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &current); err != nil {
		return fmt.Errorf("fetch workspace: %w", err)
	}
	var currentServices []string
	if current.Meta != nil {
		currentServices = current.Meta.Services
	}
	newServices := transform(currentServices, names)

	doc := map[string]any{
		"apiVersion": orchestrator.WorkspaceEnvelopeAPIVersion,
		"kind":       orchestrator.WorkspaceEnvelopeKind,
		"metadata":   map[string]any{"name": slug},
		"spec":       map[string]any{"services": newServices},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode workspace envelope: %w", err)
	}

	statusCode, respBody, err := c.PostRaw("/api/workspaces/apply", "application/yaml", body)
	if err != nil {
		return fmt.Errorf("update workspace: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("update workspace: %s", formatWorkspaceAPIError(statusCode, respBody))
	}

	var result orchestrator.WorkspaceApplyResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return renderOutput(cmd, &result, func() error {
		var services []string
		if result.Meta != nil {
			services = result.Meta.Services
		}
		fmt.Fprintf(cmd.OutOrStdout(), "workspace %s services: %s\n", result.Slug, formatStringSlice(services))
		return nil
	})
}
