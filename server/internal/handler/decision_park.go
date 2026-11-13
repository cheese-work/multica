package handler

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionPark is the issue_decision_park record of a park: what the issue was
// before a decision request parked it, so the answer can put it back. It lives
// in its own table because issue.metadata holds primitive values only.
type decisionPark struct {
	Status string `json:"status"`
	// To is the waiting status the park wrote.
	To string `json:"to"`
	// ReadinessID is the Readiness property the park set; empty when the
	// workspace has none with a gated-human option.
	ReadinessID string `json:"readiness_id,omitempty"`
	// Readiness is the prior raw value; absent when the issue had none.
	Readiness json.RawMessage `json:"readiness,omitempty"`
	// Set is the value the park wrote; a restore leaves any other value alone.
	Set json.RawMessage `json:"set,omitempty"`
}

// gatedHumanReadiness finds the Readiness property and its gated-human option.
func gatedHumanReadiness(ctx context.Context, queries *db.Queries, issue db.Issue) (propertyID, optionID string, err error) {
	properties, err := queries.ListIssueProperties(ctx, db.ListIssuePropertiesParams{WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return "", "", err
	}
	for _, property := range properties {
		if !strings.EqualFold(property.Name, "Readiness") {
			continue
		}
		var config PropertyConfig
		if err := json.Unmarshal(property.Config, &config); err != nil {
			return "", "", err
		}
		for _, option := range config.Options {
			if option.Name == "gated-human" {
				return uuidToString(property.ID), option.ID, nil
			}
		}
	}
	return "", "", nil
}

// parkIssueForDecision moves an issue that opened a decision request to its
// waiting status (need_decision, else blocked) with Readiness gated-human, and
// records what it replaced. It runs in the request transaction, under the
// issue lock. It leaves a terminal or already waiting issue, and one whose
// workflow lists neither status, as it is.
func parkIssueForDecision(ctx context.Context, queries *db.Queries, issue db.Issue) (db.Issue, *db.ActivityLog, error) {
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil || category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return issue, nil, err
	}
	var target string
	for _, candidate := range []string{idleGuardNeedDecision, issuestatus.Blocked} {
		available, err := statusAvailableToIssue(ctx, queries, issue, candidate)
		if err != nil {
			return issue, nil, err
		}
		if available {
			target = candidate
			break
		}
	}
	// An issue already `blocked` with no need_decision step to move to still gets
	// a park record, so the answer can resume it (CHE-5239). A record already
	// there keeps the prior state of the first card.
	staysBlocked := target == issuestatus.Blocked && issue.Status == target
	if target == "" || issue.Status == target && !staysBlocked {
		return issue, nil, nil
	}
	if staysBlocked {
		if _, err := queries.GetIssueDecisionPark(ctx, db.GetIssueDecisionParkParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID}); err == nil {
			return issue, nil, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return issue, nil, err
		}
	}
	park := decisionPark{Status: issue.Status, To: target}
	propertyID, optionID, err := gatedHumanReadiness(ctx, queries, issue)
	if err != nil {
		return issue, nil, err
	}
	if propertyID != "" {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(issue.Properties, &values); err != nil {
			return issue, nil, err
		}
		park.ReadinessID, park.Readiness = propertyID, values[propertyID]
		value, _ := json.Marshal(optionID)
		park.Set = value
		if _, err := queries.SetIssuePropertyValue(ctx, db.SetIssuePropertyValueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: propertyID, Value: value}); err != nil {
			return issue, nil, err
		}
	}
	if err := queries.InsertIssueDecisionPark(ctx, db.InsertIssueDecisionParkParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, PriorStatus: park.Status, ToStatus: park.To,
		ReadinessID:    pgtype.Text{String: park.ReadinessID, Valid: park.ReadinessID != ""},
		PriorReadiness: park.Readiness, SetValue: park.Set,
	}); err != nil {
		return issue, nil, err
	}
	if staysBlocked {
		return issue, nil, nil
	}
	updated, err := queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, Status: target, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return issue, nil, err
	}
	recorded, err := recordSystemStatusChange(ctx, queries, issue, updated, service.StatusCauseDecision, nil)
	return updated, recorded, err
}

