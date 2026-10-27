package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/spf13/cobra"
)

var chatRenameCmd = &cobra.Command{
	Use:   "rename <session-id> <title>",
	Short: "Rename a DM and announce the title change and actor without reading its contents",
	Args:  cobra.ExactArgs(2),
	RunE:  runChatRename,
}

func init() {
	chatRenameCmd.Flags().String("output", "json", "Output format: table or json")
	chatCmd.AddCommand(chatRenameCmd)
}

func runChatRename(command *cobra.Command, args []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result protocol.ChatRenameResponse
	if err := client.PatchJSON(ctx, "/api/chat/sessions/"+url.PathEscape(args[0]), map[string]string{"title": args[1]}, &result); err != nil {
		return fmt.Errorf("rename chat: %w", err)
	}
	output, _ := command.Flags().GetString("output")
	if output == "table" {
		cli.PrintTable(os.Stdout, []string{"ID", "TITLE", "PREVIOUS_TITLE", "MESSAGE_ID"}, [][]string{{result.ID, result.Title, result.PreviousTitle, result.MessageID}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}
