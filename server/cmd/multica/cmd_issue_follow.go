package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// DM progress follows (CHE-3403). A member names the chat with --chat; an agent
// passes none and the server derives the lane from its task's DM.

var issueFollowCmd = &cobra.Command{
	Use:   "follow <issue-id>",
	Short: "Follow an issue's progress in a DM",
	Long:  "Members pass --chat <chat-session-id> for a DM they created. Agents pass no --chat: the lane comes from the DM that started the task, and a task with no DM is refused.",
	Args:  exactArgs(1),
	RunE:  runIssueFollowAction("POST", ""),
}

var issueUnfollowCmd = &cobra.Command{
	Use:   "unfollow <issue-id>",
	Short: "Stop following an issue's progress in a DM",
	Long:  "--scope issue (default) ends this issue's follow. --scope root also ends the follows its children inherited from it; a child with an independent manual follow stays followed.",
	Args:  exactArgs(1),
	RunE:  runIssueFollowAction("DELETE", ""),
}

var issueFollowMuteCmd = &cobra.Command{
	Use:   "mute-follow <issue-id>",
	Short: "Mute a followed issue's progress cards in a DM",
	Args:  exactArgs(1),
	RunE:  runIssueFollowAction("POST", "/mute"),
}

var issueFollowUnmuteCmd = &cobra.Command{
	Use:   "unmute-follow <issue-id>",
	Short: "Unmute a followed issue's progress cards in a DM",
	Args:  exactArgs(1),
	RunE:  runIssueFollowAction("POST", "/unmute"),
}

var issueFollowsCmd = &cobra.Command{
	Use:   "follows",
	Short: "List issues followed in a DM you created",
	Args:  exactArgs(0),
	RunE:  runIssueFollows,
}

func init() {
	for _, c := range []*cobra.Command{issueFollowCmd, issueUnfollowCmd, issueFollowMuteCmd, issueFollowUnmuteCmd, issueFollowsCmd} {
		c.Flags().String("chat", "", "Chat session (DM) id; members only")
		c.Flags().String("output", "json", "Output format: table or json")
		issueCmd.AddCommand(c)
	}
	issueUnfollowCmd.Flags().String("scope", "", "Unfollow scope: issue (default) or root")
	_ = issueFollowsCmd.MarkFlagRequired("chat")
}

func chatQuery(cmd *cobra.Command) string {
	query := url.Values{}
	if chat, _ := cmd.Flags().GetString("chat"); chat != "" {
		query.Set("chat_session_id", chat)
	}
	if scope, _ := cmd.Flags().GetString("scope"); scope != "" {
		query.Set("scope", scope)
	}
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}

func printFollowResult(cmd *cobra.Command, raw json.RawMessage) error {
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		fmt.Fprintln(os.Stdout, "ok")
		return nil
	}
	return cli.PrintJSON(os.Stdout, raw)
}

func runIssueFollowAction(method, suffix string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		ctx, cancel := cli.APIContext(context.Background())
		defer cancel()
		issueRef, err := resolveIssueRef(ctx, client, args[0])
		if err != nil {
			return fmt.Errorf("resolve issue: %w", err)
		}
		path := "/api/issues/" + issueRef.ID + "/progress-follow" + suffix + chatQuery(cmd)
		var raw json.RawMessage
		if method == "DELETE" {
			err = client.DeleteJSON(ctx, path)
			raw = json.RawMessage(`{}`)
		} else {
			err = client.PostJSON(ctx, path, nil, &raw)
		}
		if err != nil {
			return fmt.Errorf("%s progress follow: %w", method, err)
		}
		return printFollowResult(cmd, raw)
	}
}

func runIssueFollows(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var raw json.RawMessage
	if err := client.GetJSON(ctx, "/api/dm-progress/follows"+chatQuery(cmd), &raw); err != nil {
		return fmt.Errorf("list progress follows: %w", err)
	}
	return printFollowResult(cmd, raw)
}
