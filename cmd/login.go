package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/profiles"
	"github.com/spf13/cobra"
)

// loginDeviceAuthTimeout / loginRevokeTimeout bound the two HTTP round
// trips login/logout make against a remote daemon — a hung daemon must not
// wedge the CLI forever.
const (
	loginDeviceAuthTimeout = 30 * time.Second
	loginRevokeTimeout     = 30 * time.Second
)

// deviceNameFlagName is the login-only flag naming the device label sent to
// POST /api/auth/device. Unlike --profile (below), there is no persistent
// flag of this name to collide with, so it is declared directly.
const deviceNameFlagName = "device-name"

// deviceAuthRequest / deviceAuthResponse mirror the wire JSON of POST
// /api/auth/device (internal/api/device_auth.go's unexported
// deviceAuthRequest/deviceAuthResponse, Phase 3 PR0). They are duplicated
// here rather than imported because the server's types are deliberately
// unexported — internal/api/device_auth.go is a server-internal handler,
// not a shared client/server contract package (contrast with
// internal/api/auth.PairRequest/PairResponse, which cmd/web.go DOES import
// because that pairing-code-issuance endpoint was designed to be shared).
// The wire shape is small and stable (fixed by PR0's ADR-like doc comments
// in device_auth.go), so keeping a second small struct in sync here is
// preferable to exporting server-internal types purely for this one CLI
// caller.
type deviceAuthRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name,omitempty"`
}

type deviceAuthResponse struct {
	DeviceID     string `json:"device_id"`
	Token        string `json:"token"`
	CanonicalURL string `json:"canonical_url,omitempty"`
}

var loginCmd = &cobra.Command{
	Use:   "login <url>",
	Short: "Pair this CLI with a remote boid daemon",
	Long: "Redeems a pairing code (issued by `boid web pair` on the daemon side)\n" +
		"for a long-lived Bearer device token, and saves it plus a connection\n" +
		"profile entry so future `boid` invocations can target that daemon via\n" +
		"--profile/BOID_PROFILE/default_profile.",
	Args: cobra.ExactArgs(1),
	// scopeNeutral + autostart=skip: login/logout require no profile
	// precondition at all (docs/plans/cli-remote-connection.md "コマンド分類"
	// table) — they are how a profile comes to exist in the first place.
	// root's PersistentPreRunE (isNeutralScope) swallows any profile
	// resolution failure for these two commands instead of hard-erroring.
	Annotations: map[string]string{
		scopeAnnotationKey:      scopeNeutral,
		annotationSkipAutostart: "skip",
	},
	RunE: runLogin,
}

var logoutCmd = &cobra.Command{
	Use:   "logout <profile>",
	Short: "Remove a connection profile and revoke its device token",
	Args:  cobra.ExactArgs(1),
	Annotations: map[string]string{
		scopeAnnotationKey:      scopeNeutral,
		annotationSkipAutostart: "skip",
	},
	RunE: runLogout,
}

func init() {
	loginCmd.Flags().String(deviceNameFlagName, "", "device name to register with the daemon (default: this machine's hostname)")
	rootCmd.AddCommand(loginCmd, logoutCmd)
}

