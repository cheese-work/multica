package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
)

const testAssigneeID = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

// serverActionAsk decodes a valid ask whose first option has the given typed fields.
func serverActionAsk(test *testing.T, fields map[string]any) (Ask, error) {
	test.Helper()
	raw := validAskMap()
	option := raw["options"].([]any)[0].(map[string]any)
	for key, value := range fields {
		option[key] = value
	}
	body, _ := json.Marshal(raw)
	return DecodeAsk(body)
}

func statusAction(status string) map[string]any {
	return map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.status.set", "params": map[string]any{"status": status}}}
}

func TestServerActionOptionIsStoredNormalized(test *testing.T) {
	ask, err := serverActionAsk(test, statusAction(" In_Review "))
	if err != nil {
		test.Fatalf("decode: %v", err)
	}
	option := ask.Options[0]
	if option.AfterChoice != AfterServerAction || option.Action == nil || option.Action.Type != ActionStatusSet || option.Action.Params.Status != "in_review" {
		test.Fatalf("option = %+v action = %+v, want server_action issue.status.set in_review", option, option.Action)
	}
	assign, err := serverActionAsk(test, map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.assign", "params": map[string]any{"assignee_type": "Agent", "assignee_id": strings.ToUpper(testAssigneeID)}}})
	if err != nil {
		test.Fatalf("decode assign: %v", err)
	}
	if got := assign.Options[0].Action.Params; got.AssigneeType != "agent" || got.AssigneeID != testAssigneeID {
		test.Fatalf("assign params = %+v, want agent and the lower-case id", got)
	}
}

func TestServerActionEffectLine(test *testing.T) {
	ask, _ := serverActionAsk(test, statusAction("done"))
	ask.Options[0].WriteEffectLine("CHE-123", "")
	ask.Options[0].WriteEffectLine("CHE-123", "") // idempotent
	want := "On pick: the server moves CHE-123 to done as you."
	if got := ask.Options[0].WhatHappens; strings.Count(got, want) != 1 || !strings.HasPrefix(got, "NOW") {
		test.Fatalf("what_happens = %q, want the original text and one %q", got, want)
	}
	assign, _ := serverActionAsk(test, map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.assign", "params": map[string]any{"assignee_type": "agent", "assignee_id": testAssigneeID}}})
	assign.Options[0].WriteEffectLine("CHE-123", "x99-codex-sol")
	if got := assign.Options[0].WhatHappens; !strings.Contains(got, "On pick: the server assigns CHE-123 to x99-codex-sol as you.") {
		test.Fatalf("what_happens = %q", got)
	}
}

func TestServerActionRefusalsNameTheFieldPath(test *testing.T) {
	waitPR := map[string]any{"until_pr": map[string]any{"url": "https://github.com/cheese-work/multica/pull/1", "state": "merged"}}
	human := func(extra map[string]any) map[string]any {
		fields := map[string]any{"after_choice": "human_steps"}
		for key, value := range extra {
			fields[key] = value
		}
		return fields
	}
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"server_action without action", map[string]any{"after_choice": "server_action"}, "ask.options[0].action is required"},
		{"action on another choice", map[string]any{"after_choice": "record_only", "action": statusAction("done")["action"]}, "ask.options[0].action needs after_choice server_action"},
		{"unknown type", map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.merge", "params": map[string]any{}}}, "ask.options[0].action.type must be issue.status.set or issue.assign"},
		{"empty status", statusAction(" "), "ask.options[0].action.params.status is required"},
		{"unknown param", map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.status.set", "params": map[string]any{"status": "done", "extra": 1}}}, "ask.options[0].action.params.extra is not a known field"},
		{"status with assignee", map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.status.set", "params": map[string]any{"status": "done", "assignee_id": testAssigneeID}}}, "ask.options[0].action.params.assignee_id is not a known field"},
		{"bad assignee type", map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.assign", "params": map[string]any{"assignee_type": "squad", "assignee_id": testAssigneeID}}}, "ask.options[0].action.params.assignee_type must be member or agent"},
		{"bad assignee id", map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.assign", "params": map[string]any{"assignee_type": "agent", "assignee_id": "sol"}}}, "ask.options[0].action.params.assignee_id must be a UUID"},
		{"params not an object", map[string]any{"after_choice": "server_action", "action": map[string]any{"type": "issue.status.set", "params": "done"}}, "ask.options[0].action.params must be an object"},
		{"after_wait without human_steps", map[string]any{"after_choice": "record_only", "after_wait": waitPR}, "ask.options[0].after_wait needs after_choice human_steps"},
		{"after_wait with two conditions", human(map[string]any{"after_wait": map[string]any{"until_status": map[string]any{"status": "done"}, "until_pr": waitPR["until_pr"]}}), "ask.options[0].after_wait must name exactly one of until_pr, until_status or until_issue"},
		{"after_wait empty", human(map[string]any{"after_wait": map[string]any{}}), "ask.options[0].after_wait must name exactly one of until_pr, until_status or until_issue"},
		{"until_pr bad state", human(map[string]any{"after_wait": map[string]any{"until_pr": map[string]any{"url": "https://github.com/cheese-work/multica/pull/1", "state": "open"}}}), "ask.options[0].after_wait.until_pr.state must be checks, merged, conflicts or dequeued"},
		{"until_pr bad url", human(map[string]any{"after_wait": map[string]any{"until_pr": map[string]any{"url": "pull 1", "state": "merged"}}}), "ask.options[0].after_wait.until_pr.url must be an https URL"},
		{"until_status empty", human(map[string]any{"after_wait": map[string]any{"until_status": map[string]any{"status": ""}}}), "ask.options[0].after_wait.until_status.status is required"},
		{"until_issue bad state", human(map[string]any{"after_wait": map[string]any{"until_issue": map[string]any{"issue": "CHE-1", "state": "open"}}}), "ask.options[0].after_wait.until_issue.state must be done, ended, in_review or blocked"},
		{"until_issue unknown field", human(map[string]any{"after_wait": map[string]any{"until_issue": map[string]any{"issue": "CHE-1", "state": "done", "x": 1}}}), "ask.options[0].after_wait.until_issue.x is not a known field"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			_, err := serverActionAsk(test, tc.fields)
			if err == nil || err.Error() != tc.want {
				test.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAfterWaitIsStoredNormalized(test *testing.T) {
	ask, err := serverActionAsk(test, map[string]any{"after_choice": "human_steps", "after_wait": map[string]any{"until_issue": map[string]any{"issue": " CHE-1 ", "state": "Done"}}})
	if err != nil {
		test.Fatalf("decode: %v", err)
	}
	wait := ask.Options[0].AfterWait
	if wait == nil || wait.UntilIssue == nil || wait.UntilIssue.Issue != "CHE-1" || wait.UntilIssue.State != "done" {
		test.Fatalf("after_wait = %+v, want until_issue CHE-1 done", wait)
	}
}

func TestTypedOptionPaths(test *testing.T) {
	plain, _ := serverActionAsk(test, map[string]any{"after_choice": "record_only"})
	if got := plain.TypedOptionPaths(); len(got) != 0 {
		test.Fatalf("plain ask paths = %v, want none", got)
	}
	typed, _ := serverActionAsk(test, statusAction("done"))
	if got := typed.TypedOptionPaths(); len(got) != 1 || got[0] != "ask.options[0].after_choice" {
		test.Fatalf("typed ask paths = %v, want ask.options[0].after_choice", got)
	}
	waiting, _ := serverActionAsk(test, map[string]any{"after_choice": "human_steps", "after_wait": map[string]any{"until_status": map[string]any{"status": "done"}}})
	if got := waiting.TypedOptionPaths(); len(got) != 1 || got[0] != "ask.options[0].after_wait" {
		test.Fatalf("waiting ask paths = %v, want ask.options[0].after_wait", got)
	}
}

func TestServerActionSettingsReadOffWhenMalformed(test *testing.T) {
	for raw, want := range map[string]bool{
		``: false, `{}`: false, `{"decision_server_actions":"on"}`: true, `{"decision_server_actions":"off"}`: false,
		`{"decision_server_actions":true}`: false, `{"decision_server_actions":"ON"}`: false, `{"decision_server_actions":null}`: false, `not json`: false,
	} {
		if got := ServerActionsOn([]byte(raw)); got != want {
			test.Errorf("ServerActionsOn(%q) = %v, want %v", raw, got, want)
		}
	}
	for raw, want := range map[string]string{
		``: "off", `{}`: "off", `{"standing_answers":{"review_cap":"split"}}`: "split", `{"standing_answers":{"review_cap":"off"}}`: "off",
		`{"standing_answers":{"review_cap":"merge"}}`: "off", `{"standing_answers":"split"}`: "off", `{"standing_answers":{"review_cap":"split","x":1}}`: "off", `{"standing_answers":null}`: "off", `nope`: "off",
	} {
		if got := ReviewCapAnswer([]byte(raw)); got != want {
			test.Errorf("ReviewCapAnswer(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestServerActionSettingsValidateWrites(test *testing.T) {
	for name, value := range map[string]map[string]any{
		"on": {"decision_server_actions": "on"}, "off": {"decision_server_actions": "off"},
		"split": {"standing_answers": map[string]any{"review_cap": "split"}}, "default": {"standing_answers": map[string]any{"review_cap": "off"}},
		"absent": {"other": 1},
	} {
		if err := ValidateServerActionSettings(value); err != nil {
			test.Errorf("%s refused: %v", name, err)
		}
	}
	for name, value := range map[string]map[string]any{
		"bool": {"decision_server_actions": true}, "word": {"decision_server_actions": "yes"}, "null": {"decision_server_actions": nil},
		"string": {"standing_answers": "split"}, "cap": {"standing_answers": map[string]any{"review_cap": "merge"}},
		"extra": {"standing_answers": map[string]any{"review_cap": "split", "run_budget": "approve"}}, "empty": {"standing_answers": map[string]any{}}, "null answers": {"standing_answers": nil},
	} {
		if err := ValidateServerActionSettings(value); err == nil {
			test.Errorf("%s accepted", name)
		}
	}
}
