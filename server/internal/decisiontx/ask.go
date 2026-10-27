package decisiontx

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
)

type Ask struct {
	AgentDid         string   `json:"agent_did"`
	Decision         string   `json:"decision"`
	Options          []string `json:"options"`
	Recommendation   string   `json:"recommendation"`
	DefaultIfNoReply string   `json:"default_if_no_reply"`
	SourceIssueID    string   `json:"source_issue_id"`
	SourceCommentID  string   `json:"source_comment_id"`
}

func ParseAsk(content string) (Ask, error) {
	var ask Ask
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ask); err != nil {
		return ask, fmt.Errorf("ask file must contain a structured JSON ask: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ask, fmt.Errorf("ask file must contain exactly one JSON object")
	}
	return ask, ask.Validate()
}

func (ask Ask) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"agent_did", ask.AgentDid}, {"decision", ask.Decision},
		{"recommendation", ask.Recommendation}, {"default_if_no_reply", ask.DefaultIfNoReply},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("ask.%s is required", field.name)
		}
	}
	if len(ask.Options) < 2 {
		return fmt.Errorf("ask.options requires at least two distinct nonempty options")
	}
	seen := make(map[string]bool)
	for _, option := range ask.Options {
		option = strings.TrimSpace(option)
		if option == "" || seen[option] {
			return fmt.Errorf("ask.options requires distinct nonempty options")
		}
		seen[option] = true
	}
	for _, field := range []struct{ name, value string }{
		{"source_issue_id", ask.SourceIssueID}, {"source_comment_id", ask.SourceCommentID},
	} {
		if _, err := util.ParseUUID(field.value); err != nil {
			return fmt.Errorf("ask.%s must be a UUID", field.name)
		}
	}
	return nil
}

func (ask Ask) Content() string {
	return fmt.Sprintf("**Agent did**\n%s\n\n**Decision**\n%s\n\n**Options**\n- %s\n\n**Recommendation**\n%s\n\n**Default if no reply**\n%s\n\n**Source**\n[Issue](mention://issue/%s) · [Comment](/issue/%s#comment-%s)",
		ask.AgentDid, ask.Decision, strings.Join(ask.Options, "\n- "), ask.Recommendation, ask.DefaultIfNoReply, ask.SourceIssueID, ask.SourceIssueID, ask.SourceCommentID)
}
