package decisiontx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
)

type AskOption struct {
	Label       string   `json:"label"`
	WhatHappens string   `json:"what_happens"`
	Pros        []string `json:"pros"`
	Cons        []string `json:"cons"`
	CostOrTime  string   `json:"cost_or_time"`
}

type Ask struct {
	ContextELI10     string      `json:"context_eli10"`
	AgentDid         string      `json:"agent_did"`
	Decision         string      `json:"decision"`
	StakesIfWrong    string      `json:"stakes_if_wrong"`
	Options          []AskOption `json:"options"`
	Recommendation   string      `json:"recommendation"`
	DefaultIfNoReply string      `json:"default_if_no_reply"`
	SourceIssueID    string      `json:"source_issue_id"`
	SourceCommentID  string      `json:"source_comment_id"`
	Subject          string      `json:"subject,omitempty"`
	Artifacts        []Artifact  `json:"artifacts,omitempty"`
}

// Artifact is a file the human approves. The server checks that it is an
// attachment of the source issue and that its stored bytes hash to SHA256.
type Artifact struct {
	AttachmentID           string             `json:"attachment_id"`
	Label                  string             `json:"label"`
	SHA256                 string             `json:"sha256"`
	SupersedesAttachmentID string             `json:"supersedes_attachment_id,omitempty"`
	SignedBy               *ArtifactSignature `json:"signed_by,omitempty"`
	// SupersedesSignedBy is the signature of the superseded revision, so a diff
	// can say who signed the file it is measured against.
	SupersedesSignedBy *ArtifactSignature `json:"supersedes_signed_by,omitempty"`
}

// ArtifactSignature names the reviewer agent and the signed review comment.
type ArtifactSignature struct {
	Reviewer  string `json:"reviewer"`
	CommentID string `json:"comment_id"`
}

// AskError carries a fixed, field-named message. It never echoes decoder text,
// Go type names or input values, so it is safe to return to a caller.
type AskError struct{ Field, Problem string }

func (err *AskError) Error() string { return err.Field + " " + err.Problem }

const askOptionShape = "must be an object with label, what_happens, pros, cons, cost_or_time"

var (
	askFields    = []string{"context_eli10", "agent_did", "decision", "stakes_if_wrong", "options", "recommendation", "default_if_no_reply", "source_issue_id", "source_comment_id", "subject", "artifacts"}
	optionFields = []string{"label", "what_happens", "pros", "cons", "cost_or_time"}
	safeKey      = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
)

func errAskObject() error { return &AskError{"ask", "must be one JSON object"} }

// DecodeAsk is the one decoder for the CLI and the server. It then validates.
func DecodeAsk(raw []byte) (Ask, error) {
	var ask Ask
	fields, err := decodeObject(raw)
	if err != nil {
		return ask, errAskObject()
	}
	if err := rejectUnknown("ask", fields, askFields); err != nil {
		return ask, err
	}
	for _, field := range []struct {
		name string
		into *string
	}{
		{"context_eli10", &ask.ContextELI10}, {"agent_did", &ask.AgentDid}, {"decision", &ask.Decision},
		{"stakes_if_wrong", &ask.StakesIfWrong}, {"recommendation", &ask.Recommendation},
		{"default_if_no_reply", &ask.DefaultIfNoReply}, {"source_issue_id", &ask.SourceIssueID}, {"source_comment_id", &ask.SourceCommentID},
		{"subject", &ask.Subject},
	} {
		if err := decodeString("ask."+field.name, fields[field.name], field.into); err != nil {
			return ask, err
		}
	}
	if err := decodeOptions(fields["options"], &ask.Options); err != nil {
		return ask, err
	}
	if err := decodeArtifacts(fields["artifacts"], &ask.Artifacts); err != nil {
		return ask, err
	}
	return ask, ask.Validate()
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errAskObject()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errAskObject()
	}
	return fields, nil
}

func rejectUnknown(prefix string, fields map[string]json.RawMessage, known []string) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if slices.Contains(known, key) {
			continue
		}
		if safeKey.MatchString(key) {
			return &AskError{prefix + "." + key, "is not a known field"}
		}
		return &AskError{prefix, "has an unknown field"}
	}
	return nil
}

func decodeString(field string, raw json.RawMessage, into *string) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return &AskError{field, "must be a string"}
	}
	return nil
}

