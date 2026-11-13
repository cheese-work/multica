package main

import (
	"context"
	"encoding/json"
	"errors"
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
	Use:   "raise <issue-id> [<issue-id>...]",
	Short: "Request a raise of issue run caps (an owner approves in the web app)",
	Long:  "Opens a spend decision request for the raise and prints its approve URL. An owner approves it with a passkey in the web app; this command does not raise the cap. With a task token, several issue IDs open one batch card (--goal names it); with a personal access token each issue gets its own request.",
	Args:  cobra.MinimumNArgs(1),
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
	Use:   "top-up <issue-id> [<issue-id>...]",
	Short: "Request a new run window on issues (an owner approves in the web app)",
	Long:  "Opens a spend decision request for the top-up and prints its approve URL. An owner approves it with a passkey in the web app; this command does not open the window. With a task token, several issue IDs open one batch card (--goal names it); with a personal access token each issue gets its own request.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runIssueBudgetTopUp,
}

const goalFlagHelp = "Task token, several issues: the goal the batch card shows (default: names the action and the issue count)"

const forFlagHelp = "Task token only: user ID of the person who approves the proposal (default: a workspace owner on an issue task; a chat task asks the chat's creator)"

func init() {
	hideMemberOnlyBudgetCommands()
	issueBudgetTopUpCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetTopUpCmd.Flags().String("for", "", forFlagHelp)
	issueBudgetTopUpCmd.Flags().String("goal", "", goalFlagHelp)
	issueBudgetCmd.AddCommand(issueBudgetTopUpCmd)
	issueBudgetGetCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetCmd.AddCommand(issueBudgetGetCmd)
	issueBudgetRaiseCmd.Flags().Int("runs", 0, "Total runs the issue may use (default: the suggested raise)")
	issueBudgetRaiseCmd.Flags().String("output", "table", "Output format: table or json")
	issueBudgetRaiseCmd.Flags().String("for", "", forFlagHelp)
	issueBudgetRaiseCmd.Flags().String("goal", "", goalFlagHelp)
	issueBudgetCmd.AddCommand(issueBudgetRaiseCmd)
	issueCmd.AddCommand(issueBudgetCmd)
}

func runIssueBudgetRaise(cmd *cobra.Command, args []string) error {
	return requestIssueBudgetActions(cmd, args, "raise")
}

func runIssueBudgetTopUp(cmd *cobra.Command, args []string) error {
	return requestIssueBudgetActions(cmd, args, "top_up")
}

// requestIssueBudgetActions keeps one issue on the single-request path. Several
// issues under a task token open one batch card; under a PAT each issue gets
// its own request, as before.
func requestIssueBudgetActions(cmd *cobra.Command, issueArgs []string, kind string) error {
	if len(issueArgs) == 1 || !strings.HasPrefix(resolveToken(cmd), cli.TaskTokenPrefix) {
		for _, issueArg := range issueArgs {
			if err := requestIssueBudgetAction(cmd, issueArg, kind); err != nil {
				return err
			}
		}
		return nil
	}
	return proposeIssueBudgetBatch(cmd, issueArgs, kind)
}

// proposeIssueBudgetBatch reads every issue's window first, so a bad issue
// ends the call before any card opens, then sends one batch proposal.
func proposeIssueBudgetBatch(cmd *cobra.Command, issueArgs []string, kind string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	actionType := "issue.run_budget." + kind
	var items []map[string]any
	seen := map[string]bool{}
	for _, issueArg := range issueArgs {
		issueRef, err := resolveIssueRef(ctx, client, issueArg)
		if err != nil {
			return fmt.Errorf("resolve issue: %w", err)
		}
		if seen[issueRef.ID] {
			continue
		}
		seen[issueRef.ID] = true
		snap, newCap, err := issueBudgetRequest(ctx, cmd, client, issueRef, kind)
		if err != nil {
			return err
		}
		params := map[string]any{"issue_id": issueRef.ID, "window_id": snap.WindowID}
		if kind == "raise" {
			params["new_cap"] = newCap
		}
		items = append(items, map[string]any{"action_type": actionType, "version": 1, "issue_id": issueRef.ID, "params": params})
	}
	goal, _ := cmd.Flags().GetString("goal")
	if goal == "" {
		goal = fmt.Sprintf("%s run budgets on %d issues", strings.ReplaceAll(kind, "_", "-"), len(items))
	}
	body := map[string]any{"goal": goal, "items": items}
	if forUser, _ := cmd.Flags().GetString("for"); forUser != "" {
		body["for"] = forUser
	}
	var raw json.RawMessage
	if err := client.PostJSON(ctx, "/api/decision-actions/proposals/batch", body, &raw); err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) {
			return fmt.Errorf("propose %s batch: server refused (%d): %s", actionType, httpErr.StatusCode, serverErrorText(httpErr.Body))
		}
		return fmt.Errorf("propose %s batch: %w", actionType, err)
	}
	return printActionProposalBatch(ctx, cmd, client, actionType, raw)
}

