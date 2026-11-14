package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// `multica audit list` (CHE-5652) reads GET /api/workspaces/{id}/audit-events.
// Owner/admin only; the server enforces it.

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Read the workspace audit trail",
}

var auditListCmd = &cobra.Command{
	Use:   "list",
	Short: "List audit events, newest first (owner/admin only)",
	Args:  cobra.NoArgs,
	RunE:  runAuditList,
}

func init() {
	rootCmd.AddCommand(auditCmd)
	auditCmd.AddCommand(auditListCmd)

	f := auditListCmd.Flags()
	f.String("kind", "", "Kind prefix, e.g. agent. matches agent.update")
	f.String("target", "", "Target agent, by id or name")
	f.String("actor", "", "Actor id")
	f.String("since", "", "Only events after this: a duration (2h, 1d) or an RFC 3339 time")
	f.String("until", "", "Only events before this: a duration (2h, 1d) or an RFC 3339 time")
	f.Int("limit", 50, "Page size (server max 200)")
	f.Bool("all", false, "Follow next_cursor until the end")
	f.String("output", "table", "Output format: table or json")
}

func runAuditList(cmd *cobra.Command, _ []string) error {
	wsID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var page map[string]any
	if err := client.GetJSON(ctx, "/api/workspaces/"+wsID+"/audit-events", &page); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, page)
}