// runLogin implements `boid login <url>` (docs/plans/cli-remote-connection.md
// "login / logout フロー"):
//
//  1. validate the URL is https:// (decision 4: plain http/unix are not a
//     supported login target — local daemons use a unix:// profile and
//     need no token at all)
//  2. derive the profile name from the URL's host when --profile is not
//     given explicitly (deriveProfileNameFromURL), then slug-validate it
//  3. warn (not silently overwrite) if that profile name already exists in
//     config.yaml
//  4. prompt for the pairing code on stderr, read it from stdin
//  5. POST /api/auth/device with an unauthenticated client (no token exists
//     yet — that is the whole point of this request)
//  6. persist the response as the token file (canonical_url — NOT the
//     literal URL the caller typed — decision 9: "token は canonical
//     origin に強く bind") and the config.yaml profile entry (also
//     canonical_url, so profiles.Resolve's origin-bind check at request
//     time is comparing the same value against itself)
//  7. set default_profile to this profile if none was set yet
func runLogin(cmd *cobra.Command, args []string) error {
	rawURL := strings.TrimSpace(args[0])
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("login: invalid URL %q: %w", rawURL, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("login: URL %q must use https:// (a local daemon uses a unix:// profile and does not need login)", rawURL)
	}

	// --profile reuses the SAME persistent flag rootCmd registers
	// (profiles.ProfileFlagName == "profile"), rather than declaring a
	// second flag under the same name on loginCmd: cobra merges a parent's
	// PersistentFlags into every descendant's FlagSet at parse time, so by
	// the time RunE runs, cmd.Flags().GetString here reads exactly what the
	// user typed on the command line either way — a locally shadowing
	// flag definition would not change that value, only its --help text
	// (see login.go's package doc / the PR's final report for the
	// alternative considered and why it was not worth the duplication).
	profileName, err := cmd.Flags().GetString(profiles.ProfileFlagName)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if profileName == "" {
		profileName, err = deriveProfileNameFromURL(parsed)
		if err != nil {
			return err
		}
	}
	if err := profiles.ValidateSlug(profileName); err != nil {
		return err
	}

	deviceName, err := cmd.Flags().GetString(deviceNameFlagName)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if deviceName == "" {
		if h, herr := os.Hostname(); herr == nil {
			deviceName = h
		}
		// os.Hostname() failing is rare and not fatal here: an empty
		// device_name in the request falls back to the pairing code's own
		// label server-side (internal/api/device_auth.go's PostDevice).
	}

	cfgPath, err := profiles.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := profiles.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	// Warn on ANY existing state that would be silently replaced — the
	// config.yaml entry AND the token file, either of which can outlive
	// the other (a partial or manually-tampered install, or the previous
	// login half-failed after WriteToken but before WriteConfig). Missing
	// this second signal on the token side would let a login silently
	// overwrite a still-issued device token — an easy way to lose the
	// ability to `logout` cleanly against that daemon.
	profileExisted := false
	if _, exists := cfg.Profiles[profileName]; exists {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: profile %q already exists in config.yaml; overwriting\n", profileName)
		profileExisted = true
	}
	if tokenPath, err := profiles.TokenPath(profileName); err == nil {
		if _, statErr := os.Stat(tokenPath); statErr == nil && !profileExisted {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: token file for profile %q already exists; overwriting\n", profileName)
		}
	}

	fmt.Fprint(cmd.ErrOrStderr(), "Enter pairing code: ")
	code, err := readLine(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("login: read pairing code: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("login: pairing code is required")
	}

	// No Bearer token exists yet — POST /api/auth/device is the public,
	// rate-limited endpoint that mints one from the pairing code.
	c, err := client.NewClient(rawURL, "")
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), loginDeviceAuthTimeout)
	defer cancel()
	req := deviceAuthRequest{Code: code, DeviceName: deviceName}
	var resp deviceAuthResponse
	if err := c.DoContext(ctx, "POST", "/api/auth/device", req, &resp); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if resp.CanonicalURL == "" {
		return fmt.Errorf("login: daemon response missing canonical_url")
	}
	// Defense-in-depth: the server-side WriteConfig path already runs
	// canonical_url through api.NormalizePublicURL at daemon startup, but
	// we validate on this side too because a compromised or misconfigured
	// daemon that returns a non-HTTPS or path-carrying canonical_url would
	// otherwise persist that garbage into the caller's config.yaml (and
	// then all future Bearer requests would go somewhere unexpected).
	// Rejecting up front keeps the token file and config.yaml in the
	// well-formed-origin invariant this whole flow depends on.
	canonicalURL, err := api.NormalizePublicURL(resp.CanonicalURL)
	if err != nil {
		return fmt.Errorf("login: daemon returned invalid canonical_url %q: %w", resp.CanonicalURL, err)
	}
	if canonicalURL == "" {
		// NormalizePublicURL treats "" as "not set" (nil error); the
		// resp.CanonicalURL != "" check above already ruled that out, so
		// hitting this arm means the daemon returned something that
		// normalized away entirely — treat as malformed.
		return fmt.Errorf("login: daemon returned unusable canonical_url %q", resp.CanonicalURL)
	}

	tok := &profiles.Token{
		DeviceID: resp.DeviceID,
		Token:    resp.Token,
		IssuedAt: time.Now().UTC(),
		URL:      canonicalURL,
	}

	// MutateConfig serializes the read-modify-write against config.yaml
	// under a flock (see profiles/write.go's MutateConfig doc), so a
	// concurrent `boid login` / `boid logout` against another profile
	// cannot lose this one's addition to the mapping. WriteToken is
	// called INSIDE the mutator (same lock) so a same-profile concurrent
	// login cannot end up with a token file whose URL disagrees with the
	// config entry — the two files always advance together.
	if err := profiles.MutateConfig(cfgPath, func(cur *profiles.Config) (*profiles.Config, error) {
		if err := profiles.WriteToken(profileName, tok); err != nil {
			return nil, err
		}
		newCfg := profiles.SetProfile(cur, profileName, profiles.Profile{URL: canonicalURL})
		if newCfg.DefaultProfile == "" {
			newCfg.DefaultProfile = profileName
		}
		return newCfg, nil
	}); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "logged in to %s (%s)\n", profileName, canonicalURL)
	return nil
}

