package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var system1Cmd = &cobra.Command{Use: "system1", Short: "Read System 1 burn-in and qualification"}

// Member-readable System 1 qualification: epoch, counted cases, agreement and status per scope and predictor version.
var system1ReportCmd = &cobra.Command{
	Use: "report", Short: "Read the System 1 qualification report", Args: cobra.NoArgs, RunE: runSystem1Report,
}

func init() {
	system1ReportCmd.Flags().String("output", "json", "Output format (JSON only)")
	system1ReportCmd.Flags().String("scope", "", "Only this scope, such as test_first_waiver or ci:<repo>:P1")
	system1Cmd.AddCommand(system1ReportCmd)
	rootCmd.AddCommand(system1Cmd)
}

func runSystem1Report(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	path := "/api/system1/report"
	if scope, _ := command.Flags().GetString("scope"); scope != "" {
		path += "?" + url.Values{"scope": {scope}}.Encode()
	}
	var result map[string]any
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("read system1 report: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
