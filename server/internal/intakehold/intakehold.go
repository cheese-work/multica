// Package intakehold is the one rule behind the CHE-6911 intake hold: a new
// root request in a project with an active entry stays in backlog until a
// person, an exemption or the end of the window releases it. Every check (the
// read field here; the status write, the run guard and the decision deliveries
// in later children) reads this package. It refuses nothing by itself.
package intakehold

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// SettingKey is the workspace settings key that holds the entries.
const SettingKey = "intake_hold"

// Modes of an entry.
const (
	ModeOff   = "off"
	ModeWarn  = "warn"
	ModeBlock = "block"
)

// Reasons of a verdict.
const (
	ReasonHeld               = "held"
	ReasonEnded              = "ended"
	ReasonNotStarted         = "not_started"
	ReasonModeOff            = "mode_off"
	ReasonCreatedBeforeStart = "created_before_start"
	ReasonLeftBacklog        = "left_backlog"
	ReasonReleasedByMember   = "released_by_member"
	ReasonExemptType         = "exempt_type"
	ReasonExemptLabel        = "exempt_label"
	ReasonExemptEscalation   = "exempt_escalation_class"
	ReasonExemptAutopilot    = "exempt_autopilot"
	ReasonExemptIssueID      = "exempt_issue_id"
)

// Entry is one project's entry of the setting.
type Entry struct {
	ProjectID               string    `json:"project_id"`
	Mode                    string    `json:"mode"`
	StartsAt                time.Time `json:"starts_at"`
	EndsAt                  time.Time `json:"ends_at"`
	ExemptTypes             []string  `json:"exempt_types"`
	ExemptLabels            []string  `json:"exempt_labels"`
	ExemptEscalationClasses []string  `json:"exempt_escalation_classes"`
	ExemptAutopilotIDs      []string  `json:"exempt_autopilot_ids"`
	ExemptIssueIDs          []string  `json:"exempt_issue_ids"`
	Source                  string    `json:"source"`
}

// StatusRow is the part of one status_changed activity row the rule reads.
type StatusRow struct {
	ActorType  string
	CauseKind  string
	AuthSource string
	From, To   string
	// Release is details.intake_release, the server's own record (child A3).
	Release string
}

// Answer is a live or withdrawn answer of a decision request.
type Answer struct {
	ByPerson   bool
	WakeAt     *time.Time
	Disclaimed bool
}

// Request is a decision request of the issue with its answers.
type Request struct {
	Authority string
	Class     string
	Status    string
	Answers   []Answer
}

// Issue is every stored fact the rule reads.
type Issue struct {
	ID          string
	Title       string
	Status      string
	Priority    string
	CreatedAt   time.Time
	HasParent   bool
	Labels      []string
	AutopilotID string
	HasPark     bool
	ParkPrior   string
	ParkTo      string
	Rows        []StatusRow
	Requests    []Request
}

// Verdict is the read of the rule on one issue.
type Verdict struct {
	Held        bool
	Owned       bool
	Reason      string
	ExemptAfter *time.Time
}

// EscalationClasses are the five classes whose answer by a person ends the
// hold of one issue. "reversible" is never one of them.
var EscalationClasses = []string{"spend", "deploy", "publish", "irreversible", "product_direction"}

// personAuthSources are the credentials that count as a person; cloud_pat and
// task_token are machine credentials and never release.
var personAuthSources = []string{"interactive_jwt", "cli_jwt", "pat"}

func (entry *Entry) active(now time.Time) bool {
	return entry != nil && (entry.Mode == ModeBlock || entry.Mode == ModeWarn) &&
		!now.Before(entry.StartsAt) && now.Before(entry.EndsAt)
}

// inBacklog is condition 4: the status is backlog, or the issue is parked from
// backlog now (the park row counts only while the status equals its to_status).
func (issue Issue) inBacklog() bool {
	return issue.Status == "backlog" || issue.HasPark && issue.ParkPrior == "backlog" && issue.ParkTo == issue.Status
}

// releases is condition 5, read row by row with no order: a person's row that
// leaves backlog by form (a) from backlog or form (b) with the server's key.
func (row StatusRow) releases() bool {
	return row.ActorType == "member" && row.CauseKind == "ui_or_api" &&
		slices.Contains(personAuthSources, row.AuthSource) && row.To != "backlog" &&
		(row.From == "backlog" || row.Release != "")
}

// escalation is the exemption of a decision request: a person's live answer of
// one of the entry's classes. It reports an answer that is not yet effective by
// its wake_at. The caller's `now` is the database clock.
func escalation(issue Issue, entry *Entry, now time.Time) (exempt bool, after *time.Time) {
	for _, request := range issue.Requests {
		if request.Authority != "human" || request.Status != "answered" || !slices.Contains(entry.ExemptEscalationClasses, request.Class) {
			continue
		}
		for _, answer := range request.Answers {
			if !answer.ByPerson || answer.Disclaimed || answer.WakeAt == nil {
				continue
			}
			if !answer.WakeAt.After(now) {
				return true, nil
			}
			if after == nil || answer.WakeAt.Before(*after) {
				wake := *answer.WakeAt
				after = &wake
			}
		}
	}
	return false, after
}