// printActionProposalBatch prints each batch card's id and the DM link where
// the person approves it.
func printActionProposalBatch(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, actionType string, raw json.RawMessage) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	var resp struct {
		Batches []struct {
			BatchID  string `json:"batch_id"`
			Requests []struct {
				ChatSessionID string `json:"chat_session_id"`
			} `json:"requests"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Batches) == 0 {
		return fmt.Errorf("read proposal: the server answered without a batch")
	}
	prefix := ""
	var ws struct {
		Slug string `json:"slug"`
	}
	if client.WorkspaceID != "" && client.GetJSON(ctx, "/api/workspaces/"+client.WorkspaceID, &ws) == nil && ws.Slug != "" {
		prefix = "/" + ws.Slug
	}
	for _, batch := range resp.Batches {
		fmt.Fprintf(os.Stdout, "proposed %d x %s (batch %s); the person approves all with one passkey at:\n", len(batch.Requests), actionType, batch.BatchID)
		if len(batch.Requests) > 0 && batch.Requests[0].ChatSessionID != "" {
			fmt.Fprintf(os.Stdout, "%s/chat?session=%s\n", prefix, batch.Requests[0].ChatSessionID)
		}
	}
	return nil
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
	snap, newCap, err := issueBudgetRequest(ctx, cmd, client, issueRef, kind)
	if err != nil {
		return err
	}
	body := map[string]any{"kind": kind, "window_id": snap.WindowID}
	if kind == "raise" {
		body["new_cap"] = newCap
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

// issueBudgetRequest reads the issue's open window and, for a raise, the total
// runs to ask for: --runs, or the suggested raise.
func issueBudgetRequest(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, issueRef resolvedID, kind string) (snap issueBudgetSnapshot, newCap int, err error) {
	if err = client.GetJSON(ctx, "/api/issues/"+issueRef.ID+"/run-budget", &snap); err != nil {
		return snap, 0, fmt.Errorf("get run budget: %w", err)
	}
	if !snap.Budgeted {
		return snap, 0, fmt.Errorf("%s has no run budget", issueRef.Display)
	}
	if kind != "raise" {
		return snap, 0, nil
	}
	newCap, _ = cmd.Flags().GetInt("runs")
	if newCap == 0 && snap.RaisePreview != nil {
		newCap = snap.RaisePreview.LimitAfter
	}
	if newCap <= 0 {
		return snap, 0, fmt.Errorf("no suggested raise for %s: pass --runs <total runs>", issueRef.Display)
	}
	return snap, newCap, nil
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
	NextCommand  string   `json:"next_command"`
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
	if snap.NextCommand != "" {
		fmt.Fprintf(os.Stdout, "next: %s\n", snap.NextCommand)
	}
	return nil
}

// hideMemberOnlyBudgetCommands keeps top-up out of a task token's help: an agent
// asks with raise. The command still runs for members and the server path.
func hideMemberOnlyBudgetCommands() {
	issueBudgetTopUpCmd.Hidden = strings.HasPrefix(strings.TrimSpace(os.Getenv("MULTICA_TOKEN")), cli.TaskTokenPrefix)
}
