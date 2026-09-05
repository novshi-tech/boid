package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// config.go implements `boid config get/set/unset/apply/edit`. Every
// subcommand reaches the daemon's config.yaml exclusively through the
// daemon's HTTP API (internal/api/config.go), never by editing a local
// file — once the daemon runs in its own container with a named-volume
// config.yaml, this host cannot see that file at all. This is a deliberate
// split from `boid web set-addr`/`set-url` (cmd/web.go), which edit
// ~/.config/boid/config.yaml on this host directly.
//
// Two distinct daemon-side surfaces back this:
//   - `set`/`unset` POST a single {op, key, value} to POST
//     /api/config/mutate; the daemon performs the whole read-modify-write
//     atomically under one lock, so concurrent calls on different keys
//     can never interleave and lose one.
//   - `apply -f`/`edit` operate on the whole document (GET current, apply
//     a local/edited replacement) via POST /api/config, gated by an
//     ETag/If-Match revision check unless --force is passed.
//
// See configCmd.Long for the scope note (secret_key is a reference, not
// the secret value) and the dotted-path limitation on forge ids containing ".".
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect and edit the daemon's config.yaml",
	Long: `boid config reads and writes the daemon's config.yaml through its
HTTP API, not by editing a local file on this host — config.yaml lives with
the daemon (a named volume once the volume-only cutover lands), which a
local editor cannot reach directly.

Scope note: this command edits config.yaml itself — including
gateway.forges.<forge>.secret_key, which is just a REFERENCE NAME into the
secret store. It does NOT edit the actual secret VALUE that name resolves
to at dispatch time (the token/PAT itself); that lives in the secret store
(` + "`boid secret set <key> <value>`" + `), separately from config.yaml.

Limitation: a forge id containing a "." (e.g. a custom "github.corp" entry)
cannot be addressed via get/set/unset's dotted-path syntax — the dot is
indistinguishable from a path separator. Use ` + "`boid config apply -f`" + `
(or ` + "`edit`" + `) for those entries instead.`,
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
	Short: "Remove a key (gateway.forges.<id> or services.<name> with no further segment removes the whole entry)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigUnset,
}

var configApplyFile string
var configApplyForce bool

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
	// --force mirrors `boid workspace edit --force`'s semantics: skip the
	// ETag/If-Match revision check and apply unconditionally (last-write-wins).
	configApplyCmd.Flags().BoolVar(&configApplyForce, "force", false,
		"apply without checking the current revision (last-write-wins; skips the concurrency guard)")

	configCmd.AddCommand(configGetCmd, configSetCmd, configUnsetCmd, configApplyCmd, configEditCmd)
	rootCmd.AddCommand(configCmd)
}

// fetchConfigYAML performs GET /api/config, returning the daemon's current
// effective config.yaml document verbatim alongside its revision — the
// value a subsequent apply/edit round-trips into If-Match.
func fetchConfigYAML(c *client.Client) (data []byte, revision string, err error) {
	status, body, rev, err := c.GetRawWithAcceptAndRevision("/api/config", "application/yaml")
	if err != nil {
		return nil, "", fmt.Errorf("fetch config: %w", err)
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("fetch config: %s", formatWorkspaceAPIError(status, body))
	}
	return body, rev, nil
}

// isConfigConflictStatus reports whether statusCode is one of the two
// ETag/If-Match failure codes ApplyConfigYAML can return — 428 Precondition
// Required (If-Match missing) or 412 Precondition Failed (If-Match stale).
func isConfigConflictStatus(statusCode int) bool {
	return statusCode == http.StatusPreconditionRequired || statusCode == http.StatusPreconditionFailed
}

// reportConfigApplyResult decodes a POST /api/config 200 response body and
// prints the result: a confirmation line, followed by every daemon-provided
// warning verbatim (the daemon is the sole source of truth for which
// changed keys triggered which warning).
func reportConfigApplyResult(cmd *cobra.Command, body []byte) error {
	var result apiwire.ConfigApplyResult
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
	data, _, err := fetchConfigYAML(c)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprint(cmd.OutOrStdout(), string(data))
		return nil
	}

	tree, err := config.ParseTree(data)
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

