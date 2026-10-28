package decisiontx

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	uuidCHE2126 = "01a17cda-b791-7da5-94fc-06fdec20e4ed"
	splitMsg    = "ask.decision asks more than one question; split into one request per question"
	inlineMsg   = " has an inline numbered list; put each item on its own line as a Markdown list"
	bareIDMsg   = " has a bare ID; link it as [what it is](link)"
	agentDidMsg = "ask.agent_did must say in at least one plain sentence what the agent did; an ID alone is not enough"
)

// decodeWith builds the base fixture, applies one mutation and decodes it.
func decodeWith(mutate func(map[string]any)) error {
	ask := validAskMap()
	mutate(ask)
	raw, _ := json.Marshal(ask)
	_, err := DecodeAsk(raw)
	return err
}

func optionOf(ask map[string]any, index int) map[string]any {
	return ask["options"].([]any)[index].(map[string]any)
}

// proseFields lists every visible prose field with a setter, by indexed path.
func proseFields() []struct {
	path string
	set  func(map[string]any, string)
} {
	type field = struct {
		path string
		set  func(map[string]any, string)
	}
	fields := []field{
		{"ask.context_eli10", func(ask map[string]any, text string) { ask["context_eli10"] = text }},
		{"ask.decision", func(ask map[string]any, text string) { ask["decision"] = text }},
		{"ask.stakes_if_wrong", func(ask map[string]any, text string) { ask["stakes_if_wrong"] = text }},
		{"ask.recommendation", func(ask map[string]any, text string) { ask["recommendation"] = text }},
		{"ask.default_if_no_reply", func(ask map[string]any, text string) { ask["default_if_no_reply"] = text }},
	}
	for index := 0; index < 2; index++ {
		for _, name := range []string{"label", "what_happens", "cost_or_time"} {
			fields = append(fields, field{fmt.Sprintf("ask.options[%d].%s", index, name), func(ask map[string]any, text string) { optionOf(ask, index)[name] = text }})
		}
		for _, name := range []string{"pros", "cons"} {
			fields = append(fields, field{fmt.Sprintf("ask.options[%d].%s[1]", index, name), func(ask map[string]any, text string) {
				optionOf(ask, index)[name] = []string{"Fine", text}
			}})
		}
	}
	return fields
}

// Test 0: guard. Every negative fixture below is this base with one field changed.
func TestValidAsksPassAllRules(test *testing.T) {
	if err := decodeWith(func(map[string]any) {}); err != nil {
		test.Fatalf("validAskMap() refused: %v", err)
	}
	raw := `{"context_eli10":"This feature adds three things to DMs on web and phone: /archive to put a chat away, /statistic to see chat numbers, and a back button that returns to the previous DM. The code is ready in 3 draft pull requests ([CHE-1755](mention://issue/01a13fcc-9e0d-7d27-aa33-aad915db21b7)). Some proof is still missing: 24 screenshots, a check on a real Android phone, and one automated test. You decide whether agents collect that proof first, or whether you accept the work without it.",
"decision":"Collect the missing proof before merge, or accept the three pull requests without it?",
"stakes_if_wrong":"If the missing proof hides a real bug, archive or the back button can misbehave on Android after merge, and you find it instead of a test.",
"options":[{"label":"KEEP GATES","what_happens":"Agents take the 24 screenshots, run the Android phone check and add the missing test. Reviewers then check the new versions again. You merge after that.","pros":["Bugs show up before merge","Nothing is accepted without proof"],"cons":["Slower","Needs a booked Android phone"],"cost_or_time":"About 1 to 2 days of agent and device time (estimate)"},
{"label":"WAIVE GAPS","what_happens":"Agents update the 3 pull requests with the latest code and send them for a fresh review without the missing proof. You merge after that review.","pros":["Faster to merge"],"cons":["Android layout or back-button bugs can show up only after merge"],"cost_or_time":"A few hours of agent time (estimate)"}],
"recommendation":"KEEP GATES. Get the missing proof first; the current checks do not cover the Android screens.",
"default_if_no_reply":"KEEP GATES. The feature stays blocked and the three pull requests stay draft.",
"agent_did":"Stale sweep on 27 Oct 2026: PRs 340, 343 and 347 are OPEN and draft at heads 554b9fc4244aaf9fd0bd464f5840975f5f17f086, 638c4502097d49525e9ee5260d173c873698da36 and b75b8a325b3efdd37f3cd233b84b13d7fbde7c5d. No gate was waived.",
"source_issue_id":"` + testIssueID + `","source_comment_id":"` + testCommentID + `"}`
	if _, err := DecodeAsk([]byte(raw)); err != nil {
		test.Fatalf("CHE-1755 draft refused: %v", err)
	}
}

