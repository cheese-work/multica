package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// agent skills runtime-set is the CLI path to the same per-agent runtime-skill
// override the web Skills tab toggles. Unlike the UI, it does not need the
// skill to be discovered on the runtime right now, so a saved disable can be
// mirrored onto another agent. The server keeps every authorization and
// identity check.
var agentSkillsRuntimeSetCmd = &cobra.Command{
	Use:   "runtime-set <agent-id>",
	Short: "Enable or disable one runtime-local skill for an agent",
	Long: `Enable or disable one runtime-local skill for an agent.

Saves or removes one entry in the agent's disabled_runtime_skills. The skill
does not need to be discovered on the runtime. --runtime-id must be the
agent's current runtime. Copy root, key, name and plugin from an existing
'multica agent get <id> --output json' disabled_runtime_skills entry to
mirror it. Prints the agent's disabled_runtime_skills after the write.`,
	Example: `  multica agent skills runtime-set <agent-id> --runtime-id <runtime-id> \
    --root provider --key gsd-add-tests --name gsd-add-tests --enabled=false`,
	Args: exactArgs(1),
	RunE: runAgentSkillsRuntimeSet,
}

func init() {
	agentSkillsCmd.AddCommand(agentSkillsRuntimeSetCmd)
	registerAgentSkillsRuntimeSetFlags(agentSkillsRuntimeSetCmd)
}

func registerAgentSkillsRuntimeSetFlags(c *cobra.Command) {
	c.Flags().String("runtime-id", "", "The agent's current runtime ID (required)")
	c.Flags().String("root", "", "Skill root: provider, universal or plugin (required)")
	c.Flags().String("key", "", "Skill key relative to its root (required)")
	c.Flags().String("name", "", "Skill invocation name (defaults to the key's last segment at run time)")
	c.Flags().String("plugin", "", "Plugin name (required when --root plugin)")
	c.Flags().Bool("enabled", true, "true removes the override; false disables the skill (required)")
	c.Flags().String("output", "json", "Output format: table or json")
}

func runAgentSkillsRuntimeSet(cmd *cobra.Command, args []string) error {
	for _, name := range []string{"runtime-id", "root", "key", "enabled"} {
		if !cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s is required", name)
		}
	}
	runtimeID, _ := cmd.Flags().GetString("runtime-id")
	root, _ := cmd.Flags().GetString("root")
	key, _ := cmd.Flags().GetString("key")
	name, _ := cmd.Flags().GetString("name")
	plugin, _ := cmd.Flags().GetString("plugin")
	enabled, _ := cmd.Flags().GetBool("enabled")

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	body := map[string]any{
		"runtime_id": runtimeID,
		"root":       root,
		"key":        key,
		"name":       name,
		"plugin":     plugin,
		"enabled":    enabled,
	}
	if err := client.PutJSON(ctx, "/api/agents/"+args[0]+"/runtime-skills/enabled", body, nil); err != nil {
		return fmt.Errorf("set agent runtime skill: %w", err)
	}

	var agent map[string]any
	if err := client.GetJSON(ctx, "/api/agents/"+args[0], &agent); err != nil {
		return fmt.Errorf("read back agent: %w", err)
	}
	disabled, _ := agent["disabled_runtime_skills"].([]any)
	if disabled == nil {
		disabled = []any{}
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, map[string]any{"disabled_runtime_skills": disabled})
	}
	headers := []string{"RUNTIME", "PROVIDER", "ROOT", "KEY", "NAME", "PLUGIN"}
	rows := make([][]string, 0, len(disabled))
	for _, raw := range disabled {
		s, _ := raw.(map[string]any)
		rows = append(rows, []string{
			strVal(s, "runtime_id"), strVal(s, "provider"), strVal(s, "root"),
			strVal(s, "key"), strVal(s, "name"), strVal(s, "plugin"),
		})
	}
	cli.PrintTable(os.Stdout, headers, rows)
	return nil
}
