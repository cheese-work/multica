package decisiontx

import (
	"encoding/json"
	"errors"
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
		"context_eli10": "A change is ready. You decide when it ships.", "agent_did": "Ran all requested checks.", "decision": "Ship now?",
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

func decodedAsk(test *testing.T) Ask {
	test.Helper()
	raw, _ := json.Marshal(validAskMap())
	ask, err := DecodeAsk(raw)
	if err != nil {
		test.Fatal(err)
	}
	return ask
}

func TestAskContentRenderOrder(test *testing.T) {
	content := decodedAsk(test).Content()
	last := -1
	for _, marker := range []string{"**Context**", "**Decision**", "**What is at stake**", "**Options**", "**Recommendation**", "**Default if no reply**", "<details>", "<summary>Details</summary>", "Ran all requested checks.", "</details>", "Investigate in issue"} {
		at := strings.Index(content, marker)
		if at <= last {
			test.Fatalf("%q missing or out of order in %q", marker, content)
		}
		last = at
	}
	if strings.Count(content, "Investigate in issue") != 1 || strings.Contains(content, "**Agent did**\nRan all requested checks.\n\n**Decision**") {
		test.Fatalf("investigate link must appear once, after details: %q", content)
	}
	for _, link := range []string{"mention://issue/" + testIssueID, "/issue/" + testIssueID + "#comment-" + testCommentID} {
		if strings.Count(content, link) != 1 || strings.Index(content, link) < strings.Index(content, "</details>") {
			test.Fatalf("link %q must appear once, after details: %q", link, content)
		}
	}
}

func TestValidateAcceptsKnownAskExamples(test *testing.T) {
	che1755 := Ask{
		ContextELI10:  "This feature adds three things to DMs on web and phone: /archive to put a chat away, /statistic to see chat numbers, and a back button that returns to the previous DM. The code is ready in 3 draft pull requests ([CHE-1755](mention://issue/01a13fcc-9e0d-7d27-aa33-aad915db21b7)). Some proof is still missing: 24 screenshots, a check on a real Android phone, and one automated test. You decide whether agents collect that proof first, or whether you accept the work without it.",
		Decision:      "Collect the missing proof before merge, or accept the three pull requests without it?",
		StakesIfWrong: "If the missing proof hides a real bug, archive or the back button can misbehave on Android after merge, and you find it instead of a test.",
		Options: []AskOption{
			{Label: "KEEP GATES", WhatHappens: "Agents take the 24 screenshots, run the Android phone check and add the missing test. Reviewers then check the new versions again. You merge after that.", Pros: []string{"Bugs show up before merge", "Nothing is accepted without proof"}, Cons: []string{"Slower", "Needs a booked Android phone"}, CostOrTime: "About 1 to 2 days of agent and device time (estimate)"},
			{Label: "WAIVE GAPS", WhatHappens: "Agents update the 3 pull requests with the latest code and send them for a fresh review without the missing proof. You merge after that review.", Pros: []string{"Faster to merge"}, Cons: []string{"Android layout or back-button bugs can show up only after merge"}, CostOrTime: "A few hours of agent time (estimate)"},
		},
		Recommendation:   "KEEP GATES. Get the missing proof first; the current checks do not cover the Android screens.",
		DefaultIfNoReply: "KEEP GATES. The feature stays blocked and the three pull requests stay draft.",
		AgentDid:         "Stale sweep on 27 Oct 2026: PRs 340, 343 and 347 are OPEN and draft at heads 554b9fc4244aaf9fd0bd464f5840975f5f17f086, 638c4502097d49525e9ee5260d173c873698da36 and b75b8a325b3efdd37f3cd233b84b13d7fbde7c5d. Gaps: L2 24 PNGs NOT-CAPTURED, full-ChatWindow evidence, historical-red, chat-thread-group-pinned P2; L4 Android newer-DM primary-action and stale-pin P2, route-level test, live UI evidence. No gate was waived.",
		SourceIssueID:    "01a13fcc-9e0d-7d27-aa33-aad915db21b7",
		SourceCommentID:  "01a16882-440f-72df-8ccf-ca9b992c30aa",
	}
	examples := []struct {
		name string
		ask  Ask
	}{
		{name: "Go fixture and card golden", ask: decodedAsk(test)},
		{name: "CHE-1755 draft", ask: che1755},
	}
	for _, example := range examples {
		test.Run(example.name, func(test *testing.T) {
			if err := example.ask.Validate(); err != nil {
				test.Fatalf("valid example rejected: %v", err)
			}
		})
	}
}

func TestValidateRefusesMultiQuestionDecision_CHE2204(test *testing.T) {
	ask := decodedAsk(test)
	ask.Decision = "Answer 1 to 5. 1. Run budgets (spend): close CHE-2164 (its PR #432 merged), and for CHE-1192 choose 1 raise the budget by 3 runs or 2 merge sub2api #71 on Luna's PASS. 2. CHE-1532: add the 3 multica-dotfiles secrets MULTICA_TOKEN, MULTICA_SERVER_URL, MULTICA_WORKSPACE_ID (5 min). 3. CHE-2007: run the packet on X99 with attachment 01a17b40, dry run then --apply (10 min). 4. CHE-2166: lane or waive for the C00 read-back. 5. Overnight queue items 1 to 5 from the brief (CHE-2127 fix, CHE-2042 review, CHE-2109 test fix, C00 read-back after your deploy approval, stale-lane triage)."
	assertAskError(test, ask.Validate(), "ask.decision", "split into one request per question")
}

func TestValidateRefusesMultiQuestionDecision_CHE2126(test *testing.T) {
	decision := "CHE-2126 plan-rev-6 decisions 5, 6, 7, 9 (decisions 1-4, 8 already recorded). 5: OAuth Connected accounts in Phase 2 or when a second human reviewer joins. 6: App owner subscribes c00-multicado to the three events when intake (CHE-2207, CHE-2208) is deployed. 7: interaction limit (collaborators only, up to 6 months) on public cheese-work/multica. 9: read-only check of App page Advanced > Recent Deliveries near 15:01:05 ICT, 26 Oct (event, response code, subscription history). None blocks stage-1 code; 6 blocks any live intake."
	test.Run("decision", func(test *testing.T) {
		ask := decodedAsk(test)
		ask.Decision = decision
		assertAskError(test, ask.Validate(), "ask.decision", "split into one request per question")
	})
	test.Run("option asks for numbered answers", func(test *testing.T) {
		ask := decodedAsk(test)
		ask.Options[1].WhatHappens = "reply with numbered answers for 5, 6, 7, 9"
		assertAskError(test, ask.Validate(), "ask.decision", "split into one request per question")
	})
}

func TestValidateRefusesMultiQuestionPatterns(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Ask)
	}{
		{name: "answer 1-5", mutate: func(ask *Ask) { ask.Decision = "Answer 1-5." }},
		{name: "answer per number", mutate: func(ask *Ask) { ask.Decision = "Answer per number." }},
		{name: "answer each", mutate: func(ask *Ask) { ask.Decision = "Answer each." }},
		{name: "answer all", mutate: func(ask *Ask) { ask.Decision = "Answer all." }},
		{name: "multiple numbered decisions", mutate: func(ask *Ask) { ask.Decision = "Decisions 5, 6, and 7." }},
		{name: "multiple question marks", mutate: func(ask *Ask) { ask.Decision = "Approve the first? Approve the second?" }},
		{name: "numbered questions", mutate: func(ask *Ask) { ask.Decision = "1. Approve the first?\n2. Approve the second?" }},
		{name: "option label asks for numbered answers", mutate: func(ask *Ask) { ask.Options[0].Label = "Reply with numbered answers" }},
		{name: "option behavior asks per number", mutate: func(ask *Ask) { ask.Options[0].WhatHappens = "Answer per number." }},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := decodedAsk(test)
			tc.mutate(&ask)
			assertAskError(test, ask.Validate(), "ask.decision", "split into one request per question")
		})
	}
}