// Evaluate returns the verdict; applies is false when the rule has no say: no
// entry, or an issue with a parent (condition 3).
func Evaluate(issue Issue, entry *Entry, now time.Time) (verdict Verdict, applies bool) {
	if entry == nil || issue.HasParent {
		return Verdict{}, false
	}
	notOwned := func(reason string) (Verdict, bool) { return Verdict{Reason: reason}, true }
	if issue.CreatedAt.Before(entry.StartsAt) {
		return notOwned(ReasonCreatedBeforeStart)
	}
	if !issue.inBacklog() {
		return notOwned(ReasonLeftBacklog)
	}
	if slices.ContainsFunc(issue.Rows, StatusRow.releases) {
		return notOwned(ReasonReleasedByMember)
	}
	if slices.Contains(entry.ExemptIssueIDs, issue.ID) {
		return notOwned(ReasonExemptIssueID)
	}
	typ, hasType := issueworkflow.TitleType(issue.Title)
	if hasType && slices.Contains(entry.ExemptTypes, typ) {
		return notOwned(ReasonExemptType)
	}
	for _, label := range issue.Labels {
		if slices.Contains(entry.ExemptLabels, label) {
			return notOwned(ReasonExemptLabel)
		}
	}
	if issue.AutopilotID != "" && slices.Contains(entry.ExemptAutopilotIDs, issue.AutopilotID) {
		return notOwned(ReasonExemptAutopilot)
	}
	exempt, after := escalation(issue, entry, now)
	if exempt {
		return notOwned(ReasonExemptEscalation)
	}
	owned := Verdict{Owned: true, ExemptAfter: after}
	switch {
	case entry.Mode == ModeOff:
		owned.Reason = ReasonModeOff
	case now.Before(entry.StartsAt):
		owned.Reason = ReasonNotStarted
	case !now.Before(entry.EndsAt):
		owned.Reason = ReasonEnded
	default:
		owned.Held, owned.Reason = true, ReasonHeld
	}
	if !owned.Held {
		owned.ExemptAfter = nil
	}
	return owned, true
}

// Owned is conditions 2 to 6: the part of the rule that ignores the window and
// the mode, so it stays true after ends_at.
func Owned(issue Issue, entry *Entry, now time.Time) bool {
	verdict, applies := Evaluate(issue, entry, now)
	return applies && verdict.Owned
}

// Held is condition 1 and Owned.
func Held(issue Issue, entry *Entry, now time.Time) bool {
	verdict, applies := Evaluate(issue, entry, now)
	return applies && verdict.Held
}

// InScope is conditions 1 to 4. Only the release record uses it.
func InScope(issue Issue, entry *Entry, now time.Time) bool {
	return entry.active(now) && !issue.CreatedAt.Before(entry.StartsAt) && !issue.HasParent && issue.inBacklog()
}

// Validate checks a stored or proposed value of the setting. It is the one
// validator for every write.
func Validate(raw []byte) error {
	var entries []Entry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return fmt.Errorf("%s: %w", SettingKey, err)
	}
	if decoder.More() {
		return fmt.Errorf("%s: trailing data", SettingKey)
	}
	var fields []map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	seen := map[string]bool{}
	for i := range entries {
		entry := &entries[i]
		fail := func(format string, args ...any) error {
			return fmt.Errorf("%s[%d]: %s", SettingKey, i, fmt.Sprintf(format, args...))
		}
		if _, err := uuid.Parse(entry.ProjectID); err != nil {
			return fail("project_id is not a UUID")
		}
		if seen[entry.ProjectID] {
			return fail("two entries for project %s", entry.ProjectID)
		}
		seen[entry.ProjectID] = true
		if source, present := fields[i]["source"]; present && entry.Source == "" {
			return fail("source, when present, is a non-empty string (%s)", source)
		}
		if !slices.Contains([]string{ModeOff, ModeWarn, ModeBlock}, entry.Mode) {
			return fail("mode must be off, warn or block")
		}
		if entry.StartsAt.IsZero() || entry.EndsAt.IsZero() {
			return fail("starts_at and ends_at are required RFC 3339 times")
		}
		if !entry.EndsAt.After(entry.StartsAt) {
			return fail("ends_at must be after starts_at")
		}
		for _, typ := range entry.ExemptTypes {
			if typ == "" || typ != strings.ToLower(typ) || strings.ContainsAny(typ, " \t") {
				return fail("exempt_types holds lowercase types without spaces")
			}
		}
		for _, label := range entry.ExemptLabels {
			if strings.TrimSpace(label) == "" {
				return fail("exempt_labels holds non-empty names")
			}
		}
		for _, class := range entry.ExemptEscalationClasses {
			if !slices.Contains(EscalationClasses, class) {
				return fail("exempt_escalation_classes holds only %s", strings.Join(EscalationClasses, ", "))
			}
		}
		for _, id := range slices.Concat(entry.ExemptAutopilotIDs, entry.ExemptIssueIDs) {
			if _, err := uuid.Parse(id); err != nil {
				return fail("%q is not a UUID", id)
			}
		}
	}
	return nil
}

// ParseStored returns the entry of a project from a stored value. A malformed
// value holds nothing and logs one warning; it never refuses a write.
func ParseStored(raw []byte, projectID string) *Entry {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := Validate(raw); err != nil {
		slog.Warn("intake_hold setting is malformed and holds nothing", "error", err)
		return nil
	}
	var entries []Entry
	_ = json.Unmarshal(raw, &entries)
	for i := range entries {
		if entries[i].ProjectID == projectID {
			return &entries[i]
		}
	}
	return nil
}
