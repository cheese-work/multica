package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica issue budget raise <issue-id> --runs N — lift one issue's run cap
// (CHE-2226). Members only; the server rejects an agent.
// multica issue budget top-up <issue-id> — open a new run window (CHE-2297 L2d).

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

var issueBudgetGetCmd = &cobra.Command{
	Use:   "get <issue-id>",
	Short: "Show an issue's run budget",
	Long:  "Shows the runs used in the issue's budget window, the limit in force, its state and what a suggested raise would leave.",
	Args:  exactArgs(1),
	RunE:  runIssueBudgetGet,
}

var issueBudgetTopUpCmd = &cobra.Command{
	Use:   "top-up <issue-id>",
	Short: "Open a new run window on an issue (members only)",
	Long: "Gives the issue a fresh run window, as a status move by a person does, without moving its status: " +
		"the window credit is the workflow's run cap. Usage history is kept. The server rejects an agent.",
	Args: exactArgs(1),
	RunE: runIssueBudgetTopUp,
}

func init() {
	issueBudgetTopUpCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetTopUpCmd)
	issueBudgetGetCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetGetCmd)
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

// issueBudgetSnapshot is the part of GET /api/issues/{id}/run-budget the CLI
// prints; the server's service.RunBudgetSnapshot is the full contract. The CLI
// does not import the service package, which pulls in the database layer.
type issueBudgetSnapshot struct {
	Budgeted     bool     `json:"budgeted"`
	RunsUsed     int64    `json:"runs_used"`
	RunsLimit    int      `json:"runs_limit"`
	State        string   `json:"state"`
	Blocking     []string `json:"blocking"`
	CanRaise     bool     `json:"can_raise"`
	RaisePreview *struct {
		AddRuns       int `json:"add_runs"`
		BalanceAfter  int `json:"balance_after"`
		LimitAfter    int `json:"limit_after"`
		MaxAllowedAdd int `json:"max_allowed_add"`
	} `json:"raise_preview"`
}

func runIssueBudgetGet(cmd *cobra.Command, args []string) error {
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
	var raw json.RawMessage
	if err := client.GetJSON(ctx, "/api/issues/"+issueRef.ID+"/run-budget", &raw); err != nil {
		return fmt.Errorf("get run budget: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	var snap issueBudgetSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("read run budget: %w", err)
	}
	if !snap.Budgeted {
		fmt.Fprintf(os.Stdout, "%s has no run budget\n", issueRef.Display)
		return nil
	}
	fmt.Fprintf(os.Stdout, "%s runs: %d/%d (%s)\n", issueRef.Display, snap.RunsUsed, snap.RunsLimit, snap.State)
	if p := snap.RaisePreview; p != nil {
		fmt.Fprintf(os.Stdout, "raise: multica issue budget raise %s --runs %d (%d left)\n", issueRef.Display, p.LimitAfter, p.BalanceAfter)
	}
	return nil
}

func runIssueBudgetTopUp(cmd *cobra.Command, args []string) error {
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
	if err := client.PostJSON(ctx, "/api/issues/"+issueRef.ID+"/run-budget/top-up", nil, &result); err != nil {
		return fmt.Errorf("top up run budget: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	fmt.Fprintf(os.Stdout, "%s opened a new run window of %v runs\n", issueRef.Display, result["max_runs"])
	return nil
}
