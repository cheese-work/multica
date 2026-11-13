package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// Member-readable System 1 shadow report: per kind and class decided/predicted/agreed counts.
var decisionShadowReportCmd = &cobra.Command{
	Use: "shadow-report", Short: "Read the System 1 shadow predictor report", Args: cobra.NoArgs, RunE: runDecisionShadowReport,
}

func init() {
	decisionShadowReportCmd.Flags().String("output", "json", "Output format (JSON only)")
	decisionShadowReportCmd.Flags().String("since", "", "Route window start, RFC 3339 (default: 24 hours before --until)")
	decisionShadowReportCmd.Flags().String("until", "", "Route window end, RFC 3339 (default: now)")
	decisionCmd.AddCommand(decisionShadowReportCmd)
}

func runDecisionShadowReport(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	window := url.Values{}
	for _, name := range []string{"since", "until"} {
		if value, _ := command.Flags().GetString(name); value != "" {
			window.Set(name, value)
		}
	}
	path := "/api/decisions/shadow-report"
	if len(window) > 0 {
		path += "?" + window.Encode()
	}
	var result map[string]any
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("read shadow report: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