// runLogout implements `boid logout <profile>` (docs/plans/
// cli-remote-connection.md "login / logout フロー"). Every step is
// individually best-effort/idempotent so a second `boid logout` on an
// already-cleaned-up profile — or one that was only ever half set up —
// never fails:
//
//   - no token file → warn, skip the daemon revoke call entirely (nothing
//     to authenticate the DELETE with)
//   - no config.yaml entry → warn, skip the config.yaml rewrite entirely
//     (nothing to remove, and — importantly — do not conjure a fresh empty
//     config.yaml into existence for a profile name that was never
//     registered, e.g. a typo)
//   - the daemon revoke call itself failing (network error, daemon down,
//     already revoked) is reported as a warning, not a hard error — the
//     local cleanup (token file, config.yaml entry) still proceeds either
//     way, so a stuck token can always be forgotten locally even if the
//     daemon can't be reached to tell it to forget too
func runLogout(cmd *cobra.Command, args []string) error {
	profileName := strings.TrimSpace(args[0])
	if err := profiles.ValidateSlug(profileName); err != nil {
		return err
	}

	cfgPath, err := profiles.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := profiles.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	_, hasProfile := cfg.Profiles[profileName]

	tok, tokErr := profiles.LoadToken(profileName)
	if !hasProfile {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: profile %q not found in config.yaml; skipping config cleanup\n", profileName)
	}
	// Revoke on the daemon the token was ISSUED against (tok.URL), not
	// wherever config.yaml happens to point now — this stays valid even
	// when config was deleted or edited between login and logout. See
	// revokeDeviceOnDaemon's own doc comment for the cross-origin
	// leakage this ordering is guarding against.
	switch {
	case tokErr != nil && os.IsNotExist(tokErr):
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: no token file for profile %q; skipping daemon revoke\n", profileName)
	case tokErr != nil:
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not read token file for profile %q (%v); skipping daemon revoke\n", profileName, tokErr)
	default:
		revokeDeviceOnDaemon(cmd, tok)
	}

	// Both DeleteToken and the config.yaml mutation happen under the
	// shared config lock, so a same-profile concurrent login cannot end
	// up with only one of the two rolled back — token and config advance
	// together (codex PR2 review round 2). The MutateConfig block runs
	// unconditionally now: even when the earlier LoadConfig showed no
	// entry, another writer may have added one by the time we grab the
	// lock (rare, but possible), and we want THAT entry cleaned up too
	// — the mutator's own recheck handles the no-op case.
	err = profiles.MutateConfig(cfgPath, func(cur *profiles.Config) (*profiles.Config, error) {
		if err := profiles.DeleteToken(profileName); err != nil {
			return nil, err
		}
		if _, present := cur.Profiles[profileName]; !present {
			// Nothing to write to config.yaml.
			return nil, nil
		}
		newCfg := profiles.RemoveProfile(cur, profileName)
		if newCfg.DefaultProfile == profileName {
			newCfg.DefaultProfile = ""
		}
		return newCfg, nil
	})
	if err != nil {
		return fmt.Errorf("logout: %w", err)
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "logged out from %s\n", profileName)
	return nil
}

// revokeDeviceOnDaemon calls DELETE /api/auth/devices/<device_id> to revoke
// tok on the daemon it was originally issued against — the URL is taken
// from tok.URL (the canonical_url that came back from POST /api/auth/device
// at login time), NOT the profile's URL from config.yaml. This matters
// because Resolve's "profile URL == token URL" hard-error is bypassed for
// scope=neutral commands (root.go's PersistentPreRunE), and blindly sending
// the Bearer token to the profile URL when the two diverge would leak it
// cross-origin: an attacker who managed to edit config.yaml to point the
// profile at their own host would then receive the token the next time
// this user typed `boid logout` (docs/plans/cli-remote-connection.md
// 決定事項 9 "token は canonical origin に強く bind" — same rule applies
// to logout as to every other request). tok.URL is that canonical origin.
//
// Any failure (building the client, making the request) is reported as a
// warning on stderr and otherwise ignored — runLogout's caller proceeds
// with local cleanup regardless (see runLogout's own doc comment for why).
func revokeDeviceOnDaemon(cmd *cobra.Command, tok *profiles.Token) {
	c, err := client.NewClient(tok.URL, tok.Token)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke device on daemon (%v); token file will still be removed locally\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), loginRevokeTimeout)
	defer cancel()
	if err := c.DoContext(ctx, "DELETE", "/api/auth/devices/"+tok.DeviceID, nil, nil); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke device on daemon (%v); token file will still be removed locally\n", err)
	}
}

// deriveProfileNameFromURL implements decision 3 / login step 1
// (docs/plans/cli-remote-connection.md): "--profile 未指定なら URL host か
// ら候補生成 (例: work.example.com → work)" — the first dot-separated label
// of the host, lowercased so it satisfies ValidateSlug's lowercase-only
// pattern regardless of how the URL was typed. A host with no dot at all
// (bare hostname like "localhost", or an IPv4 literal's first octet) is
// used whole.
func deriveProfileNameFromURL(u *url.URL) (string, error) {
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("login: URL %q has no host to derive a profile name from; pass --profile explicitly", u.String())
	}
	label := host
	if i := strings.IndexByte(host, '.'); i > 0 {
		label = host[:i]
	}
	return strings.ToLower(label), nil
}

// readLine reads a single line from r (the pairing-code prompt's stdin),
// tolerating a final line with no trailing newline (io.EOF right after the
// last byte) — the common case in a test harness feeding
// strings.NewReader("CODE") with no "\n", and also for a real terminal if
// the user's shell/pty happens to deliver EOF without one.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}
