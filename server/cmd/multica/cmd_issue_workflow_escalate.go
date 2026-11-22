package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueWorkflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Work with an issue's workflow",
}

var issueWorkflowEscalateCmd = &cobra.Command{
	Use:   "escalate <id>",
	Short: "Move a Quick issue to its full workflow",
	Long: "Move an issue that follows Quick back to the full workflow it followed before, " +
		"and say why with --reason. The server picks the target from the issue's newest " +
		"Quick entry; no workflow can be named. From todo or investigating the issue needs an " +
		"`Estimate:` line in its description or a comment, unless the reason is `rollback`.\n\n" +
		"With no live task the issue enters the full workflow's step now. With a live task " +
		"the switch is pending and applies after the last task ends. An issue that was never " +
		"on Quick, or that has no record of entering it, is refused.",
	Example: "  multica issue workflow escalate MUL-12 --reason \"needs a schema change\"\n" +
		"  multica issue workflow escalate MUL-12 --reason rollback",
	Args: exactArgs(1),
	RunE: runIssueWorkflowEscalate,
}

func runIssueWorkflowEscalate(cmd *cobra.Command, args []string) error {
	reason, _ := cmd.Flags().GetString("reason")
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("--reason is required: say why the issue leaves Quick")
	}
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
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/issues/"+issueRef.ID+"/workflow/escalate", map[string]any{"reason": reason}, &result); err != nil {
		return fmt.Errorf("escalate issue: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	if strVal(result, "state") == "pending" {
		fmt.Fprintf(os.Stdout, "Escalation to workflow %s is pending: the issue has a live task, and the switch applies after the last one ends.\n", strVal(result, "workflow_id"))
		return nil
	}
	fmt.Fprintf(os.Stdout, "Issue escalated to workflow %s.\n", strVal(result, "workflow_id"))
	return nil
}

func init() {
	issueCmd.AddCommand(issueWorkflowCmd)
	issueWorkflowCmd.AddCommand(issueWorkflowEscalateCmd)
	issueWorkflowEscalateCmd.Flags().String("reason", "", "Why the issue leaves Quick (required); `rollback` needs no Estimate")
	issueWorkflowEscalateCmd.Flags().String("output", "table", "Output format: table or json")
}
