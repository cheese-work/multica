package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueRelationsCmd = &cobra.Command{
	Use:   "relations <id>",
	Short: "Get issue relations in both directions as JSON",
	Long:  "Get the same nodes and typed edges as the issue sidebar graph, including issues that wait on or mention this issue. Returns at most 40 nodes and an omitted_count. Output is JSON.",
	Args:  exactArgs(1),
	RunE:  runIssueRelations,
}

func init() {
	issueCmd.AddCommand(issueRelationsCmd)
	issueRelationsCmd.Flags().Int("depth", 1, "Relation depth (1 or 2)")
}

func runIssueRelations(command *cobra.Command, args []string) error {
	depth, err := command.Flags().GetInt("depth")
	if err != nil {
		return err
	}
	if depth != 1 && depth != 2 {
		return fmt.Errorf("depth must be 1 or 2")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var graph json.RawMessage
	if err := client.GetJSON(ctx, "/api/issues/"+url.PathEscape(issue.ID)+"/relations?depth="+strconv.Itoa(depth), &graph); err != nil {
		return fmt.Errorf("get issue relations: %w", err)
	}
	return cli.PrintJSON(os.Stdout, graph)
}
