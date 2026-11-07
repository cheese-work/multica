package main

import (
	"context"
	"fmt"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// Member-readable System 1 shadow report: per kind and class decided/predicted/agreed counts.
var decisionShadowReportCmd = &cobra.Command{
	Use: "shadow-report", Short: "Read the System 1 shadow predictor report", Args: cobra.NoArgs, RunE: runDecisionShadowReport,
}

func init() {
	decisionShadowReportCmd.Flags().String("output", "json", "Output format: table or json")
	decisionCmd.AddCommand(decisionShadowReportCmd)
}

func runDecisionShadowReport(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := client.GetJSON(ctx, "/api/decisions/shadow-report", &result); err != nil {
		return fmt.Errorf("read shadow report: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
