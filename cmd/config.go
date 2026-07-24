package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// config.go implements `boid config get/set/unset/apply/edit`
// (docs/plans/volume-only-daemon.md §論点 f, the CLI half — the Web UI's
// /settings page is a separate PR). Every subcommand reaches the daemon's
// config.yaml exclusively through GET/POST /api/config
// (internal/api/config.go) rather than editing a local file directly: once
// the daemon runs in its own container with a named-volume config.yaml
// (the whole point of this plan doc), this host cannot see that file at
// all — see the plan doc's §背景と pivot 経緯 for why. This is a
// deliberate architectural split from the older, still-present `boid web
// set-addr`/`set-url` (cmd/web.go), which edit ~/.config/boid/config.yaml
// on THIS host directly and only ever worked because the CLI and daemon
// happened to run on the same host with the same $XDG_CONFIG_HOME.
//
// `set`/`unset` both read-modify-write via a client-side round trip: GET
// the current effective config, mutate a local copy with
// internal/config's dotted-path Set/Unset (schema-validated locally, so an
// unknown key or bad value is rejected before any daemon round trip), then
// POST the whole document back. The daemon (internal/server/config_edit.go)
// re-validates independently and is the sole authority on which changed
// keys were actually reloaded live vs which need a restart — see
// ConfigApplyResult.Warnings.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect and edit the daemon's config.yaml",
	Long: `boid config reads and writes the daemon's config.yaml through its
HTTP API (GET/POST /api/config), not by editing a local file on this host —
config.yaml lives with the daemon (a named volume once the volume-only
cutover lands), which a local editor cannot reach directly.

Scope note: this command edits config.yaml itself — including
gateway.forges.<forge>.secret_key, which is just a REFERENCE NAME into the
secret store. It does NOT edit the actual secret VALUE that name resolves
to at dispatch time (the token/PAT itself); that lives in the secret store
(` + "`boid secret set <key> <value>`" + `), separately from config.yaml.`,
}

var configGetCmd = &cobra.Command{
	Use:   "get [dotted.key]",
	Short: "Print the full config as YAML, or a single dotted key's value",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runConfigGet,
}

var configSetCmd = &cobra.Command{
	Use:   "set <dotted.key> <value> [value...]",
	Short: "Set a scalar value (1 arg) or replace an array wholesale (multiple args)",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runConfigSet,
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <dotted.key>",
	Short: "Remove a key (gateway.forges.<id> with no further segment removes the whole entry)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigUnset,
}

var configApplyFile string

var configApplyCmd = &cobra.Command{
	Use:   "apply -f <file>",
	Short: "Apply a full config.yaml from file (validated locally first, then by the daemon)",
	Args:  cobra.NoArgs,
	RunE:  runConfigApply,
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit the config in $EDITOR (or vi), validating on save before applying",
	Args:  cobra.NoArgs,
	RunE:  runConfigEdit,
}

func init() {
	// Every config subcommand talks to the daemon's HTTP API — all
	// scopeRemote, same classification as `boid workspace *`.
	for _, c := range []*cobra.Command{configGetCmd, configSetCmd, configUnsetCmd, configApplyCmd, configEditCmd} {
		c.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	}
	configApplyCmd.Flags().StringVarP(&configApplyFile, "file", "f", "", "yaml file to apply (required)")

	configCmd.AddCommand(configGetCmd, configSetCmd, configUnsetCmd, configApplyCmd, configEditCmd)
	rootCmd.AddCommand(configCmd)
}