func TestValidateRefusesInlineNumberedList(test *testing.T) {
	fields := []struct {
		path string
		set  func(*Ask, string)
	}{
		{path: "ask.context_eli10", set: func(ask *Ask, value string) { ask.ContextELI10 = value }},
		{path: "ask.decision", set: func(ask *Ask, value string) { ask.Decision = value }},
		{path: "ask.stakes_if_wrong", set: func(ask *Ask, value string) { ask.StakesIfWrong = value }},
		{path: "ask.recommendation", set: func(ask *Ask, value string) { ask.Recommendation = value }},
		{path: "ask.default_if_no_reply", set: func(ask *Ask, value string) { ask.DefaultIfNoReply = value }},
		{path: "ask.options[0].label", set: func(ask *Ask, value string) { ask.Options[0].Label = value }},
		{path: "ask.options[0].what_happens", set: func(ask *Ask, value string) { ask.Options[0].WhatHappens = value }},
		{path: "ask.options[0].pros[0]", set: func(ask *Ask, value string) { ask.Options[0].Pros[0] = value }},
		{path: "ask.options[1].cons[0]", set: func(ask *Ask, value string) { ask.Options[1].Cons[0] = value }},
		{path: "ask.options[1].cost_or_time", set: func(ask *Ask, value string) { ask.Options[1].CostOrTime = value }},
	}
	for _, field := range fields {
		test.Run(field.path, func(test *testing.T) {
			ask := decodedAsk(test)
			field.set(&ask, "1. first step 2. second step")
			assertAskError(test, ask.Validate(), field.path, "has an inline numbered list; put each item on its own line as a Markdown list")
		})
		test.Run(field.path+" markdown list", func(test *testing.T) {
			ask := decodedAsk(test)
			field.set(&ask, "1. first step\n2. second step")
			if err := ask.Validate(); err != nil {
				test.Fatalf("Markdown list rejected: %v", err)
			}
		})
	}
}