// unparkIssueAfterDecision puts a parked issue back once its last open
// decision request is closed and every answer is delivered. `delivering` lists
// the answers the caller is delivering now. Run it under the issue lock. An issue someone
// moved on in the meantime keeps its status; only the record goes.
func unparkIssueAfterDecision(ctx context.Context, queries *db.Queries, issue db.Issue, delivering []db.DecisionAnswer) (db.Issue, *db.ActivityLog, error) {
	row, err := queries.GetIssueDecisionPark(ctx, db.GetIssueDecisionParkParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return issue, nil, nil
	}
	if err != nil {
		return issue, nil, err
	}
	park := decisionPark{Status: row.PriorStatus, To: row.ToStatus, ReadinessID: row.ReadinessID.String, Readiness: row.PriorReadiness, Set: row.SetValue}
	requests, err := queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		return issue, nil, err
	}
	for _, request := range requests {
		if request.Status == "open" {
			return issue, nil, nil
		}
		if request.Status != "answered" || slices.ContainsFunc(delivering, func(answer db.DecisionAnswer) bool { return answer.DecisionRequestID == request.ID }) {
			continue
		}
		// An answered request holds the park until its answer is delivered.
		answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: issue.WorkspaceID, ID: request.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return issue, nil, err
		}
		if err == nil && !answer.WokenAt.Valid {
			return issue, nil, nil
		}
	}
	var recorded *db.ActivityLog
	if issue.Status == park.To {
		if park.ReadinessID != "" && readinessUnchanged(issue, park) {
			if len(park.Readiness) == 0 {
				_, err = queries.DeleteIssuePropertyValue(ctx, db.DeleteIssuePropertyValueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: park.ReadinessID})
			} else {
				_, err = queries.SetIssuePropertyValue(ctx, db.SetIssuePropertyValueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: park.ReadinessID, Value: park.Readiness})
			}
			if err != nil {
				return issue, nil, err
			}
		}
		previous := issue
		target := park.Status
		if len(delivering) > 0 { // an answer starts a run; a cancellation does not
			if target, err = unparkTarget(ctx, queries, issue, park); err != nil {
				return previous, nil, err
			}
		}
		if target != issue.Status {
			if issue, err = queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, Status: target, WorkspaceID: issue.WorkspaceID}); err != nil {
				return previous, nil, err
			}
		}
		// Runs stamped with a step either side of the park follow the system move,
		// or their next status write is refused as stale.
		if err := queries.RestampActiveTasksWorkflowStep(ctx, db.RestampActiveTasksWorkflowStepParams{IssueID: issue.ID, ToStep: pgtype.Text{String: target, Valid: true}, FromSteps: []string{park.To, park.Status}}); err != nil {
			return previous, nil, err
		}
		if previous.Status != issue.Status {
			if recorded, err = recordSystemStatusChange(ctx, queries, previous, issue, service.StatusCauseDecision, nil); err != nil {
				return previous, nil, err
			}
		}
	}
	if err := queries.DeleteIssueDecisionPark(ctx, db.DeleteIssueDecisionParkParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
		return issue, nil, err
	}
	return issue, recorded, nil
}

// unparkTarget is the status a parked issue returns to. An issue parked from
// `blocked` whose only blocker was the card has nothing left to wait for, so it
// resumes in `in_progress`; an armed wait elsewhere keeps it `blocked` (CHE-5239).
func unparkTarget(ctx context.Context, queries *db.Queries, issue db.Issue, park decisionPark) (string, error) {
	if park.Status != issuestatus.Blocked {
		return park.Status, nil
	}
	waiting, err := queries.IssueHasOpenWait(ctx, issue.ID)
	if err != nil || waiting {
		return park.Status, err
	}
	available, err := statusAvailableToIssue(ctx, queries, issue, issuestatus.InProgress)
	if err != nil || !available {
		return park.Status, err
	}
	return issuestatus.InProgress, nil
}

// readinessUnchanged reports whether Readiness still holds the value the park set.
func readinessUnchanged(issue db.Issue, park decisionPark) bool {
	var values map[string]json.RawMessage
	return json.Unmarshal(issue.Properties, &values) == nil && string(values[park.ReadinessID]) == string(park.Set)
}

// publishDecisionStatusChange announces a park or restore, after the commit.
func (handler *Handler) publishDecisionStatusChange(ctx context.Context, previous, updated db.Issue, recorded *db.ActivityLog, source string) {
	handler.publishSystemStatusChange(uuidToString(updated.WorkspaceID), recorded, map[string]any{
		"issue":            handler.issueResponseWithCategory(ctx, updated),
		"assignee_changed": false,
		"status_changed":   true,
		"prev_status":      previous.Status,
		"creator_type":     previous.CreatorType,
		"creator_id":       uuidToString(previous.CreatorID),
		"source":           source,
	})
}
