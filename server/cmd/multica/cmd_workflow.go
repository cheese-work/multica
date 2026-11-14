package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Workflow commands wrap /api/issue-workflows and the project workflow
// switch. Permission checks stay on the server: edits and deletes are limited
// to the workflow owner and workspace admins, switching a project's workflow
// to admins and the project lead. The CLI reports the server's error as is.
//
// Only step and handoff edits (update --steps-file, set-instructions, handoff
// set) take --decision-id/--print-hash, the CHE-2448 decision-bound path an
// agent task token needs. create, delete and project workflow set carry no
// such flags and the server refuses machine credentials on them.

var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Work with issue workflows (statuses and their handoffs)",
}

var workflowListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workflows in the workspace",
	RunE:  runWorkflowList,
}

var workflowGetCmd = &cobra.Command{
	Use:   "get <workflow>",
	Short: "Get a workflow and its steps (accepts an ID, ID prefix, exact name, or \"default\")",
	Args:  exactArgs(1),
	RunE:  runWorkflowGet,
}

var workflowLintCmd = &cobra.Command{
	Use:   "lint [<id>]",
	Short: "Lint one workflow, or every workflow with --all",
	Long: `Run the workflow lint rules against the live server. Waived findings are
listed but do not count. Exits non-zero when any workflow has an unwaived error.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkflowLint,
}

var workflowCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a workflow",
	Long: `Create a workflow.

--steps-file takes a JSON array of steps in board order, the same shape the
API accepts, e.g.:

  [
    {"status_key": "todo"},
    {"status_key": "in_review", "handler": {"type": "agent", "id": "<agent-id>"},
     "instructions": "Review the PR", "next_status_key": "done"},
    {"new_status": {"name": "QA", "category": "started"}},
    {"status_key": "done"}
  ]

Todo, done and cancelled are added when missing.`,
	RunE: runWorkflowCreate,
}

var workflowUpdateCmd = &cobra.Command{
	Use:   "update <workflow>",
	Short: "Update a workflow's name, description, steps or run rules",
	Long: `Update a workflow's name, description, steps or run rules.

--steps-file replaces all steps (same JSON shape as "workflow create" and the
"steps" array "workflow get --output json" prints: status_key, handler,
instructions, next_status_key, back_status_key, edges (guarded exits:
[{"to","guard","timeout","on_timeout"}]; PR guards need a timeout), entry (gates
a status write must meet to enter the step: [{"guard","mode":"off|warn|block",
"member_override"}]; omit to keep the stored gates, [] to clear them)). When the
new steps drop a status that issues still use, map each dropped status with
--map old=new. Use --dry-run to print the migration plan without writing.

Run rules change with their own flags, with or without --steps-file; a rule
flag left out keeps its value. --max-runs-per-issue N (1 to 1000) caps the runs
one issue can use before the engine blocks it (0 restores the default of 15);
--include-child-prs=false stops PR guards from reading the PRs of sub-issues.
--start-status KEY sets the status where new issues of the workflow start (an
unstarted or started step of the workflow; "" restores todo).
A task token changes --max-runs-per-issue or --include-child-prs only through
an owner's approval: with --issue <issue> (and --for <user-id>) the command reads
the live workflow, opens one workflow.budget.set card with only the budget flags
you named, prints the card URL and writes nothing until the owner approves with a
passkey. Repeating the same call returns the same card. A budget call carries no
--name, --description, --steps-file, --start-status, --map or --dry-run: run two
commands instead. --print-hash prints before_sha256 and after_sha256 of the
budget change; "multica decision request" with the workflow.budget field and
--decision-id is the manual path. "multica config propose workflow.budget"
names the same budget as --max-runs N (0 restores the default) and
--include-child-prs.
Prints the server's readback.`,
	Args: exactArgs(1),
	RunE: runWorkflowUpdate,
}

var workflowSetInstructionsCmd = &cobra.Command{
	Use:   "set-instructions <workflow>",
	Short: "Change one step's instructions from a file (owner/admin only)",
	Long: `Read the workflow, replace the "instructions" of the single step whose
status_key equals --status with the contents of --file, save, and print the
readback. Every other step is sent back unchanged. Fails without sending
anything when --status matches no step or more than one.`,
	Args: exactArgs(1),
	RunE: runWorkflowSetInstructions,
}

