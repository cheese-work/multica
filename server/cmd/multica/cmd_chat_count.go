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

var chatCountCmd = &cobra.Command{
	Use:   "count",
	Short: "Count one member's DM messages of one kind in a time window",
	Long: `Count the progress cards in one member's DMs since a time. Read-only and
count-only: the output is {count, since, until}; no message text leaves the server.

An agent run counts on behalf of the member only when the run is the member's or
the calling agent is owned by the member. A member counts only their own DMs.

Example (flow metric 7 of the daily brief):
  multica chat count --member <user-id> --kind progress-card --since 2026-11-23T00:00:00Z --output json`,
	Args: cobra.NoArgs,
	RunE: runChatCount,
}

func init() {
	chatCountCmd.Flags().String("member", "", "User id of the member whose DMs are counted (required)")
	chatCountCmd.Flags().String("kind", "progress-card", "Message kind to count: progress-card")
	chatCountCmd.Flags().String("since", "", "Window start, RFC3339 (required)")
	chatCountCmd.Flags().String("until", "", "Window end, RFC3339 (default: now)")
	chatCountCmd.Flags().String("output", "json", "Output format: json or table")
	chatCmd.AddCommand(chatCountCmd)
}

func runChatCount(cmd *cobra.Command, _ []string) error {
	member, _ := cmd.Flags().GetString("member")
	kind, _ := cmd.Flags().GetString("kind")
	since, _ := cmd.Flags().GetString("since")
	until, _ := cmd.Flags().GetString("until")
	if member == "" {
		return fmt.Errorf("--member is required")
	}
	if since == "" {
		return fmt.Errorf("--since is required")
	}
	query := url.Values{"member": {member}, "kind": {kind}, "since": {since}}
	if until != "" {
		query.Set("until", until)
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp map[string]any
	if err := client.GetJSON(ctx, "/api/chat/count?"+query.Encode(), &resp); err != nil {
		return fmt.Errorf("count chat messages: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		n, _ := resp["count"].(float64)
		cli.PrintTable(os.Stdout, []string{"COUNT", "SINCE", "UNTIL"}, [][]string{{strconv.Itoa(int(n)), strVal(resp, "since"), strVal(resp, "until")}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, resp)
}
