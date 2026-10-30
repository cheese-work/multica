package main

import (
	"context"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// The daily brief autopilot prints its "Agents decided — veto?" section from
// `multica decision brief`. The owner flips the pilot with `agent-setting`.
func init() {
	brief := &cobra.Command{Use: "brief", Short: "Read agent-made decisions for the daily brief: records, delivery failures and pilot metrics", Args: cobra.NoArgs, RunE: runAgentDecisionBrief}
	setting := &cobra.Command{Use: "agent-setting", Short: "Turn agent decisions on or off for the workspace (owner only; default off)", Args: cobra.NoArgs, RunE: runAgentDecisionSetting}
	setting.Flags().Bool("enabled", false, "true turns agent decisions on, false turns them off")
	_ = setting.MarkFlagRequired("enabled")
	decisionCmd.AddCommand(brief, setting)
}

func runAgentDecisionBrief(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.GetJSON(ctx, "/api/agent-decisions/brief", &out); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}

func runAgentDecisionSetting(command *cobra.Command, _ []string) error {
	enabled, _ := command.Flags().GetBool("enabled")
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.PutJSON(ctx, "/api/agent-decisions/setting", map[string]bool{"enabled": enabled}, &out); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}
