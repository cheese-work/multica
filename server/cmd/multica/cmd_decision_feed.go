package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// The answer change feed is for the X99 Hindsight sweep only: the server refuses
// it unless the PAT carries the decisions:feed scope.
var decisionFeedCmd = &cobra.Command{
	Use: "feed", Short: "Read decision answers whose state changed (sweep PAT only)", Args: cobra.NoArgs, RunE: runDecisionFeed,
}

var decisionFeedAckCmd = &cobra.Command{
	Use: "ack <answer id>", Short: "Mark an answer's state version as written to Hindsight", Args: cobra.ExactArgs(1), RunE: runDecisionFeedAck,
}

func init() {
	decisionFeedAckCmd.Flags().Int64("version", 0, "The state_version the sweep wrote; applied only when higher than the stored one")
	_ = decisionFeedAckCmd.MarkFlagRequired("version")
	decisionFeedCmd.AddCommand(decisionFeedAckCmd)
	decisionFeedCmd.PersistentFlags().String("output", "json", "Output format: table or json")
	decisionCmd.AddCommand(decisionFeedCmd)
}

func runDecisionFeed(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := client.GetJSON(ctx, "/api/decisions/feed", &result); err != nil {
		return fmt.Errorf("read decision feed: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runDecisionFeedAck(command *cobra.Command, args []string) error {
	version, _ := command.Flags().GetInt64("version")
	if version < 1 {
		return fmt.Errorf("--version must be a positive integer")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/decisions/feed/"+url.PathEscape(args[0])+"/ack", map[string]any{"version": version}, &result); err != nil {
		return fmt.Errorf("ack decision feed: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
