package decisiontx

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-7358: the typed-option contract. An option with after_choice
// server_action carries a typed action the server runs as the answerer on
// pick; an option with after_choice human_steps may carry an after_wait that
// names the event the agent waits on. Only typed fields are read, never
// what_happens (CHE-3345). The wait vocabulary is the issue wakeup's.

const (
	AfterServerAction = "server_action"

	ActionStatusSet = "issue.status.set"
	ActionAssign    = "issue.assign"

	SettingServerActions   = "decision_server_actions"
	SettingStandingAnswers = "standing_answers"

	// ServerActionsOffCode is the refusal code of a typed option while decision_server_actions is off.
	ServerActionsOffCode = "server_actions_off"
)

// OptionAction is the typed action of a server_action option.
type OptionAction struct {
	Type   string       `json:"type"`
	Params ActionParams `json:"params"`
}

// ActionParams holds the params of both action types; the type decides which fields exist.
type ActionParams struct {
	Status       string `json:"status,omitempty"`
	AssigneeType string `json:"assignee_type,omitempty"`
	AssigneeID   string `json:"assignee_id,omitempty"`
}

// AfterWait names the one event an agent waits on after a human_steps option; exactly one field is set.
type AfterWait struct {
	UntilPR     *UntilPR     `json:"until_pr,omitempty"`
	UntilStatus *UntilStatus `json:"until_status,omitempty"`
	UntilIssue  *UntilIssue  `json:"until_issue,omitempty"`
}

type UntilPR struct {
	URL   string `json:"url"`
	State string `json:"state"`
}

type UntilStatus struct {
	Status string `json:"status"`
}

type UntilIssue struct {
	Issue string `json:"issue"`
	State string `json:"state"`
}

var (
	actionFieldsByType = map[string][]string{ActionStatusSet: {"status"}, ActionAssign: {"assignee_type", "assignee_id"}}
	afterWaitFields    = []string{"until_pr", "until_status", "until_issue"}
	untilPRStates      = []string{"checks", "merged", "conflicts", "dequeued"}
	untilIssueStates   = []string{"done", "ended", "in_review", "blocked"}
)

// decodeAction reads the action of one option and normalizes it.
func decodeAction(prefix string, raw json.RawMessage, into **OptionAction) error {
	if len(raw) == 0 {
		return nil
	}
	field := prefix + ".action"
	fields, err := decodeSub(field, raw, "type", "params")
	if err != nil {
		return err
	}
	action := &OptionAction{}
	if err := decodeString(field+".type", fields["type"], &action.Type); err != nil {
		return err
	}
	allowed, ok := actionFieldsByType[action.Type]
	if !ok {
		return &AskError{field + ".type", "must be issue.status.set or issue.assign"}
	}
	params := map[string]json.RawMessage{}
	if p := fields["params"]; len(p) > 0 {
		if err := json.Unmarshal(p, &params); err != nil || params == nil {
			return &AskError{field + ".params", "must be an object"}
		}
	}
	if err := rejectUnknown(field+".params", params, allowed); err != nil {
		return err
	}
	pfield := field + ".params."
	for _, f := range []struct {
		name string
		into *string
	}{{"status", &action.Params.Status}, {"assignee_type", &action.Params.AssigneeType}, {"assignee_id", &action.Params.AssigneeID}} {
		if err := decodeString(pfield+f.name, params[f.name], f.into); err != nil {
			return err
		}
		*f.into = strings.ToLower(strings.TrimSpace(*f.into))
	}
	switch action.Type {
	case ActionStatusSet:
		if action.Params.Status == "" {
			return &AskError{pfield + "status", "is required"}
		}
		if !safeKey.MatchString(action.Params.Status) {
			return &AskError{pfield + "status", "must be a status key such as in_review"}
		}
	case ActionAssign:
		if t := action.Params.AssigneeType; t != "member" && t != "agent" {
			return &AskError{pfield + "assignee_type", "must be member or agent"}
		}
		if _, err := util.ParseUUID(action.Params.AssigneeID); err != nil {
			return &AskError{pfield + "assignee_id", "must be a UUID"}
		}
	}
	*into = action
	return nil
}

// decodeAfterWait reads the after_wait of one option and normalizes it.
func decodeAfterWait(prefix string, raw json.RawMessage, into **AfterWait) error {
	if len(raw) == 0 {
		return nil
	}
	field := prefix + ".after_wait"
	fields, err := decodeSub(field, raw, afterWaitFields...)
	if err != nil {
		return err
	}
	if len(fields) != 1 {
		return &AskError{field, "must name exactly one of until_pr, until_status or until_issue"}
	}
	wait := &AfterWait{}
	switch {
	case fields["until_pr"] != nil:
		var v UntilPR
		if err := decodeLeaf(field+".until_pr", fields["until_pr"], []string{"url", "state"}, map[string]*string{"url": &v.URL, "state": &v.State}); err != nil {
			return err
		}
		if u, err := url.Parse(v.URL); err != nil || u.Scheme != "https" || u.Host == "" || hasSpace(v.URL) {
			return &AskError{field + ".until_pr.url", "must be an https URL"}
		}
		if v.State = strings.ToLower(v.State); !inList(untilPRStates, v.State) {
			return &AskError{field + ".until_pr.state", "must be checks, merged, conflicts or dequeued"}
		}
		wait.UntilPR = &v
	case fields["until_status"] != nil:
		var v UntilStatus
		if err := decodeLeaf(field+".until_status", fields["until_status"], []string{"status"}, map[string]*string{"status": &v.Status}); err != nil {
			return err
		}
		if v.Status = strings.ToLower(v.Status); v.Status == "" {
			return &AskError{field + ".until_status.status", "is required"}
		}
		if !safeKey.MatchString(v.Status) {
			return &AskError{field + ".until_status.status", "must be a status key such as in_review"}
		}
		wait.UntilStatus = &v
	default:
		var v UntilIssue
		if err := decodeLeaf(field+".until_issue", fields["until_issue"], []string{"issue", "state"}, map[string]*string{"issue": &v.Issue, "state": &v.State}); err != nil {
			return err
		}
		if v.Issue == "" {
			return &AskError{field + ".until_issue.issue", "is required"}
		}
		if v.State = strings.ToLower(v.State); !inList(untilIssueStates, v.State) {
			return &AskError{field + ".until_issue.state", "must be done, ended, in_review or blocked"}
		}
		wait.UntilIssue = &v
	}
	*into = wait
	return nil
}

