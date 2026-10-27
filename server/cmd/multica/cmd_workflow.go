package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Workflow commands — read and edit project workflows (/api/issue-workflows).
// Reads are open to any member; the server gates writes to owners/admins.

var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Work with project workflows",
}

var workflowListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workflows in the workspace",
	RunE:  runWorkflowList,
}

var workflowGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show a workflow with its steps and instructions",
	Args:  exactArgs(1),
	RunE:  runWorkflowGet,
}

var workflowUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Replace a workflow's steps or change its run rules (owner/admin only)",
	Long: `Replace a workflow's steps with the JSON array in --steps-file. The array uses
the same shape "workflow get --output json" prints under "steps": status_key,
handler, instructions, next_status_key, back_status_key, edges (guarded exits:
[{"to","guard","timeout","on_timeout"}]; PR guards need a timeout).

Run rules change with their own flags, with or without --steps-file; a rule
flag left out keeps its value. --max-runs-per-issue N (1 to 1000) caps the runs
one issue can use before the engine blocks it (0 restores the default of 15);
--include-child-prs=false stops PR guards from reading the PRs of sub-issues.
Prints the server's readback.`,
	Args: exactArgs(1),
	RunE: runWorkflowUpdate,
}

var workflowSetInstructionsCmd = &cobra.Command{
	Use:   "set-instructions <id>",
	Short: "Change one step's instructions from a file (owner/admin only)",
	Long: `Read the workflow, replace the "instructions" of the single step whose
status_key equals --status with the contents of --file, save, and print the
readback. Every other step is sent back unchanged. Fails without sending
anything when --status matches no step or more than one.`,
	Args: exactArgs(1),
	RunE: runWorkflowSetInstructions,
}

func init() {
	workflowCmd.AddCommand(workflowListCmd, workflowGetCmd, workflowUpdateCmd, workflowSetInstructionsCmd)

	workflowListCmd.Flags().String("output", "table", "Output format: table or json")
	workflowListCmd.Flags().Bool("full-id", false, "Show full UUIDs in table output")
	workflowGetCmd.Flags().String("output", "json", "Output format: table or json")
	workflowUpdateCmd.Flags().String("steps-file", "", "JSON file holding the full steps array (required unless a rule flag is given)")
	workflowUpdateCmd.Flags().Int("max-runs-per-issue", 0, "Runs one issue can use before it is blocked, 1-1000 (0 restores the default)")
	workflowUpdateCmd.Flags().Bool("include-child-prs", true, "Whether PR guards also read the PRs of sub-issues")
	workflowUpdateCmd.Flags().String("output", "json", "Output format: table or json")
	workflowSetInstructionsCmd.Flags().String("status", "", "status_key of the step to change (required)")
	workflowSetInstructionsCmd.Flags().String("file", "", "File holding the new instructions text (required)")
	workflowSetInstructionsCmd.Flags().String("output", "json", "Output format: table or json")
}

func runWorkflowList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result map[string]any
	if err := client.GetJSON(ctx, "/api/issue-workflows", &result); err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	workflows, _ := result["workflows"].([]any)

	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, workflows)
	}

	fullID, _ := cmd.Flags().GetBool("full-id")
	rows := make([][]string, 0, len(workflows))
	for _, raw := range workflows {
		wf, _ := raw.(map[string]any)
		steps, _ := wf["steps"].([]any)
		projects, _ := wf["project_ids"].([]any)
		def := ""
		if b, _ := wf["is_default"].(bool); b {
			def = "yes"
		}
		rows = append(rows, []string{
			displayID(strVal(wf, "id"), fullID), strVal(wf, "name"), def,
			fmt.Sprint(len(steps)), fmt.Sprint(len(projects)),
		})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "NAME", "DEFAULT", "STEPS", "PROJECTS"}, rows)
	for _, raw := range workflows {
		wf, _ := raw.(map[string]any)
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

	wf, err := getWorkflow(ctx, client, args[0])
	if err != nil {
		return err
	}
	return printWorkflow(cmd, wf)
}

func runWorkflowUpdate(cmd *cobra.Command, args []string) error {
	body := map[string]any{}
	if path, _ := cmd.Flags().GetString("steps-file"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read steps file: %w", err)
		}
		var steps []any
		if err := json.Unmarshal(data, &steps); err != nil {
			return fmt.Errorf("steps file must be a JSON array of steps: %w", err)
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
	if len(body) == 0 {
		return errors.New("give --steps-file, --max-runs-per-issue or --include-child-prs")
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	wf, err := patchWorkflow(ctx, client, args[0], body)
	if err != nil {
		return err
	}
	return printWorkflow(cmd, wf)
}

func runWorkflowSetInstructions(cmd *cobra.Command, args []string) error {
	status, _ := cmd.Flags().GetString("status")
	path, _ := cmd.Flags().GetString("file")
	if status == "" || path == "" {
		return errors.New("--status and --file are required")
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

	wf, err := getWorkflow(ctx, client, args[0])
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
	match["instructions"] = string(text)

	// expected_version makes the server refuse the write if someone edited the
	// workflow after the read above, instead of overwriting their change.
	body := map[string]any{"steps": steps}
	if v := strVal(wf, "version"); v != "" {
		body["expected_version"] = v
	}
	wf, err = patchWorkflow(ctx, client, args[0], body)
	if err != nil {
		return err
	}
	return printWorkflow(cmd, wf)
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
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("update workflow: only a workspace owner or admin can edit workflows (server said: %s)", httpErr.Body)
		}
		return nil, fmt.Errorf("update workflow: %w", err)
	}
	return wf, nil
}

func printWorkflow(cmd *cobra.Command, wf map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, wf)
	}
	printWorkflowTable(wf)
	return nil
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
	fmt.Printf("max runs per issue: %s, include child PRs: %t\n", maxRuns, include)
}
