package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica issue budget raise / top-up call the run-budget API. The server takes
// them from a person's interactive session, or from an agent's task token while
// the agent cap is on and has room (CHE-3251); everything else gets a 403.

var issueBudgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Manage an issue's run budget",
}

var issueBudgetRaiseCmd = &cobra.Command{
	Use:   "raise <issue-id>",
	Short: "Raise one issue's run cap to a total",
	Long: "Sets the issue's own run cap to --runs in total. An agent may raise another agent's issue inside the daily and monthly agent cap; " +
		"a refusal names the reason, what is left and when the cap resets. A person's PAT is refused: approve in the web app.",
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
	Short: "Open a new run window on an issue",
	Long: "Opens a new run window worth the cap in force. An agent may top up another agent's issue inside the daily and monthly agent cap; " +
		"a refusal names the reason, what is left and when the cap resets. A person's PAT is refused: approve in the web app.",
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

// budgetRefusal is the part of a 403/429 body the CLI explains.
type budgetRefusal struct {
	Error         string  `json:"error"`
	Reason        string  `json:"reason"`
	LimitUSD      float64 `json:"limit_usd"`
	UsedUSD       float64 `json:"used_usd"`
	RemainingUSD  float64 `json:"remaining_usd"`
	RemainingRuns int64   `json:"remaining_runs"`
	RequestedRuns int64   `json:"requested_runs"`
	ResetsAt      string  `json:"resets_at"`
}

// explainBudgetError spells a cap refusal out; any other error passes through.
func explainBudgetError(action string, err error) error {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) || (httpErr.StatusCode != http.StatusForbidden && httpErr.StatusCode != http.StatusTooManyRequests) {
		return fmt.Errorf("%s: %w", action, err)
	}
	var r budgetRefusal
	if json.Unmarshal([]byte(httpErr.Body), &r) != nil || r.Error == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	msg := fmt.Sprintf("%s refused: %s", action, r.Error)
	if r.Reason != "" {
		msg += " (" + r.Reason + ")"
	}
	if r.Reason == "daily" || r.Reason == "monthly_share" {
		msg += fmt.Sprintf(": used $%.2f of $%.2f, $%.2f left (%d runs), asked for %d runs, resets %s",
			r.UsedUSD, r.LimitUSD, r.RemainingUSD, r.RemainingRuns, r.RequestedRuns, r.ResetsAt)
	}
	return errors.New(msg)
}

func printBudgetResult(cmd *cobra.Command, raw json.RawMessage, line string) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	fmt.Fprintln(os.Stdout, line)
	return nil
}

func runIssueBudgetRaise(cmd *cobra.Command, args []string) error {
	runs, _ := cmd.Flags().GetInt("runs")
	if runs <= 0 {
		return errors.New("--runs is required and must be positive")
	}
	return postIssueBudget(cmd, args[0], "/run-budget", map[string]int{"runs": runs}, "raise",
		func(raw json.RawMessage, ref string) string {
			var out struct {
				MaxRuns         int `json:"max_runs"`
				PreviousMaxRuns int `json:"previous_max_runs"`
			}
			_ = json.Unmarshal(raw, &out)
			return fmt.Sprintf("%s run cap raised from %d to %d", ref, out.PreviousMaxRuns, out.MaxRuns)
		})
}

func runIssueBudgetTopUp(cmd *cobra.Command, args []string) error {
	return postIssueBudget(cmd, args[0], "/run-budget/top-up", map[string]int{}, "top-up",
		func(raw json.RawMessage, ref string) string {
			var out struct {
				MaxRuns int `json:"max_runs"`
			}
			_ = json.Unmarshal(raw, &out)
			return fmt.Sprintf("%s opened a new window of %d runs", ref, out.MaxRuns)
		})
}

func postIssueBudget(cmd *cobra.Command, issueArg, suffix string, body any, action string, line func(json.RawMessage, string) string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveIssueRef(ctx, client, issueArg)
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var raw json.RawMessage
	if err := client.PostJSON(ctx, "/api/issues/"+ref.ID+suffix, body, &raw); err != nil {
		return explainBudgetError(action, err)
	}
	return printBudgetResult(cmd, raw, line(raw, ref.Display))
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
		fmt.Fprintf(os.Stdout, "raise: approve a raise to %d runs in the web app (%d left)\n", p.LimitAfter, p.BalanceAfter)
	}
	return nil
}
