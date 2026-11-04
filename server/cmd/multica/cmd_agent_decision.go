package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func init() {
	command := &cobra.Command{Use: "rely", Short: "Register task reliance on an effective agent decision; lookup is display only", Args: cobra.NoArgs, RunE: runAgentDecisionRely}
	for _, flag := range []string{"issue", "gate", "candidate-sha256", "purpose"} {
		command.Flags().String(flag, "", map[string]string{"issue": "Issue UUID or identifier", "gate": "Allowlisted gate name", "candidate-sha256": "Frozen candidate SHA256", "purpose": "Work relying on this gate decision"}[flag])
		_ = command.MarkFlagRequired(flag)
	}
	decisionCmd.AddCommand(command)
}

func runAgentDecisionRely(command *cobra.Command, _ []string) error {
	values := map[string]string{}
	for _, flag := range []string{"issue", "gate", "candidate-sha256", "purpose"} {
		value, _ := command.Flags().GetString(flag)
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("--%s is required", flag)
		}
		values[flag] = value
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, values["issue"])
	if err != nil {
		return err
	}
	var result struct {
		Decided    bool   `json:"decided"`
		State      string `json:"state"`
		InstanceID string `json:"instance_id,omitempty"`
		RelianceID string `json:"reliance_id,omitempty"`
	}
	if err := client.PostJSON(ctx, "/api/issues/"+url.PathEscape(issue.ID)+"/agent-decisions/rely", map[string]string{"gate": values["gate"], "candidate_sha256": values["candidate-sha256"], "purpose": values["purpose"]}, &result); err != nil {
		return fmt.Errorf("not decided: %w", err)
	}
	// Only a committed registration with both server IDs can return decided.
	if result.Decided && (result.State != "decided" || result.InstanceID == "" || result.RelianceID == "") {
		return fmt.Errorf("not decided: incomplete reliance receipt")
	}
	return cli.PrintJSON(os.Stdout, result)
}
