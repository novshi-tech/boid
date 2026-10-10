package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/novshi-tech/boid/internal/checkout"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use: "checkout [name]", Short: "Clone a registered repository inside the sandbox", Args: cobra.MaximumNArgs(1),
		Annotations:       map[string]string{scopeAnnotationKey: scopeLocal, annotationSkipAutostart: "skip"},
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := os.Getenv("BOID_PRIMARY_REPO")
			if len(args) > 0 {
				resp, err := sandbox.RunBoidShim([]string{"project", "list"})
				if err != nil {
					return err
				}
				if resp.ExitCode != 0 {
					return fmt.Errorf("project list: %s", resp.Stderr)
				}
				repo, err = checkoutNamedRepo(args[0], []byte(resp.Stdout))
				if err != nil {
					return err
				}
			}
			path, err := checkout.Run(cmd.Context(), repo, os.Getenv("BOID_GIT_BASE"), os.Getenv("BOID_GIT_CACHE"), "/workspace", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	})
}

func checkoutNamedRepo(name string, data []byte) (string, error) {
	var projects []struct {
		Name        string `json:"name"`
		UpstreamURL string `json:"upstream_url"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return "", fmt.Errorf("project list: %w", err)
	}
	upstream := ""
	matches := 0
	for _, p := range projects {
		if p.Name == name {
			upstream = p.UpstreamURL
			matches++
		}
	}
	if matches != 1 {
		return "", fmt.Errorf("registered repository %q: expected one match, found %d", name, matches)
	}
	var repo string
	if strings.Contains(upstream, "://") {
		u, err := url.Parse(upstream)
		if err != nil {
			return "", err
		}
		host := u.Host
		if u.Scheme == "ssh" {
			host = u.Hostname()
		}
		repo = host + "/" + strings.TrimPrefix(u.Path, "/")
	} else {
		_, rest, _ := strings.Cut(upstream, "@")
		if rest == "" {
			rest = upstream
		}
		host, path, ok := strings.Cut(rest, ":")
		if !ok {
			return "", fmt.Errorf("repository %q has no usable upstream URL", name)
		}
		repo = host + "/" + path
	}
	repo = strings.TrimSuffix(repo, ".git")
	if err := checkout.ValidateRepo(repo); err != nil {
		return "", err
	}
	return repo, nil
}
