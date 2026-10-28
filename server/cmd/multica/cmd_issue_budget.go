package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica issue budget raise <issue-id> --runs N — lift one issue's run cap
// (CHE-2226). Members only; the server rejects an agent.

var issueBudgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Manage an issue's run budget",
}

var issueBudgetRaiseCmd = &cobra.Command{
	Use:   "raise <issue-id>",
	Short: "Raise one issue's run cap (members only)",
	Long: "Sets the issue's own cap to --runs total runs in its budget window. " +
		"--runs must be above the cap now in force (the workflow's, or an earlier raise). " +
		"Other issues on the workflow keep the workflow's cap.",
	Args: exactArgs(1),
	RunE: runIssueBudgetRaise,
}

func init() {
	issueBudgetRaiseCmd.Flags().Int("runs", 0, "Total runs the issue may use (required)")
	issueBudgetRaiseCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetRaiseCmd)
	issueCmd.AddCommand(issueBudgetCmd)
}

func runIssueBudgetRaise(cmd *cobra.Command, args []string) error {
	runs, _ := cmd.Flags().GetInt("runs")
	if runs <= 0 {
		return fmt.Errorf("--runs is required and must be positive")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/issues/"+issueRef.ID+"/run-budget", map[string]any{"runs": runs}, &result); err != nil {
		return fmt.Errorf("raise run budget: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	fmt.Fprintf(os.Stdout, "%s run cap: %v -> %v\n", issueRef.Display, result["previous_max_runs"], result["max_runs"])
	return nil
}