// Tests 1 and 2: the verbatim CHE-2204 and CHE-2126 asks.
func TestValidateRefusesMultiQuestionDecision(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"CHE-2204", func(ask map[string]any) {
			ask["decision"] = "Answer 1 to 5. 1. Run budgets (spend): close CHE-2164 (its PR #432 merged), and for CHE-1192 choose 1 raise the budget by 3 runs or 2 merge sub2api #71 on Luna's PASS. 2. CHE-1532: add the 3 multica-dotfiles secrets MULTICA_TOKEN, MULTICA_SERVER_URL, MULTICA_WORKSPACE_ID (5 min). 3. CHE-2007: run the packet on X99 with attachment 01a17b40, dry run then --apply (10 min). 4. CHE-2166: lane or waive for the C00 read-back. 5. Overnight queue items 1 to 5 from the brief (CHE-2127 fix, CHE-2042 review, CHE-2109 test fix, C00 read-back after your deploy approval, stale-lane triage)."
		}},
		{"CHE-2126", func(ask map[string]any) {
			ask["decision"] = "CHE-2126 plan-rev-6 decisions 5, 6, 7, 9 (decisions 1-4, 8 already recorded). 5: OAuth Connected accounts in Phase 2 or when a second human reviewer joins. 6: App owner subscribes c00-multicado to the three events."
			second := optionOf(ask, 1)
			second["label"], second["what_happens"] = "Approve with changes", "reply with numbered answers for 5, 6, 7, 9"
		}},
		{"answer per number option", func(ask map[string]any) { optionOf(ask, 0)["label"] = "Answer per number" }},
		{"answer each", func(ask map[string]any) { ask["decision"] = "Answer each of the open points." }},
		{"two questions", func(ask map[string]any) { ask["decision"] = "Ship now? Or wait?" }},
		{"decisions 5 and 6", func(ask map[string]any) { ask["decision"] = "Settle decisions 5 and 6." }},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			err := decodeWith(tc.mutate)
			if got := errString(err); got != splitMsg {
				test.Fatalf("error = %q, want %q", got, splitMsg)
			}
		})
	}
	for _, text := range []string{"Ship now?", "Pick the window for the 1 to 5 day rollout.", "Which of the answers do you want?"} {
		if err := decodeWith(func(ask map[string]any) { ask["decision"] = text }); err != nil {
			test.Fatalf("%q refused: %v", text, err)
		}
	}
}

// Test 3.
func TestValidateRefusesInlineNumberedList(test *testing.T) {
	for _, field := range proseFields() {
		for _, inline := range []string{"1. first step 2. second step", "Do this: 1) first step 2) second step"} {
			test.Run(field.path, func(test *testing.T) {
				err := decodeWith(func(ask map[string]any) { field.set(ask, inline) })
				if want := field.path + inlineMsg; errString(err) != want {
					test.Fatalf("error = %q, want %q", errString(err), want)
				}
			})
		}
		if err := decodeWith(func(ask map[string]any) { field.set(ask, "Steps:\n1. first step\n2. second step") }); err != nil {
			test.Fatalf("%s Markdown list refused: %v", field.path, err)
		}
	}
	if err := decodeWith(func(ask map[string]any) { ask["recommendation"] = "Ready in 1.5 hours, version 2.0 later." }); err != nil {
		test.Fatalf("decimals refused: %v", err)
	}
}

