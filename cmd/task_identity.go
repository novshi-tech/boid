package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

func init() {
	identity := &cobra.Command{Use: "identity", Short: "Manage task identity bindings"}
	for _, operation := range []string{"link", "unlink", "resolve"} {
		use := operation + " <identity>"
		count := 1
		if operation == "link" {
			count = 2
			use += " <task-id>"
		}
		command := &cobra.Command{Use: use, Args: cobra.ExactArgs(count), RunE: func(cmd *cobra.Command, args []string) error { return runIdentityCommand(cmd, args, operation) }}
		addIdentityScopeFlags(command)
		if operation == "link" {
			addIdentityMetadataFlags(command)
		}
		identity.AddCommand(command)
	}
	capture := &cobra.Command{Use: "resolve-or-capture <identity>", Short: "Resolve an identity or capture a new card", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return runIdentityCommand(cmd, args, "capture") }}
	addIdentityScopeFlags(capture)
	capture.Flags().String("title", "", "Title for a new card")
	capture.Flags().String("description", "", "Description for a new card")
	capture.Flags().String("description-file", "", "Read the description from a file")
	capture.MarkFlagsMutuallyExclusive("description", "description-file")
	addIdentityMetadataFlags(capture)
	taskCmd.AddCommand(identity, capture)
}

func addIdentityScopeFlags(command *cobra.Command) {
	command.Flags().String("project-id", "", "Repository scope (defaults to workspace only)")
	command.Flags().String("workspace", "default", "Workspace for repository-free identities")
	command.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	command.MarkFlagsMutuallyExclusive("project-id", "workspace")
}

func runIdentityCommand(cmd *cobra.Command, args []string, operation string) error {
	projectID, _ := cmd.Flags().GetString("project-id")
	workspaceID, _ := cmd.Flags().GetString("workspace")
	c := client.FromContext(cmd.Context())
	if projectID != "" {
		p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), projectID)
		if err != nil {
			return err
		}
		projectID = p.ID
		workspaceID = ""
	}
	var url, displayName *string
	if cmd.Flags().Changed("url") {
		value, _ := cmd.Flags().GetString("url")
		url = &value
	}
	if cmd.Flags().Changed("display-name") {
		value, _ := cmd.Flags().GetString("display-name")
		displayName = &value
	}
	var request any
	path := "/api/tasks/identity/" + operation
	if operation == "capture" {
		title, _ := cmd.Flags().GetString("title")
		description, _ := cmd.Flags().GetString("description")
		if path, _ := cmd.Flags().GetString("description-file"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			description = string(data)
		}
		request = api.ResolveOrCaptureRequest{ProjectID: projectID, WorkspaceID: workspaceID, Identity: args[0], Title: title, Description: description, URL: url, DisplayName: displayName}
		path = "/api/tasks/resolve-or-capture"
	} else {
		req := api.TaskIdentityRequest{ProjectID: projectID, WorkspaceID: workspaceID, Identity: args[0], URL: url, DisplayName: displayName}
		if operation == "link" {
			req.TaskID = args[1]
		}
		request = req
	}
	var result json.RawMessage
	if err := c.Do("POST", path, request, &result); err != nil {
		return err
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), string(result))
	return err
}

func addIdentityMetadataFlags(command *cobra.Command) {
	command.Flags().String("url", "", "Identity URL (an explicit empty value clears it)")
	command.Flags().String("display-name", "", "Identity display name")
}
