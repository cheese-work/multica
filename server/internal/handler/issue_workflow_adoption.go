package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// adoption_linked (CHE-3487, CHE-2948 S3): a Setup change closes only when
// every adoption issue it lists is terminal. The facts are read under the
// issue lock in the write transaction, so a link appended before the lock is
// released is seen. Every read failure lands in AdoptionFacts.Err and refuses
// the close (fail closed), a person's recorded override still passes it.

const setupChangePropertyName = "Setup change"

// changeWord is a verb that says a registry component was touched.
var changeWord = regexp.MustCompile(`(?i)\b(upgrade[sd]?|upgrading|update[sd]?|updating|bump(s|ed|ing)?|install(s|ed|ing)?|add(s|ed|ing)?|remov(e|es|ed|ing)|sunset(s|ting)?|deploy(s|ed|ing|ment)?)\b`)

// loadAdoptionFacts reads the Setup change value, the metadata keys, the
// status of each listed adoption issue and the `none` cross-check.
func (h *Handler) loadAdoptionFacts(ctx context.Context, q *db.Queries, issue db.Issue) *issueworkflow.AdoptionFacts {
	facts := &issueworkflow.AdoptionFacts{Registry: h.cfg.SetupComponents}
	value, err := setupChangeValue(ctx, q, issue)
	if err != nil {
		facts.Err = fmt.Sprintf("Setup change could not be read: %v", err)
		return facts
	}
	facts.SetupChange = value

	var meta map[string]any
	if len(issue.Metadata) > 0 {
		if err := json.Unmarshal(issue.Metadata, &meta); err != nil {
			facts.Err = "issue metadata could not be parsed"
			return facts
		}
	}
	listed, ok := metadataList(meta, "adoption_issues")
	if !ok {
		facts.Err = "metadata adoption_issues is not a list of issue ids"
		return facts
	}
	facts.Components, ok = metadataList(meta, "components")
	if !ok {
		facts.Err = "metadata components is not a list of component ids"
		return facts
	}
	if text, isString := meta["adoption_none"].(string); isString {
		facts.AdoptionNone = text
	} else if meta["adoption_none"] != nil {
		facts.Err = "metadata adoption_none is not text"
		return facts
	}

	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	for _, id := range listed {
		link, err := adoptionLink(ctx, q, issue, prefix, id)
		if err != nil {
			facts.Err = fmt.Sprintf("adoption issue %s could not be read: %v", id, err)
			return facts
		}
		facts.Links = append(facts.Links, link)
	}
	if value == "none" {
		facts.CrossCheckHit = crossCheckHit(h.cfg.SetupComponents, issue.Title+"\n"+issue.Description.String)
	}
	return facts
}

// setupChangeValue is the option name (none or yes) the issue holds in the
// Setup change property; "" when the property is unset or not defined.
func setupChangeValue(ctx context.Context, q *db.Queries, issue db.Issue) (string, error) {
	properties, err := q.ListIssueProperties(ctx, db.ListIssuePropertiesParams{WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return "", err
	}
	for _, property := range properties {
		if !strings.EqualFold(property.Name, setupChangePropertyName) {
			continue
		}
		var config PropertyConfig
		if err := json.Unmarshal(property.Config, &config); err != nil {
			return "", err
		}
		var values map[string]json.RawMessage
		if len(issue.Properties) > 0 {
			if err := json.Unmarshal(issue.Properties, &values); err != nil {
				return "", err
			}
		}
		var optionID string
		if err := json.Unmarshal(values[uuidToString(property.ID)], &optionID); err != nil {
			return "", nil // unset (null or absent)
		}
		for _, option := range config.Options {
			if option.ID == optionID {
				return strings.ToLower(option.Name), nil
			}
		}
		return "", nil
	}
	return "", nil
}

// metadataList reads a comma, semicolon or space separated string; absent is
// an empty list and any other type is not a list.
func metadataList(meta map[string]any, key string) ([]string, bool) {
	raw, present := meta[key]
	if !present || raw == nil {
		return nil, true
	}
	text, ok := raw.(string)
	if !ok {
		return nil, false
	}
	return strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' }), true
}

// adoptionLink resolves a listed identifier in the issue's workspace. An id
// that names no issue here is Missing, so it refuses the close.
func adoptionLink(ctx context.Context, q *db.Queries, issue db.Issue, prefix, id string) (issueworkflow.AdoptionLink, error) {
	link := issueworkflow.AdoptionLink{ID: id, Missing: true}
	parts := splitIdentifier(id)
	if parts == nil || !strings.EqualFold(parts.prefix, prefix) {
		return link, nil
	}
	target, err := q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: issue.WorkspaceID, Number: parts.number})
	if errors.Is(err, pgx.ErrNoRows) {
		return link, nil
	}
	if err != nil {
		return link, err
	}
	category, err := issuestatus.CategoryWithError(ctx, q, target.WorkspaceID, target.Status)
	if err != nil {
		return link, err
	}
	link.Missing = false
	link.Terminal = category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed
	return link, nil
}

// crossCheckHit is the first registry component the text names next to a
// change word; "" when none does.
func crossCheckHit(registry []string, text string) string {
	if !changeWord.MatchString(text) {
		return ""
	}
	for _, id := range registry {
		if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(id) + `\b`).MatchString(text) {
			return id
		}
	}
	return ""
}

// createPropertyGate refuses a create into a project whose workflow guards a
// close with adoption_linked in block mode when Setup change is unset. In warn
// mode the create passes; the close gate warns and the owner sees the gap.
func (h *Handler) createPropertyGate(workspaceID pgtype.UUID) service.CreatePropertyGate {
	return func(ctx context.Context, q *db.Queries, def *issueworkflow.Definition, properties []byte, projectID pgtype.UUID, title string) error {
		if err := h.upstreamCreateGate(ctx, q, workspaceID, def, properties, projectID, title); err != nil {
			return err
		}
		if adoptionGateMode(def) != issueworkflow.EntryModeBlock {
			return nil
		}
		value, err := setupChangeValue(ctx, q, db.Issue{WorkspaceID: workspaceID, Properties: properties})
		if err != nil {
			return err
		}
		if value == "none" || value == "yes" {
			return nil
		}
		fact := (&issueworkflow.AdoptionFacts{}).Refusal()
		return &EntryGateError{Step: "create", Failed: []issueworkflow.EntryFailure{{
			Guard: issueworkflow.GuardAdoptionLinked, Mode: issueworkflow.EntryModeBlock, Fact: fact,
			Fix: "set the Setup change property to none or yes", MemberOverride: false,
		}}}
	}
}

// adoptionGateMode is the strictest mode of an adoption_linked entry gate in
// the workflow; "" when it has none.
func adoptionGateMode(def *issueworkflow.Definition) string {
	mode := ""
	for _, step := range def.Steps {
		for _, g := range step.Entry {
			if g.Guard != issueworkflow.GuardAdoptionLinked || g.Mode == issueworkflow.EntryModeOff {
				continue
			}
			if g.Mode == issueworkflow.EntryModeBlock || mode == "" {
				mode = g.Mode
			}
		}
	}
	return mode
}
