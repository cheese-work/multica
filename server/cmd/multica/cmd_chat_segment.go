package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// CHE-2819 L2b. A compacted DM hides its closed segments from the agent's
// normal reads; these two commands are the explicit way back. The task token
// names the DM, so there is no session argument: another DM's divider is a 404.

var chatSegmentsCmd = &cobra.Command{
	Use:   "segments",
	Short: "List the closed segments of this DM, oldest first, by divider id",
	Long: `List the closed segments of the DM you are running for. Each row is the divider
that closed the segment (its divider_id), when it was written, and how many
messages the segment holds. Read one with "multica chat segment <divider-id>".`,
	Args: cobra.NoArgs,
	RunE: runChatSegments,
}

var chatSegmentCmd = &cobra.Command{
	Use:   "segment <divider-id>",
	Short: "Read one closed segment of this DM (newest page first)",
	Long: `Read the messages of one closed segment, named by a divider_id from
"multica chat segments". The reply lists the newest page; when has_more is
true it carries next_before, and --before <next_before> reads the next older
page. A divider of another DM, or an id that is not a divider, is not found.`,
	Args: cobra.ExactArgs(1),
	RunE: runChatSegment,
}

func init() {
	chatSegmentsCmd.Flags().String("output", "json", "Output format: table or json")
	chatSegmentCmd.Flags().Int("limit", 0, "Maximum number of messages to return (the server clamps the range)")
	chatSegmentCmd.Flags().String("before", "", "next_before from a prior page (<created_at>,<id>) to read older messages")
	chatCmd.AddCommand(chatSegmentsCmd, chatSegmentCmd)
}

func runChatSegments(cmd *cobra.Command, _ []string) error {
	var resp struct {
		Segments []struct {
			DividerID    string `json:"divider_id"`
			CreatedAt    string `json:"created_at"`
			MessageCount int    `json:"message_count"`
		} `json:"segments"`
	}
	if err := getChatSegmentJSON(cmd, "/api/chat/segments", &resp); err != nil {
		return err
	}
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(cmd.OutOrStdout(), resp)
	}
	rows := make([][]string, len(resp.Segments))
	for i, s := range resp.Segments {
		rows[i] = []string{s.DividerID, s.CreatedAt, strconv.Itoa(s.MessageCount)}
	}
	cli.PrintTable(cmd.OutOrStdout(), []string{"DIVIDER_ID", "CREATED_AT", "MESSAGES"}, rows)
	return nil
}

func runChatSegment(cmd *cobra.Command, args []string) error {
	q := url.Values{"divider": {args[0]}}
	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before, _ := cmd.Flags().GetString("before"); before != "" {
		at, id, ok := strings.Cut(before, ",")
		if !ok || at == "" || id == "" {
			return fmt.Errorf("invalid --before %q: want the next_before of a prior page (<created_at>,<id>)", before)
		}
		q.Set("before_created_at", at)
		q.Set("before_id", id)
	}
	var resp map[string]any
	if err := getChatSegmentJSON(cmd, "/api/chat/segment?"+q.Encode(), &resp); err != nil {
		return err
	}
	// Hand the agent the cursor in the form --before takes.
	if c, ok := resp["next_cursor"].(map[string]any); ok {
		resp["next_before"] = fmt.Sprintf("%v,%v", c["created_at"], c["id"])
	}
	return cli.PrintJSON(cmd.OutOrStdout(), resp)
}

func getChatSegmentJSON(cmd *cobra.Command, path string, out any) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	if err := client.GetJSON(ctx, path, out); err != nil {
		return fmt.Errorf("read chat segment: %w", err)
	}
	return nil
}
