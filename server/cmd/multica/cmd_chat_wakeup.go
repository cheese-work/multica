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

Add --title (at most 120 characters) for a short label the chat shows on the
wakeup row; the model still receives only the instruction.`,
}

var chatWakeupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Schedule a wakeup (--after <duration> or --at <RFC3339>)",
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
	if (after == "") == (at == "") {
		return fmt.Errorf("exactly one of --after or --at is required")
	}
	body := map[string]any{"instruction": instruction}
	if title, _ := cmd.Flags().GetString("title"); strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	if after != "" {
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
		table = append(table, []string{strVal(row, "id"), strVal(row, "fire_at"), strVal(row, "status"), strVal(row, "title"), strVal(row, "instruction")})
	}
	cli.PrintTable(cmd.OutOrStdout(), []string{"ID", "FIRE AT", "STATUS", "TITLE", "INSTRUCTION"}, table)
	return nil
}
