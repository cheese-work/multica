package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica issue budget raise / top-up only point to the web app: the server
// takes both from an interactive session alone (CHE-2798 S1a).

var issueBudgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Manage an issue's run budget",
}

var issueBudgetRaiseCmd = &cobra.Command{
	Use:   "raise <issue-id>",
	Short: "Raise one issue's run cap (approve in the web app)",
	Long:  "Not available from the CLI: approve a raise in the web app. Until then, move the status in the web app.",
	Args:  exactArgs(1),
	RunE:  runIssueBudgetRaise,
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
	Short: "Open a new run window on an issue (approve in the web app)",
	Long:  "Not available from the CLI: approve a top-up in the web app. Until then, move the status in the web app.",
	Args:  exactArgs(1),
	RunE:  runIssueBudgetTopUp,
}

func init() {
	issueBudgetTopUpCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetTopUpCmd)
	issueBudgetGetCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetGetCmd)
	issueBudgetRaiseCmd.Flags().Int("runs", 0, "Total runs the issue may use (ignored; raise in the web app)")
	issueBudgetRaiseCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetRaiseCmd)
	issueCmd.AddCommand(issueBudgetCmd)
}

// errBudgetWebApp is what raise and top-up return: the server accepts both only
// from an interactive session, which no CLI credential is (CHE-2798 S1a).
var errBudgetWebApp = errors.New("a run-budget raise or top-up is not available from the CLI: approve in the web app; until then move the status in the web app")

func runIssueBudgetRaise(*cobra.Command, []string) error { return errBudgetWebApp }

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
		fmt.Fprintf(os.Stdout, "raise: approve a raise to %d runs in the web app (%d left)\n", p.LimitAfter, p.BalanceAfter)
	}
	return nil
}

func runIssueBudgetTopUp(*cobra.Command, []string) error { return errBudgetWebApp }