func TestValidateRefusesBareUUID(test *testing.T) {
	identifier := "01a17cda-b791-7da5-94fc-06fdec20e4ed"
	fields := []struct {
		path  string
		value string
		set   func(*Ask, string)
	}{
		{path: "ask.context_eli10", value: "See [" + identifier + "](https://example.com/comment)", set: func(ask *Ask, value string) { ask.ContextELI10 = value }},
		{path: "ask.decision", value: "Issue comment " + identifier, set: func(ask *Ask, value string) { ask.Decision = value }},
		{path: "ask.stakes_if_wrong", value: "Reference " + identifier, set: func(ask *Ask, value string) { ask.StakesIfWrong = value }},
		{path: "ask.recommendation", value: "https://example.com/comments/" + identifier, set: func(ask *Ask, value string) { ask.Recommendation = value }},
		{path: "ask.default_if_no_reply", value: "Use " + identifier, set: func(ask *Ask, value string) { ask.DefaultIfNoReply = value }},
		{path: "ask.options[0].label", value: identifier, set: func(ask *Ask, value string) { ask.Options[0].Label = value }},
		{path: "ask.options[0].what_happens", value: "Wait for " + identifier, set: func(ask *Ask, value string) { ask.Options[0].WhatHappens = value }},
		{path: "ask.options[0].pros[1]", value: "From " + identifier, set: func(ask *Ask, value string) { ask.Options[0].Pros = append(ask.Options[0].Pros, value) }},
		{path: "ask.options[1].cons[0]", value: "Avoid " + identifier, set: func(ask *Ask, value string) { ask.Options[1].Cons[0] = value }},
		{path: "ask.options[1].cost_or_time", value: "Wait for " + identifier, set: func(ask *Ask, value string) { ask.Options[1].CostOrTime = value }},
	}
	for _, field := range fields {
		test.Run(field.path, func(test *testing.T) {
			ask := decodedAsk(test)
			field.set(&ask, field.value)
			assertAskError(test, ask.Validate(), field.path, "has a bare ID; link it as [what it is](link)")
		})
	}
	test.Run("Markdown destinations are exempt", func(test *testing.T) {
		ask := decodedAsk(test)
		ask.ContextELI10 = "Read [the ranked list](https://example.com/issues/" + identifier + ")."
		ask.Recommendation = "Use [CHE-1755](mention://issue/" + testIssueID + ")."
		if err := ask.Validate(); err != nil {
			test.Fatalf("Markdown link destination rejected: %v", err)
		}
	})
}

func TestValidateRefusesIDOnlyAgentDid(test *testing.T) {
	for _, value := range []string{"75eed3bd-33c6-42f5-b612-cf0f03b8879a", "Done.", "Only three words."} {
		test.Run(value, func(test *testing.T) {
			ask := decodedAsk(test)
			ask.AgentDid = value
			assertAskError(test, ask.Validate(), "ask.agent_did", "must say in at least one plain sentence what the agent did; an ID alone is not enough")
		})
	}
}

func assertAskError(test *testing.T, err error, field, message string) {
	test.Helper()
	var askError *AskError
	if !errors.As(err, &askError) {
		test.Fatalf("error = %v, want AskError for %s", err, field)
	}
	if askError.Field != field || !strings.Contains(askError.Error(), message) {
		test.Fatalf("error = %q, want field %q and message containing %q", askError.Error(), field, message)
	}
}

func TestAskContentKeepsOptionFields(test *testing.T) {
	raw := validAskMap()
	raw["options"] = []any{
		map[string]any{"label": "NOW", "what_happens": "Ships today.", "pros": []string{"Users get it today", "No backlog"}, "cons": []string{"Less testing"}, "cost_or_time": "Ready in 1 hour"},
		map[string]any{"label": "LATER", "what_happens": "Ships next week.", "pros": []string{"Full test pass"}, "cons": []string{"Users wait", "Backlog grows"}, "cost_or_time": "Ready in 5 days"},
	}
	encoded, _ := json.Marshal(raw)
	ask, err := DecodeAsk(encoded)
	if err != nil {
		test.Fatal(err)
	}
	content := ask.Content()
	options := content[strings.Index(content, "**Options**"):strings.Index(content, "**Recommendation**")]
	want := "**Options**\n" +
		"- **NOW**: Ships today.\n  - Pro: Users get it today\n  - Pro: No backlog\n  - Con: Less testing\n  - Cost or time: Ready in 1 hour\n" +
		"- **LATER**: Ships next week.\n  - Pro: Full test pass\n  - Con: Users wait\n  - Con: Backlog grows\n  - Cost or time: Ready in 5 days\n\n"
	if options != want {
		test.Fatalf("options section = %q, want %q", options, want)
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