// Test 4.
func TestValidateRefusesBareUUID(test *testing.T) {
	comment := "[the ranked list](/issue/" + testIssueID + "#comment-" + uuidCHE2126 + ")"
	cases := []struct {
		path, text string
	}{
		{"ask.decision", "issue comment " + uuidCHE2126},
		{"ask.options[0].label", uuidCHE2126},
		{"ask.options[1].cost_or_time", "Wait for " + uuidCHE2126},
		{"ask.options[0].pros[1]", "See " + uuidCHE2126 + " first"},
		{"ask.context_eli10", "[" + uuidCHE2126 + "](/issue/" + testIssueID + ")"},
		{"ask.recommendation", "https://example.com/issue/" + testIssueID + "#comment-" + uuidCHE2126},
	}
	for _, tc := range cases {
		test.Run(tc.path, func(test *testing.T) {
			for _, field := range proseFields() {
				if field.path == tc.path {
					err := decodeWith(func(ask map[string]any) { field.set(ask, tc.text) })
					if want := tc.path + bareIDMsg; errString(err) != want {
						test.Fatalf("error = %q, want %q", errString(err), want)
					}
					return
				}
			}
			test.Fatalf("no prose field %s", tc.path)
		})
	}
	for _, field := range proseFields() {
		for _, text := range []string{"Read " + comment, "[CHE-1755](mention://issue/" + testIssueID + ")"} {
			if err := decodeWith(func(ask map[string]any) { field.set(ask, text) }); err != nil {
				test.Fatalf("%s link refused: %v", field.path, err)
			}
		}
	}
}

// Test 5.
func TestValidateRefusesIDOnlyAgentDid(test *testing.T) {
	for _, text := range []string{"75eed3bd-33c6-42f5-b612-cf0f03b8879a", "Done.", "[x](/issue/" + testIssueID + ") 904221117", "Ran checks."} {
		err := decodeWith(func(ask map[string]any) { ask["agent_did"] = text })
		if got := errString(err); got != agentDidMsg {
			test.Fatalf("agent_did %q: error = %q, want %q", text, got, agentDidMsg)
		}
	}
	if err := decodeWith(func(ask map[string]any) {
		ask["agent_did"] = "Ran all requested checks on " + strings.Repeat("a", 7) + "."
	}); err != nil {
		test.Fatalf("sentence refused: %v", err)
	}
}

// Only a real Markdown link destination is exempt from rule 3 (Sol review F1).
func TestBareUUIDMasksOnlyRealLinkDestinations(test *testing.T) {
	cases := []struct {
		name, text string
		refused    bool
	}{
		{"unmatched closing bracket", "Read ](" + uuidCHE2126 + ") before deciding.", true},
		{"inline code span", "Read `[proof](" + uuidCHE2126 + ")` before deciding.", true},
		{"double-backtick code span", "Read ``[proof](" + uuidCHE2126 + ")`` first.", true},
		{"id after a link", "[proof](https://example.com/a) then " + uuidCHE2126, true},
		{"id in the label of a link", "[" + uuidCHE2126 + "](https://example.com/a)", true},
		{"balanced parentheses in destination", "Read [proof](https://example.com/doc_(draft)/" + uuidCHE2126 + ") first.", false},
		{"escaped parenthesis in destination", `Read [proof](https://example.com/a\)b/` + uuidCHE2126 + ") first.", false},
		{"destination with title", `Read [proof](https://example.com/` + uuidCHE2126 + ` "the proof") first.`, false},
		{"brackets in label", "Read [proof [v2]](https://example.com/" + uuidCHE2126 + ") first.", false},
		{"two links", "[a](https://example.com/" + uuidCHE2126 + ") and [b](mention://issue/" + uuidCHE2126 + ")", false},
		{"code span elsewhere, real link kept", "Run `go test` and read [proof](https://example.com/" + uuidCHE2126 + ").", false},
		{"unterminated destination", "Read [proof](https://example.com/" + uuidCHE2126, true},
		{"escaped opening bracket", `Read \[proof](` + uuidCHE2126 + ") before deciding.", true},
		{"escaped opening bracket with web link", `Read \[proof](https://example.com/` + uuidCHE2126 + ") before deciding.", true},
		{"escaped backslash before a real link", `Read \\[proof](https://example.com/` + uuidCHE2126 + ") first.", false},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			err := decodeWith(func(ask map[string]any) { ask["context_eli10"] = tc.text })
			if got := errString(err); tc.refused && got != "ask.context_eli10"+bareIDMsg {
				test.Fatalf("error = %q, want bare-ID refusal for %q", got, tc.text)
			} else if !tc.refused && err != nil {
				test.Fatalf("valid text refused: %v", err)
			}
		})
	}
}
