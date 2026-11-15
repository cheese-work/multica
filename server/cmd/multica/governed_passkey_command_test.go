package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// CHE-5843: the command a refusal names must run. The test fills the
// placeholders, parses the command with the real `config propose` flags and
// runs it.
func TestPasskeyOnlyCommandRunsOnTheRealProposeParser(t *testing.T) {
	proposeServer(t, map[string]any{
		"/api/agents/" + proposeAgentID:             map[string]any{"instructions": "old"},
		"/api/squads/" + proposeSquadID:             map[string]any{"instructions": "old"},
		"/api/workspaces/ws-1":                      map[string]any{"context": "old"},
		"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"steps": json.RawMessage(`[{"status_key":"review","handler":{"type":"agent","id":"a"},"instructions":"b"}]`)},
	})
	input := writeInput(t, `[{"status_key":"review","handler":{"type":"agent","id":"a"},"instructions":"c"}]`)
	for _, field := range []string{"workflow.instructions", "workflow.steps", "workspace.context", "squad.instructions", "agent.instructions"} {
		t.Run(field, func(t *testing.T) {
			command, ok := decisiontx.PasskeyOnlyCommand(field, "", "")
			if !ok {
				t.Fatalf("no command for %s", field)
			}
			ids := map[string]string{"<workflow-id>": proposeWorkflowID, "<squad-id>": proposeSquadID, "<agent-id>": proposeAgentID, "<key>": "review", "<path>": input, "<out>": filepath.Join(t.TempDir(), "action.json")}
			for placeholder, value := range ids {
				command = strings.ReplaceAll(command, placeholder, value)
			}
			args := strings.Fields(strings.TrimPrefix(command, "multica config propose "))
			t.Cleanup(func() {
				configProposeCmd.Flags().VisitAll(func(flag *pflag.Flag) {
					if slice, ok := flag.Value.(pflag.SliceValue); ok {
						_ = slice.Replace(nil)
					} else {
						_ = flag.Value.Set(flag.DefValue)
					}
					flag.Changed = false
				})
			})
			if err := configProposeCmd.ParseFlags(args); err != nil {
				t.Fatalf("%q: %v", command, err)
			}
			if _, err := captureStdout(t, func() error { return runConfigPropose(configProposeCmd, configProposeCmd.Flags().Args()) }); err != nil {
				t.Fatalf("%q: %v", command, err)
			}
		})
	}
}

// Every `multica config propose` example in the built-in skill parses on the
// real flags and names --out or --dry-run, so a copied example runs.
func TestSkillConfigProposeExamplesNameOut(t *testing.T) {
	text, err := os.ReadFile("../../internal/service/builtin_skills/multica-platform/references/decisions.md")
	if err != nil {
		t.Fatal(err)
	}
	examples := 0
	for _, line := range strings.Split(string(text), "\n") {
		if !strings.HasPrefix(line, "multica config propose ") {
			continue
		}
		examples++
		args := strings.Fields(strings.TrimPrefix(line, "multica config propose "))
		flags := newConfigProposeTestCmd().Flags()
		if err := flags.Parse(args); err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		out, _ := flags.GetString("out")
		dryRun, _ := flags.GetBool("dry-run")
		if out == "" && !dryRun {
			t.Errorf("%q needs --out or --dry-run", line)
		}
	}
	if examples < 5 {
		t.Fatalf("found %d examples, want at least 5", examples)
	}
}