func decodeStrings(field string, raw json.RawMessage, into *[]string) error {
	if len(raw) == 0 {
		return nil
	}
	var items []*string
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{field, "must be a list of strings"}
	}
	list := make([]string, len(items))
	for index, item := range items {
		if item == nil {
			return &AskError{field, "must be a list of strings"}
		}
		list[index] = *item
	}
	*into = list
	return nil
}

func decodeOptions(raw json.RawMessage, into *[]AskOption) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{"ask.options", "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, askOptionShape}
		}
		if err := rejectUnknown(prefix, fields, optionFields); err != nil {
			return err
		}
		var option AskOption
		for _, field := range []struct {
			name string
			into *string
		}{{"label", &option.Label}, {"what_happens", &option.WhatHappens}, {"cost_or_time", &option.CostOrTime}} {
			if err := decodeString(prefix+"."+field.name, fields[field.name], field.into); err != nil {
				return err
			}
		}
		if err := decodeStrings(prefix+".pros", fields["pros"], &option.Pros); err != nil {
			return err
		}
		if err := decodeStrings(prefix+".cons", fields["cons"], &option.Cons); err != nil {
			return err
		}
		*into = append(*into, option)
	}
	return nil
}

func (ask Ask) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"context_eli10", ask.ContextELI10}, {"agent_did", ask.AgentDid}, {"decision", ask.Decision},
		{"stakes_if_wrong", ask.StakesIfWrong},
		{"recommendation", ask.Recommendation}, {"default_if_no_reply", ask.DefaultIfNoReply},
	} {
		if strings.TrimSpace(field.value) == "" {
			return &AskError{"ask." + field.name, "is required"}
		}
	}
	if len(ask.Options) < 2 {
		return &AskError{"ask.options", "requires at least two structured options"}
	}
	labels := make(map[string]bool)
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		for _, field := range []struct{ name, value string }{
			{"label", option.Label}, {"what_happens", option.WhatHappens}, {"cost_or_time", option.CostOrTime},
		} {
			if strings.TrimSpace(field.value) == "" {
				return &AskError{prefix + "." + field.name, "is required"}
			}
		}
		for _, list := range []struct {
			name  string
			items []string
		}{{"pros", option.Pros}, {"cons", option.Cons}} {
			if !slices.ContainsFunc(list.items, func(item string) bool { return strings.TrimSpace(item) != "" }) {
				return &AskError{prefix + "." + list.name, "requires at least one nonblank item"}
			}
		}
		label := strings.ToLower(strings.TrimSpace(option.Label))
		if labels[label] {
			return &AskError{prefix + ".label", "must differ from the other labels"}
		}
		labels[label] = true
	}
	for _, field := range []struct{ name, value string }{
		{"source_issue_id", ask.SourceIssueID}, {"source_comment_id", ask.SourceCommentID},
	} {
		if _, err := util.ParseUUID(field.value); err != nil {
			return &AskError{"ask." + field.name, "must be a UUID"}
		}
	}
	return ask.validateArtifacts()
}

func (ask Ask) Content(workspaceSlug string) string {
	labels := make([]string, len(ask.Options))
	for index, option := range ask.Options {
		labels[index] = option.Label
	}
	commentPath := util.WorkspaceIssuePath(workspaceSlug, ask.SourceIssueID)
	return fmt.Sprintf("**Agent did**\n%s\n\n**Decision**\n%s\n\n**Stakes if wrong**\n%s\n\n**Options**\n- %s\n\n**Recommendation**\n%s\n\n**Default if no reply**\n%s%s\n\n**Source**\n[Issue](mention://issue/%s) · [Comment](%s#comment-%s)",
		ask.AgentDid, ask.Decision, ask.StakesIfWrong, strings.Join(labels, "\n- "), ask.Recommendation, ask.DefaultIfNoReply, ask.artifactsContent(), ask.SourceIssueID, commentPath, ask.SourceCommentID)
}

// artifactsContent lists each artifact before the source link, so the re-ask
// check, which compares the text without that link, tells two files apart.
func (ask Ask) artifactsContent() string {
	if len(ask.Artifacts) == 0 {
		return ""
	}
	lines := make([]string, len(ask.Artifacts))
	for index, artifact := range ask.Artifacts {
		lines[index] = fmt.Sprintf("- %s · `%s` · sha256 `%s`", artifact.Label, artifact.AttachmentID, artifact.SHA256)
	}
	return "\n\n**Artifacts**\n" + strings.Join(lines, "\n")
}
