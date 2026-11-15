package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var chatReplyStateCmd = &cobra.Command{
	Use:   "reply-state question|done|info [--ask <id>] [--new]",
	Short: "Declare the outcome of this DM turn",
	Long: `Declare what this turn leaves the human owing, from a running DM task.

  question [--ask <id> | --new]  The reply asks the human something. --ask continues
                                 one of your open asks; --new opens another. With
                                 neither, the server uses your only open ask or opens
                                 one; with two or more open asks it refuses.
  done --ask <id> [--ask <id>]   The reply settles the named asks. --ask is required.
  info                           Nothing is owed by the human.

The prompt lists your open ask ids. An invalid outcome is refused and the turn is
recorded as unclassified.`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"question", "done", "info"},
	RunE:      runChatReplyState,
}

func init() {
	chatReplyStateCmd.Flags().StringArray("ask", nil, "Open ask id to continue (question) or settle (done); repeatable for done")
	chatReplyStateCmd.Flags().Bool("new", false, "With question: open a new ask")
	chatCmd.AddCommand(chatReplyStateCmd)
}

func runChatReplyState(cmd *cobra.Command, args []string) error {
	kind := args[0]
	asks, _ := cmd.Flags().GetStringArray("ask")
	isNew, _ := cmd.Flags().GetBool("new")
	body := map[string]any{"kind": kind}
	switch kind {
	case "question":
		switch {
		case isNew && len(asks) > 0:
			return fmt.Errorf("use either --ask or --new, not both")
		case len(asks) > 1:
			return fmt.Errorf("question takes at most one --ask")
		case isNew:
			body["new"] = true
		case len(asks) == 1:
			body["ask"] = asks[0]
		}
	case "done":
		if isNew {
			return fmt.Errorf("done does not take --new")
		}
		if len(asks) == 0 {
			return fmt.Errorf("done needs --ask <id> for each ask it settles")
		}
		body["ask_ids"] = asks
	case "info":
		if isNew || len(asks) > 0 {
			return fmt.Errorf("info takes no --ask or --new")
		}
	default:
		return fmt.Errorf("outcome must be question, done or info, got %q", kind)
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var response struct {
		AskIDs []string `json:"ask_ids"`
	}
	if err := client.PostJSON(ctx, "/api/chat/outcome", body, &response); err != nil {
		return fmt.Errorf("reply-state %s: %w", kind, err)
	}
	if len(response.AskIDs) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s.\n", kind)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s for ask %s.\n", kind, strings.Join(response.AskIDs, ", "))
	return nil
}
