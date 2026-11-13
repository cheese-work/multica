package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// Every action type registers a human template (B v9 S1, CHE-5408): a title, one
// summary line and, from the action's preview, a before → after diff.

func TestEveryActionTypeHasATemplate(test *testing.T) {
	checked := 0
	for _, definition := range decisionaction.Definitions() {
		if strings.HasPrefix(definition.Type, "test.") {
			continue // registered by a test, not by the product
		}
		checked++
		template := definition.Template
		switch {
		case strings.TrimSpace(template.Title) == "":
			test.Errorf("%s has no template title", definition.Type)
		case strings.TrimSpace(template.Summary) == "":
			test.Errorf("%s has no template summary", definition.Type)
		case strings.ContainsAny(template.Title+template.Summary, "\r\n"):
			test.Errorf("%s template must be single-line text", definition.Type)
		}
	}
	if checked < 16 {
		test.Fatalf("checked %d product action types, want at least the 16 registered today", checked)
	}
}

func TestProposalAskLeadsWithTheTemplateAndShowsBeforeAfter(test *testing.T) {
	useProposalGuard(test)
	fixture := newProposalFixture(test)
	body := fixture.proposal(test)
	body["action_type"], body["params"] = "agent.pause", map[string]any{"agent_id": fixture.agentID}
	view := fixture.propose(test, body, http.StatusCreated)
	definition, _ := decisionaction.Lookup("agent.pause", 1)
	for _, want := range []string{definition.Template.Title, definition.Template.Summary, "**Before → after**", "Scheduling: Running → Paused"} {
		if want == "" || !strings.Contains(view.Ask, want) {
			test.Fatalf("ask lacks %q:\n%s", want, view.Ask)
		}
	}
	if strings.Index(view.Ask, definition.Template.Title) > strings.Index(view.Ask, "**Parameters**") {
		test.Fatalf("template must come before the raw parameters:\n%s", view.Ask)
	}
}

// The ask never names the proposing agent, and a value cannot close its fence.
func TestProposalChangeRowsHideTheProposerAndCannotEscapeTheFence(test *testing.T) {
	block := renderRowsBlock([]ActionPreviewField{
		{"Agent", "Proposer Bot", "Proposer Bot"},
		{"Name", "Proposer Bot", "new ``` name"},
		{"Same", "x", "x"},
	}, "Proposer Bot")
	if strings.Contains(block, "Proposer Bot") || strings.Contains(block, "Same") {
		test.Fatalf("block names the proposer or lists an unchanged row:\n%s", block)
	}
	if !strings.HasPrefix(block, "````text\n") || !strings.HasSuffix(block, "\n````") {
		test.Fatalf("fence is not longer than the quoted backticks:\n%s", block)
	}
}

// A config action's diff in the ask is the diff stored in its preview: one
// source, so the ask can never show a change the approval does not sign.
func TestProposalAskDiffIsTheStoredPreviewDiff(test *testing.T) {
	useProposalGuard(test)
	fixture := newProposalFixture(test)
	target := instructionSetTargets()[1] // workspace.context.set
	target.set(test, testWorkspaceID, governedText)
	setContextProposerGrant(test, fixture.agentID, true)
	body := fixture.proposal(test)
	params := target.params(testWorkspaceID, governedText, editedText)
	params["base_sha256"], params["after_sha256"] = decisionaction.TextDigest(governedText), decisionaction.TextDigest(editedText)
	body["action_type"], body["params"] = target.actionType, params
	view := fixture.propose(test, body, http.StatusCreated)

	var stored string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE requested_by_agent_id = $1`, fixture.agentID).Scan(&stored)
	var preconditions struct {
		Preview struct {
			Diff []decisiontx.DiffHunk `json:"diff"`
		} `json:"preview"`
	}
	if err := json.Unmarshal([]byte(stored), &preconditions); err != nil || len(preconditions.Preview.Diff) == 0 {
		test.Fatalf("stored preview has no diff (err %v): %s", err, stored)
	}
	if !strings.Contains(view.Ask, "**Before → after**") || !strings.Contains(view.Ask, "```diff\n") {
		test.Fatalf("ask has no diff block:\n%s", view.Ask)
	}
	for _, hunk := range preconditions.Preview.Diff {
		for _, line := range hunk.Lines {
			if !strings.Contains(view.Ask, "\n"+line+"\n") {
				test.Fatalf("ask lacks preview diff line %q:\n%s", line, view.Ask)
			}
		}
	}
}
