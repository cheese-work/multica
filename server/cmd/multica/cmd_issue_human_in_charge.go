package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueHumanInChargeCmd = &cobra.Command{
	Use:   "human-in-charge <issue>",
	Short: "Show who to ask for a decision on an issue",
	Long:  "Prints the member to address a decision request to, or \"none\" with a reason. Call it from a running task on the issue before sending the request.",
	Args:  exactArgs(1),
	RunE:  runIssueHumanInCharge,
}

func init() {
	issueHumanInChargeCmd.Flags().String("output", "table", "Output format: table or json")
	issueCmd.AddCommand(issueHumanInChargeCmd)
}

func runIssueHumanInCharge(cmd *cobra.Command, args []string) error {
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
	var result struct {
		Human *struct {
			UserID string `json:"user_id"`
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"human"`
		Reason string `json:"reason"`
	}
	if err := client.GetJSON(ctx, "/api/issues/"+url.PathEscape(issueRef.ID)+"/human-in-charge", &result); err != nil {
		return fmt.Errorf("get human in charge: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	if result.Human == nil {
		fmt.Fprintf(os.Stdout, "none: %s\n", result.Reason)
		return nil
	}
	fmt.Fprintf(os.Stdout, "%s\t%s\t%s\n", result.Human.UserID, result.Human.Name, result.Human.Source)
	return nil
}
