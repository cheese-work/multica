package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var chatWakeupCmd = &cobra.Command{
	Use:   "wakeup",
	Short: "Schedule a one-shot refresh turn in the current DM",
	Long: `Schedule, list or cancel wakeups for the DM you are running in.

At the set time the server sends a labelled scheduled turn into this DM, so you
can refresh a stale answer without a user message. A turn already running makes
the wakeup queue behind it. A DM holds at most 3 pending wakeups, each at most
24h ahead. Only the DM's own agent, from a running chat task, may manage them.

Instead of a time, --issue <id> watches an issue and wakes this DM once when
--event comment.created (optionally --filter-actor-type member|agent and
--filter-actor-id) happens, or when the issue reaches --until-status. The
refresh turn quotes the triggering comment or status. Add --expires-in
(at most 24h, default 24h) and --on-timeout wake to get a turn if nothing
happened; the default is to drop silently. You must be able to read the issue.

Add --title (at most 120 characters) for a short label the chat shows on the
wakeup row; the model still receives only the instruction.`,
}

var chatWakeupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Schedule a wakeup (--after, --at, or --issue with --event/--until-status)",
	Args:  cobra.NoArgs,
	RunE:  runChatWakeupCreate,
}

var chatWakeupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending wakeups of the current DM",
	Args:  cobra.NoArgs,
	RunE:  runChatWakeupList,
}

var chatWakeupCancelCmd = &cobra.Command{
	Use:   "cancel <wakeup-id>",
	Short: "Cancel a pending wakeup",
	Args:  cobra.ExactArgs(1),
	RunE:  runChatWakeupCancel,
}

func init() {
	chatWakeupCreateCmd.Flags().String("after", "", "Delay before the wakeup fires, e.g. 25m or 2h")
	chatWakeupCreateCmd.Flags().String("at", "", "Absolute fire time (RFC3339)")
	chatWakeupCreateCmd.Flags().String("issue", "", "Watch this issue (UUID or identifier) instead of a time")
	chatWakeupCreateCmd.Flags().String("event", "", "With --issue: wake on this event (comment.created)")
	chatWakeupCreateCmd.Flags().String("filter-actor-type", "", "With --event: only comments by member or agent")
	chatWakeupCreateCmd.Flags().String("filter-actor-id", "", "With --filter-actor-type: only this actor's UUID")
	chatWakeupCreateCmd.Flags().String("until-status", "", "With --issue: wake when the issue reaches this status")
	chatWakeupCreateCmd.Flags().String("expires-in", "", "With --issue: give up after this long, e.g. 2h (max and default 24h)")
	chatWakeupCreateCmd.Flags().String("on-timeout", "", "With --issue: wake (send a timed-out turn) or drop (default)")
	chatWakeupCreateCmd.Flags().String("instruction", "", "What the scheduled turn should do (required)")
	chatWakeupCreateCmd.Flags().String("title", "", "Optional short label shown on the wakeup row in chat (max 120 characters)")
	chatWakeupCreateCmd.Flags().String("output", "json", "Output format: table or json")
	chatWakeupListCmd.Flags().String("output", "json", "Output format: table or json")
	chatWakeupCmd.AddCommand(chatWakeupCreateCmd, chatWakeupListCmd, chatWakeupCancelCmd)
	chatCmd.AddCommand(chatWakeupCmd)
}

func runChatWakeupCreate(cmd *cobra.Command, _ []string) error {
	after, _ := cmd.Flags().GetString("after")
	at, _ := cmd.Flags().GetString("at")
	instruction, _ := cmd.Flags().GetString("instruction")
	if strings.TrimSpace(instruction) == "" {
		return fmt.Errorf("--instruction is required")
	}
	body := map[string]any{"instruction": instruction}
	if issue, _ := cmd.Flags().GetString("issue"); issue != "" {
		if after != "" || at != "" {
			return fmt.Errorf("--issue cannot be combined with --after or --at; use --expires-in")
		}
		if err := addChatWatchFlags(cmd, body, issue); err != nil {
			return err
		}
	} else if err := rejectChatWatchFlags(cmd); err != nil {
		return err
	} else if (after == "") == (at == "") {
		return fmt.Errorf("exactly one of --after or --at is required")
	}
	if title, _ := cmd.Flags().GetString("title"); strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	if body["issue"] != nil {
		// issue watch: no fire time, expiry is --expires-in
	} else if after != "" {
		delay, err := time.ParseDuration(after)
		if err != nil {
			return fmt.Errorf("--after must be a duration like 25m or 2h: %w", err)
		}
		body["after_seconds"] = int64(delay.Seconds())
	} else {
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			return fmt.Errorf("--at must be an RFC3339 time: %w", err)
		}
		body["at"] = at
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var response map[string]any
	if err := client.PostJSON(ctx, "/api/chat/wakeups", body, &response); err != nil {
		return fmt.Errorf("create wakeup: %w", err)
	}
	return printChatWakeups(cmd, response, []map[string]any{response})
}

func runChatWakeupList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var response []map[string]any
	if err := client.GetJSON(ctx, "/api/chat/wakeups", &response); err != nil {
		return fmt.Errorf("list wakeups: %w", err)
	}
	return printChatWakeups(cmd, response, response)
}

func runChatWakeupCancel(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	if err := client.DeleteJSON(ctx, "/api/chat/wakeups/"+url.PathEscape(args[0])); err != nil {
		return fmt.Errorf("cancel wakeup: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wakeup %s cancelled.\n", args[0])
	return nil
}

func printChatWakeups(cmd *cobra.Command, asJSON any, rows []map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output != "table" {
		return cli.PrintJSON(cmd.OutOrStdout(), asJSON)
	}
	table := make([][]string, 0, len(rows))
	for _, row := range rows {
		table = append(table, []string{strVal(row, "id"), strVal(row, "fire_at"), strVal(row, "status"), chatWakeupLabel(row), strVal(row, "instruction")})
	}
	cli.PrintTable(cmd.OutOrStdout(), []string{"ID", "FIRE AT", "STATUS", "TITLE", "INSTRUCTION"}, table)
	return nil
}

var chatWatchStringFlags = map[string]string{
	"event": "event", "filter-actor-type": "filter_actor_type", "filter-actor-id": "filter_actor_id",
	"until-status": "until_status", "on-timeout": "on_timeout",
}

func addChatWatchFlags(cmd *cobra.Command, body map[string]any, issue string) error {
	body["issue"] = issue
	for flag, key := range chatWatchStringFlags {
		if value, _ := cmd.Flags().GetString(flag); value != "" {
			body[key] = value
		}
	}
	if body["event"] == nil && body["until_status"] == nil {
		return fmt.Errorf("--issue needs --event or --until-status")
	}
	if expires, _ := cmd.Flags().GetString("expires-in"); expires != "" {
		d, err := time.ParseDuration(expires)
		if err != nil {
			return fmt.Errorf("--expires-in must be a duration like 2h: %w", err)
		}
		body["expires_in_seconds"] = int64(d.Seconds())
	}
	return nil
}

func rejectChatWatchFlags(cmd *cobra.Command) error {
	for _, flag := range []string{"event", "filter-actor-type", "filter-actor-id", "until-status", "expires-in", "on-timeout"} {
		if value, _ := cmd.Flags().GetString(flag); value != "" {
			return fmt.Errorf("--%s needs --issue", flag)
		}
	}
	return nil
}

func chatWakeupLabel(row map[string]any) string {
	if issue := strVal(row, "issue_id"); issue != "" {
		return "watch " + issue + " " + strVal(row, "event") + strVal(row, "until_status")
	}
	return strVal(row, "title")
}
