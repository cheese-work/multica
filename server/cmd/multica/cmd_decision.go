package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var decisionCmd = &cobra.Command{Use: "decision", Short: "Request and read issue decisions"}

var decisionRequestCmd = &cobra.Command{
	Use: "request", Short: "Ask a named person in this issue task's decision window", Args: cobra.NoArgs, RunE: runDecisionRequest,
}

var decisionGetCmd = &cobra.Command{
	Use: "get <id>", Short: "Read a decision request", Args: cobra.ExactArgs(1), RunE: runDecisionGet,
}

var decisionListCmd = &cobra.Command{
	Use: "list", Short: "List decision requests on an issue", Args: cobra.NoArgs, RunE: runDecisionList,
}

func init() {
	for _, command := range []*cobra.Command{decisionRequestCmd, decisionListCmd} {
		command.Flags().String("issue", "", "Issue UUID or identifier")
		_ = command.MarkFlagRequired("issue")
	}
	decisionRequestCmd.Flags().String("session", "", "DM lane UUID from chat sessions --issue")
	decisionRequestCmd.Flags().Bool("new-dm", false, "Explicitly create a Decisions DM instead of selecting a listed lane")
	decisionRequestCmd.Flags().String("for", "", "Named person's user ID (required with --new-dm)")
	decisionRequestCmd.Flags().String("class", "", "reversible, spend, deploy, publish, irreversible, or product_direction")
	decisionRequestCmd.Flags().String("ask-file", "", "UTF-8 file containing the exact ask")
	for _, flag := range []string{"class", "ask-file"} {
		_ = decisionRequestCmd.MarkFlagRequired(flag)
	}
	for _, command := range []*cobra.Command{decisionRequestCmd, decisionGetCmd, decisionListCmd} {
		command.Flags().String("output", "json", "Output format: table or json")
		decisionCmd.AddCommand(command)
	}
	rootCmd.AddCommand(decisionCmd)
}

func runDecisionRequest(command *cobra.Command, _ []string) error {
	issueRef, _ := command.Flags().GetString("issue")
	sessionID, _ := command.Flags().GetString("session")
	newDM, _ := command.Flags().GetBool("new-dm")
	humanID, _ := command.Flags().GetString("for")
	class, _ := command.Flags().GetString("class")
	askPath, _ := command.Flags().GetString("ask-file")
	if issueRef == "" || class == "" || askPath == "" {
		return fmt.Errorf("--issue, --class, and --ask-file are required")
	}
	if newDM == (sessionID != "") || newDM && humanID == "" || !newDM && humanID != "" {
		return fmt.Errorf("choose --session or --new-dm with --for")
	}
	ask, err := os.ReadFile(askPath)
	if err != nil {
		return fmt.Errorf("read ask file: %w", err)
	}
	if strings.TrimSpace(string(ask)) == "" {
		return fmt.Errorf("ask file must not be empty")
	}
	if !utf8.Valid(ask) {
		return fmt.Errorf("ask file must contain valid UTF-8")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, issueRef)
	if err != nil {
		return err
	}
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/decisions", map[string]any{
		"issue_id": issue.ID, "session_id": sessionID, "new_dm": newDM, "for": humanID, "ask": string(ask), "escalation_class": class,
	}, &result); err != nil {
		return fmt.Errorf("request decision: %w", err)
	}
	return printDecisions(command, []map[string]any{result}, false)
}

func runDecisionGet(command *cobra.Command, args []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := client.GetJSON(ctx, "/api/decisions/"+url.PathEscape(args[0]), &result); err != nil {
		return fmt.Errorf("get decision: %w", err)
	}
	return printDecisions(command, []map[string]any{result}, false)
}

func runDecisionList(command *cobra.Command, _ []string) error {
	issueRef, _ := command.Flags().GetString("issue")
	if issueRef == "" {
		return fmt.Errorf("--issue is required")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, issueRef)
	if err != nil {
		return err
	}
	var requests []map[string]any
	if err := client.GetJSON(ctx, "/api/decisions?"+url.Values{"issue": {issue.ID}}.Encode(), &requests); err != nil {
		return fmt.Errorf("list decisions: %w", err)
	}
	return printDecisions(command, requests, true)
}

func printDecisions(command *cobra.Command, requests []map[string]any, list bool) error {
	output, _ := command.Flags().GetString("output")
	if output != "table" {
		if list {
			return cli.PrintJSON(os.Stdout, requests)
		}
		return cli.PrintJSON(os.Stdout, requests[0])
	}
	rows := make([][]string, 0, len(requests))
	for _, request := range requests {
		rows = append(rows, []string{strVal(request, "id"), strVal(request, "status"), strVal(request, "escalation_class"), strVal(request, "ask")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "STATUS", "CLASS", "ASK"}, rows)
	return nil
}
