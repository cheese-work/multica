package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testIssueID   = "11111111-1111-4111-8111-111111111111"
	testCommentID = "22222222-2222-4222-8222-222222222222"
)

func validAskMap() map[string]any {
	option := func(label string) map[string]any {
		return map[string]any{"label": label, "what_happens": label + " happens.", "pros": []string{"Pro"}, "cons": []string{"Con"}, "cost_or_time": "No extra time"}
	}
	return map[string]any{
		"context_eli10": "A change is ready. You decide when it ships.", "agent_did": "Ran checks.", "decision": "Ship now?",
		"stakes_if_wrong": "A bad change could reach users.", "options": []any{option("NOW"), option("LATER")},
		"recommendation": "LATER", "default_if_no_reply": "LATER", "source_issue_id": testIssueID, "source_comment_id": testCommentID,
	}
}

func TestDecodeAskMessages(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		raw    string
		want   string
	}{
		{name: "valid"},
		{name: "unsafe key", mutate: func(ask map[string]any) { ask["Bad Key!"] = 1 }, want: "ask has an unknown field"},
		{name: "wrong type", mutate: func(ask map[string]any) { ask["context_eli10"] = 5 }, want: "ask.context_eli10 must be a string"},
		{name: "options not a list", mutate: func(ask map[string]any) { ask["options"] = "x" }, want: "ask.options must be a list"},
		{name: "one option", mutate: func(ask map[string]any) { ask["options"] = ask["options"].([]any)[:1] }, want: "ask.options requires at least two structured options"},
		{name: "duplicate labels", mutate: func(ask map[string]any) { ask["options"].([]any)[1].(map[string]any)["label"] = "now" }, want: "ask.options[1].label must differ from the other labels"},
		{name: "no cons", mutate: func(ask map[string]any) { ask["options"].([]any)[0].(map[string]any)["cons"] = []string{" "} }, want: "ask.options[0].cons requires at least one nonblank item"},
		{name: "null pro", mutate: func(ask map[string]any) { ask["options"].([]any)[0].(map[string]any)["pros"] = []any{"Useful", nil} }, want: "ask.options[0].pros must be a list of strings"},
		{name: "number con", mutate: func(ask map[string]any) { ask["options"].([]any)[0].(map[string]any)["cons"] = []any{"Slow", 3} }, want: "ask.options[0].cons must be a list of strings"},
		{name: "blank cost", mutate: func(ask map[string]any) { ask["options"].([]any)[1].(map[string]any)["cost_or_time"] = "" }, want: "ask.options[1].cost_or_time is required"},
		{name: "trailing data", raw: `{} {}`, want: "ask must be one JSON object"},
		{name: "not an object", raw: `["x"]`, want: "ask must be one JSON object"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			raw := []byte(tc.raw)
			if tc.raw == "" {
				ask := validAskMap()
				if tc.mutate != nil {
					tc.mutate(ask)
				}
				raw, _ = json.Marshal(ask)
			}
			_, err := DecodeAsk(raw)
			if got := errString(err); got != tc.want {
				test.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestContentKeepsLegacyOrderWithLabels(test *testing.T) {
	raw, _ := json.Marshal(validAskMap())
	ask, err := DecodeAsk(raw)
	if err != nil {
		test.Fatal(err)
	}
	content := ask.Content("acme")
	if !strings.Contains(content, "[Comment](/acme/issues/"+testIssueID+"#comment-"+testCommentID+")") {
		test.Fatalf("ask has wrong comment link: %s", content)
	}
	last := -1
	for _, heading := range []string{"**Agent did**", "**Decision**", "**Options**\n- NOW\n- LATER", "**Recommendation**", "**Default if no reply**", "**Source**"} {
		at := strings.Index(content, heading)
		if at <= last {
			test.Fatalf("heading %q out of order in %q", heading, content)
		}
		last = at
	}
}

func TestLintAsk(test *testing.T) {
	raw, _ := json.Marshal(validAskMap())
	ask, _ := DecodeAsk(raw)
	if hints := LintAsk(ask); len(hints) != 0 {
		test.Fatalf("clean ask got hints: %q", hints)
	}
	ask.StakesIfWrong = "See [CHE-1](mention://issue/" + testIssueID + ") and https://example.com/abcdef1234567 for NOT-RUN, effaced."
	if hints := LintAsk(ask); len(hints) != 1 || !strings.Contains(hints[0], "NOT-RUN") {
		test.Fatalf("hints = %q, want only NOT-RUN (links stripped, plain words ignored)", hints)
	}
	ask.StakesIfWrong = "Reverts commit 904221117 and 1234567890abcdef."
	if hints := LintAsk(ask); len(hints) != 2 || !strings.Contains(hints[0], "904221117") || !strings.Contains(hints[1], "1234567890abcdef") {
		test.Fatalf("hints = %q, want both numeric-leading SHAs named", hints)
	}
	ask.StakesIfWrong = "Fine."
	ask.ContextELI10 = "One sentence only."
	if hints := LintAsk(ask); len(hints) != 1 || !strings.Contains(hints[0], "1 sentences") {
		test.Fatalf("hints = %q, want a sentence-count hint", hints)
	}
}
