package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var agentPresetCmd = &cobra.Command{
	Use:   "preset",
	Short: "Manage an agent's presets (model, thinking level, service tier, context args)",
}

var agentPresetListCmd = &cobra.Command{
	Use:   "list <agent>",
	Short: "List an agent's presets",
	Args:  exactArgs(1),
	RunE:  runAgentPresetList,
}

var agentPresetCreateCmd = &cobra.Command{
	Use:   "create <agent> <name>",
	Short: "Create a preset; unset fields inherit from the agent",
	Args:  exactArgs(2),
	RunE:  runAgentPresetCreate,
}

var agentPresetUpdateCmd = &cobra.Command{
	Use:               "update <agent> <name>",
	Short:             "Change a preset; omitted fields stay as they are, --reset makes one inherit again",
	Args:              exactArgs(2),
	RunE:              runAgentPresetUpdate,
	ValidArgsFunction: completePresetNames(true),
}

var agentPresetDeleteCmd = &cobra.Command{
	Use:               "delete <agent> <name>",
	Short:             "Delete a preset",
	Args:              exactArgs(2),
	RunE:              runAgentPresetDelete,
	ValidArgsFunction: completePresetNames(true),
}

var agentPresetUseCmd = &cobra.Command{
	Use:   "use <agent> [name|base]",
	Short: "Make a preset the agent default (--default) or pin it on an issue (--issue)",
	Long: `Make a preset the agent default, or pin it on one issue.

  multica agent preset use <agent> fast --default      agent default (base clears it)
  multica agent preset use <agent> fast --issue <id>   issue label preset:fast
  multica agent preset use <agent> --issue <id> --clear`,
	Args:              cobra.RangeArgs(1, 2),
	RunE:              runAgentPresetUse,
	ValidArgsFunction: completePresetNames(false),
}

func init() {
	agentCmd.AddCommand(agentPresetCmd)
	agentPresetCmd.AddCommand(agentPresetListCmd, agentPresetCreateCmd, agentPresetUpdateCmd, agentPresetDeleteCmd, agentPresetUseCmd)

	agentPresetListCmd.Flags().String("output", "table", "Output format: table or json")
	addPresetSettingFlags(agentPresetCreateCmd)
	addPresetSettingFlags(agentPresetUpdateCmd)
	agentPresetUpdateCmd.Flags().StringArray("reset", nil, "Field to make inherit again: model, thinking_level, service_tier or context_args (repeatable)")
	agentPresetUseCmd.Flags().Bool("default", false, "Make the preset the agent default")
	agentPresetUseCmd.Flags().String("issue", "", "Pin the preset on this issue")
	agentPresetUseCmd.Flags().Bool("clear", false, "With --issue: remove the issue's preset")
}

func addPresetSettingFlags(cmd *cobra.Command) {
	cmd.Flags().String("model", "", "Model")
	cmd.Flags().String("thinking-level", "", "Thinking level")
	cmd.Flags().String("service-tier", "", "Service tier")
	cmd.Flags().StringArray("context-arg", nil, "Context argument, one token per flag (repeatable)")
	cmd.Flags().Bool("default", false, "Make this the agent default preset")
}

// completePresetNames completes `<agent> <TAB>`. stored=false (use) offers what
// the agent can run: its usable presets plus base, from the invoke-gated
// completions endpoint without `reset`, a DM keyword `use` cannot execute.
// stored=true (update, delete) offers every stored preset, stale ones too, from
// the list endpoint, which a manager without invoke access can read.
func completePresetNames(stored bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		none := cobra.ShellCompDirectiveNoFileComp
		if len(args) != 1 {
			return nil, none
		}
		client, err := newAPIClient(cmd)
		if err != nil {
			return nil, none
		}
		ctx, cancel := cli.APIContext(context.Background())
		defer cancel()
		agentID, err := resolveAgent(ctx, client, args[0])
		if err != nil {
			return nil, none
		}
		var names []string
		if stored {
			var rows []struct {
				Name string `json:"name"`
			}
			if err := client.GetJSON(ctx, "/api/agents/"+agentID+"/presets", &rows); err != nil {
				return nil, none
			}
			for _, r := range rows {
				names = append(names, r.Name)
			}
		} else {
			var resp struct {
				Names []string `json:"names"`
			}
			if err := client.GetJSON(ctx, "/api/agents/"+agentID+"/presets/completions", &resp); err != nil {
				return nil, none
			}
			names = resp.Names
		}
		var out []string
		for _, n := range names {
			if (stored || n != "reset") && strings.HasPrefix(n, toComplete) {
				out = append(out, n)
			}
		}
		return out, none
	}
}

func presetAgentClient(cmd *cobra.Command, agent string) (*cli.APIClient, context.Context, context.CancelFunc, string, error) {
	client, err := newAPIClient(cmd)
	if err != nil {
		return nil, nil, nil, "", err
	}
	ctx, cancel := cli.APIContext(context.Background())
	id, err := resolveAgent(ctx, client, agent)
	if err != nil {
		cancel()
		return nil, nil, nil, "", err
	}
	return client, ctx, cancel, id, nil
}

