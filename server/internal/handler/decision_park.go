package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// decisionParkKey is the issue.metadata record of a park: what the issue was
// before a decision request parked it, so the answer can put it back.
const decisionParkKey = "decision_park"

type decisionPark struct {
	Status string `json:"status"`
	// To is the waiting status the park wrote.
	To string `json:"to"`
	// ReadinessID is the Readiness property the park set; empty when the
	// workspace has none with a gated-human option.
	ReadinessID string `json:"readiness_id,omitempty"`
	// Readiness is the prior raw value; null when the issue had none.
	Readiness json.RawMessage `json:"readiness,omitempty"`
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
func parkIssueForDecision(ctx context.Context, queries *db.Queries, issue db.Issue) (db.Issue, bool, error) {
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil || category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return issue, false, err
	}
	var target string
	for _, candidate := range []string{idleGuardNeedDecision, issuestatus.Blocked} {
		available, err := statusAvailableToIssue(ctx, queries, issue, candidate)
		if err != nil {
			return issue, false, err
		}
		if available {
			target = candidate
			break
		}
	}
	if target == "" || issue.Status == target {
		return issue, false, nil
	}
	park := decisionPark{Status: issue.Status, To: target}
	propertyID, optionID, err := gatedHumanReadiness(ctx, queries, issue)
	if err != nil {
		return issue, false, err
	}
	if propertyID != "" {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(issue.Properties, &values); err != nil {
			return issue, false, err
		}
		park.ReadinessID, park.Readiness = propertyID, values[propertyID]
		value, _ := json.Marshal(optionID)
		if _, err := queries.SetIssuePropertyValue(ctx, db.SetIssuePropertyValueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: propertyID, Value: value}); err != nil {
			return issue, false, err
		}
	}
	record, err := json.Marshal(park)
	if err != nil {
		return issue, false, err
	}
	if _, err := queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: decisionParkKey, Value: record}); err != nil {
		return issue, false, err
	}
	updated, err := queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, Status: target, WorkspaceID: issue.WorkspaceID})
	return updated, err == nil, err
}

// unparkIssueAfterDecision puts a parked issue back once its last open
// decision request is answered. Run it under the issue lock. An issue someone
// moved on in the meantime keeps its status; only the record goes.
func unparkIssueAfterDecision(ctx context.Context, queries *db.Queries, issue db.Issue) (db.Issue, bool, error) {
	var park decisionPark
	if raw, ok := issueMetadataValue(issue, decisionParkKey); !ok || json.Unmarshal(raw, &park) != nil {
		return issue, false, nil
	}
	requests, err := queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		return issue, false, err
	}
	for _, request := range requests {
		if request.Status == "open" {
			return issue, false, nil
		}
	}
	restored := issue.Status == park.To
	if restored {
		if park.ReadinessID != "" {
			if len(park.Readiness) == 0 {
				_, err = queries.DeleteIssuePropertyValue(ctx, db.DeleteIssuePropertyValueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: park.ReadinessID})
			} else {
				_, err = queries.SetIssuePropertyValue(ctx, db.SetIssuePropertyValueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: park.ReadinessID, Value: park.Readiness})
			}
			if err != nil {
				return issue, false, err
			}
		}
		if issue, err = queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, Status: park.Status, WorkspaceID: issue.WorkspaceID}); err != nil {
			return issue, false, err
		}
	}
	if _, err := queries.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: decisionParkKey}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return issue, false, err
	}
	return issue, restored, nil
}

func issueMetadataValue(issue db.Issue, key string) (json.RawMessage, bool) {
	var values map[string]json.RawMessage
	if json.Unmarshal(issue.Metadata, &values) != nil {
		return nil, false
	}
	raw, ok := values[key]
	return raw, ok
}

// publishDecisionStatusChange announces a park or restore, after the commit.
func (handler *Handler) publishDecisionStatusChange(ctx context.Context, previous, updated db.Issue, source string) {
	handler.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), "system", "", map[string]any{
		"issue":            handler.issueResponseWithCategory(ctx, updated),
		"assignee_changed": false,
		"status_changed":   true,
		"prev_status":      previous.Status,
		"creator_type":     previous.CreatorType,
		"creator_id":       uuidToString(previous.CreatorID),
		"source":           source,
	})
}
