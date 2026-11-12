package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/spf13/cobra"
)

func init() {
	command := &cobra.Command{Use: "rely", Short: "Register task reliance on an effective agent decision; lookup is display only", Args: cobra.NoArgs, RunE: runAgentDecisionRely}
	for _, flag := range []string{"issue", "gate", "candidate-sha256", "purpose"} {
		command.Flags().String(flag, "", map[string]string{"issue": "Issue UUID or identifier", "gate": "Allowlisted gate name", "candidate-sha256": "Frozen candidate SHA256", "purpose": "Work relying on this gate decision"}[flag])
		_ = command.MarkFlagRequired(flag)
	}
	decisionCmd.AddCommand(command)

	// The same command lives at `decision ask` and at `agent decision ask`.
	agentDecisionCmd := &cobra.Command{Use: "decision", Short: "Decisions asked from this agent's run"}
	agentDecisionCmd.AddCommand(newDecisionAskCmd())
	agentCmd.AddCommand(agentDecisionCmd)
	decisionCmd.AddCommand(newDecisionAskCmd())
}

func newDecisionAskCmd() *cobra.Command {
	ask := &cobra.Command{Use: "ask", Short: "Ask the issue's requester one decision from this run; the server picks the person and the DM lane", Args: cobra.NoArgs, RunE: runDecisionAsk}
	ask.Flags().String("class", "", "reversible, spend, deploy, publish, irreversible, or product_direction")
	ask.Flags().String("ask-file", "", "UTF-8 JSON ask, same shape as decision request --ask-file")
	ask.Flags().String("topic", "", "Closed ask topic: test_first_waiver; must equal ask.topic in the ask file. The ask file needs option_key on each of exactly two options (waive, refuse) and a waiver{repo, head_sha, red_impossible_category, weaker_evidence_kind, cross_lab_pass_on_head, tests_retrospective, change_kind} block")
	ask.Flags().String("footer-file", "", "Optional UTF-8 file appended to the issue comment only, e.g. RHC-2 lines")
	ask.Flags().String("parent", "", "Comment UUID to reply under (default: top-level)")
	ask.Flags().String("output", "json", "Output format: table or json")
	return ask
}

// runDecisionAsk is decision request without the issue, lane and person: the
// run's task token already names the issue, and the server picks the rest.
func runDecisionAsk(command *cobra.Command, _ []string) error {
	class, _ := command.Flags().GetString("class")
	askPath, _ := command.Flags().GetString("ask-file")
	footerPath, _ := command.Flags().GetString("footer-file")
	parentID, _ := command.Flags().GetString("parent")
	if class == "" || askPath == "" {
		return fmt.Errorf("--class and --ask-file are required")
	}
	askText, err := readDecisionText(askPath, "ask")
	if err != nil {
		return err
	}
	ask, err := decisiontx.DecodeAsk([]byte(askText))
	if err != nil {
		return err
	}
	// --topic states the intent on the command line; the ask file must carry the same topic.
	if topic, _ := command.Flags().GetString("topic"); topic != "" && (topic != decisiontx.TopicTestFirstWaiver || ask.Topic != topic) {
		return fmt.Errorf("--topic must be %s and equal ask.topic in the ask file", decisiontx.TopicTestFirstWaiver)
	}
	footer := ""
	if footerPath != "" {
		if footer, err = readDecisionText(footerPath, "footer"); err != nil {
			return err
		}
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	body := map[string]any{"ask": ask, "footer": footer, "parent_id": parentID, "escalation_class": class}
	if err := client.PostJSON(ctx, "/api/decisions/ask", body, &result); err != nil {
		return fmt.Errorf("ask decision: %w", err)
	}
	printAskHints(command, decisiontx.LintAsk(ask), result)
	return printDecisions(command, []map[string]any{result}, false)
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
