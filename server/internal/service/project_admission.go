package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrProjectInactive = errors.New("project_inactive")

func ProjectInactive(status string) bool {
	return status == "paused" || status == "cancelled" || status == "completed"
}

func IsProjectInactive(err error) bool {
	var databaseError *pgconn.PgError
	return errors.Is(err, ErrProjectInactive) || errors.As(err, &databaseError) && databaseError.Code == "P0001" && databaseError.Message == ErrProjectInactive.Error()
}

func RequireActiveProject(ctx context.Context, queries *db.Queries, workspaceID, projectID pgtype.UUID) error {
	if !projectID.Valid {
		return nil
	}
	status, err := queries.RequireActiveProject(ctx, db.RequireActiveProjectParams{ProjectID: projectID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ProjectInactive(status) {
		return ErrProjectInactive
	}
	return nil
}

func lockTaskProjectForStart(ctx context.Context, queries *db.Queries, task db.AgentTaskQueue) error {
	if !task.IssueID.Valid {
		return requireActiveTaskProject(ctx, queries, task)
	}
	issue, err := queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		return err
	}
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		return err
	}
	if err := queries.LockIssueStatusCatalogShared(ctx, issue.WorkspaceID); err != nil {
		return err
	}
	admissionErr := RequireActiveProject(ctx, queries, issue.WorkspaceID, issue.ProjectID)
	if admissionErr != nil && !IsProjectInactive(admissionErr) {
		return admissionErr
	}
	current, err := queries.LockWakeupIssue(ctx, issue.ID)
	if err != nil {
		return err
	}
	if current.ProjectID != issue.ProjectID {
		return pgx.ErrNoRows
	}
	return admissionErr
}

func RequireActiveIssueProject(ctx context.Context, queries *db.Queries, issueID pgtype.UUID) error {
	if !issueID.Valid {
		return nil
	}
	issue, err := queries.GetIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return RequireActiveProject(ctx, queries, issue.WorkspaceID, issue.ProjectID)
}

func requireActiveTaskProject(ctx context.Context, queries *db.Queries, task db.AgentTaskQueue) error {
	if task.IssueID.Valid {
		return RequireActiveIssueProject(ctx, queries, task.IssueID)
	}
	if task.AutopilotRunID.Valid {
		run, err := queries.GetAutopilotRun(ctx, task.AutopilotRunID)
		if err != nil {
			return err
		}
		autopilot, err := queries.GetAutopilot(ctx, run.AutopilotID)
		if err != nil {
			return err
		}
		return RequireActiveProject(ctx, queries, autopilot.WorkspaceID, autopilot.ProjectID)
	}
	var payload QuickCreateContext
	if err := json.Unmarshal(task.Context, &payload); len(task.Context) > 0 && err != nil {
		return err
	}
	if payload.ProjectID != "" {
		projectID, err := util.ParseUUID(payload.ProjectID)
		if err != nil {
			return err
		}
		agent, err := queries.GetAgent(ctx, task.AgentID)
		if err != nil {
			return err
		}
		return RequireActiveProject(ctx, queries, agent.WorkspaceID, projectID)
	}
	return nil
}

func (service *TaskService) UpdateProject(ctx context.Context, workspaceID pgtype.UUID, params db.UpdateProjectParams, agentWrite bool) (db.Project, error) {
	tx, err := service.TxStarter.Begin(ctx)
	if err != nil {
		return db.Project{}, err
	}
	defer tx.Rollback(ctx)
	queries := service.Queries.WithTx(tx)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return db.Project{}, err
	}
	if _, err := queries.LockProjectForDelete(ctx, db.LockProjectForDeleteParams{ID: params.ID, WorkspaceID: workspaceID}); err != nil {
		return db.Project{}, err
	}
	previous, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: params.ID, WorkspaceID: workspaceID})
	if err != nil {
		return db.Project{}, err
	}
	if agentWrite && ProjectInactive(previous.Status) {
		return db.Project{}, ErrProjectInactive
	}
	project, err := queries.UpdateProject(ctx, params)
	if err != nil {
		return db.Project{}, err
	}
	var cancelled []db.AgentTaskQueue
	if ProjectInactive(project.Status) || ProjectInactive(previous.Status) {
		wakeups := &IssueWakeupService{Tasks: service}
		rules, err := queries.ListProjectWakeups(ctx, db.ListProjectWakeupsParams{ProjectID: project.ID, WorkspaceID: workspaceID})
		if err != nil {
			return db.Project{}, err
		}
		for _, rule := range rules {
			if err := wakeups.settleInactiveProjectRule(ctx, tx, queries, rule); err != nil {
				return db.Project{}, err
			}
			if !ProjectInactive(project.Status) {
				if err := queries.NoteWakeupFailure(ctx, db.NoteWakeupFailureParams{ID: rule.ID}); err != nil {
					return db.Project{}, err
				}
			}
		}
		if err := queries.BumpProjectWakeupRevisions(ctx, db.BumpProjectWakeupRevisionsParams{ProjectID: project.ID, WorkspaceID: workspaceID}); err != nil {
			return db.Project{}, err
		}
	}
	if ProjectInactive(project.Status) {
		cancelled, err = queries.CancelProjectAgentTasks(ctx, db.CancelProjectAgentTasksParams{ProjectID: project.ID, WorkspaceID: workspaceID})
		if err != nil {
			return db.Project{}, err
		}
		if err := SettleTerminalTaskState(ctx, queries, cancelled...); err != nil {
			return db.Project{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Project{}, err
	}
	if len(cancelled) > 0 {
		service.BroadcastCancelledTasks(ctx, util.UUIDToString(project.WorkspaceID), cancelled)
	}
	return project, nil
}

func (service *IssueWakeupService) settleInactiveProjectRule(ctx context.Context, tx pgx.Tx, queries *db.Queries, rule db.IssueWakeup) error {
	if err := queries.DiscardWakeupReceipts(ctx, rule.ID); err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	if len(rule.Condition) > 0 {
		if err := service.baselineCondition(ctx, tx, queries, rule, now); err != nil {
			return err
		}
	} else if rule.Kind == "at" && rule.NextFireAt.Valid && !rule.NextFireAt.Time.After(now) {
		if err := queries.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: rule.ID, Enabled: false}); err != nil {
			return err
		}
	} else if err := skipMissedSlots(ctx, tx, queries, rule); err != nil {
		return err
	}
	return queries.NoteWakeupFailure(ctx, db.NoteWakeupFailureParams{ID: rule.ID, LastError: pgtype.Text{String: ErrProjectInactive.Error(), Valid: true}})
}

func (service *IssueWakeupService) settleInactiveProject(ctx context.Context, previous db.IssueWakeup) (bool, error) {
	status, err := service.Tasks.Queries.GetIssueProjectStatus(ctx, previous.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || !ProjectInactive(status) {
		return false, err
	}
	tx, err := service.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	queries := service.Tasks.Queries.WithTx(tx)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, previous.WorkspaceID); err != nil {
		return false, err
	}
	status, err = queries.LockIssueProjectStatus(ctx, previous.IssueID)
	if err != nil || !ProjectInactive(status) {
		return false, err
	}
	rule, err := queries.LockIssueWakeup(ctx, previous.ID)
	if err != nil {
		return false, err
	}
	if rule.Revision != previous.Revision {
		return true, tx.Commit(ctx)
	}
	if err := service.settleInactiveProjectRule(ctx, tx, queries, rule); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
