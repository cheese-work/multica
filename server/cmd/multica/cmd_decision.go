package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/spf13/cobra"
)

var decisionCmd = &cobra.Command{Use: "decision", Short: "Request and read issue decisions"}

var decisionRequestCmd = &cobra.Command{
	Use: "request", Short: "Ask a named person with one issue comment and one DM message", Args: cobra.NoArgs, RunE: runDecisionRequest,
}

var decisionGetCmd = &cobra.Command{
	Use: "get <id>", Short: "Read a decision request", Args: cobra.ExactArgs(1), RunE: runDecisionGet,
}

var decisionListCmd = &cobra.Command{
	Use: "list", Short: "List decision requests on an issue, or a member's open asks", Args: cobra.NoArgs, RunE: runDecisionList,
}

var decisionWithdrawCmd = &cobra.Command{
	Use: "withdraw <id>", Short: "Withdraw an unanswered decision request", Args: cobra.ExactArgs(1), RunE: runDecisionWithdraw,
}

func init() {
	decisionRequestCmd.Flags().String("issue", "", "Issue UUID or identifier")
	_ = decisionRequestCmd.MarkFlagRequired("issue")
	decisionListCmd.Flags().String("issue", "", "Issue UUID or identifier (or use --for with --state open)")
	decisionListCmd.Flags().String("for", "", "Member's user ID: list that member's open asks workspace-wide (needs --state open)")
	decisionListCmd.Flags().String("state", "", "With --for: open")
	decisionListCmd.Flags().Bool("section", false, "With --for: print the evening brief section (Markdown) instead of JSON")
	decisionListCmd.Flags().Int("older-than-hours", 24, "With --section: only asks open at least this long (24 is PROPOSED, not agreed)")
	decisionListCmd.Flags().Int("max-lines", 5, "With --section: line budget for the section, header and omitted count included")
	decisionListCmd.Flags().Int("brief-lines-used", 0, "With --section: brief lines used outside this section, including lines still to come; the section stays within the 30-line brief maximum")
	decisionListCmd.Flags().StringSlice("exclude-issue", nil, "With --section: skip asks on these issue IDs (already listed in the 08:00 digest)")
	decisionRequestCmd.Flags().String("session", "", "DM lane UUID from chat sessions --issue; must be eligible (it discussed this issue)")
	decisionRequestCmd.Flags().Bool("new-dm", false, "Create a DM for this issue only, when no listed lane discussed it")
	decisionRequestCmd.Flags().String("for", "", "Named person's user or workspace member ID (required unless an open intent names the person)")
	decisionRequestCmd.Flags().String("parent", "", "Comment UUID to reply under (default: top-level; not allowed with an open intent)")
	decisionRequestCmd.Flags().String("class", "", "reversible, spend, deploy, publish, irreversible, or product_direction")
	decisionRequestCmd.Flags().String("ask-file", "", "UTF-8 JSON ask: context_eli10, agent_did, decision, stakes_if_wrong, options[{label, what_happens, pros[], cons[], cost_or_time}], recommendation, default_if_no_reply, source_issue_id, source_comment_id, and for an ask that approves a file: subject \"artifact_approval\" with artifacts[{attachment_id, label, sha256, supersedes_attachment_id, signed_by{reviewer, comment_id}}]")
	decisionRequestCmd.Flags().String("action-file", "", "Optional UTF-8 JSON privileged action Cheese approves on the card: {type, version, params, secret_slots[{slot, target}], expires_in_seconds}. Never put a secret value in it")
	decisionRequestCmd.Flags().String("footer-file", "", "Optional UTF-8 file appended to the issue comment only, e.g. RHC-2 lines")
	for _, flag := range []string{"class", "ask-file"} {
		_ = decisionRequestCmd.MarkFlagRequired(flag)
	}
	decisionWithdrawCmd.Flags().String("reason", "", "Why the ask no longer needs an answer")
	_ = decisionWithdrawCmd.MarkFlagRequired("reason")
	for _, command := range []*cobra.Command{decisionRequestCmd, decisionGetCmd, decisionListCmd, decisionWithdrawCmd} {
		command.Flags().String("output", "json", "Output format: table or json")
		decisionCmd.AddCommand(command)
	}
	rootCmd.AddCommand(decisionCmd)
}

func runDecisionWithdraw(command *cobra.Command, args []string) error {
	reason, _ := command.Flags().GetString("reason")
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason must not be empty")
	}
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/decisions/"+url.PathEscape(args[0])+"/withdraw", map[string]any{"reason": reason}, &result); err != nil {
		return fmt.Errorf("withdraw decision: %w", err)
	}
	return printDecisions(command, []map[string]any{result}, false)
}

