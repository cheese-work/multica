package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueDescriptionCmd = &cobra.Command{
	Use:   "description",
	Short: "Work with an issue's description revisions",
}

var issueDescriptionHistoryCmd = &cobra.Command{
	Use:   "history <id>",
	Short: "List the recorded description revisions of an issue",
	Args:  exactArgs(1),
	RunE:  runIssueDescriptionHistory,
}

var issueDescriptionDiffCmd = &cobra.Command{
	Use:   "diff <id> --from N --to M",
	Short: "Print a unified diff between two description revisions",
	Args:  exactArgs(1),
	RunE:  runIssueDescriptionDiff,
}

func init() {
	issueCmd.AddCommand(issueDescriptionCmd)
	issueDescriptionCmd.AddCommand(issueDescriptionHistoryCmd, issueDescriptionDiffCmd)

	issueDescriptionHistoryCmd.Flags().String("output", "table", "Output format: table or json")
	issueDescriptionDiffCmd.Flags().String("output", "table", "Output format: table or json")
	issueDescriptionDiffCmd.Flags().Int64("from", 0, "Earlier revision number (required)")
	issueDescriptionDiffCmd.Flags().Int64("to", 0, "Later revision number (required)")
}

func runIssueDescriptionHistory(cmd *cobra.Command, args []string) error {
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
	if err := client.GetJSON(ctx, "/api/issues/"+issueRef.ID+"/description/history", &result); err != nil {
		return fmt.Errorf("description history: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	revisions, _ := result["revisions"].([]any)
	rows := make([][]string, 0, len(revisions))
	for _, item := range revisions {
		rev, _ := item.(map[string]any)
		actor := "-"
		if t, _ := rev["actor_type"].(string); t != "" {
			id, _ := rev["actor_id"].(string)
			actor = t + ":" + id
		}
		n, _ := rev["revision"].(float64)
		rows = append(rows, []string{strconv.FormatInt(int64(n), 10), ictTime(rev["created_at"]), actor})
	}
	cli.PrintTable(os.Stdout, []string{"REVISION", "TIME", "ACTOR"}, rows)
	return nil
}

func runIssueDescriptionDiff(cmd *cobra.Command, args []string) error {
	from, _ := cmd.Flags().GetInt64("from")
	to, _ := cmd.Flags().GetInt64("to")
	if from < 1 {
		return fmt.Errorf("--from is required (revision number, 1 or more)")
	}
	if to < 1 {
		return fmt.Errorf("--to is required (revision number, 1 or more)")
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
	q := url.Values{"from": {strconv.FormatInt(from, 10)}, "to": {strconv.FormatInt(to, 10)}}
	var result map[string]any
	if err := client.GetJSON(ctx, "/api/issues/"+issueRef.ID+"/description/diff?"+q.Encode(), &result); err != nil {
		return fmt.Errorf("description diff: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	diff, _ := result["diff"].(string)
	fmt.Fprint(os.Stdout, diff)
	return nil
}
