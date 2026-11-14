package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueInputsCmd = &cobra.Command{
	Use:   "inputs",
	Short: "List, resume or discard the triggers held or parked for an issue's agent",
	Long: "Triggers that arrive while a project is paused or at its WIP limit are held. " +
		"A run that ends any way other than completed parks its batch; parked inputs " +
		"start nothing until a member resumes them or discards them.",
}

var issueInputsListCmd = &cobra.Command{
	Use:   "list <issue-id>",
	Short: "List held, claimed and parked inputs of an issue",
	Args:  exactArgs(1),
	RunE:  runIssueInputsList,
}

var issueInputsResumeCmd = &cobra.Command{
	Use:   "resume <issue-id>",
	Short: "Return parked inputs to held and queue a run to carry them",
	Args:  exactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runIssueInputsAction(cmd, args, "resume") },
}

var issueInputsDiscardCmd = &cobra.Command{
	Use:   "discard <issue-id>",
	Short: "Discard the parked inputs of an issue's agent",
	Args:  exactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runIssueInputsAction(cmd, args, "discard") },
}

func init() {
	issueCmd.AddCommand(issueInputsCmd)
	issueInputsCmd.AddCommand(issueInputsListCmd, issueInputsResumeCmd, issueInputsDiscardCmd)
	issueInputsListCmd.Flags().String("output", "table", "Output format: table or json")
	for _, c := range []*cobra.Command{issueInputsResumeCmd, issueInputsDiscardCmd} {
		c.Flags().String("agent", "", "Agent ID whose batch to act on (default: the issue's agent assignee)")
		c.Flags().String("output", "json", "Output format: table or json")
	}
}

func runIssueInputsList(cmd *cobra.Command, args []string) error {
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
	var out struct {
		Inputs []map[string]any `json:"inputs"`
	}
	if err := client.GetJSON(ctx, "/api/issues/"+issueRef.ID+"/inputs", &out); err != nil {
		return fmt.Errorf("list inputs: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, out)
	}
	rows := make([][]string, 0, len(out.Inputs))
	for _, in := range out.Inputs {
		rows = append(rows, []string{strVal(in, "event_id"), strVal(in, "kind"), strVal(in, "state"), strVal(in, "park_reason"), strVal(in, "failure_class")})
	}
	cli.PrintTable(os.Stdout, []string{"EVENT", "KIND", "STATE", "PARK REASON", "FAILURE CLASS"}, rows)
	return nil
}

func runIssueInputsAction(cmd *cobra.Command, args []string, action string) error {
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
	body := map[string]any{}
	if agent, _ := cmd.Flags().GetString("agent"); agent != "" {
		body["agent_id"] = agent
	}
	var out map[string]any
	if err := client.PostJSON(ctx, "/api/issues/"+issueRef.ID+"/inputs/"+action, body, &out); err != nil {
		return fmt.Errorf("%s inputs: %w", action, err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, out)
	}
	fmt.Fprintf(os.Stdout, "%s: %v inputs\n", action, out["count"])
	return nil
}

var projectWIPLimitCmd = &cobra.Command{
	Use:   "wip-limit <project> <n|none>",
	Short: "Set the project's WIP limit, or remove it with `none` (default: unlimited)",
	Long: "A run claimed while the project is paused or holds this many dispatched, running " +
		"or reserved runs is held until a slot frees. Held runs start in focus order: " +
		"project priority, issue priority, issue creation time.",
	Args: exactArgs(2),
	RunE: runProjectWIPLimit,
}

func init() {
	projectCmd.AddCommand(projectWIPLimitCmd)
}

func runProjectWIPLimit(cmd *cobra.Command, args []string) error {
	var limit any
	if args[1] != "none" {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return fmt.Errorf("limit must be a positive integer or `none`")
		}
		limit = n
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveProjectID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	var out map[string]any
	if err := client.PutJSON(ctx, "/api/projects/"+ref.ID+"/wip-limit", map[string]any{"limit": limit}, &out); err != nil {
		return fmt.Errorf("set WIP limit: %w", err)
	}
	return cli.PrintJSON(os.Stdout, out)
}
