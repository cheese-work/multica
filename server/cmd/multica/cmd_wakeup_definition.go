package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
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

	list := &cobra.Command{
		Use:   "list",
		Short: "List the wakeup rules defined at a scope",
		Args:  cobra.NoArgs,
		RunE:  runWakeupDefinitionList,
	}
	list.Flags().String("project", "", "List the rules of this project (default: workspace scope)")
	list.Flags().String("output", "json", "Output format (json or table)")
	root.AddCommand(list)

	update := &cobra.Command{
		Use:   "update <rule>",
		Short: "Replace a wakeup rule's definition at a scope",
		Long:  "Replace the definition of a rule at workspace scope, or project scope with --project. --revision is the revision `list` shows (0 creates an override). Only an owner or admin may edit; an agent gets 403 from the server, so a rule it proposes stays disabled until a human enables it.",
		Args:  cobra.ExactArgs(1),
		RunE:  runWakeupDefinitionUpdate,
	}
	update.Flags().String("project", "", "Update the rule at this project (default: workspace scope)")
	update.Flags().String("config", "", "Rule config as JSON")
	update.Flags().String("config-file", "", "Read the rule config from a UTF-8 JSON file")
	update.Flags().Int64("revision", 0, "Revision shown by `list` (required)")
	root.AddCommand(update)

	del := &cobra.Command{
		Use:   "delete <rule>",
		Short: "Delete a custom rule or reset a rule to its inherited settings",
		Args:  cobra.ExactArgs(1),
		RunE:  runWakeupDefinitionDelete,
	}
	del.Flags().String("project", "", "Delete the rule at this project (default: workspace scope)")
	del.Flags().Int64("revision", 0, "Revision shown by `list` (required)")
	root.AddCommand(del)

	preview := &cobra.Command{
		Use:   "preview",
		Short: "Show what a rule would do, without saving it",
		Long:  "Resolve a proposed definition and print the effective result. For a condition rule, already_satisfied counts the issues that would match at once and lists them (issues); it looks at the first 200 issues of the scope, evaluates the open, non-backlog ones, and sets truncated when the scope has more. An event rule has no predicate, so it has no already_satisfied. Nothing is stored. Use --rule to preview a change to an existing rule; omit it to preview a new custom rule. Use --output table for a readable summary.",
		Args:  cobra.NoArgs,
		RunE:  runWakeupDefinitionPreview,
	}
	preview.Flags().String("project", "", "Preview at this project (default: workspace scope)")
	preview.Flags().String("rule", "", "Existing rule key (default: a new custom rule)")
	preview.Flags().String("config", "", "Rule config as JSON")
	preview.Flags().String("config-file", "", "Read the rule config from a UTF-8 JSON file")
	preview.Flags().String("output", "json", "Output format (json or table)")
	root.AddCommand(preview)
	return root
}

// wakeupDefinitionsPath resolves --project and returns the scope's
// wakeup-definitions collection path.
func wakeupDefinitionsPath(ctx context.Context, cmd *cobra.Command, client *cli.APIClient) (string, error) {
	project, _ := cmd.Flags().GetString("project")
	if project == "" {
		return "/api/wakeup-definitions", nil
	}
	ref, err := resolveProjectID(ctx, client, project)
	if err != nil {
		return "", err
	}
	return "/api/projects/" + url.PathEscape(ref.ID) + "/wakeup-definitions", nil
}

func requiredConfig(cmd *cobra.Command) (string, error) {
	config, ok, err := resolveTextFlag(cmd, "config")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("--config or --config-file is required")
	}
	return config, nil
}

func requiredRevision(cmd *cobra.Command) (int64, error) {
	if !cmd.Flags().Changed("revision") {
		return 0, fmt.Errorf("--revision is required")
	}
	return cmd.Flags().GetInt64("revision")
}

func runWakeupDefinitionList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path, err := wakeupDefinitionsPath(ctx, cmd, client)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := client.GetJSON(ctx, path, &raw); err != nil {
		return err
	}
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(cmd.OutOrStdout(), raw)
	}
	var list struct {
		Definitions []struct {
			RuleKey  string `json:"rule_key"`
			Scope    string `json:"scope"`
			Revision string `json:"revision"`
			Config   struct {
				Enabled bool `json:"enabled"`
			} `json:"config"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	rows := make([][]string, 0, len(list.Definitions))
	for _, d := range list.Definitions {
		rows = append(rows, []string{d.RuleKey, d.Scope, fmt.Sprint(d.Config.Enabled), d.Revision})
	}
	cli.PrintTable(cmd.OutOrStdout(), []string{"RULE", "SCOPE", "ENABLED", "REVISION"}, rows)
	return nil
}

func runWakeupDefinitionUpdate(cmd *cobra.Command, args []string) error {
	config, err := requiredConfig(cmd)
	if err != nil {
		return err
	}
	revision, err := requiredRevision(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path, err := wakeupDefinitionsPath(ctx, cmd, client)
	if err != nil {
		return err
	}
	body := map[string]any{"revision": strconv.FormatInt(revision, 10), "config": json.RawMessage(config)}
	var raw json.RawMessage
	if err := client.PutJSON(ctx, path+"/"+url.PathEscape(args[0]), body, &raw); err != nil {
		return err
	}
	return cli.PrintJSON(cmd.OutOrStdout(), raw)
}

func runWakeupDefinitionDelete(cmd *cobra.Command, args []string) error {
	revision, err := requiredRevision(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path, err := wakeupDefinitionsPath(ctx, cmd, client)
	if err != nil {
		return err
	}
	if err := client.DeleteJSON(ctx, path+"/"+url.PathEscape(args[0])+"?revision="+strconv.FormatInt(revision, 10)); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s.\n", args[0])
	return nil
}

func runWakeupDefinitionPreview(cmd *cobra.Command, _ []string) error {
	config, err := requiredConfig(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path, err := wakeupDefinitionsPath(ctx, cmd, client)
	if err != nil {
		return err
	}
	body := map[string]any{"config": json.RawMessage(config)}
	if rule, _ := cmd.Flags().GetString("rule"); rule != "" {
		body["rule_key"] = rule
	}
	var raw json.RawMessage
	if err := client.PostJSON(ctx, path+"/preview", body, &raw); err != nil {
		return err
	}
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(cmd.OutOrStdout(), raw)
	}
	var preview struct {
		AlreadySatisfied *struct {
			Satisfied int  `json:"satisfied"`
			Examined  int  `json:"examined"`
			Truncated bool `json:"truncated"`
			Issues    []struct {
				Identifier string `json:"identifier"`
				Title      string `json:"title"`
			} `json:"issues"`
		} `json:"already_satisfied"`
	}
	if err := json.Unmarshal(raw, &preview); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	sat := preview.AlreadySatisfied
	if sat == nil {
		fmt.Fprintln(out, "No condition to evaluate: an event rule fires on future events and matches no existing issue.")
		return nil
	}
	fmt.Fprintf(out, "%d of %d examined issues already match.\n", sat.Satisfied, sat.Examined)
	if sat.Truncated {
		fmt.Fprintln(out, "Preview is incomplete: the scope has more issues than the preview looks at (the first 200), so more issues may match.")
	}
	rows := make([][]string, 0, len(sat.Issues))
	for _, i := range sat.Issues {
		rows = append(rows, []string{i.Identifier, i.Title})
	}
	if len(rows) > 0 {
		cli.PrintTable(out, []string{"ISSUE", "TITLE"}, rows)
	}
	return nil
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
