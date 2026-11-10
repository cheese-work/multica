package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica issue budget raise / top-up open a spend decision request and print
// its approve URL. A CLI credential cannot approve it: an owner does that with a
// passkey in the web app (CHE-2798 A1d).

var issueBudgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Manage an issue's run budget",
}

var issueBudgetRaiseCmd = &cobra.Command{
	Use:   "raise <issue-id>",
	Short: "Request a raise of one issue's run cap (an owner approves in the web app)",
	Long:  "Opens a spend decision request for the raise and prints its approve URL. An owner approves it with a passkey in the web app; this command does not raise the cap.",
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
	Short: "Request a new run window on an issue (an owner approves in the web app)",
	Long:  "Opens a spend decision request for the top-up and prints its approve URL. An owner approves it with a passkey in the web app; this command does not open the window.",
	Args:  exactArgs(1),
	RunE:  runIssueBudgetTopUp,
}

const forFlagHelp = "Task token only: user ID of the person who approves the proposal (an issue task needs it; a chat task asks the chat's creator)"

func init() {
	issueBudgetTopUpCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetTopUpCmd.Flags().String("for", "", forFlagHelp)
	issueBudgetCmd.AddCommand(issueBudgetTopUpCmd)
	issueBudgetGetCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetGetCmd)
	issueBudgetRaiseCmd.Flags().Int("runs", 0, "Total runs the issue may use (default: the suggested raise)")
	issueBudgetRaiseCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetRaiseCmd.Flags().String("for", "", forFlagHelp)
	issueBudgetCmd.AddCommand(issueBudgetRaiseCmd)
	issueCmd.AddCommand(issueBudgetCmd)
}

func runIssueBudgetRaise(cmd *cobra.Command, args []string) error {
	return requestIssueBudgetAction(cmd, args[0], "raise")
}

func runIssueBudgetTopUp(cmd *cobra.Command, args []string) error {
	return requestIssueBudgetAction(cmd, args[0], "top_up")
}

// requestIssueBudgetAction reads the issue's open window, asks the server for a
// raise or top-up decision request and prints where an owner approves it.
func requestIssueBudgetAction(cmd *cobra.Command, issueArg, kind string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, issueArg)
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	path := "/api/issues/" + issueRef.ID + "/run-budget"
	var snap issueBudgetSnapshot
	if err := client.GetJSON(ctx, path, &snap); err != nil {
		return fmt.Errorf("get run budget: %w", err)
	}
	if !snap.Budgeted {
		return fmt.Errorf("%s has no run budget", issueRef.Display)
	}
	body := map[string]any{"kind": kind, "window_id": snap.WindowID}
	if kind == "raise" {
		runs, _ := cmd.Flags().GetInt("runs")
		if runs == 0 && snap.RaisePreview != nil {
			runs = snap.RaisePreview.LimitAfter
		}
		if runs <= 0 {
			return fmt.Errorf("no suggested raise for %s: pass --runs <total runs>", issueRef.Display)
		}
		body["new_cap"] = runs
	}
	if strings.HasPrefix(resolveToken(cmd), cli.TaskTokenPrefix) {
		return proposeIssueBudgetAction(ctx, cmd, client, issueRef, kind, body)
	}
	var raw json.RawMessage
	if err := client.PostJSON(ctx, path+"/requests", body, &raw); err != nil {
		return fmt.Errorf("request run budget %s: %w", kind, err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	var resp struct {
		Requests []struct {
			ApproveURL string `json:"approve_url"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	fmt.Fprintf(os.Stdout, "requested %s on %s; an owner approves with a passkey at:\n", kind, issueRef.Display)
	for _, r := range resp.Requests {
		fmt.Fprintln(os.Stdout, r.ApproveURL)
	}
	return nil
}

// proposeIssueBudgetAction is the task-token path: an agent cannot open the
// spend request itself, so it proposes the action and the person approves the
// card. The PAT path above is unchanged (CHE-2964).
func proposeIssueBudgetAction(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, issue resolvedID, kind string, body map[string]any) error {
	actionType := "issue.run_budget." + kind
	params := map[string]json.RawMessage{}
	for key, value := range map[string]any{"issue_id": issue.ID, "window_id": body["window_id"], "new_cap": body["new_cap"]} {
		if value != nil {
			params[key], _ = json.Marshal(value)
		}
	}
	forUser, _ := cmd.Flags().GetString("for")
	raw, err := postActionProposal(ctx, client, actionType, 1, issue.ID, forUser, "", params)
	if err != nil {
		return err
	}
	return printActionProposal(ctx, cmd, client, actionType, issue, raw)
}

// issueBudgetSnapshot is the part of GET /api/issues/{id}/run-budget the CLI
// prints; the server's service.RunBudgetSnapshot is the full contract. The CLI
// does not import the service package, which pulls in the database layer.
type issueBudgetSnapshot struct {
	Budgeted     bool     `json:"budgeted"`
	WindowID     string   `json:"window_id"`
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
