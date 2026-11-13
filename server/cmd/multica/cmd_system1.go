package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// System 1 burn-in labeller commands (CHE-4383 B4b). Both run inside the
// assigned labeller's agent task: the server reads the task identity from the
// token, so there is no agent or task flag.
var system1Cmd = &cobra.Command{Use: "system1", Short: "System 1 burn-in labelling"}

var system1LabelQueueCmd = &cobra.Command{
	Use: "label-queue", Short: "Read the blind label queue (observation ids and typed features, never a prediction)", Args: cobra.NoArgs, RunE: runSystem1LabelQueue,
}

var system1LabelCmd = &cobra.Command{Use: "label", Short: "Write System 1 labels"}

var system1LabelSubmitCmd = &cobra.Command{
	Use: "submit", Short: "Write one label for one observation", Args: cobra.NoArgs, RunE: runSystem1LabelSubmit,
}

func init() {
	system1LabelQueueCmd.Flags().String("scope", "", "Label scope, for example test_first_waiver (required)")
	system1LabelQueueCmd.Flags().String("kind", "", "burn_in or eval (required)")
	system1LabelQueueCmd.Flags().String("output", "json", "Output format (JSON only)")
	system1LabelSubmitCmd.Flags().String("observation", "", "Observation id from the queue (required)")
	system1LabelSubmitCmd.Flags().String("kind", "", "burn_in or eval (required)")
	system1LabelSubmitCmd.Flags().String("label", "", "Label value of the scope (required)")
	system1LabelSubmitCmd.Flags().String("supersedes", "", "Id of the live label this one corrects")
	system1LabelSubmitCmd.Flags().String("output", "json", "Output format (JSON only)")
	system1LabelCmd.AddCommand(system1LabelSubmitCmd)
	system1Cmd.AddCommand(system1LabelQueueCmd, system1LabelCmd)
	rootCmd.AddCommand(system1Cmd)
}

func runSystem1LabelQueue(command *cobra.Command, _ []string) error {
	scope, _ := command.Flags().GetString("scope")
	kind, _ := command.Flags().GetString("kind")
	if scope == "" || kind == "" {
		return fmt.Errorf("--scope and --kind are required")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	query := url.Values{"scope": {scope}, "kind": {kind}}
	if err := client.GetJSON(ctx, "/api/system1/label-queue?"+query.Encode(), &result); err != nil {
		return fmt.Errorf("read label queue: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runSystem1LabelSubmit(command *cobra.Command, _ []string) error {
	observation, _ := command.Flags().GetString("observation")
	kind, _ := command.Flags().GetString("kind")
	label, _ := command.Flags().GetString("label")
	supersedes, _ := command.Flags().GetString("supersedes")
	if observation == "" || kind == "" || label == "" {
		return fmt.Errorf("--observation, --kind and --label are required")
	}
	body := map[string]any{"observation_id": observation, "kind": kind, "label": label}
	if supersedes != "" {
		body["supersedes"] = supersedes
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/system1/labels", body, &result); err != nil {
		return fmt.Errorf("submit label: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
