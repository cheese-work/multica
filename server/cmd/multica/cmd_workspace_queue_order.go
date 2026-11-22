package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/queueorder"
)

var workspaceQueueOrderCmd = &cobra.Command{
	Use:   "queue-order",
	Short: "Show or set how queued runs are ordered",
	Long: "Shows or sets the workspace settings queue_order (priority, the default, or " +
		"project_rank) and queue_aging_minutes (5 to 720, default 30). With priority " +
		"the order is today's: run priority, then age. With project_rank a run is " +
		"ranked by project priority, then parent issue priority, then its own, and " +
		"a waiting run gains one step each aging period. Today the setting is stored " +
		"and audited only; the claim path reads it in a later release. Setting needs a workspace " +
		"owner or admin; every change is audited.",
}

var workspaceQueueOrderShowCmd = &cobra.Command{
	Use:   "show [workspace-id|slug|prefix]",
	Short: "Show queue_order and queue_aging_minutes (effective values)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorkspaceQueueOrderShow,
}

var workspaceQueueOrderSetCmd = &cobra.Command{
	Use:   "set [workspace-id|slug|prefix]",
	Short: "Set queue_order and/or queue_aging_minutes (owner/admin only)",
	Long: "Sets one or both keys. A value outside the allowed set is refused with the " +
		"allowed values named. Set --order priority to roll back to today's order.",
	Example: "  multica workspace queue-order set --order project_rank --aging-minutes 30\n" +
		"  multica workspace queue-order set --order priority",
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkspaceQueueOrderSet,
}

func init() {
	workspaceCmd.AddCommand(workspaceQueueOrderCmd)
	workspaceQueueOrderCmd.AddCommand(workspaceQueueOrderShowCmd)
	workspaceQueueOrderCmd.AddCommand(workspaceQueueOrderSetCmd)
	workspaceQueueOrderSetCmd.Flags().String("order", "", "queue_order: "+strings.Join(queueorder.Modes, " or "))
	workspaceQueueOrderSetCmd.Flags().Int("aging-minutes", 0, fmt.Sprintf("queue_aging_minutes: %d to %d", queueorder.MinAgingMinutes, queueorder.MaxAgingMinutes))
}

// printQueueOrder prints the effective values of a workspace's settings.
func printQueueOrder(ws struct {
	Settings map[string]any `json:"settings"`
}) error {
	return cli.PrintJSON(os.Stdout, map[string]any{
		queueorder.KeyOrder: queueorder.Order(ws.Settings),
		queueorder.KeyAging: queueorder.AgingMinutes(ws.Settings),
	})
}

func queueOrderWorkspace(cmd *cobra.Command, args []string) (string, error) {
	wsID, err := resolveWorkspaceArg(cmd, args)
	if err != nil {
		return "", err
	}
	if wsID == "" {
		return "", fmt.Errorf("workspace ID is required: pass an id/slug/prefix as argument or set MULTICA_WORKSPACE_ID")
	}
	return wsID, nil
}

func runWorkspaceQueueOrderShow(cmd *cobra.Command, args []string) error {
	wsID, err := queueOrderWorkspace(cmd, args)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var ws struct {
		Settings map[string]any `json:"settings"`
	}
	if err := client.GetJSON(ctx, "/api/workspaces/"+wsID, &ws); err != nil {
		return fmt.Errorf("get workspace: %w", err)
	}
	return printQueueOrder(ws)
}

func runWorkspaceQueueOrderSet(cmd *cobra.Command, args []string) error {
	body := map[string]any{}
	if cmd.Flags().Changed("order") {
		order, _ := cmd.Flags().GetString("order")
		body[queueorder.KeyOrder] = order
	}
	if cmd.Flags().Changed("aging-minutes") {
		minutes, _ := cmd.Flags().GetInt("aging-minutes")
		body[queueorder.KeyAging] = minutes
	}
	if len(body) == 0 {
		return fmt.Errorf("nothing to set; pass --order and/or --aging-minutes")
	}
	// The server runs the same check; failing here names the allowed values
	// without a request.
	if err := queueorder.ValidateSetting(body); err != nil {
		return err
	}
	wsID, err := queueOrderWorkspace(cmd, args)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var ws struct {
		Settings map[string]any `json:"settings"`
	}
	if err := client.PatchJSON(ctx, "/api/workspaces/"+wsID, body, &ws); err != nil {
		return fmt.Errorf("set queue order: %w", err)
	}
	return printQueueOrder(ws)
}