// runConfigSet implements `boid config set`: a single POST
// /api/config/mutate call — the daemon performs the read-modify-write
// atomically, so this function never GETs the current document at all
// (contrast runConfigApply/runConfigEdit, which need the whole document
// and keep the GET-then-POST shape, protected by If-Match).
func runConfigSet(cmd *cobra.Command, args []string) error {
	path := args[0]
	values := args[1:]

	c := client.FromContext(cmd.Context())
	var result apiwire.ConfigMutateResult
	if err := c.Do(http.MethodPost, "/api/config/mutate", apiwire.ConfigMutateRequest{
		Op:    apiwire.ConfigMutateSet,
		Key:   path,
		Value: values,
	}, &result); err != nil {
		return fmt.Errorf("set %s: %w", path, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "config applied")
	for _, w := range result.Warnings {
		fmt.Fprintln(out, w)
	}
	return nil
}

// runConfigUnset implements `boid config unset` — see runConfigSet's doc
// comment; the unset half of the same server-side mutate endpoint.
func runConfigUnset(cmd *cobra.Command, args []string) error {
	path := args[0]

	c := client.FromContext(cmd.Context())
	var result apiwire.ConfigMutateResult
	if err := c.Do(http.MethodPost, "/api/config/mutate", apiwire.ConfigMutateRequest{
		Op:  apiwire.ConfigMutateUnset,
		Key: path,
	}, &result); err != nil {
		return fmt.Errorf("unset %s: %w", path, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "config applied")
	for _, w := range result.Warnings {
		fmt.Fprintln(out, w)
	}
	return nil
}

// runConfigApply implements `boid config apply -f`: fetches the daemon's
// current revision FIRST — before reading or validating the local file —
// then POSTs the file gated by that revision as If-Match unless --force is
// passed.
//
// The GET-before-ReadFile ordering is load-bearing: reading the file first
// would let a stale `data` pair with a fresh `ifMatch` and sail through the
// daemon's own If-Match check undetected, silently discarding a concurrent
// write. See
// TestRunConfigApply_EndToEnd_ConcurrentSet_GETPrecedesReadFile_NoSilentLoss
// for the regression test protecting this ordering.
//
// A malformed/invalid file is rejected client-side before any POST is
// attempted — see TestRunConfigApply_InvalidFile_ValidationErrorReported.
func runConfigApply(cmd *cobra.Command, args []string) error {
	if configApplyFile == "" {
		return fmt.Errorf("-f/--file is required")
	}

	c := client.FromContext(cmd.Context())

	var ifMatch string
	if !configApplyForce {
		_, rev, err := fetchConfigYAML(c)
		if err != nil {
			return fmt.Errorf("fetch current revision: %w", err)
		}
		ifMatch = rev
	}

	// configApplyTestSyncAfterGET is nil (a no-op) in production. It exists
	// solely so the regression test can deterministically land a concurrent
	// `config set` in the exact [GET, ReadFile] window this ordering
	// narrows the race to, rather than relying on an unforced goroutine race.
	if configApplyTestSyncAfterGET != nil {
		configApplyTestSyncAfterGET()
	}

	data, err := os.ReadFile(configApplyFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", configApplyFile, err)
	}

	if _, err := config.ValidateYAML(data); err != nil {
		return fmt.Errorf("validate %s: %w", configApplyFile, err)
	}

	return postConfigApply(cmd, c, data, ifMatch, configApplyForce, configApplyFile)
}

// configApplyTestSyncAfterGET, when set by a test, is called synchronously
// immediately after runConfigApply's GET (and before it reads
// configApplyFile off disk) — see runConfigApply's own doc comment. Always
// nil outside of tests.
var configApplyTestSyncAfterGET func()

// postConfigApply POSTs data to /api/config with the given If-Match/force
// and reports the result. Factored out so the conflict-rejection contract
// can be tested directly with a deliberately stale ifMatch. fileForMessage
// is threaded through explicitly rather than read from the package var so
// this function has no hidden dependency on CLI global state.
func postConfigApply(cmd *cobra.Command, c *client.Client, data []byte, ifMatch string, force bool, fileForMessage string) error {
	path := "/api/config"
	if force {
		path += "?force=true"
	}
	statusCode, body, err := c.PostRawWithIfMatch(path, "application/yaml", data, ifMatch)
	if err != nil {
		return fmt.Errorf("apply config: %w", err)
	}
	if isConfigConflictStatus(statusCode) {
		return fmt.Errorf("config changed since %s was validated; re-run `boid config apply -f %s` (or pass --force to overwrite unconditionally): %s",
			fileForMessage, fileForMessage, formatWorkspaceAPIError(statusCode, body))
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("apply config: %s", formatWorkspaceAPIError(statusCode, body))
	}
	return reportConfigApplyResult(cmd, body)
}

// runConfigEdit implements `boid config edit`: fetch the current config
// (capturing its revision), open it in $EDITOR (falling back to "vi") on a
// temp copy, and — only if the file actually changed — validate then apply
// with If-Match set to the revision captured at the start. If the config
// changed on the daemon between this GET and the POST, the conflict is
// reported and the temp file is kept so the operator can re-run `boid
// config edit` and merge their changes: any validation OR conflict failure
// keeps the temp file and reports its path, rather than silently discarding
// the edit.
func runConfigEdit(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	data, rev, err := fetchConfigYAML(c)
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

	statusCode, body, err := c.PostRawWithIfMatch("/api/config", "application/yaml", edited, rev)
	if err != nil {
		return fmt.Errorf("apply config: %w (edited config kept at %s — fix it and rerun `boid config apply -f %s`)", err, tmpPath, tmpPath)
	}
	if isConfigConflictStatus(statusCode) {
		return fmt.Errorf("config changed since edit started; re-run `boid config edit` and merge your changes (edited config kept at %s)", tmpPath)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("apply config: %s (edited config kept at %s — fix it and rerun `boid config apply -f %s`)", formatWorkspaceAPIError(statusCode, body), tmpPath, tmpPath)
	}
	if err := reportConfigApplyResult(cmd, body); err != nil {
		return fmt.Errorf("%w (edited config kept at %s — fix it and rerun `boid config apply -f %s`)", err, tmpPath, tmpPath)
	}
	_ = os.Remove(tmpPath)
	return nil
}
