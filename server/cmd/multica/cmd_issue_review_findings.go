package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueReviewFindingsCmd = &cobra.Command{
	Use:   "review-findings",
	Short: "List review findings and the top finding categories of the last days",
	Long: "List the most frequent review finding categories (default: top 10 of the last 14 days).\n" +
		"An author runs the top categories as a pre-check before asking for review.\n" +
		"With --list, print every finding with its category, severity and issue instead.",
	Args: exactArgs(0),
	RunE: runIssueReviewFindings,
}

func init() {
	issueReviewFindingsCmd.Flags().String("output", "table", "Output format: table or json")
	issueReviewFindingsCmd.Flags().Int("days", 14, "Look back this many days")
	issueReviewFindingsCmd.Flags().Int("limit", 10, "Number of top categories")
	issueReviewFindingsCmd.Flags().Bool("list", false, "List each finding instead of the category counts")
	issueCmd.AddCommand(issueReviewFindingsCmd)
}

func runIssueReviewFindings(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	days, _ := cmd.Flags().GetInt("days")
	limit, _ := cmd.Flags().GetInt("limit")
	var resp struct {
		Days          int              `json:"days"`
		TopCategories []map[string]any `json:"top_categories"`
		Findings      []map[string]any `json:"findings"`
	}
	path := "/api/issues/review-findings?days=" + strconv.Itoa(days) + "&limit=" + strconv.Itoa(limit)
	if err := client.GetJSON(ctx, path, &resp); err != nil {
		return fmt.Errorf("list review findings: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if list, _ := cmd.Flags().GetBool("list"); list {
		rows := make([][]string, 0, len(resp.Findings))
		for _, f := range resp.Findings {
			rows = append(rows, []string{fmt.Sprintf("%v", f["issue_number"]), strVal(f, "verdict"), strVal(f, "severity"), strVal(f, "category"), strVal(f, "summary")})
		}
		cli.PrintTable(os.Stdout, []string{"ISSUE", "VERDICT", "SEVERITY", "CATEGORY", "SUMMARY"}, rows)
		return nil
	}
	rows := make([][]string, 0, len(resp.TopCategories))
	for _, c := range resp.TopCategories {
		rows = append(rows, []string{strVal(c, "category"), fmt.Sprintf("%v", c["count"])})
	}
	cli.PrintTable(os.Stdout, []string{"CATEGORY", "COUNT"}, rows)
	return nil
}
