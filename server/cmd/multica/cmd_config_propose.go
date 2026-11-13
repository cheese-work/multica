package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// configProposeCmd builds the action file for a workspace config change. It
// reads the live value, hashes before and after with the recipes the server
// recomputes, and writes the file `multica decision request --action-file`
// takes. It never sends anything and never writes a secret.
var configProposeCmd = &cobra.Command{
	Use:   "propose <field> [target-id]",
	Short: "Write the decision action file for a config change (instructions, context, workflow steps, settings)",
	Long: `Read the live value, compute before_sha256 and after_sha256, and write the
action file for "multica decision request --action-file".

Fields:
  agent.instructions            <agent-id>     --file <text>
  squad.instructions            <squad-id>     --file <text>
  workspace.context                            --file <text>
  workflow.steps                <workflow-id>  --file <steps JSON array, as the workflow GET returns it>
  workflow.step.instructions    <workflow-id>  --status-key <key> --file <text>
  workflow.budget               <workflow-id>  --max-runs N (0 clears) and/or --include-child-prs=true|false
  workspace.settings                           --set key=value | key:=<json> (repeatable)

Any other field, and any settings key off the allow table, is refused before
anything is read. Use --dry-run to print both digests and the action.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runConfigPropose,
}

func init() {
	configProposeCmd.Flags().String("file", "", "UTF-8 file holding the new text (or the steps JSON array)")
	configProposeCmd.Flags().String("out", "", "Where to write the action file")
	configProposeCmd.Flags().Bool("dry-run", false, "Print both digests and the action; write nothing")
	configProposeCmd.Flags().String("status-key", "", "Step status key (workflow.step.instructions)")
	configProposeCmd.Flags().StringArray("set", nil, "Settings key=value (string) or key:=<json>; repeatable (workspace.settings)")
	configProposeCmd.Flags().Int("max-runs", 0, "Run limit per issue, 0 clears it (workflow.budget)")
	configProposeCmd.Flags().Bool("include-child-prs", false, "Count child PRs in the run budget (workflow.budget)")
	configProposeCmd.Flags().Int("expires-in", 0, "Seconds the request stays open (0 = server default)")
	configCmd.AddCommand(configProposeCmd)
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// objectMergeKeys mirrors the server: these settings are merged field by field.
var objectMergeKeys = []string{"auto_compact", "idle_started_guard"}

// proposal is the action file plus the digests shown to the person.
type proposal struct {
	Type, BaseSHA256, AfterSHA256 string
	Params                        map[string]any
}

func runConfigPropose(cmd *cobra.Command, args []string) error {
	field := args[0]
	file, _ := cmd.Flags().GetString("file")
	out, _ := cmd.Flags().GetString("out")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	statusKey, _ := cmd.Flags().GetString("status-key")
	sets, _ := cmd.Flags().GetStringArray("set")
	expires, _ := cmd.Flags().GetInt("expires-in")

	settings, err := parseSettingsSets(sets)
	if err != nil {
		return err
	}
	if err := checkProposeField(field, settings); err != nil {
		return err
	}
	if !dryRun && out == "" {
		return errors.New("--out is required (or use --dry-run)")
	}
	budget := cmd.Flags().Changed("max-runs") || cmd.Flags().Changed("include-child-prs")
	if budget && field != workflowBudgetField {
		return errors.New("--max-runs and --include-child-prs apply to workflow.budget only")
	}
	switch {
	case field == workflowBudgetField:
		if file != "" {
			return errors.New("workflow.budget takes --max-runs and --include-child-prs, not --file")
		}
		if !budget {
			return errors.New("workflow.budget needs --max-runs or --include-child-prs")
		}
	case field == decisionaction.SettingsField:
		if file != "" {
			return errors.New("workspace.settings takes --set, not --file")
		}
	case file == "":
		return errors.New("--file is required")
	}
	if field != "workflow.step.instructions" && statusKey != "" {
		return errors.New("--status-key applies to workflow.step.instructions only")
	}

	target := ""
	if len(args) == 2 {
		target = args[1]
	}
	wantsTarget := field != "workspace.context" && field != decisionaction.SettingsField
	if wantsTarget != (target != "") {
		return fmt.Errorf("%s: target id %s", field, map[bool]string{true: "is required", false: "is not accepted"}[wantsTarget])
	}
	if target != "" && !uuidShape.MatchString(target) {
		return fmt.Errorf("target id %q is not a UUID", target)
	}
	text := ""
	if file != "" {
		if text, err = readDecisionText(file, "input"); err != nil {
			return err
		}
		if strings.ContainsRune(text, 0) {
			return errors.New("input must not contain a NUL byte")
		}
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var p *proposal
	if field == workflowBudgetField {
		var maxRuns *int
		var includeChildPRs *bool
		if cmd.Flags().Changed("max-runs") {
			n, _ := cmd.Flags().GetInt("max-runs")
			maxRuns = &n
		}
		if cmd.Flags().Changed("include-child-prs") {
			b, _ := cmd.Flags().GetBool("include-child-prs")
			includeChildPRs = &b
		}
		p, err = workflowBudgetProposal(ctx, client, target, maxRuns, includeChildPRs)
	} else {
		p, err = buildProposal(ctx, client, field, target, text, statusKey, settings)
	}
	if err != nil {
		return err
	}
	action := map[string]any{"type": p.Type, "version": 1, "params": p.Params}
	if expires > 0 {
		action["expires_in_seconds"] = expires
	}
	encoded, err := marshalIndent(action)
	if err != nil {
		return err
	}
	fmt.Printf("before_sha256: %s\nafter_sha256:  %s\n", p.BaseSHA256, p.AfterSHA256)
	if dryRun {
		fmt.Printf("%s\n", encoded)
		return nil
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write action file: %w", err)
	}
	fmt.Printf("wrote %s\nnext: multica decision request <issue> --action-file %s\n", out, out)
	return nil
}

// checkProposeField refuses before any read. Only the fields below are
// proposed here; the server gate stays the authority and runs again at approval.
func checkProposeField(field string, settings map[string]json.RawMessage) error {
	gateField := field
	switch field {
	case "agent.instructions", "squad.instructions", "workspace.context", "workflow.steps", workflowBudgetField, decisionaction.SettingsField:
	case "workflow.step.instructions":
		gateField = "workflow.instructions"
	default:
		return fmt.Errorf("%s: %s", decisionaction.CodeDenylisted, "this field cannot be proposed through config propose")
	}
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if err := decisionaction.CheckConfigChange(gateField, keys); err != nil {
		return fmt.Errorf("%s: %w", decisionaction.CodeDenylisted, err)
	}
	return nil
}

func parseSettingsSets(sets []string) (map[string]json.RawMessage, error) {
	settings := map[string]json.RawMessage{}
	for _, pair := range sets {
		key, value, isJSON, ok := splitParam(pair)
		if !ok {
			return nil, fmt.Errorf("--set %q must look like key=value or key:=<json>", pair)
		}
		if _, dup := settings[key]; dup {
			return nil, fmt.Errorf("--set names %s twice", key)
		}
		if !isJSON {
			encoded, _ := json.Marshal(value)
			settings[key] = encoded
			continue
		}
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("--set %s is not valid JSON", key)
		}
		settings[key] = json.RawMessage(value)
	}
	return settings, nil
}

func buildProposal(ctx context.Context, client *cli.APIClient, field, target, text, statusKey string, settings map[string]json.RawMessage) (*proposal, error) {
	switch field {
	case "agent.instructions":
		return textProposal(ctx, client, "/api/agents/"+url.PathEscape(target), "instructions", "agent.instructions.set", "agent_id", target, text)
	case "squad.instructions":
		return textProposal(ctx, client, "/api/squads/"+url.PathEscape(target), "instructions", "squad.instructions.set", "squad_id", target, text)
	case "workspace.context":
		return textProposal(ctx, client, "/api/workspaces/"+url.PathEscape(client.WorkspaceID), "context", "workspace.context.set", "workspace_id", client.WorkspaceID, text)
	case "workflow.steps":
		return workflowStepsProposal(ctx, client, target, text)
	case "workflow.step.instructions":
		return workflowStepInstructionsProposal(ctx, client, target, statusKey, text)
	default:
		return settingsProposal(ctx, client, settings)
	}
}

const workflowBudgetField = "workflow.budget"

// workflowBudgetProposal reads the workflow's live budget and sends only the
// fields the caller named. The after digest mirrors the server: a cap of 0
// clears the stored cap, and a field left out keeps its live value.
func workflowBudgetProposal(ctx context.Context, client *cli.APIClient, workflowID string, maxRuns *int, includeChildPRs *bool) (*proposal, error) {
	if maxRuns != nil && (*maxRuns < 0 || *maxRuns > issueworkflow.MaxRunsPerIssueLimit) {
		return nil, fmt.Errorf("--max-runs must be between 0 and %d", issueworkflow.MaxRunsPerIssueLimit)
	}
	live, err := getLive(ctx, client, "/api/issue-workflows/"+url.PathEscape(workflowID))
	if err != nil {
		return nil, err
	}
	var liveCap *int
	liveChildren := true
	if raw, ok := live["max_runs_per_issue"]; ok {
		if err := json.Unmarshal(raw, &liveCap); err != nil {
			return nil, errors.New("live max_runs_per_issue is not a number")
		}
	}
	if raw, ok := live["include_child_prs"]; ok {
		if err := json.Unmarshal(raw, &liveChildren); err != nil {
			return nil, errors.New("live include_child_prs is not a boolean")
		}
	}
	afterCap, afterChildren := liveCap, liveChildren
	params := map[string]any{"workflow_id": workflowID}
	if maxRuns != nil {
		params["max_runs_per_issue"] = *maxRuns
		afterCap = maxRuns
		if *maxRuns == 0 {
			afterCap = nil
		}
	}
	if includeChildPRs != nil {
		params["include_child_prs"] = *includeChildPRs
		afterChildren = *includeChildPRs
	}
	p := &proposal{Type: decisionaction.WorkflowBudgetSet, Params: params}
	p.BaseSHA256 = decisionaction.WorkflowBudgetDigest(workflowID, liveCap, liveChildren)
	p.AfterSHA256 = decisionaction.WorkflowBudgetDigest(workflowID, afterCap, afterChildren)
	params["base_sha256"] = p.BaseSHA256
	return p, nil
}

func getLive(ctx context.Context, client *cli.APIClient, path string) (map[string]json.RawMessage, error) {
	var live map[string]json.RawMessage
	if err := client.GetJSON(ctx, path, &live); err != nil {
		return nil, fmt.Errorf("read live value: %w", err)
	}
	return live, nil
}

// liveText reads a text column; NULL reads as "" like the server's digest.
func liveText(live map[string]json.RawMessage, key string) (string, error) {
	var value *string
	if raw, ok := live[key]; ok {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("live %s is not text", key)
		}
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
}

func textProposal(ctx context.Context, client *cli.APIClient, path, key, actionType, idKey, id, text string) (*proposal, error) {
	live, err := getLive(ctx, client, path)
	if err != nil {
		return nil, err
	}
	before, err := liveText(live, key)
	if err != nil {
		return nil, err
	}
	p := &proposal{Type: actionType, BaseSHA256: decisionaction.TextDigest(before), AfterSHA256: decisionaction.TextDigest(text)}
	p.Params = map[string]any{idKey: id, key: text, "base_sha256": p.BaseSHA256, "after_sha256": p.AfterSHA256}
	return p, nil
}

func liveSteps(ctx context.Context, client *cli.APIClient, workflowID string) (json.RawMessage, error) {
	live, err := getLive(ctx, client, "/api/issue-workflows/"+url.PathEscape(workflowID))
	if err != nil {
		return nil, err
	}
	if len(live["steps"]) == 0 {
		return nil, errors.New("workflow has no steps in the response")
	}
	return live["steps"], nil
}

func workflowStepsProposal(ctx context.Context, client *cli.APIClient, workflowID, text string) (*proposal, error) {
	var steps []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &steps); err != nil {
		return nil, errors.New("steps file must be one JSON array (edit the steps the workflow GET returns)")
	}
	for _, step := range steps {
		if err := normalizeStep(step); err != nil {
			return nil, err
		}
	}
	normalized, err := json.Marshal(steps)
	if err != nil {
		return nil, err
	}
	current, err := liveSteps(ctx, client, workflowID)
	if err != nil {
		return nil, err
	}
	p := &proposal{Type: "workflow.steps.set"}
	if p.BaseSHA256, err = decisionaction.WorkflowDigest(workflowID, current); err != nil {
		return nil, err
	}
	// The server hashes the steps it would store, so hash the normalized ones.
	if p.AfterSHA256, err = decisionaction.WorkflowDigest(workflowID, normalized); err != nil {
		return nil, err
	}
	p.Params = map[string]any{"workflow_id": workflowID, "steps": json.RawMessage(normalized), "base_sha256": p.BaseSHA256, "after_sha256": p.AfterSHA256}
	return p, nil
}

func workflowStepInstructionsProposal(ctx context.Context, client *cli.APIClient, workflowID, statusKey, text string) (*proposal, error) {
	if statusKey == "" {
		return nil, errors.New("--status-key is required")
	}
	current, err := liveSteps(ctx, client, workflowID)
	if err != nil {
		return nil, err
	}
	var steps []map[string]json.RawMessage
	if err := json.Unmarshal(current, &steps); err != nil {
		return nil, errors.New("live steps are not a JSON array")
	}
	found := false
	for _, step := range steps {
		var key string
		if json.Unmarshal(step["status_key"], &key) == nil && key == statusKey {
			step["instructions"], _ = json.Marshal(text)
			if err := normalizeStep(step); err != nil {
				return nil, err
			}
			var stored string
			if err := json.Unmarshal(step["instructions"], &stored); err != nil {
				return nil, err
			}
			if stored == "" && strings.TrimSpace(text) != "" {
				return nil, fmt.Errorf("step %q has no handler, so the server drops its instructions", statusKey)
			}
			text = stored // the text the server stores
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("workflow has no step with status key %q", statusKey)
	}
	after, err := json.Marshal(steps)
	if err != nil {
		return nil, err
	}
	p := &proposal{Type: "workflow.step.instructions.set"}
	if p.BaseSHA256, err = decisionaction.WorkflowDigest(workflowID, current); err != nil {
		return nil, err
	}
	// The server computes this action's after digest; the CLI shows it for the person to compare.
	if p.AfterSHA256, err = decisionaction.WorkflowDigest(workflowID, after); err != nil {
		return nil, err
	}
	p.Params = map[string]any{"workflow_id": workflowID, "status_key": statusKey, "instructions": text, "base_sha256": p.BaseSHA256}
	return p, nil
}

func settingsProposal(ctx context.Context, client *cli.APIClient, patch map[string]json.RawMessage) (*proposal, error) {
	live, err := getLive(ctx, client, "/api/workspaces/"+url.PathEscape(client.WorkspaceID))
	if err != nil {
		return nil, err
	}
	var stored map[string]json.RawMessage
	if len(live["settings"]) > 0 {
		_ = json.Unmarshal(live["settings"], &stored)
	}
	before := make(map[string]json.RawMessage, len(patch))
	after := make(map[string]json.RawMessage, len(patch))
	for key, value := range patch {
		if isJSONNull(stored[key]) {
			before[key] = nil
		} else {
			before[key] = stored[key]
		}
		after[key] = settingAfter(key, value, stored[key])
	}
	p := &proposal{Type: "workspace.settings.patch"}
	if p.BaseSHA256, err = decisionaction.SettingsDigest(client.WorkspaceID, before); err != nil {
		return nil, err
	}
	if p.AfterSHA256, err = decisionaction.SettingsDigest(client.WorkspaceID, after); err != nil {
		return nil, err
	}
	p.Params = map[string]any{"workspace_id": client.WorkspaceID, "settings": patch, "base_sha256": p.BaseSHA256, "after_sha256": p.AfterSHA256}
	return p, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// settingAfter is the stored value after the patch: null removes the key, an
// unpaused switch reads as unset, object keys merge, the rest are replaced.
func settingAfter(key string, value, stored json.RawMessage) json.RawMessage {
	switch {
	case isJSONNull(value):
		return nil
	case key == "wakeups_paused":
		if bytes.Equal(bytes.TrimSpace(value), []byte("true")) {
			return value
		}
		return nil
	case !slices.Contains(objectMergeKeys, key):
		return value
	}
	merged := map[string]json.RawMessage{}
	_ = json.Unmarshal(stored, &merged)
	var named map[string]json.RawMessage
	_ = json.Unmarshal(value, &named)
	for name, fieldValue := range named {
		merged[name] = fieldValue
	}
	out, _ := json.Marshal(merged)
	return out
}

func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// normalizeStep applies the server's issueworkflow.NormalizeHandoff to one
// step in the GET shape, so the digest is of the step the server would store:
// trimmed fields, no instructions on a handler-less step, no stale handler id.
func normalizeStep(step map[string]json.RawMessage) error {
	var s issueworkflow.Step
	raw, err := json.Marshal(step)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return errors.New("a step is not a workflow step")
	}
	s = issueworkflow.NormalizeHandoff(s)
	set := func(key string, value any, omit bool) {
		if omit {
			delete(step, key)
			return
		}
		step[key], _ = json.Marshal(value)
	}
	set("status_key", s.StatusKey, false)
	set("handler", s.Handler, false)
	set("instructions", s.Instructions, false)
	set("next_status_key", s.NextStatusKey, s.NextStatusKey == "")
	set("back_status_key", s.BackStatusKey, s.BackStatusKey == "")
	if _, ok := step["edges"]; ok {
		set("edges", s.Edges, len(s.Edges) == 0)
	}
	if _, ok := step["entry"]; ok {
		set("entry", s.Entry, len(s.Entry) == 0)
	}
	if _, ok := step["sub_workflow"]; ok {
		set("sub_workflow", s.SubWorkflow, s.SubWorkflow == nil)
	}
	return nil
}
