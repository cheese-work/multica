package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueUrgentOrphansCmd = &cobra.Command{
	Use:   "urgent-orphans",
	Short: "List urgent issues that no task or wakeup will pick up",
	Long: "List open urgent issues with no queued, dispatched or running task and no\n" +
		"enabled wakeup. The PULSE sweep reads this list so an urgent issue never\n" +
		"sits unowned.",
	Args: exactArgs(0),
	RunE: runIssueUrgentOrphans,
}

func runIssueUrgentOrphans(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var resp struct {
		Issues []map[string]any `json:"issues"`
	}
	if err := client.GetJSON(ctx, "/api/issues/urgent-orphans", &resp); err != nil {
		return fmt.Errorf("list urgent orphan issues: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		rows := make([][]string, 0, len(resp.Issues))
		for _, i := range resp.Issues {
			rows = append(rows, []string{
				fmt.Sprintf("%v", i["number"]), strVal(i, "title"), strVal(i, "status"),
				strVal(i, "assignee_type"), strVal(i, "updated_at"),
			})
		}
		cli.PrintTable(os.Stdout, []string{"NUMBER", "TITLE", "STATUS", "ASSIGNEE TYPE", "UPDATED"}, rows)
		return nil
	}
	return cli.PrintJSON(os.Stdout, resp)
}
