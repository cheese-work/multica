package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	return RequireProjectWrite(ctx, queries, workspaceID, projectID, pgtype.UUID{})
}

// RequireProjectWrite is RequireActiveProject for a write on an issue of the project made by the
// run taskID. A pause drains: a run that is still running on an issue of the paused project may
// keep writing. A zero taskID, any other project or any other status is refused as inactive.
func RequireProjectWrite(ctx context.Context, queries *db.Queries, workspaceID, projectID, taskID pgtype.UUID) error {
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
	if !ProjectInactive(status) {
		return nil
	}
	if status == "paused" && taskID.Valid {
		task, err := queries.GetAgentTask(ctx, taskID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && task.Status == "running" && task.IssueID.Valid {
			issue, err := queries.GetIssue(ctx, task.IssueID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil && issue.WorkspaceID == workspaceID && issue.ProjectID == projectID {
				return nil
			}
		}
	}
	return ErrProjectInactive
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

func (service *TaskService) UpdateProject(ctx context.Context, workspaceID pgtype.UUID, params db.UpdateProjectParams, agentWrite bool, audit func(ctx context.Context, queries *db.Queries, previous, updated db.Project) error) (db.Project, error) {
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
	var snapshot pgx.Tx
	var owned []db.BumpProjectWakeupRevisionsRow
	defer func() {
		if snapshot != nil {
			_ = snapshot.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if ProjectInactive(project.Status) || ProjectInactive(previous.Status) {
		// Fixes the facts the condition baseline records: anything that changes
		// after this point, even before the pass runs, is news to the rule.
		if snapshot, err = beginFactSnapshot(ctx, service.TxStarter); err != nil {
			return db.Project{}, err
		}
		if owned, err = settleProjectWakeups(ctx, tx, queries, project); err != nil {
			return db.Project{}, err
		}
	}
	if ProjectInactive(project.Status) {
		cancelled, err = queries.CancelProjectAgentTasks(ctx, db.CancelProjectAgentTasksParams{ProjectID: project.ID, WorkspaceID: workspaceID, KeepRunning: project.Status == "paused"})
		if err != nil {
			return db.Project{}, err
		}
		if err := SettleTerminalTaskState(ctx, queries, cancelled...); err != nil {
			return db.Project{}, err
		}
	}
	// The audit row shares the update's transaction; a failed insert rolls it back.
	if audit != nil {
		if err := audit(ctx, queries, previous, project); err != nil {
			return db.Project{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Project{}, err
	}
	if len(cancelled) > 0 {
		service.BroadcastCancelledTasks(ctx, util.UUIDToString(project.WorkspaceID), cancelled)
	}
	if snapshot != nil {
		facts := snapshot
		snapshot = nil
		run := func() {
			defer facts.Rollback(context.WithoutCancel(ctx))
			(&IssueWakeupService{Tasks: service}).baselineProjectConditions(context.WithoutCancel(ctx), project, owned, facts)
		}
		if service.startProjectBaseline != nil {
			service.startProjectBaseline(run)
		} else {
			go run()
		}
	}
	return project, nil
}

// projectConditionHold keeps a condition rule from coming due between the
// status commit and its baseline pass. The pass moves it to the ordinary poll
// interval; the hold only matters if the pass never runs (a crash).
const projectConditionHold = 5 * time.Minute

// settleProjectWakeups applies a project status flip to its wakeup rules in the
// flip's transaction. Every step is one set-based statement or a batch, so the
// cost does not grow with the number of rules (CHE-9000: the per-rule loop
// timed out resuming a 5,365-issue project). Condition rules, which need a
// fact evaluation each, are only held here and baselined after the commit by
// baselineProjectConditions.
func settleProjectWakeups(ctx context.Context, tx pgx.Tx, queries *db.Queries, project db.Project) ([]db.BumpProjectWakeupRevisionsRow, error) {
	scope := func() (pgtype.UUID, pgtype.UUID) { return project.ID, project.WorkspaceID }
	projectID, workspaceID := scope()
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return nil, err
	}
	if err := queries.DiscardProjectWakeupReceipts(ctx, db.DiscardProjectWakeupReceiptsParams{ProjectID: projectID, WorkspaceID: workspaceID}); err != nil {
		return nil, err
	}
	if err := queries.DisableProjectDueOneShots(ctx, db.DisableProjectDueOneShotsParams{ProjectID: projectID, WorkspaceID: workspaceID}); err != nil {
		return nil, err
	}
	overdue, err := queries.ListProjectOverdueRecurringWakeups(ctx, db.ListProjectOverdueRecurringWakeupsParams{ProjectID: projectID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	var batch db.AdvanceWakeupsBatchParams
	for _, rule := range overdue {
		next, enabled, err := nextSlotAfter(rule, now)
		if err != nil {
			return nil, err
		}
		batch.Ids = append(batch.Ids, rule.ID)
		batch.Enabled = append(batch.Enabled, enabled)
		batch.NextFireAt = append(batch.NextFireAt, next)
	}
	if len(batch.Ids) > 0 {
		if err := queries.AdvanceWakeupsBatch(ctx, batch); err != nil {
			return nil, err
		}
	}
	settled := db.MarkProjectWakeupsSettledParams{ProjectID: projectID, WorkspaceID: workspaceID, HoldSeconds: int32(projectConditionHold / time.Second)}
	if ProjectInactive(project.Status) {
		settled.LastError = pgtype.Text{String: ErrProjectInactive.Error(), Valid: true}
	}
	if err := queries.MarkProjectWakeupsSettled(ctx, settled); err != nil {
		return nil, err
	}
	return queries.BumpProjectWakeupRevisions(ctx, db.BumpProjectWakeupRevisionsParams{ProjectID: projectID, WorkspaceID: workspaceID})
}

// beginFactSnapshot opens a read transaction whose view of the data is fixed
// now. The first statement takes the snapshot.
func beginFactSnapshot(ctx context.Context, starter TxStarter) (pgx.Tx, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// baselineProjectConditions baselines the condition rules a status flip left
// held (owned), each in its own short transaction, so no request waits
// for it and the workspace lock is held for one rule at a time. A rule records
// the facts of the flip's snapshot, not of the moment the pass runs, so a
// condition that turns true after the commit still wakes the agent. The pass
// discards no receipts: the flip already did, and later ones are live. Each
// write is fenced against later changes to the rule. Failures are logged; the rule's hold ends and the scheduler's
// ordinary poll takes over.
func (s *IssueWakeupService) baselineProjectConditions(ctx context.Context, project db.Project, owned []db.BumpProjectWakeupRevisionsRow, facts pgx.Tx) {
	for _, fence := range owned {
		if err := s.baselineProjectCondition(ctx, project.WorkspaceID, fence, facts); err != nil {
			slog.Warn("project condition baseline failed", "project_id", util.UUIDToString(project.ID), "wakeup_id", util.UUIDToString(fence.ID), "error", err)
		}
	}
}

// baselineProjectCondition writes one rule's baseline only while the rule is
// as the flip left it. A later flip, edit or poll changes the revision or the
// hold, and then this pass is obsolete and leaves the newer state alone.
func (s *IssueWakeupService) baselineProjectCondition(ctx context.Context, workspaceID pgtype.UUID, fence db.BumpProjectWakeupRevisionsRow, facts pgx.Tx) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := s.Tasks.Queries.WithTx(tx)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return err
	}
	rule, err := queries.LockIssueWakeup(ctx, fence.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rule.Enabled || len(rule.Condition) == 0 || rule.Revision != fence.Revision || !rule.NextFireAt.Time.Equal(fence.NextFireAt.Time) {
		return nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	fingerprint, diagnostic, err := s.baselineFingerprint(ctx, facts, s.Tasks.Queries.WithTx(facts), rule)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the rule's issue is gone
	}
	if err != nil {
		return err
	}
	if err := writeConditionBaseline(ctx, queries, rule, fingerprint, diagnostic, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *IssueWakeupService) settleInactiveProjectRule(ctx context.Context, tx pgx.Tx, queries *db.Queries, rule db.IssueWakeup) error {
	if err := queries.DiscardWakeupReceipts(ctx, rule.ID); err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	if len(rule.Condition) > 0 {
		if err := s.baselineCondition(ctx, tx, queries, rule, now); err != nil {
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

func (s *IssueWakeupService) settleInactiveProject(ctx context.Context, previous db.IssueWakeup) (bool, error) {
	status, err := s.Tasks.Queries.GetIssueProjectStatus(ctx, previous.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || !ProjectInactive(status) {
		return false, err
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	queries := s.Tasks.Queries.WithTx(tx)
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
	if err := s.settleInactiveProjectRule(ctx, tx, queries, rule); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
