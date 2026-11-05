package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func init() { rootCmd.AddCommand(newWakeupDefinitionCommand()) }

func newWakeupDefinitionCommand() *cobra.Command {
	root := &cobra.Command{Use: "wakeup-definition", Short: "Manage workspace and project wakeup rules"}
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a workspace or project wakeup rule",
		Long:  "Create a rule at workspace scope, or at project scope with --project. A rule an agent creates is stored disabled until an owner or admin enables it, whatever the config asks for. The result shows the stored state.",
		Args:  cobra.NoArgs,
		RunE:  runWakeupDefinitionCreate,
	}
	create.Flags().String("project", "", "Create the rule for this project (default: workspace scope)")
	create.Flags().String("config", "", "Rule config as JSON")
	create.Flags().String("config-file", "", "Read the rule config from a UTF-8 JSON file")
	root.AddCommand(create)
	return root
}

func runWakeupDefinitionCreate(cmd *cobra.Command, _ []string) error {
	config, ok, err := resolveTextFlag(cmd, "config")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("--config or --config-file is required")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path := "/api/wakeup-definitions"
	if project, _ := cmd.Flags().GetString("project"); project != "" {
		ref, err := resolveProjectID(ctx, client, project)
		if err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(ref.ID) + "/wakeup-definitions"
	}
	var row struct {
		Config map[string]any `json:"config"`
	}
	var raw json.RawMessage
	if err := client.PostJSON(ctx, path, map[string]json.RawMessage{"config": json.RawMessage(config)}, &raw); err != nil {
		return err
	}
	_ = json.Unmarshal(raw, &row)
	if enabled, _ := row.Config["enabled"].(bool); !enabled {
		fmt.Fprintln(cmd.ErrOrStderr(), "Rule stored disabled: it will not fire until an owner or admin enables it.")
	}
	return cli.PrintJSON(cmd.OutOrStdout(), raw)
}
