package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var chatUnseenCmd = &cobra.Command{
	Use: "unseen", Short: "List DMs with unread messages",
	Args: cobra.NoArgs, RunE: runChatUnseen,
}

var chatMarkUnseenCmd = &cobra.Command{
	Use: "mark-unseen <session-id>", Short: "Mark a DM unseen so it shows unread again",
	Args: cobra.ExactArgs(1), RunE: runChatMarkUnseen,
}

func init() {
	chatUnseenCmd.Flags().String("output", "table", "Output format: table or json")
	chatCmd.AddCommand(chatUnseenCmd, chatMarkUnseenCmd)
}

type chatUnseenSession struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	UnreadCount int    `json:"unread_count"`
	Unseen      bool   `json:"unseen"`
	UpdatedAt   string `json:"updated_at"`
}

func runChatUnseen(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var sessions []chatUnseenSession
	if err := client.GetJSON(ctx, "/api/chat/sessions", &sessions); err != nil {
		return fmt.Errorf("list chat sessions: %w", err)
	}
	unseen := make([]chatUnseenSession, 0, len(sessions))
	for _, session := range sessions {
		if session.UnreadCount > 0 {
			session.Unseen = true
			unseen = append(unseen, session)
		}
	}
	if output, _ := command.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, unseen)
	}
	rows := make([][]string, 0, len(unseen))
	for _, session := range unseen {
		rows = append(rows, []string{session.ID, session.Title, strconv.Itoa(session.UnreadCount), session.UpdatedAt})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "TITLE", "UNREAD", "UPDATED_AT"}, rows)
	return nil
}

func runChatMarkUnseen(command *cobra.Command, args []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	if err := client.PostJSON(ctx, "/api/chat/sessions/"+url.PathEscape(args[0])+"/unread", map[string]any{}, nil); err != nil {
		return fmt.Errorf("mark chat session unseen: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Marked chat session %s unseen.\n", args[0])
	return nil
}