var workflowDeleteCmd = &cobra.Command{
	Use:   "delete <workflow>",
	Short: "Delete a workflow (rejected while a project still uses it)",
	Args:  exactArgs(1),
	RunE:  runWorkflowDelete,
}

var workflowHandoffCmd = &cobra.Command{
	Use:   "handoff",
	Short: "Manage what happens when an issue enters a workflow status",
}

var workflowHandoffSetCmd = &cobra.Command{
	Use:   "set <workflow> <status-key>",
	Short: "Set the handler, instructions, and next/back status for one step",
	Long: `Set the handoff of one step. Only the flags you pass change; the rest of
the step is kept.

--handler accepts none, project_lead, creator, or a member, agent, or squad
name or UUID.`,
	Args: exactArgs(2),
	RunE: runWorkflowHandoffSet,
}

var projectWorkflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Show or switch the workflow a project uses",
}

var projectWorkflowGetCmd = &cobra.Command{
	Use:   "get <project-id>",
	Short: "Show the project's effective workflow (Default when none is set)",
	Args:  exactArgs(1),
	RunE:  runProjectWorkflowGet,
}

var projectWorkflowSetCmd = &cobra.Command{
	Use:   "set <project-id> <workflow|default>",
	Short: "Switch the project to another workflow",
	Long: `Switch the project to another workflow, or back to the workspace Default.

Issues on statuses the new workflow does not list must be mapped with
--map old=new. Use --dry-run to print the migration plan first. Moving issues
this way neither reassigns them nor starts runs.`,
	Args: exactArgs(2),
	RunE: runProjectWorkflowSet,
}

func init() {
	workflowCmd.AddCommand(workflowListCmd)
	workflowCmd.AddCommand(workflowGetCmd)
	workflowCmd.AddCommand(workflowLintCmd)
	workflowCmd.AddCommand(workflowCreateCmd)
	workflowCmd.AddCommand(workflowUpdateCmd)
	workflowCmd.AddCommand(workflowSetInstructionsCmd)
	workflowCmd.AddCommand(workflowDeleteCmd)
	workflowCmd.AddCommand(workflowHandoffCmd)
	workflowHandoffCmd.AddCommand(workflowHandoffSetCmd)

	projectCmd.AddCommand(projectWorkflowCmd)
	projectWorkflowCmd.AddCommand(projectWorkflowGetCmd)
	projectWorkflowCmd.AddCommand(projectWorkflowSetCmd)

	workflowListCmd.Flags().String("output", "table", "Output format: table or json")
	workflowListCmd.Flags().Bool("full-id", false, "Show full UUIDs in table output")

	workflowGetCmd.Flags().String("output", "json", "Output format: table or json")
	workflowLintCmd.Flags().Bool("all", false, "Lint every workflow in the workspace")
	workflowLintCmd.Flags().String("output", "table", "Output format: table or json")

	workflowCreateCmd.Flags().String("name", "", "Workflow name (required)")
	workflowCreateCmd.Flags().String("description", "", "Workflow description")
	workflowCreateCmd.Flags().String("steps-file", "", "Path to a JSON array of steps")
	workflowCreateCmd.Flags().String("output", "json", "Output format: table or json")

	workflowUpdateCmd.Flags().String("name", "", "New name")
	workflowUpdateCmd.Flags().String("description", "", "New description")
	workflowUpdateCmd.Flags().String("steps-file", "", "Path to a JSON array of steps; replaces all steps")
	workflowUpdateCmd.Flags().StringArray("map", nil, "Move issues off a dropped status: old=new (repeatable)")
	workflowUpdateCmd.Flags().Bool("dry-run", false, "Print the migration plan without writing")
	workflowUpdateCmd.Flags().Int("max-runs-per-issue", 0, "Runs one issue can use before it is blocked, 1-1000 (0 restores the default)")
	workflowUpdateCmd.Flags().Bool("include-child-prs", true, "Whether PR guards also read the PRs of sub-issues")
	workflowUpdateCmd.Flags().String("start-status", "", "Status where new issues of the workflow start; an unstarted or started step (\"\" restores todo)")
	workflowUpdateCmd.Flags().String("output", "json", "Output format: table or json")
	workflowUpdateCmd.Flags().String("issue", "", "Task token budget change: issue UUID or identifier the card is about (required)")
	workflowUpdateCmd.Flags().String("for", "", forFlagHelp)

	workflowSetInstructionsCmd.Flags().String("status", "", "status_key of the step to change (required)")
	workflowSetInstructionsCmd.Flags().String("file", "", "File holding the new instructions text (required)")
	workflowSetInstructionsCmd.Flags().String("output", "json", "Output format: table or json")
	for _, c := range []*cobra.Command{workflowUpdateCmd, workflowSetInstructionsCmd, workflowHandoffSetCmd} {
		c.Flags().String("decision-id", "", "CHE-2448 decision path: id of the decision request Cheese answered for this exact change. The CLI sends decision_id, before_sha256 and after_sha256 and takes the steps from the live workflow read. Agent task tokens only.")
		c.Flags().Bool("print-hash", false, "Print before_sha256 and after_sha256 for this change and exit without writing")
	}

	workflowDeleteCmd.Flags().String("output", "table", "Output format: table or json")

	workflowHandoffSetCmd.Flags().String("handler", "", "none, project_lead, creator, or a member/agent/squad name or UUID")
	workflowHandoffSetCmd.Flags().String("instructions", "", "Instructions the handler's run gets")
	workflowHandoffSetCmd.Flags().String("instructions-file", "", "Read instructions from a file")
	workflowHandoffSetCmd.Flags().String("next", "", "Status the handler moves the issue to when done (empty to clear)")
	workflowHandoffSetCmd.Flags().String("back", "", "Status the handler sends the issue back to (empty to clear)")
	workflowHandoffSetCmd.Flags().String("copy-for-project", "", "Save on a copy of the workflow used only by this project")
	workflowHandoffSetCmd.Flags().String("output", "json", "Output format: table or json")

	projectWorkflowGetCmd.Flags().String("output", "json", "Output format: table or json")

	projectWorkflowSetCmd.Flags().StringArray("map", nil, "Move issues off a status the new workflow lacks: old=new (repeatable)")
	projectWorkflowSetCmd.Flags().Bool("dry-run", false, "Print the migration plan without writing")
	projectWorkflowSetCmd.Flags().String("output", "json", "Output format: table or json")
}

