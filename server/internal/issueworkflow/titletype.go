package issueworkflow

import (
	"encoding/json"
	"fmt"
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

// SettingsKeyQuickCreateFill is the workspace settings key (CHE-6090) that
// turns the quick-create fill rules on or off: "on" (default) | "off".
const SettingsKeyQuickCreateFill = "quick_create_fill"

// QuickCreateFill reports whether quick-create agents fill the fields the
// modal left empty. Only an explicit "off" turns it off.
func QuickCreateFill(settings []byte) bool {
	var parsed struct {
		Fill string `json:"quick_create_fill"`
	}
	_ = json.Unmarshal(settings, &parsed)
	return strings.ToLower(strings.TrimSpace(parsed.Fill)) != "off"
}

// WorkflowTypeKeys returns the lowercased, sorted type keys of workflow_by_type.
func WorkflowTypeKeys(settings []byte) []string {
	var parsed struct {
		ByType map[string]string `json:"workflow_by_type"`
	}
	_ = json.Unmarshal(settings, &parsed)
	keys := make([]string, 0, len(parsed.ByType))
	for key := range parsed.ByType {
		if k := strings.ToLower(strings.TrimSpace(key)); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// ValidateWorkflowByType checks a workflow_by_type value before it is stored
// (CHE-5946): a non-empty object whose keys are lowercase title types without
// whitespace and whose values are workflow UUIDs. The reader skips a bad entry
// silently, so a typo here would quietly route a type to the project workflow.
func ValidateWorkflowByType(raw []byte) error {
	var byType map[string]any
	if err := json.Unmarshal(raw, &byType); err != nil || byType == nil {
		return fmt.Errorf("workflow_by_type must be an object of title type to workflow id")
	}
	if len(byType) == 0 {
		return fmt.Errorf("workflow_by_type must name at least one title type; use null to remove it")
	}
	for key, value := range byType {
		if key == "" || key != strings.ToLower(key) || strings.ContainsAny(key, " \t\r\n") {
			return fmt.Errorf("workflow_by_type key %q must be a lowercase title type without spaces", key)
		}
		text, isString := value.(string)
		var id pgtype.UUID
		if !isString || id.Scan(text) != nil || !id.Valid {
			return fmt.Errorf("workflow_by_type.%s must be a workflow id (UUID)", key)
		}
	}
	return nil
}
