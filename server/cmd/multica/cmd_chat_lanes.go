package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var chatSessionsCmd = &cobra.Command{
	Use: "sessions", Short: "List DM lane metadata for a named person in this issue task's decision window",
	Args: cobra.NoArgs, RunE: runChatSessions,
}

func init() {
	chatSessionsCmd.Flags().String("issue", "", "Issue with an open decision-intent window bound to the calling task")
	chatSessionsCmd.Flags().String("for", "", "Named person's user ID (required for a multi-person window)")
	chatSessionsCmd.Flags().String("output", "json", "Output format: table or json")
	_ = chatSessionsCmd.MarkFlagRequired("issue")
	chatCmd.AddCommand(chatSessionsCmd)
}

func runChatSessions(command *cobra.Command, _ []string) error {
	issueID, _ := command.Flags().GetString("issue")
	if issueID == "" {
		return fmt.Errorf("--issue is required for windowed lane access")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	query := url.Values{"issue": {issueID}}
	target, _ := command.Flags().GetString("for")
	if target != "" || command.Flags().Changed("for") {
		query.Set("for", target)
	}
	var lanes []map[string]any
	if err := client.GetJSON(ctx, "/api/chat/sessions?"+query.Encode(), &lanes); err != nil {
		return fmt.Errorf("list decision lanes: %w", err)
	}
	output, _ := command.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, lanes)
	}
	rows := make([][]string, 0, len(lanes))
	for _, lane := range lanes {
		rows = append(rows, []string{strVal(lane, "id"), strVal(lane, "title"), strVal(lane, "lane_summary"), strVal(lane, "lane_summary_updated_at"), strVal(lane, "updated_at")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "TITLE", "LANE_SUMMARY", "SUMMARY_UPDATED_AT", "UPDATED_AT"}, rows)
	return nil
}
