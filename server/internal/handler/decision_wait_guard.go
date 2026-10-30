package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var decisionWaitKeys = []string{"decision_wait_started_at", "decision_wait_alert_id", "decision_wait_missing_request", "decision_wait_route_error"}

// This is an alert, not an execution gate. Workspace settings can disable it
// or change the default ten-minute observation period without changing prompts.
type decisionWaitSettings struct {
	Guard struct {
		Enabled      *bool   `json:"enabled"`
		AfterMinutes float64 `json:"after_minutes"`
	} `json:"decision_wait_guard"`
}

func (handler *Handler) checkDecisionWaits(ctx context.Context) error {
	// ponytail: scan only waiting issues and existing episodes; paginate this
	// working set if a workspace grows beyond a worker pass's time budget.
	issues, err := handler.Queries.ListDecisionWaitGuardIssues(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, issue := range issues {
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		failures = errors.Join(failures, decisionAttempt(ctx, func() error { return handler.checkDecisionWait(ctx, issue) }))
	}
	return failures
}

func (handler *Handler) checkDecisionWait(ctx context.Context, candidate db.Issue) error {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, candidate.WorkspaceID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := queries.LockDecisionIssueNowait(ctx, db.LockDecisionIssueNowaitParams{WorkspaceID: candidate.WorkspaceID, ID: candidate.ID}); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: candidate.WorkspaceID, ID: candidate.ID})
	if err != nil {
		return err
	}
	workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	var settings decisionWaitSettings
	if err := json.Unmarshal(workspace.Settings, &settings); err != nil {
		return fmt.Errorf("decision wait settings: %w", err)
	}
	eligible, err := decisionWaitEligible(ctx, queries, issue)
	if err != nil {
		return err
	}
	requests, err := queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		return err
	}
	for _, request := range requests {
		if request.Status == "open" {
			eligible = false
		}
	}
	if settings.Guard.Enabled != nil && !*settings.Guard.Enabled {
		eligible = false
	}
	if !eligible {
		for _, key := range decisionWaitKeys {
			if _, err := queries.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: key}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return transaction.Commit(ctx)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(issue.Metadata, &metadata); err != nil {
		return err
	}
	now, err := queries.DecisionExpiryClock(ctx)
	if err != nil {
		return err
	}
	var started string
	if raw := metadata["decision_wait_started_at"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &started); err != nil {
			return fmt.Errorf("decision wait timestamp: %w", err)
		}
	}
	if started == "" {
		if err := setDecisionWaitMetadata(ctx, queries, issue, "decision_wait_started_at", now.Time.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	since, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return fmt.Errorf("decision wait timestamp: %w", err)
	}
	threshold := 10 * time.Minute
	if minutes := settings.Guard.AfterMinutes; minutes > 0 && minutes <= 1440 {
		threshold = time.Duration(minutes * float64(time.Minute))
	}
	if now.Time.Sub(since) <= threshold {
		return nil
	}
	if err := setDecisionWaitMetadata(ctx, queries, issue, "decision_wait_missing_request", true); err != nil {
		return err
	}
	if len(metadata["decision_wait_alert_id"]) > 0 {
		return transaction.Commit(ctx)
	}
	squad, err := queries.GetDecisionWaitGuardSquad(ctx, db.GetDecisionWaitGuardSquadParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		if err := setDecisionWaitMetadata(ctx, queries, issue, "decision_wait_route_error", "owning squad is missing or ambiguous"); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	if err != nil {
		return err
	}
	leader, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: squad.LeaderID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && leader.ArchivedAt.Valid {
		if err := setDecisionWaitMetadata(ctx, queries, issue, "decision_wait_route_error", "owning squad leader is unavailable"); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	if err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"kind": "missing_decision_request", "started_at": started, "squad_id": uuidToString(squad.ID)})
	if err != nil {
		return err
	}
	item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		RecipientType: "agent", RecipientID: leader.ID, Type: "mentioned", Severity: "info",
		Title: "Decision wait has no request", Body: pgtype.Text{String: fmt.Sprintf("%s has waited more than %g minutes without an open decision request. Relay the source ask and verify its delivery.", issue.Title, threshold.Minutes()), Valid: true}, Details: details,
	})
	if err != nil {
		return err
	}
	if !inserted {
		return errors.New("decision wait leader alert was not inserted")
	}
	if err := setDecisionWaitMetadata(ctx, queries, issue, "decision_wait_alert_id", uuidToString(item.ID)); err != nil {
		return err
	}
	if _, err := queries.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: "decision_wait_route_error"}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	handler.publish(protocol.EventInboxNew, uuidToString(issue.WorkspaceID), "system", "", map[string]any{"item": inboxToResponse(item)})
	return nil
}

func setDecisionWaitMetadata(ctx context.Context, queries *db.Queries, issue db.Issue, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: key, Value: raw})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func decisionWaitEligible(ctx context.Context, queries *db.Queries, issue db.Issue) (bool, error) {
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil || category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return false, err
	}
	if issue.Status == "need_decision" {
		return true, nil
	}
	properties, err := queries.ListIssueProperties(ctx, db.ListIssuePropertiesParams{WorkspaceID: issue.WorkspaceID, IncludeArchived: false})
	if err != nil {
		return false, err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(issue.Properties, &values); err != nil {
		return false, err
	}
	for _, property := range properties {
		if !strings.EqualFold(property.Name, "Readiness") {
			continue
		}
		var value string
		if raw := values[uuidToString(property.ID)]; len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
			continue
		}
		if value == "gated-human" {
			return true, nil
		}
		var config PropertyConfig
		if err := json.Unmarshal(property.Config, &config); err != nil {
			return false, err
		}
		for _, option := range config.Options {
			if option.ID == value && option.Name == "gated-human" {
				return true, nil
			}
		}
	}
	return false, nil
}