func inList(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// decodeSub decodes a JSON object whose keys must be among known.
func decodeSub(field string, raw json.RawMessage, known ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, &AskError{field, "must be an object"}
	}
	return fields, rejectUnknown(field, fields, known)
}

// decodeLeaf decodes an object of string fields into the given targets, trimmed.
func decodeLeaf(field string, raw json.RawMessage, known []string, into map[string]*string) error {
	fields, err := decodeSub(field, raw, known...)
	if err != nil {
		return err
	}
	for _, name := range known {
		if err := decodeString(field+"."+name, fields[name], into[name]); err != nil {
			return err
		}
		*into[name] = strings.TrimSpace(*into[name])
	}
	return nil
}

// validateTyped checks the cross-field rules of the typed fields of one option.
func (option AskOption) validateTyped(prefix string) error {
	switch {
	case option.AfterChoice == AfterServerAction && option.Action == nil:
		return &AskError{prefix + ".action", "is required"}
	case option.AfterChoice != AfterServerAction && option.Action != nil:
		return &AskError{prefix + ".action", "needs after_choice server_action"}
	case option.AfterChoice != AfterHumanSteps && option.AfterWait != nil:
		return &AskError{prefix + ".after_wait", "needs after_choice human_steps"}
	}
	return nil
}

// TypedOptionPaths lists the field path of every typed option: the paths a card that cannot carry them names when refused.
func (ask Ask) TypedOptionPaths() []string {
	var paths []string
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		if option.AfterChoice == AfterServerAction {
			paths = append(paths, prefix+".after_choice")
		}
		if option.AfterWait != nil {
			paths = append(paths, prefix+".after_wait")
		}
	}
	return paths
}

// WriteEffectLine appends the one-line effect of a server_action option to what_happens, once.
// assigneeName is the display name of an issue.assign target; empty falls back to the id.
func (option *AskOption) WriteEffectLine(issue, assigneeName string) {
	if option.Action == nil {
		return
	}
	var line string
	switch option.Action.Type {
	case ActionStatusSet:
		line = fmt.Sprintf("On pick: the server moves %s to %s as you.", issue, option.Action.Params.Status)
	case ActionAssign:
		if assigneeName == "" {
			assigneeName = option.Action.Params.AssigneeID
		}
		line = fmt.Sprintf("On pick: the server assigns %s to %s as you.", issue, assigneeName)
	}
	if line != "" && !strings.Contains(option.WhatHappens, line) {
		option.WhatHappens = strings.TrimSpace(option.WhatHappens + " " + line)
	}
}

func readSettings(raw []byte) map[string]any {
	var settings map[string]any
	_ = json.Unmarshal(raw, &settings) // a blob that is not an object reads as no setting
	return settings
}

// ServerActionsOn reads decision_server_actions: only the exact string "on" turns it on; anything else reads as off.
func ServerActionsOn(raw []byte) bool {
	return readSettings(raw)[SettingServerActions] == "on"
}

// ReviewCapAnswer reads standing_answers.review_cap: "split" or "off"; a missing or malformed value reads as "off".
func ReviewCapAnswer(raw []byte) string {
	if value, err := parseStandingAnswers(readSettings(raw)[SettingStandingAnswers]); err == nil {
		return value
	}
	return "off"
}

func parseStandingAnswers(value any) (string, error) {
	answers, ok := value.(map[string]any)
	if !ok || len(answers) != 1 {
		return "", fmt.Errorf("%s must be {\"review_cap\": \"off\" or \"split\"}", SettingStandingAnswers)
	}
	if cap, _ := answers["review_cap"].(string); cap == "off" || cap == "split" {
		return cap, nil
	}
	return "", fmt.Errorf("%s.review_cap must be off or split", SettingStandingAnswers)
}

// ValidateServerActionSettings checks the two settings of a write; a key the write omits is not checked.
func ValidateServerActionSettings(settings map[string]any) error {
	if value, ok := settings[SettingServerActions]; ok && value != "on" && value != "off" {
		return fmt.Errorf("%s must be on or off", SettingServerActions)
	}
	if value, ok := settings[SettingStandingAnswers]; ok {
		if _, err := parseStandingAnswers(value); err != nil {
			return err
		}
	}
	return nil
}
