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

// lintResult is one workflow's result from /api/issue-workflows/.../lint.
type lintResult struct {
	WorkflowID string        `json:"workflow_id"`
	Workflow   string        `json:"workflow"`
	Findings   []lintFinding `json:"findings"`
	Errors     int           `json:"errors"`
	Warnings   int           `json:"warnings"`
	Infos      int           `json:"infos"`
}

type lintFinding struct {
	Rule        string `json:"rule"`
	Level       string `json:"level"`
	Step        string `json:"step"`
	Message     string `json:"message"`
	Hint        string `json:"hint"`
	Waived      bool   `json:"waived"`
	WaiveReason string `json:"waive_reason"`
}

func runWorkflowLint(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	if all == (len(args) == 1) {
		return errors.New("give a workflow id or --all, not both and not neither")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var results []lintResult
	var raw any
	if all {
		var resp struct {
			Workflows []lintResult `json:"workflows"`
		}
		if err := client.GetJSON(ctx, "/api/issue-workflows/lint", &resp); err != nil {
			return fmt.Errorf("lint workflows: %w", err)
		}
		results, raw = resp.Workflows, resp.Workflows
	} else {
		var one lintResult
		if err := client.GetJSON(ctx, "/api/issue-workflows/"+args[0]+"/lint", &one); err != nil {
			return fmt.Errorf("lint workflow: %w", err)
		}
		results, raw = []lintResult{one}, one
	}

	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		if err := cli.PrintJSON(os.Stdout, raw); err != nil {
			return err
		}
	} else {
		printLintTable(results)
	}

	errs := 0
	for _, r := range results {
		errs += r.Errors
	}
	if errs > 0 {
		return fmt.Errorf("workflow lint found %d unwaived error(s)", errs)
	}
	return nil
}

func printLintTable(results []lintResult) {
	var rows [][]string
	var errs, warns, infos int
	for _, r := range results {
		errs, warns, infos = errs+r.Errors, warns+r.Warnings, infos+r.Infos
		for _, f := range r.Findings {
			msg := f.Message
			if f.Waived {
				msg += " [waived: " + f.WaiveReason + "]"
			}
			rows = append(rows, []string{r.Workflow, f.Rule, f.Level, f.Step, msg})
		}
	}
	if len(rows) > 0 {
		cli.PrintTable(os.Stdout, []string{"WORKFLOW", "RULE", "LEVEL", "STEP", "MESSAGE"}, rows)
	}
	fmt.Printf("%d workflow(s): %d error(s), %d warning(s), %d info (unwaived)\n", len(results), errs, warns, infos)
}

// lintRefusal formats the 422 a save gets when it has unwaived lint errors:
// each error with its fix hint, then how to override. "" when body is not that
// refusal.
func lintRefusal(body string) string {
	var refusal struct {
		Code     string        `json:"code"`
		Findings []lintFinding `json:"findings"`
	}
	if json.Unmarshal([]byte(body), &refusal) != nil || refusal.Code != "workflow_lint_errors" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "workflow not saved: %d lint error(s)\n", len(refusal.Findings))
	for _, f := range refusal.Findings {
		fmt.Fprintf(&b, "  %s step %q: %s\n    fix: %s\n", f.Rule, f.Step, f.Message, f.Hint)
	}
	b.WriteString("Fix them, or save again with --lint-override \"<reason>\".")
	return b.String()
}

// printSaveLintFindings prints the findings a save returned (warnings, info and
// waived errors) to stderr, so stdout stays the workflow readback.
func printSaveLintFindings(wf map[string]any) {
	raw, _ := wf["lint_findings"].([]any)
	for _, r := range raw {
		f, _ := r.(map[string]any)
		fmt.Fprintf(os.Stderr, "%s %s step %q: %s\n", strVal(f, "level"), strVal(f, "rule"), strVal(f, "step"), strVal(f, "message"))
	}
}
