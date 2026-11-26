package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueCloseCheckCmd = &cobra.Command{
	Use:   "close-check <issue>",
	Short: "Show why an issue did or would not auto-close (read-only)",
	Long: `Reads every auto-close predicate on the issue's current rows: its value, the
first one that does not hold, and the effective auto_close mode with its source
(workspace or step). It changes nothing.

With --activity <row>, the same predicates run over the inputs stored in one
auto_close_skipped activity row, so the answer is the one the server gave when
it skipped the close, however the issue changed since. Find rows with
` + "`multica issue timeline <issue> --action auto_close_skipped`" + `.`,
	Example: `  multica issue close-check CHE-123
  multica issue close-check CHE-123 --activity <auto_close_skipped row id> --output json`,
	Args: exactArgs(1),
	RunE: runIssueCloseCheck,
}

func init() {
	issueCloseCheckCmd.Flags().String("activity", "", "Replay the predicates on the inputs of this auto_close_skipped activity row")
	issueCloseCheckCmd.Flags().String("output", "table", "Output format: table or json")
	issueCmd.AddCommand(issueCloseCheckCmd)
}

type closeCheckResult struct {
	IssueID    string `json:"issue_id"`
	Source     string `json:"source"`
	ActivityID string `json:"activity_id"`
	CheckedAt  string `json:"checked_at"`
	Mode       struct {
		Value  string `json:"value"`
		Source string `json:"source"`
	} `json:"mode"`
	Outcome              string `json:"outcome"`
	FirstFailure         string `json:"first_failure"`
	RecordedFirstFailure string `json:"recorded_first_failure"`
	Predicates           []struct {
		Name  string `json:"name"`
		Read  bool   `json:"read"`
		Held  bool   `json:"held"`
		Value string `json:"value"`
		Error string `json:"error"`
	} `json:"predicates"`
}

func runIssueCloseCheck(cmd *cobra.Command, args []string) error {
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
	path := "/api/issues/" + url.PathEscape(issueRef.ID) + "/close-check"
	if row, _ := cmd.Flags().GetString("activity"); row != "" {
		path += "?activity=" + url.QueryEscape(row)
	}
	var raw json.RawMessage
	if err := client.GetJSON(ctx, path, &raw); err != nil {
		return fmt.Errorf("close-check: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	var result closeCheckResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("read close-check response: %w", err)
	}
	printCloseCheck(os.Stdout, result)
	return nil
}

func printCloseCheck(w io.Writer, r closeCheckResult) {
	fmt.Fprintf(w, "source: %s", r.Source)
	if r.ActivityID != "" {
		fmt.Fprintf(w, " (activity %s)", r.ActivityID)
	}
	fmt.Fprintf(w, "\nchecked: %s\nmode: %s (from %s)\noutcome: %s\n", r.CheckedAt, r.Mode.Value, r.Mode.Source, r.Outcome)
	for _, p := range r.Predicates {
		mark := "ok  "
		switch {
		case !p.Read:
			mark = "--  "
		case !p.Held:
			mark = "FAIL"
		}
		detail := p.Value
		if p.Error != "" {
			detail = "unreadable: " + p.Error
		}
		fmt.Fprintf(w, "  %s %-26s %s\n", mark, p.Name, detail)
	}
	if r.FirstFailure == "" {
		fmt.Fprintln(w, "first failure: none")
	} else {
		fmt.Fprintf(w, "first failure: %s\n", r.FirstFailure)
	}
	if r.RecordedFirstFailure != "" {
		fmt.Fprintf(w, "recorded first failure: %s\n", r.RecordedFirstFailure)
	}
}
