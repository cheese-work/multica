package issueworkflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// SettingsKeyQuickFirst is the workspace setting (CHE-7680) that draws the
// quick_first arm at issue create: within the window a root issue of a listed
// type whose full-workflow route is bound takes the Quick workflow on an even
// issue number and keeps its route on an odd one. Unset, the create path is
// unchanged.
const SettingsKeyQuickFirst = "quick_first"

// StepKeyAuthor is the sticky-assignee key of the author an arm-Q issue was
// created for. The status key pattern excludes a leading underscore, so the
// key cannot be a workflow status.
const StepKeyAuthor = "_author"

// Workflow source of an issue create: who chose p.WorkflowID. The HTTP handler
// sets it; empty is every other creator.
const (
	WorkflowSourceCaller    = "caller"
	WorkflowSourceTitleType = "title_type"
)

// QuickFirst is the decoded quick_first setting. The window is [StartsAt,
// EndsAt): the start is in, the end is out.
type QuickFirst struct {
	WorkflowID pgtype.UUID
	Types      []string
	StartsAt   time.Time
	EndsAt     time.Time
}

type quickFirstWire struct {
	WorkflowID string    `json:"workflow_id"`
	Types      []string  `json:"types"`
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
}

var quickFirstType = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidateQuickFirst checks one stored value. It is the validator of the
// settings patch and of the ordinary settings save.
func ValidateQuickFirst(raw []byte) error {
	_, err := decodeQuickFirst(raw)
	return err
}

func decodeQuickFirst(raw []byte) (*QuickFirst, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire quickFirstWire
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%s: %w", SettingsKeyQuickFirst, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", SettingsKeyQuickFirst)
	}
	var id pgtype.UUID
	if err := id.Scan(wire.WorkflowID); err != nil || !id.Valid {
		return nil, fmt.Errorf("%s.workflow_id must be a workflow id", SettingsKeyQuickFirst)
	}
	if len(wire.Types) == 0 {
		return nil, fmt.Errorf("%s.types needs at least one type", SettingsKeyQuickFirst)
	}
	for _, typ := range wire.Types {
		if !quickFirstType.MatchString(typ) {
			return nil, fmt.Errorf("%s.types: %q is not a lowercase title type", SettingsKeyQuickFirst, typ)
		}
	}
	if wire.StartsAt.IsZero() || wire.EndsAt.IsZero() {
		return nil, fmt.Errorf("%s needs starts_at and ends_at as RFC 3339 timestamps", SettingsKeyQuickFirst)
	}
	if !wire.EndsAt.After(wire.StartsAt) {
		return nil, fmt.Errorf("%s.ends_at must be after starts_at", SettingsKeyQuickFirst)
	}
	return &QuickFirst{WorkflowID: id, Types: wire.Types, StartsAt: wire.StartsAt, EndsAt: wire.EndsAt}, nil
}

// DecodeQuickFirst reads the setting from the workspace settings. Missing,
// null or invalid is nil: the arm is off.
func DecodeQuickFirst(settings []byte) *QuickFirst {
	var parsed map[string]json.RawMessage
	if json.Unmarshal(settings, &parsed) != nil {
		return nil
	}
	raw, ok := parsed[SettingsKeyQuickFirst]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	qf, err := decodeQuickFirst(raw)
	if err != nil {
		return nil
	}
	return qf
}

// Open reports whether now is inside the window.
func (q *QuickFirst) Open(now time.Time) bool {
	return !now.Before(q.StartsAt) && now.Before(q.EndsAt)
}

// Lists reports whether the type is one the setting draws.
func (q *QuickFirst) Lists(typ string) bool { return slices.Contains(q.Types, typ) }
