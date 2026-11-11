package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var runtimeBreakerCmd = &cobra.Command{
	Use:   "breaker",
	Short: "Inspect and reset provider circuit breakers",
}

var runtimeBreakerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List provider circuit breakers in the workspace",
	Args:  exactArgs(0),
	RunE:  runRuntimeBreakerList,
}

var runtimeBreakerResetCmd = &cobra.Command{
	Use:   "reset <provider>",
	Short: "Close a provider circuit breaker (owner or admin)",
	Long:  "Close a provider circuit breaker now instead of waiting for its probe. The reset is audited with who ran it and the previous state.",
	Args:  exactArgs(1),
	RunE:  runRuntimeBreakerReset,
}

func init() {
	runtimeCmd.AddCommand(runtimeBreakerCmd)
	runtimeBreakerCmd.AddCommand(runtimeBreakerListCmd)
	runtimeBreakerCmd.AddCommand(runtimeBreakerResetCmd)
	runtimeBreakerListCmd.Flags().String("output", "table", "Output format: table or json")
	runtimeBreakerResetCmd.Flags().String("output", "table", "Output format: table or json")
}

func runRuntimeBreakerList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result struct {
		Breakers []map[string]any `json:"breakers"`
	}
	if err := client.GetJSON(ctx, fmt.Sprintf("/api/workspaces/%s/provider-breakers", workspaceID), &result); err != nil {
		return fmt.Errorf("list provider breakers: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Breakers)
	}
	rows := make([][]string, 0, len(result.Breakers))
	for _, b := range result.Breakers {
		rows = append(rows, []string{
			strVal(b, "provider"), strVal(b, "state"), strVal(b, "failure_count"),
			strVal(b, "opened_at"), strVal(b, "next_probe_at"), strVal(b, "last_success_at"),
		})
	}
	cli.PrintTable(os.Stdout, []string{"PROVIDER", "STATE", "FAILURES", "OPENED_AT", "NEXT_PROBE_AT", "LAST_SUCCESS_AT"}, rows)
	return nil
}

func runRuntimeBreakerReset(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	provider := strings.ToLower(strings.TrimSpace(args[0]))
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result map[string]any
	path := fmt.Sprintf("/api/workspaces/%s/provider-breakers/%s/reset", workspaceID, url.PathEscape(provider))
	if err := client.PostJSON(ctx, path, nil, &result); err != nil {
		return fmt.Errorf("reset provider breaker: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	fmt.Printf("Provider breaker %s reset: %s -> %s\n", strVal(result, "provider"), strVal(result, "previous_state"), strVal(result, "state"))
	return nil
}