func runDecisionRequest(command *cobra.Command, _ []string) error {
	issueRef, _ := command.Flags().GetString("issue")
	sessionID, _ := command.Flags().GetString("session")
	newDM, _ := command.Flags().GetBool("new-dm")
	humanID, _ := command.Flags().GetString("for")
	class, _ := command.Flags().GetString("class")
	askPath, _ := command.Flags().GetString("ask-file")
	footerPath, _ := command.Flags().GetString("footer-file")
	actionPath, _ := command.Flags().GetString("action-file")
	parentID, _ := command.Flags().GetString("parent")
	if issueRef == "" || class == "" || askPath == "" {
		return fmt.Errorf("--issue, --class, and --ask-file are required")
	}
	if newDM && (sessionID != "" || humanID == "") || !newDM && sessionID == "" && humanID == "" {
		return fmt.Errorf("choose --session, or --for (add --new-dm to force a new DM)")
	}
	askText, err := readDecisionText(askPath, "ask")
	if err != nil {
		return err
	}
	ask, err := decisiontx.DecodeAsk([]byte(askText))
	if err != nil {
		return err
	}
	footer := ""
	if footerPath != "" {
		if footer, err = readDecisionText(footerPath, "footer"); err != nil {
			return err
		}
	}
	var action json.RawMessage
	if actionPath != "" {
		if action, err = readDecisionAction(actionPath); err != nil {
			return err
		}
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
	if !strings.EqualFold(ask.SourceIssueID, issue.ID) {
		return fmt.Errorf("ask.source_issue_id must match --issue")
	}
	body := map[string]any{
		"issue_id": issue.ID, "session_id": sessionID, "new_dm": newDM, "for": humanID, "ask": ask, "footer": footer, "parent_id": parentID, "escalation_class": class,
	}
	if action != nil {
		body["action"] = action
	}
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/decisions", body, &result); err != nil {
		return fmt.Errorf("request decision: %w", err)
	}
	printAskHints(command, decisiontx.LintAsk(ask), result)
	return printDecisions(command, []map[string]any{result}, false)
}

// printAskHints writes each distinct advisory hint once on stderr and drops the
// server copy so JSON stdout keeps the stored request shape.
func printAskHints(command *cobra.Command, local []string, result map[string]any) {
	seen := make(map[string]bool)
	hints := local
	if server, ok := result["hints"].([]any); ok {
		for _, hint := range server {
			if text, ok := hint.(string); ok {
				hints = append(hints, text)
			}
		}
	}
	delete(result, "hints")
	for _, hint := range hints {
		if !seen[hint] {
			seen[hint] = true
			fmt.Fprintln(command.ErrOrStderr(), "hint: "+hint)
		}
	}
}

// readDecisionAction loads --action-file. It checks only the envelope the
// server's wire type accepts, so a typo such as "secrets" fails here instead
// of being dropped; the allowlist and params are the server's to validate.
func readDecisionAction(path string) (json.RawMessage, error) {
	text, err := readDecisionText(path, "action")
	if err != nil {
		return nil, err
	}
	var action struct {
		Type             string          `json:"type"`
		Version          int             `json:"version"`
		Params           json.RawMessage `json:"params,omitempty"`
		SecretSlots      json.RawMessage `json:"secret_slots,omitempty"`
		ExpiresInSeconds int64           `json:"expires_in_seconds,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&action); err != nil || decoder.More() {
		return nil, fmt.Errorf("action file must contain exactly one JSON object with only type, version, params, secret_slots and expires_in_seconds")
	}
	if action.Type == "" || action.Version < 1 {
		return nil, fmt.Errorf("action file needs a type and a version of 1 or more")
	}
	return json.Marshal(action)
}

func readDecisionText(path, label string) (string, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", label, err)
	}
	if !utf8.Valid(text) {
		return "", fmt.Errorf("%s file must contain valid UTF-8", label)
	}
	if (label == "ask" || label == "action") && strings.TrimSpace(string(text)) == "" {
		return "", fmt.Errorf("%s file must not be empty", label)
	}
	return string(text), nil
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
	if memberID, _ := command.Flags().GetString("for"); memberID != "" {
		if issueRef != "" {
			return fmt.Errorf("use --issue or --for, not both")
		}
		return runDecisionListOpenAsks(command, memberID)
	}
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