func runAgentPresetList(cmd *cobra.Command, args []string) error {
	client, ctx, cancel, id, err := presetAgentClient(cmd, args[0])
	if err != nil {
		return err
	}
	defer cancel()
	var presets []map[string]any
	if err := client.GetJSON(ctx, "/api/agents/"+id+"/presets", &presets); err != nil {
		return fmt.Errorf("list presets: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, presets)
	}
	rows := make([][]string, 0, len(presets))
	for _, p := range presets {
		def := ""
		if p["is_default"] == true {
			def = "default"
		}
		rows = append(rows, []string{strVal(p, "name"), strVal(p, "model"), strVal(p, "thinking_level"), strVal(p, "service_tier"),
			fmt.Sprint(p["context_args"]), def, strVal(p, "stale")})
	}
	cli.PrintTable(os.Stdout, []string{"NAME", "MODEL", "THINKING", "TIER", "CONTEXT ARGS", "DEFAULT", "STALE"}, rows)
	return nil
}

// presetSettingsBody collects only the flags the user passed, so an unset flag
// is never sent (PATCH leaves it, POST inherits it).
func presetSettingsBody(cmd *cobra.Command) map[string]any {
	body := map[string]any{}
	for flag, key := range map[string]string{"model": "model", "thinking-level": "thinking_level", "service-tier": "service_tier"} {
		if cmd.Flags().Changed(flag) {
			body[key], _ = cmd.Flags().GetString(flag)
		}
	}
	if cmd.Flags().Changed("context-arg") {
		body["context_args"], _ = cmd.Flags().GetStringArray("context-arg")
	}
	if cmd.Flags().Changed("default") {
		body["is_default"], _ = cmd.Flags().GetBool("default")
	}
	return body
}

func runAgentPresetCreate(cmd *cobra.Command, args []string) error {
	client, ctx, cancel, id, err := presetAgentClient(cmd, args[0])
	if err != nil {
		return err
	}
	defer cancel()
	body := presetSettingsBody(cmd)
	body["name"] = args[1]
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/agents/"+id+"/presets", body, &result); err != nil {
		return fmt.Errorf("create preset: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runAgentPresetUpdate(cmd *cobra.Command, args []string) error {
	body := presetSettingsBody(cmd)
	resets, _ := cmd.Flags().GetStringArray("reset")
	for _, field := range resets {
		switch field {
		case "model", "thinking_level", "service_tier", "context_args":
			body[field] = nil
		default:
			return fmt.Errorf("--reset %q: want model, thinking_level, service_tier or context_args", field)
		}
	}
	if len(body) == 0 {
		return fmt.Errorf("nothing to change: pass a setting flag, --default or --reset")
	}
	client, ctx, cancel, id, err := presetAgentClient(cmd, args[0])
	if err != nil {
		return err
	}
	defer cancel()
	var result map[string]any
	if err := client.PatchJSON(ctx, "/api/agents/"+id+"/presets/"+args[1], body, &result); err != nil {
		return fmt.Errorf("update preset: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runAgentPresetDelete(cmd *cobra.Command, args []string) error {
	client, ctx, cancel, id, err := presetAgentClient(cmd, args[0])
	if err != nil {
		return err
	}
	defer cancel()
	if err := client.DeleteJSON(ctx, "/api/agents/"+id+"/presets/"+args[1]); err != nil {
		return fmt.Errorf("delete preset: %w", err)
	}
	fmt.Printf("Deleted preset %s\n", args[1])
	return nil
}

func runAgentPresetUse(cmd *cobra.Command, args []string) error {
	asDefault, _ := cmd.Flags().GetBool("default")
	issue, _ := cmd.Flags().GetString("issue")
	clear, _ := cmd.Flags().GetBool("clear")
	if asDefault == (issue != "") {
		return fmt.Errorf("choose one target: --default (agent default) or --issue <id>")
	}
	if clear && issue == "" {
		return fmt.Errorf("--clear needs --issue")
	}
	if len(args) < 2 && !clear {
		return fmt.Errorf("preset name required (use base for none)")
	}

	client, ctx, cancel, id, err := presetAgentClient(cmd, args[0])
	if err != nil {
		return err
	}
	defer cancel()

	var result any
	switch {
	case clear:
		err = client.DeleteJSONResponse(ctx, "/api/issues/"+issue+"/preset", &result)
	case issue != "":
		err = client.PutJSON(ctx, "/api/issues/"+issue+"/preset", map[string]any{"name": args[1]}, &result)
	default:
		err = client.PutJSON(ctx, "/api/agents/"+id+"/presets/default", map[string]any{"name": args[1]}, &result)
	}
	if err != nil {
		return fmt.Errorf("set preset: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
