package issueworkflow

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// SettingsKeyWorkflowByType is the workspace settings key holding the
// title-type → workflow id map (CHE-5271), e.g.
// {"design": "<uuid>", "plan": "<uuid>", "fix": "<uuid>"}.
const SettingsKeyWorkflowByType = "workflow_by_type"

// WorkflowForTitle returns the workflow the settings map names for the
// Conventional-Commit type at the start of title ("design(x): …" → "design").
// It reports false when the title has no type, the map has no entry for it, or
// the entry is not a UUID, so the caller keeps the project's workflow.
func WorkflowForTitle(settings []byte, title string) (pgtype.UUID, bool) {
	var parsed struct {
		ByType map[string]string `json:"workflow_by_type"`
	}
	if json.Unmarshal(settings, &parsed) != nil || len(parsed.ByType) == 0 {
		return pgtype.UUID{}, false
	}
	typ, ok := TitleType(title)
	if !ok {
		return pgtype.UUID{}, false
	}
	// Keys that differ only by case (Design, design) resolve in sorted order,
	// so the same settings always give the same workflow.
	keys := make([]string, 0, len(parsed.ByType))
	for key := range parsed.ByType {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := parsed.ByType[key]
		if strings.ToLower(strings.TrimSpace(key)) != typ {
			continue
		}
		var id pgtype.UUID
		if id.Scan(strings.TrimSpace(raw)) == nil && id.Valid {
			return id, true
		}
	}
	return pgtype.UUID{}, false
}

// TitleType returns the lowercase type of a "type(scope)!: subject" title.
func TitleType(title string) (string, bool) {
	end := strings.IndexAny(title, "(!:")
	if end <= 0 {
		return "", false
	}
	typ := strings.ToLower(strings.TrimSpace(title[:end]))
	if typ == "" || strings.ContainsAny(typ, " \t") {
		return "", false
	}
	return typ, true
}
