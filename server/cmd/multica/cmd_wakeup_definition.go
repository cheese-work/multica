package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

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
	get := &cobra.Command{
		Use:   "get <rule>",
		Short: "Read a wakeup rule and its resolved value",
		Long:  "Print the effective definition of a rule: its resolved config, the scope each field came from (sources), the fields this scope overrides, and the runtime instance state (execution). Reads workspace scope, project scope with --project, or issue scope with --issue.",
		Args:  cobra.ExactArgs(1),
		RunE:  runWakeupDefinitionGet,
	}
	get.Flags().String("project", "", "Read the rule at this project (default: workspace scope)")
	get.Flags().String("issue", "", "Read the rule as resolved for this issue")
	get.Flags().String("output", "json", "Output format (json or table)")
	root.AddCommand(get)
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

func runWakeupDefinitionGet(cmd *cobra.Command, args []string) error {
	project, _ := cmd.Flags().GetString("project")
	issue, _ := cmd.Flags().GetString("issue")
	if project != "" && issue != "" {
		return fmt.Errorf("--project and --issue cannot be combined")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	base := "/api"
	switch {
	case project != "":
		ref, err := resolveProjectID(ctx, client, project)
		if err != nil {
			return err
		}
		base = "/api/projects/" + url.PathEscape(ref.ID)
	case issue != "":
		ref, err := resolveIssueRef(ctx, client, issue)
		if err != nil {
			return err
		}
		base = "/api/issues/" + url.PathEscape(ref.ID)
	}
	var raw json.RawMessage
	if err := client.GetJSON(ctx, base+"/wakeup-definitions/"+url.PathEscape(args[0])+"/effective", &raw); err != nil {
		return err
	}
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(cmd.OutOrStdout(), raw)
	}
	var eff struct {
		RuleKey   string            `json:"rule_key"`
		Scope     string            `json:"scope"`
		Enabled   bool              `json:"enabled"`
		Config    json.RawMessage   `json:"config"`
		Sources   map[string]string `json:"sources"`
		Overrides []string          `json:"overrides"`
		Execution *struct {
			Enabled      bool    `json:"enabled"`
			PausedReason *string `json:"paused_reason"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(raw, &eff); err != nil {
		return err
	}
	sources := make([]string, 0, len(eff.Sources))
	for field, scope := range eff.Sources {
		sources = append(sources, field+"="+scope)
	}
	sort.Strings(sources)
	execution := "none"
	if e := eff.Execution; e != nil {
		execution = fmt.Sprintf("enabled=%t", e.Enabled)
		if e.PausedReason != nil {
			execution += " paused=" + *e.PausedReason
		}
	}
	config := "none"
	var compact bytes.Buffer
	if len(eff.Config) > 0 && json.Compact(&compact, eff.Config) == nil {
		config = compact.String()
	}
	cli.PrintTable(cmd.OutOrStdout(), []string{"RULE", "SCOPE", "ENABLED", "CONFIG", "SOURCES", "OVERRIDES", "EXECUTION"}, [][]string{{
		eff.RuleKey, eff.Scope, fmt.Sprint(eff.Enabled), config, strings.Join(sources, ","), strings.Join(eff.Overrides, ","), execution,
	}})
	return nil
}
