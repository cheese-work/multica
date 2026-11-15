package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Scheduling pause (CHE-2954): stops claims for one agent or a whole workspace.
// Running tasks finish and queued work is kept. Person owner/admin only; agent
// tokens get 403 from the server.
const schedulingPauseNote = "\n\nRunning tasks finish; queued work is kept. Owner/admin only (agent tokens are refused)."

var agentPauseCmd = &cobra.Command{
	Use:   "pause <id>",
	Short: "Pause scheduling for an agent",
	Long: "Stops new runs from being claimed for this agent. Enqueueing continues." +
		schedulingPauseNote + " Undo with 'multica agent resume'.",
	Args: exactArgs(1),
	RunE: runAgentPause,
}

var agentResumeCmd = &cobra.Command{
	Use:   "resume <id>",
	Short: "Resume scheduling for a paused agent",
	Long:  "Lets queued work for this agent be claimed again on the next poll. A no-op if it is not paused." + schedulingPauseNote,
	Args:  exactArgs(1),
	RunE:  runAgentResume,
}

var workspacePauseCmd = &cobra.Command{
	Use:   "pause [workspace-id|slug|prefix]",
	Short: "Pause scheduling for a workspace",
	Long: "Stops new runs from being claimed for every agent in the workspace. Enqueueing continues." +
		schedulingPauseNote + " If the argument is omitted, the current default workspace is used. Undo with 'multica workspace resume'.",
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkspacePause,
}

var workspaceResumeCmd = &cobra.Command{
	Use:   "resume [workspace-id|slug|prefix]",
	Short: "Resume scheduling for a paused workspace",
	Long: "Lets queued work in the workspace be claimed again on the next poll. A no-op if it is not paused." +
		schedulingPauseNote + " If the argument is omitted, the current default workspace is used.",
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkspaceResume,
}

func init() {
	agentCmd.AddCommand(agentPauseCmd, agentResumeCmd)
	workspaceCmd.AddCommand(workspacePauseCmd, workspaceResumeCmd)
	for _, c := range []*cobra.Command{agentPauseCmd, agentResumeCmd, workspacePauseCmd, workspaceResumeCmd} {
		c.Flags().String("output", "json", "Output format: table or json")
	}
}

func runAgentPause(cmd *cobra.Command, args []string) error {
	return runSchedulingChange(cmd, "Agent", "/api/agents/"+args[0], "pause")
}

func runAgentResume(cmd *cobra.Command, args []string) error {
	return runSchedulingChange(cmd, "Agent", "/api/agents/"+args[0], "resume")
}

func runWorkspacePause(cmd *cobra.Command, args []string) error {
	return runWorkspaceSchedulingChange(cmd, args, "pause")
}

func runWorkspaceResume(cmd *cobra.Command, args []string) error {
	return runWorkspaceSchedulingChange(cmd, args, "resume")
}

func runWorkspaceSchedulingChange(cmd *cobra.Command, args []string, action string) error {
	wsID, err := resolveWorkspaceArg(cmd, args)
	if err != nil {
		return err
	}
	if wsID == "" {
		return fmt.Errorf("workspace ID is required: pass an id/slug/prefix as argument or set MULTICA_WORKSPACE_ID")
	}
	return runSchedulingChange(cmd, "Workspace", "/api/workspaces/"+wsID, action)
}

// runSchedulingChange POSTs <base>/<action> ("pause" or "resume") and prints the
// updated resource.
func runSchedulingChange(cmd *cobra.Command, kind, base, action string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result map[string]any
	if err := client.PostJSON(ctx, base+"/"+action, nil, &result); err != nil {
		return fmt.Errorf("%s %s: %w", action, kind, err)
	}

	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	state := "paused"
	if action == "resume" {
		state = "resumed"
	}
	fmt.Printf("%s scheduling %s: %s (%s)\n", kind, state, strVal(result, "name"), strVal(result, "id"))
	return nil
}