// fetchConfigYAML performs GET /api/config, returning the daemon's current
// effective config.yaml document verbatim.
func fetchConfigYAML(c *client.Client) ([]byte, error) {
	status, body, err := c.GetRawWithAccept("/api/config", "application/yaml")
	if err != nil {
		return nil, fmt.Errorf("fetch config: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("fetch config: %s", formatWorkspaceAPIError(status, body))
	}
	return body, nil
}

// parseConfigTree decodes a GET /api/config response body into the generic
// Tree dotted-path operations act on, guaranteeing a non-nil map even for
// a genuinely empty document (a fresh install with no config.yaml written
// yet — GET /api/config returns the on-disk file verbatim, sparse, not a
// defaults-expanded view; see internal/server/config_edit.go's
// ConfigYAML doc comment for why).
func parseConfigTree(data []byte) (config.Tree, error) {
	var tree config.Tree
	if err := yaml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if tree == nil {
		tree = config.Tree{}
	}
	return tree, nil
}

// configApplyResponse mirrors api.ConfigApplyResult (internal/client's own
// architecture rule forbids importing internal/api's behavior — see
// internal/client/architecture_test.go — so this is a hand-kept copy of
// just the wire shape, the same convention internal/client/client.go's
// wsAttachClientMsg/wsAttachServerMsg already use for ws_attach.go's frames).
type configApplyResponse struct {
	Warnings []string `json:"warnings"`
}

// applyConfigAndReport POSTs data to /api/config and prints the result:
// a confirmation line, followed by every daemon-provided warning verbatim
// (docs/plans/volume-only-daemon.md §論点 f's exact restart-required /
// sandbox.backend-retirement wording — the daemon is the sole source of
// truth for which changed keys triggered which warning, see
// internal/server/config_edit.go's applyDynamicConfigLocked).
func applyConfigAndReport(cmd *cobra.Command, c *client.Client, data []byte) error {
	status, body, err := c.PostRaw("/api/config", "application/yaml", data)
	if err != nil {
		return fmt.Errorf("apply config: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("apply config: %s", formatWorkspaceAPIError(status, body))
	}
	var result configApplyResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "config applied")
	for _, w := range result.Warnings {
		fmt.Fprintln(out, w)
	}
	return nil
}

func runConfigGet(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	data, err := fetchConfigYAML(c)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprint(cmd.OutOrStdout(), string(data))
		return nil
	}

	tree, err := parseConfigTree(data)
	if err != nil {
		return err
	}
	value, err := config.Get(tree, args[0])
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	fmt.Fprint(cmd.OutOrStdout(), string(out))
	return nil
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	path := args[0]
	values := args[1:]

	c := client.FromContext(cmd.Context())
	data, err := fetchConfigYAML(c)
	if err != nil {
		return err
	}
	tree, err := parseConfigTree(data)
	if err != nil {
		return err
	}
	if _, err := config.Set(tree, path, values); err != nil {
		return err
	}
	newData, err := yaml.Marshal(tree)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return applyConfigAndReport(cmd, c, newData)
}

func runConfigUnset(cmd *cobra.Command, args []string) error {
	path := args[0]

	c := client.FromContext(cmd.Context())
	data, err := fetchConfigYAML(c)
	if err != nil {
		return err
	}
	tree, err := parseConfigTree(data)
	if err != nil {
		return err
	}
	if _, err := config.Unset(tree, path); err != nil {
		return err
	}
	newData, err := yaml.Marshal(tree)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return applyConfigAndReport(cmd, c, newData)
}

func runConfigApply(cmd *cobra.Command, args []string) error {
	if configApplyFile == "" {
		return fmt.Errorf("-f/--file is required")
	}
	data, err := os.ReadFile(configApplyFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", configApplyFile, err)
	}

	// Client-side pre-check (mirrors `boid workspace edit`'s MINOR 1
	// precedent, cmd/workspace.go's runWorkspaceEdit: fail fast on a bad
	// file before making any daemon call at all).
	if _, err := config.ValidateYAML(data); err != nil {
		return fmt.Errorf("validate %s: %w", configApplyFile, err)
	}

	c := client.FromContext(cmd.Context())
	return applyConfigAndReport(cmd, c, data)
}

// runConfigEdit implements `boid config edit`: fetch the current config,
// open it in $EDITOR (falling back to "vi") on a temp copy, and — only if
// the file actually changed — validate then apply. Per docs/plans/
// volume-only-daemon.md §論点 f's unilateral decision on edit-failure
// behavior: a validation failure (locally OR at the daemon) keeps the temp
// file and reports its path so the operator can fix it and rerun
// `boid config apply -f <path>`, rather than silently discarding the edit.
func runConfigEdit(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	data, err := fetchConfigYAML(c)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "boid-config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("$EDITOR is set to an empty/whitespace-only value")
	}
	editCmd := exec.Command(parts[0], append(append([]string(nil), parts[1:]...), tmpPath)...)
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr
	if err := editCmd.Run(); err != nil {
		return fmt.Errorf("run editor %q: %w (edited config kept at %s)", editor, err, tmpPath)
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("read edited file: %w", err)
	}

	if bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(edited)) {
		_ = os.Remove(tmpPath)
		fmt.Fprintln(cmd.OutOrStdout(), "no changes")
		return nil
	}

	if _, err := config.ValidateYAML(edited); err != nil {
		return fmt.Errorf("validation failed (edited config kept at %s — fix it and rerun `boid config apply -f %s`): %w", tmpPath, tmpPath, err)
	}

	if err := applyConfigAndReport(cmd, c, edited); err != nil {
		return fmt.Errorf("%w (edited config kept at %s — fix it and rerun `boid config apply -f %s`)", err, tmpPath, tmpPath)
	}
	_ = os.Remove(tmpPath)
	return nil
}