// --- helpers ---

func listWorkflows(ctx context.Context, client *cli.APIClient) ([]map[string]any, error) {
	path := "/api/issue-workflows"
	if client.WorkspaceID != "" {
		path += "?" + url.Values{"workspace_id": {client.WorkspaceID}}.Encode()
	}
	var result struct {
		Workflows []map[string]any `json:"workflows"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return nil, err
	}
	return result.Workflows, nil
}

// resolveWorkflow accepts "default", a full UUID, an exact name
// (case-insensitive), or a unique ID prefix.
func resolveWorkflow(ctx context.Context, client *cli.APIClient, input string) (map[string]any, error) {
	ref := strings.TrimSpace(input)
	if ref == "" {
		return nil, fmt.Errorf("workflow is required")
	}
	if uuidRegexp.MatchString(ref) {
		var wf map[string]any
		if err := client.GetJSON(ctx, "/api/issue-workflows/"+ref, &wf); err != nil {
			return nil, err
		}
		return wf, nil
	}
	workflows, err := listWorkflows(ctx, client)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(ref, "default") {
		for _, wf := range workflows {
			if b, _ := wf["is_default"].(bool); b {
				return wf, nil
			}
		}
	}
	var byName []map[string]any
	for _, wf := range workflows {
		if strings.EqualFold(strVal(wf, "name"), ref) {
			byName = append(byName, wf)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		return nil, fmt.Errorf("more than one workflow is named %q; pass its ID", ref)
	}
	prefix, err := normalizeUUIDPrefix(ref)
	if err != nil {
		return nil, fmt.Errorf("no workflow named %q", ref)
	}
	var matches []idCandidate
	var found map[string]any
	for _, wf := range workflows {
		if strings.HasPrefix(compactUUID(strVal(wf, "id")), prefix) {
			matches = append(matches, idCandidate{ID: strVal(wf, "id")})
			found = wf
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no workflow found matching %q; run `multica workflow list --full-id`", ref)
	case 1:
		return found, nil
	default:
		return nil, ambiguousIDPrefixError("workflow", ref, matches)
	}
}

// parseStatusMapping turns repeated old=new flags into a mapping.
func parseStatusMapping(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		from, to, ok := strings.Cut(p, "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("invalid --map %q; expected old=new", p)
		}
		if prev, dup := out[from]; dup && prev != to {
			return nil, fmt.Errorf("--map maps %q twice (%q and %q)", from, prev, to)
		}
		out[from] = to
	}
	return out, nil
}

func readStepsFile(path string) ([]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --steps-file: %w", err)
	}
	var steps []any
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, fmt.Errorf("--steps-file must be a JSON array of steps: %w", err)
	}
	return steps, nil
}

func workflowSteps(wf map[string]any) []map[string]any {
	raw, _ := wf["steps"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func printWorkflowTable(wf map[string]any) {
	fmt.Printf("%s  %s (version %s)\n", strVal(wf, "id"), strVal(wf, "name"), strVal(wf, "version"))
	printWorkflowRules(wf)
	steps, _ := wf["steps"].([]any)
	rows := make([][]string, 0, len(steps))
	var instructions strings.Builder
	for _, raw := range steps {
		s, _ := raw.(map[string]any)
		h, _ := s["handler"].(map[string]any)
		handler := strVal(h, "type")
		if id := strVal(h, "id"); id != "" {
			handler += ":" + id
		}
		rows = append(rows, []string{strVal(s, "status_key"), handler, strVal(s, "next_status_key"), strVal(s, "back_status_key")})
		if text := strVal(s, "instructions"); text != "" {
			fmt.Fprintf(&instructions, "\n--- %s ---\n%s\n", strVal(s, "status_key"), text)
		}
	}
	cli.PrintTable(os.Stdout, []string{"STATUS", "HANDLER", "NEXT", "BACK"}, rows)
	fmt.Print(instructions.String())
}

// printWorkflowRules prints the run rules; a server that sends none gets the
// defaults, which are what such a workflow runs with.
func printWorkflowRules(wf map[string]any) {
	maxRuns := fmt.Sprintf("%d (default)", issueworkflow.DefaultMaxRunsPerIssue)
	if n, ok := wf["max_runs_per_issue"].(float64); ok {
		maxRuns = fmt.Sprint(int(n))
	}
	include := true
	if b, ok := wf["include_child_prs"].(bool); ok {
		include = b
	}
	start := issueworkflow.StartStatus
	if key := strVal(wf, "start_status_key"); key != "" {
		start = key
	}
	fmt.Printf("max runs per issue: %s, include child PRs: %t, start status: %s\n", maxRuns, include, start)
}

func printMappingPlan(plan map[string]any) {
	fmt.Fprintf(os.Stdout, "Dry run: %v of %v issue(s) would move.\n", plan["affected_issues"], plan["total_issues"])
	if required, _ := plan["required"].([]any); len(required) > 0 {
		printMappingRequired(plan)
	}
}

// reportMappingRequired handles the server's 409
// workflow_status_mapping_required. The default error formatter keeps only
// the first message line, which drops the plan a script needs to supply every
// mapping at once. This prints the full body (JSON) or the plan (table) to
// stdout, the server message to stderr, and returns errSilent so the command
// still exits non-zero (1, as for any other conflict) without printing twice. Any other error returns nil and is left to the
// caller.
func reportMappingRequired(cmd *cobra.Command, err error) error {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict {
		return nil
	}
	var body map[string]any
	if json.Unmarshal([]byte(httpErr.Body), &body) != nil || strVal(body, "code") != "workflow_status_mapping_required" {
		return nil
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		if plan, ok := body["plan"].(map[string]any); ok {
			printMappingRequired(plan)
		}
	} else if perr := cli.PrintJSON(os.Stdout, body); perr != nil {
		return perr
	}
	fmt.Fprintf(os.Stderr, "Error: %s; pass --map old=new for each status listed\n", strVal(body, "error"))
	return errSilent
}

func printMappingRequired(plan map[string]any) {
	required, _ := plan["required"].([]any)
	headers := []string{"FROM STATUS", "ISSUES", "SUGGESTED"}
	rows := make([][]string, 0, len(required))
	for _, raw := range required {
		r, _ := raw.(map[string]any)
		rows = append(rows, []string{strVal(r, "status_key"), fmt.Sprint(r["issue_count"]), strVal(r, "suggested_status_key")})
	}
	cli.PrintTable(os.Stdout, headers, rows)
}

// resolveHandoffHandler maps the --handler value to a handler payload.
func resolveHandoffHandler(ctx context.Context, client *cli.APIClient, value string) (map[string]any, error) {
	v := strings.TrimSpace(value)
	switch strings.ToLower(v) {
	case "", "none":
		return map[string]any{"type": "none"}, nil
	case "project_lead", "creator":
		return map[string]any{"type": strings.ToLower(v)}, nil
	}
	var typ, id string
	var err error
	if uuidRegexp.MatchString(v) {
		typ, id, err = resolveAssigneeByID(ctx, client, v, issueAssigneeKinds)
	} else {
		typ, id, err = resolveAssignee(ctx, client, v, issueAssigneeKinds)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve handler: %w", err)
	}
	return map[string]any{"type": typ, "id": id}, nil
}

// workflowRefID is the workflow ID for a ref. A UUID is used as given, so a
// write needs no extra read; a name, "default" or ID prefix is resolved.
func workflowRefID(ctx context.Context, client *cli.APIClient, ref string) (string, error) {
	if uuidRegexp.MatchString(strings.TrimSpace(ref)) {
		return strings.TrimSpace(ref), nil
	}
	wf, err := resolveWorkflow(ctx, client, ref)
	if err != nil {
		return "", err
	}
	return strVal(wf, "id"), nil
}

func getWorkflow(ctx context.Context, client *cli.APIClient, id string) (map[string]any, error) {
	var wf map[string]any
	if err := client.GetJSON(ctx, "/api/issue-workflows/"+id, &wf); err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	return wf, nil
}

func patchWorkflow(ctx context.Context, client *cli.APIClient, id string, body map[string]any) (map[string]any, error) {
	var wf map[string]any
	if err := client.PatchJSON(ctx, "/api/issue-workflows/"+id, body, &wf); err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden && body["decision_id"] == nil {
			return nil, fmt.Errorf("update workflow: only a workspace owner or admin can edit workflows (server said: %s)", httpErr.Body)
		}
		return nil, fmt.Errorf("update workflow: %w", err)
	}
	return wf, nil
}

func printWorkflow(cmd *cobra.Command, wf map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		printWorkflowTable(wf)
		return nil
	}
	return cli.PrintJSON(os.Stdout, wf)
}

// bindWorkflow hashes steps as the workflow's new steps; before is the hash of
// the live workflow taken earlier.
func bindWorkflow(cmd *cobra.Command, live map[string]any, before string, steps []any) (workflowBinding, error) {
	after, err := workflowStoredStepsHash(strVal(live, "id"), steps)
	if err != nil {
		return workflowBinding{}, fmt.Errorf("hash new steps: %w", err)
	}
	return workflowBinding{decisionID: mustGetString(cmd, "decision-id"), before: before, after: after}, nil
}

// liveWorkflowHash is the hash of the live workflow's steps.
func liveWorkflowHash(live map[string]any) (string, error) {
	steps, _ := live["steps"].([]any)
	before, err := workflowStepsHash(strVal(live, "id"), steps)
	if err != nil {
		return "", fmt.Errorf("hash live workflow: %w", err)
	}
	return before, nil
}

// bindHandoff hashes the steps the server stores for a handoff PUT: the live
// steps with this one replaced by body, as SetIssueWorkflowHandoff builds it
// (NormalizeHandoff, then the step's own edges, sub-workflow and entry kept).
func bindHandoff(cmd *cobra.Command, live map[string]any, statusKey string, body map[string]any) (workflowBinding, error) {
	before, err := liveWorkflowHash(live)
	if err != nil {
		return workflowBinding{}, err
	}
	liveSteps, _ := live["steps"].([]any)
	typed, err := decodeSteps(liveSteps)
	if err != nil {
		return workflowBinding{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return workflowBinding{}, err
	}
	var step issueworkflow.Step
	if err := json.Unmarshal(raw, &step); err != nil {
		return workflowBinding{}, fmt.Errorf("handoff: %w", err)
	}
	step.StatusKey = statusKey
	step = issueworkflow.NormalizeHandoff(step)
	for i, stored := range typed {
		if stored.StatusKey != step.StatusKey {
			continue
		}
		step.Edges, step.SubWorkflow, step.Entry = stored.Edges, stored.SubWorkflow, stored.Entry
		typed[i] = step
	}
	after, err := hashSteps(strVal(live, "id"), typed)
	if err != nil {
		return workflowBinding{}, fmt.Errorf("hash new steps: %w", err)
	}
	return workflowBinding{decisionID: mustGetString(cmd, "decision-id"), before: before, after: after}, nil
}

// --- workflow commands ---

func runWorkflowList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	workflows, err := listWorkflows(ctx, client)
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, workflows)
	}
	fullID, _ := cmd.Flags().GetBool("full-id")
	headers := []string{"ID", "NAME", "DEFAULT", "STEPS", "PROJECTS"}
	rows := make([][]string, 0, len(workflows))
	for _, wf := range workflows {
		isDefault := ""
		if b, _ := wf["is_default"].(bool); b {
			isDefault = "yes"
		}
		projects, _ := wf["project_ids"].([]any)
		rows = append(rows, []string{
			displayID(strVal(wf, "id"), fullID),
			strVal(wf, "name"),
			isDefault,
			strconv.Itoa(len(workflowSteps(wf))),
			strconv.Itoa(len(projects)),
		})
	}
	cli.PrintTable(os.Stdout, headers, rows)
	for _, wf := range workflows {
		fmt.Println()
		printWorkflowTable(wf)
	}
	return nil
}

func runWorkflowGet(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	wf, err := resolveWorkflow(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("get workflow: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		printWorkflowTable(wf)
		return nil
	}
	return cli.PrintJSON(os.Stdout, wf)
}

func runWorkflowCreate(cmd *cobra.Command, _ []string) error {
	name, _ := cmd.Flags().GetString("name")
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("--name is required")
	}
	body := map[string]any{"name": name}
	if cmd.Flags().Changed("description") {
		v, _ := cmd.Flags().GetString("description")
		body["description"] = v
	}
	if path, _ := cmd.Flags().GetString("steps-file"); path != "" {
		steps, err := readStepsFile(path)
		if err != nil {
			return err
		}
		body["steps"] = steps
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result map[string]any
	if err := client.PostJSON(ctx, "/api/issue-workflows", body, &result); err != nil {
		return fmt.Errorf("create workflow: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		printWorkflowTable(result)
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runWorkflowUpdate(cmd *cobra.Command, args []string) error {
	if proposesWorkflowBudget(cmd) {
		return proposeWorkflowBudget(cmd, args[0])
	}
	body := map[string]any{}
	if cmd.Flags().Changed("name") {
		v, _ := cmd.Flags().GetString("name")
		body["name"] = v
	}
	if cmd.Flags().Changed("description") {
		v, _ := cmd.Flags().GetString("description")
		body["description"] = v
	}
	if path, _ := cmd.Flags().GetString("steps-file"); path != "" {
		steps, err := readStepsFile(path)
		if err != nil {
			return err
		}
		body["steps"] = steps
	}
	if cmd.Flags().Changed("max-runs-per-issue") {
		n, _ := cmd.Flags().GetInt("max-runs-per-issue")
		body["max_runs_per_issue"] = n
	}
	if cmd.Flags().Changed("include-child-prs") {
		include, _ := cmd.Flags().GetBool("include-child-prs")
		body["include_child_prs"] = include
	}
	if cmd.Flags().Changed("start-status") {
		key, _ := cmd.Flags().GetString("start-status")
		body["start_status_key"] = key
	}
	if len(body) == 0 {
		return errors.New("nothing to update; provide --name, --description, --steps-file, --max-runs-per-issue, --include-child-prs and/or --start-status")
	}
	if err := checkWorkflowBindingFlags(cmd); err != nil {
		return err
	}
	newSteps, hasSteps := body["steps"].([]any)
	_, hasCap := body["max_runs_per_issue"]
	_, hasChildren := body["include_child_prs"]
	budgetWrite := hasCap || hasChildren
	if workflowBindingRequested(cmd) && hasSteps == budgetWrite {
		return errors.New("a decision-bound write carries either --steps-file or the run budget flags, not both")
	}
	pairs, _ := cmd.Flags().GetStringArray("map")
	mapping, err := parseStatusMapping(pairs)
	if err != nil {
		return err
	}
	if mapping != nil {
		body["status_mapping"] = mapping
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		body["dry_run"] = true
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	id, err := workflowRefID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve workflow: %w", err)
	}
	if workflowBindingRequested(cmd) && budgetWrite {
		if err := bindBudgetWrite(cmd, ctx, client, id, body); err != nil {
			return err
		}
		if printHashRequested(cmd) {
			return nil
		}
	} else if workflowBindingRequested(cmd) {
		live, err := getWorkflow(ctx, client, id)
		if err != nil {
			return err
		}
		before, err := liveWorkflowHash(live)
		if err != nil {
			return err
		}
		binding, err := bindWorkflow(cmd, live, before, newSteps)
		if err != nil {
			return err
		}
		if done, err := binding.apply(cmd, body); done {
			return err
		}
		if v := strVal(live, "version"); v != "" {
			body["expected_version"] = v
		}
	}

	result, err := patchWorkflow(ctx, client, id, body)
	if err != nil {
		if reported := reportMappingRequired(cmd, err); reported != nil {
			return reported
		}
		return err
	}
	if plan, ok := result["plan"].(map[string]any); ok && dryRun {
		if output, _ := cmd.Flags().GetString("output"); output == "table" {
			printMappingPlan(plan)
			return nil
		}
	}
	return printWorkflow(cmd, result)
}

func runWorkflowSetInstructions(cmd *cobra.Command, args []string) error {
	status, _ := cmd.Flags().GetString("status")
	path, _ := cmd.Flags().GetString("file")
	if status == "" || path == "" {
		return errors.New("--status and --file are required")
	}
	if err := checkWorkflowBindingFlags(cmd); err != nil {
		return err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read instructions file: %w", err)
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	id, err := workflowRefID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve workflow: %w", err)
	}
	wf, err := getWorkflow(ctx, client, id)
	if err != nil {
		return err
	}
	steps, _ := wf["steps"].([]any)
	var match map[string]any
	for _, raw := range steps {
		if s, _ := raw.(map[string]any); strVal(s, "status_key") == status {
			if match != nil {
				return fmt.Errorf("status %q matches more than one step; nothing was changed", status)
			}
			match = s
		}
	}
	if match == nil {
		return fmt.Errorf("status %q matches no step; nothing was changed", status)
	}
	var before string
	if workflowBindingRequested(cmd) {
		if before, err = liveWorkflowHash(wf); err != nil {
			return err
		}
	}
	match["instructions"] = string(text)

	// expected_version makes the server refuse the write if someone edited the
	// workflow after the read above, instead of overwriting their change.
	body := map[string]any{"steps": steps}
	if v := strVal(wf, "version"); v != "" {
		body["expected_version"] = v
	}
	if workflowBindingRequested(cmd) {
		binding, err := bindWorkflow(cmd, wf, before, steps)
		if err != nil {
			return err
		}
		if done, err := binding.apply(cmd, body); done {
			return err
		}
	}
	wf, err = patchWorkflow(ctx, client, id, body)
	if err != nil {
		return err
	}
	return printWorkflow(cmd, wf)
}

func runWorkflowDelete(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	wf, err := resolveWorkflow(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve workflow: %w", err)
	}
	id := strVal(wf, "id")
	if err := client.DeleteJSON(ctx, "/api/issue-workflows/"+id); err != nil {
		return fmt.Errorf("delete workflow: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, map[string]any{"id": id, "deleted": true})
	}
	fmt.Fprintf(os.Stdout, "Workflow %s deleted.\n", strVal(wf, "name"))
	return nil
}

func runWorkflowHandoffSet(cmd *cobra.Command, args []string) error {
	statusKey := strings.TrimSpace(args[1])
	if cmd.Flags().Changed("instructions") && cmd.Flags().Changed("instructions-file") {
		return fmt.Errorf("use either --instructions or --instructions-file, not both")
	}
	if err := checkWorkflowBindingFlags(cmd); err != nil {
		return err
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	wf, err := resolveWorkflow(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve workflow: %w", err)
	}
	var current map[string]any
	for _, s := range workflowSteps(wf) {
		if strVal(s, "status_key") == statusKey {
			current = s
			break
		}
	}
	if current == nil {
		return fmt.Errorf("workflow %s has no step %q", strVal(wf, "name"), statusKey)
	}

	// The API replaces the whole handoff; start from the current step so
	// only the flags passed change.
	body := map[string]any{
		"handler":         current["handler"],
		"instructions":    strVal(current, "instructions"),
		"next_status_key": strVal(current, "next_status_key"),
		"back_status_key": strVal(current, "back_status_key"),
	}
	if cmd.Flags().Changed("handler") {
		v, _ := cmd.Flags().GetString("handler")
		handler, err := resolveHandoffHandler(ctx, client, v)
		if err != nil {
			return err
		}
		body["handler"] = handler
	}
	if cmd.Flags().Changed("instructions") {
		v, _ := cmd.Flags().GetString("instructions")
		body["instructions"] = v
	}
	if path, _ := cmd.Flags().GetString("instructions-file"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read --instructions-file: %w", err)
		}
		body["instructions"] = string(data)
	}
	if cmd.Flags().Changed("next") {
		v, _ := cmd.Flags().GetString("next")
		body["next_status_key"] = v
	}
	if cmd.Flags().Changed("back") {
		v, _ := cmd.Flags().GetString("back")
		body["back_status_key"] = v
	}
	if v, _ := cmd.Flags().GetString("copy-for-project"); v != "" {
		projectRef, err := resolveProjectID(ctx, client, v)
		if err != nil {
			return fmt.Errorf("resolve project: %w", err)
		}
		body["copy_for_project_id"] = projectRef.ID
	}

	if workflowBindingRequested(cmd) {
		live, err := getWorkflow(ctx, client, strVal(wf, "id"))
		if err != nil {
			return err
		}
		binding, err := bindHandoff(cmd, live, statusKey, body)
		if err != nil {
			return err
		}
		if done, err := binding.apply(cmd, body); done {
			return err
		}
	}

	var result map[string]any
	path := "/api/issue-workflows/" + strVal(wf, "id") + "/handoffs/" + url.PathEscape(statusKey)
	if err := client.PutJSON(ctx, path, body, &result); err != nil {
		return fmt.Errorf("set handoff: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		printWorkflowTable(result)
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}

// --- project workflow commands ---

func runProjectWorkflowGet(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	projectRef, err := resolveProjectID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	var project map[string]any
	if err := client.GetJSON(ctx, "/api/projects/"+projectRef.ID, &project); err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	ref := strVal(project, "workflow_id")
	usesDefault := ref == ""
	if usesDefault {
		ref = "default"
	}
	wf, err := resolveWorkflow(ctx, client, ref)
	if err != nil {
		return fmt.Errorf("get workflow: %w", err)
	}

	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		if usesDefault {
			fmt.Fprintln(os.Stdout, "Project uses the workspace Default workflow.")
		}
		printWorkflowTable(wf)
		return nil
	}
	return cli.PrintJSON(os.Stdout, map[string]any{
		"project_id":   projectRef.ID,
		"uses_default": usesDefault,
		"workflow":     wf,
	})
}

func runProjectWorkflowSet(cmd *cobra.Command, args []string) error {
	pairs, _ := cmd.Flags().GetStringArray("map")
	mapping, err := parseStatusMapping(pairs)
	if err != nil {
		return err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	projectRef, err := resolveProjectID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	// workflow_id null switches back to Default.
	body := map[string]any{"workflow_id": nil}
	if !strings.EqualFold(strings.TrimSpace(args[1]), "default") {
		wf, err := resolveWorkflow(ctx, client, args[1])
		if err != nil {
			return fmt.Errorf("resolve workflow: %w", err)
		}
		body["workflow_id"] = strVal(wf, "id")
	}
	if mapping != nil {
		body["status_mapping"] = mapping
	}
	if dryRun {
		body["dry_run"] = true
	}

	var result map[string]any
	if err := client.PostJSON(ctx, "/api/projects/"+projectRef.ID+"/workflow", body, &result); err != nil {
		if reported := reportMappingRequired(cmd, err); reported != nil {
			return reported
		}
		return fmt.Errorf("set project workflow: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		plan, _ := result["plan"].(map[string]any)
		if dryRun {
			printMappingPlan(plan)
			return nil
		}
		fmt.Fprintf(os.Stdout, "Project workflow changed; %v issue(s) moved.\n", result["updated_issues"])
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}
