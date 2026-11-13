package decisionaction

import (
	"encoding/json"
	"regexp"
)

// TextSet is the params of an action that replaces one text field of a squad or
// the workspace: the target id, the new text and the digests the agent built
// the change on. The server recomputes both digests; these are only compared.
type TextSet struct {
	ID, Content, BaseSHA256, AfterSHA256 string
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseTextSet reads params with exactly the four keys idKey, contentKey,
// base_sha256 and after_sha256, so a params object naming any other field is
// refused. It is used at request and again at approval.
func ParseTextSet(raw json.RawMessage, idKey, contentKey string) (TextSet, error) {
	var fields map[string]string
	if err := decodeStrict(raw, &fields); err != nil {
		return TextSet{}, err
	}
	for key := range fields {
		if key != idKey && key != contentKey && key != "base_sha256" && key != "after_sha256" {
			return TextSet{}, invalid("params may only name %s, %s, base_sha256 and after_sha256", idKey, contentKey)
		}
	}
	set := TextSet{ID: fields[idKey], Content: fields[contentKey], BaseSHA256: fields["base_sha256"], AfterSHA256: fields["after_sha256"]}
	if _, present := fields[contentKey]; !present {
		return TextSet{}, invalid("%s is required", contentKey)
	}
	var err error
	if set.ID, err = canonicalUUID(idKey, set.ID); err != nil {
		return TextSet{}, err
	}
	if !sha256Pattern.MatchString(set.BaseSHA256) || !sha256Pattern.MatchString(set.AfterSHA256) {
		return TextSet{}, invalid("base_sha256 and after_sha256 must be lowercase hex SHA-256 digests")
	}
	return set, nil
}

func normalizeTextSet(idKey, contentKey string) NormalizeFunc {
	return func(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
		set, err := ParseTextSet(raw, idKey, contentKey)
		if err != nil {
			return nil, nil, err
		}
		canonical, err := json.Marshal(map[string]string{idKey: set.ID, contentKey: set.Content, "base_sha256": set.BaseSHA256, "after_sha256": set.AfterSHA256})
		return canonical, slots, err
	}
}

func init() {
	// Spend is the lowest class that sits above a reversible agent edit: the
	// action is owner-only, so the class only orders the card.
	Register(&Definition{Type: "squad.instructions.set", Version: 1, Template: Template{Title: "Replace a squad's instructions", Summary: "Replaces the squad's instructions with the proposed text."}, MinClass: "spend", Target: TargetSquad, RequiresPreview: true, OwnerOnly: true, Normalize: normalizeTextSet("squad_id", "instructions")})
	Register(&Definition{Type: "workspace.context.set", Version: 1, Template: Template{Title: "Replace the workspace context", Summary: "Replaces the workspace context text with the proposed text."}, MinClass: "spend", Target: TargetWorkspace, RequiresPreview: true, OwnerOnly: true, Normalize: normalizeTextSet("workspace_id", "context")})
}
