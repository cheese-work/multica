package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica decision propose names one allowlisted action and its params. The
// server writes the ask and shows a card to the person; nothing runs until
// that person approves it with a passkey (CHE-2949 D6).
var decisionProposeCmd = &cobra.Command{
	Use:   "propose <action-type>",
	Short: "Propose an allowlisted action for a person to approve on a card",
	Long: `Proposes one action, for example issue.run_budget.raise, and prints the card link.
Only a person's passkey approval runs it; proposing changes nothing.

Params: --param key=value sends a string; --param key:=<json> sends JSON
(numbers, booleans, objects), e.g. --param new_cap:=71. Or pass one JSON
object with --params-file. Run only inside the issue's running task.`,
	Args: exactArgs(1),
	RunE: runDecisionPropose,
}

func init() {
	decisionProposeCmd.Flags().Int("version", 0, "Action version (required)")
	decisionProposeCmd.Flags().String("issue", "", "Issue UUID or identifier the proposal is about (required)")
	decisionProposeCmd.Flags().String("for", "", "Person's user ID to ask (an issue task needs it; a chat task asks the chat's creator)")
	decisionProposeCmd.Flags().StringArray("param", nil, "Param key=value (string) or key:=<json>; repeatable")
	decisionProposeCmd.Flags().String("params-file", "", "UTF-8 file holding the params as one JSON object (instead of --param)")
	decisionProposeCmd.Flags().String("reason", "", "Why you propose it; shown to the person as an unverified quote (1 KiB at most)")
	decisionProposeCmd.Flags().String("output", "table", "Output format: table or json")
	_ = decisionProposeCmd.MarkFlagRequired("version")
	_ = decisionProposeCmd.MarkFlagRequired("issue")
	decisionCmd.AddCommand(decisionProposeCmd)
}

func runDecisionPropose(cmd *cobra.Command, args []string) error {
	version, _ := cmd.Flags().GetInt("version")
	issueArg, _ := cmd.Flags().GetString("issue")
	forUser, _ := cmd.Flags().GetString("for")
	reason, _ := cmd.Flags().GetString("reason")
	if version < 1 {
		return fmt.Errorf("--version must be 1 or more")
	}
	params, err := proposeParams(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, issueArg)
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	raw, err := postActionProposal(ctx, client, args[0], version, issueRef.ID, forUser, reason, params)
	if err != nil {
		return err
	}
	return printActionProposal(ctx, cmd, client, args[0], issueRef, raw)
}

// proposeParams builds the params object from --param flags or --params-file.
func proposeParams(cmd *cobra.Command) (map[string]json.RawMessage, error) {
	pairs, _ := cmd.Flags().GetStringArray("param")
	file, _ := cmd.Flags().GetString("params-file")
	switch {
	case file != "" && len(pairs) > 0:
		return nil, fmt.Errorf("use --param or --params-file, not both")
	case file == "" && len(pairs) == 0:
		return nil, fmt.Errorf("give the action's params with --param key=value or --params-file")
	case file != "":
		text, err := readDecisionText(file, "params")
		if err != nil {
			return nil, err
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &params); err != nil || params == nil {
			return nil, fmt.Errorf("params file must contain one JSON object")
		}
		return params, nil
	}
	params := make(map[string]json.RawMessage, len(pairs))
	for _, pair := range pairs {
		key, value, isJSON, ok := splitParam(pair)
		if !ok {
			return nil, fmt.Errorf("--param %q: use key=value or key:=<json>", pair)
		}
		if _, dup := params[key]; dup {
			return nil, fmt.Errorf("--param %q: key given twice", key)
		}
		if isJSON {
			if !json.Valid([]byte(value)) {
				return nil, fmt.Errorf("--param %s:=...: not valid JSON", key)
			}
			params[key] = json.RawMessage(value)
			continue
		}
		params[key], _ = json.Marshal(value)
	}
	return params, nil
}

// splitParam splits "k=v" or "k:=json" at the first "=".
func splitParam(pair string) (key, value string, isJSON, ok bool) {
	key, value, found := strings.Cut(pair, "=")
	if !found || key == "" {
		return "", "", false, false
	}
	if strings.HasSuffix(key, ":") {
		key, isJSON = strings.TrimSuffix(key, ":"), true
	}
	return key, value, isJSON, key != ""
}

// postActionProposal sends one proposal. A refusal keeps the server's code
// (for example proposals_unavailable) in the error, whatever the status.
func postActionProposal(ctx context.Context, client *cli.APIClient, actionType string, version int, issueID, forUser, reason string, params map[string]json.RawMessage) (json.RawMessage, error) {
	body := map[string]any{"action_type": actionType, "version": version, "issue_id": issueID, "params": params}
	if forUser != "" {
		body["for"] = forUser
	}
	if reason != "" {
		body["reason"] = reason
	}
	var raw json.RawMessage
	if err := client.PostJSON(ctx, "/api/decision-actions/proposals", body, &raw); err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) {
			return nil, fmt.Errorf("propose %s: server refused (%d): %s", actionType, httpErr.StatusCode, serverErrorText(httpErr.Body))
		}
		return nil, fmt.Errorf("propose %s: %w", actionType, err)
	}
	return raw, nil
}

// serverErrorText is the {"error": "..."} text of a body, or the body itself.
func serverErrorText(body string) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	return strings.TrimSpace(body)
}

func printActionProposal(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, actionType string, issue resolvedID, raw json.RawMessage) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, raw)
	}
	var resp struct {
		ID            string `json:"id"`
		ChatSessionID string `json:"chat_session_id"`
		Action        struct {
			ID string `json:"id"`
		} `json:"action"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Action.ID == "" {
		return fmt.Errorf("read proposal: the server answered without an action")
	}
	// ponytail: a failed slug read drops the slug prefix; the action id still prints.
	var ws struct {
		Slug string `json:"slug"`
	}
	prefix := ""
	if client.WorkspaceID != "" && client.GetJSON(ctx, "/api/workspaces/"+client.WorkspaceID, &ws) == nil && ws.Slug != "" {
		prefix = "/" + ws.Slug
	}
	fmt.Fprintf(os.Stdout, "proposed %s on %s (request %s); the person approves with a passkey at:\n%s/issues/%s?approve=%s\n",
		actionType, issue.Display, resp.ID, prefix, issue.ID, resp.Action.ID)
	if resp.ChatSessionID != "" {
		fmt.Fprintf(os.Stdout, "the card also sits in the DM at:\n%s/chat?session=%s\n", prefix, resp.ChatSessionID)
	}
	return nil
}
