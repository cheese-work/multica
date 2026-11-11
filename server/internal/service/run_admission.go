package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Dispatch-time admission (CHE-5213). A run of a paused project, or of a project
// at its WIP limit, stays queued with admission_state 'held'. Every start path
// runs admission in one transaction under a per-project advisory lock.

const reservationLease = 2 * time.Minute

// RunInput is one trigger of an (issue, agent) run, with a stable event id.
type RunInput struct {
	WorkspaceID, IssueID, AgentID pgtype.UUID
	EventID, Kind                 string
	Payload                       []byte
}

type skipRunInputKey struct{}

// wipLimit reads project.settings.wip_limit; 0 means unlimited (the default).
func wipLimit(settings []byte) int {
	var s struct {
		WIPLimit int `json:"wip_limit"`
	}
	if len(settings) == 0 || json.Unmarshal(settings, &s) != nil || s.WIPLimit < 0 {
		return 0
	}
	return s.WIPLimit
}

func lockProjectAdmission(ctx context.Context, q *db.Queries, projectID pgtype.UUID) (db.GetProjectAdmissionRow, error) {
	if err := q.AcquireProjectAdmissionLock(ctx, util.UUIDToString(projectID)); err != nil {
		return db.GetProjectAdmissionRow{}, fmt.Errorf("lock project admission: %w", err)
	}
	return q.GetProjectAdmission(ctx, projectID)
}

// admitClaimedTask runs inside the claim transaction. A refused run goes back to
// the queue as held; an admitted run takes every held input of its (issue, agent).
func admitClaimedTask(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) (held bool, err error) {
	if !task.IssueID.Valid {
		return false, nil
	}
	issue, err := q.GetIssue(ctx, task.IssueID)
	if err != nil {
		return false, fmt.Errorf("get issue for admission: %w", err)
	}
	if issue.ProjectID.Valid {
		project, err := lockProjectAdmission(ctx, q, issue.ProjectID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if err == nil {
			reason := ""
			if project.Status == "paused" {
				reason = "project paused"
			} else if limit := wipLimit(project.Settings); limit > 0 {
				n, err := q.CountProjectWIP(ctx, db.CountProjectWIPParams{ProjectID: issue.ProjectID, ExcludedTaskID: task.ID})
				if err != nil {
					return false, fmt.Errorf("count project wip: %w", err)
				}
				if int(n) >= limit {
					reason = fmt.Sprintf("WIP %d/%d", limit, limit)
				}
			}
			if reason != "" {
				if _, err := q.HoldClaimedTask(ctx, db.HoldClaimedTaskParams{ID: task.ID, HoldReason: pgtype.Text{String: reason, Valid: true}}); err != nil {
					return false, fmt.Errorf("hold task: %w", err)
				}
				return true, nil
			}
		}
	}
	if _, err := q.BindHeldRunInputs(ctx, db.BindHeldRunInputsParams{TaskID: task.ID, IssueID: task.IssueID, AgentID: task.AgentID}); err != nil {
		return false, fmt.Errorf("bind run inputs: %w", err)
	}
	return false, nil
}

// releaseLocked admits held runs of the project in focus order while a slot is
// free. The caller holds the project's admission lock. skip leaves out runs that
// just came back from an expired reservation.
func releaseLocked(ctx context.Context, q *db.Queries, projectID pgtype.UUID, skip map[pgtype.UUID]bool) ([]db.AgentTaskQueue, error) {
	project, err := q.GetProjectAdmission(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ProjectInactive(project.Status) || project.Status == "paused" {
		return nil, nil
	}
	held, err := q.ListHeldProjectTasks(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list held runs: %w", err)
	}
	limit := wipLimit(project.Settings)
	count, err := q.CountProjectWIP(ctx, db.CountProjectWIPParams{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("count project wip: %w", err)
	}
	var admitted []db.AgentTaskQueue
	for _, h := range held {
		if skip[h.ID] {
			continue
		}
		if limit == 0 {
			t, err := q.UnholdTask(ctx, h.ID)
			if err != nil {
				return nil, fmt.Errorf("admit held run: %w", err)
			}
			admitted = append(admitted, t)
			continue
		}
		if int(count) >= limit {
			break
		}
		t, err := q.ReserveHeldTask(ctx, db.ReserveHeldTaskParams{ID: h.ID, LeaseID: dbid.NewV7(), LeaseSecs: reservationLease.Seconds()})
		if err != nil {
			return nil, fmt.Errorf("reserve held run: %w", err)
		}
		count++
		admitted = append(admitted, t)
	}
	return admitted, nil
}

func (s *TaskService) notifyAdmitted(tasks []db.AgentTaskQueue) {
	for _, t := range tasks {
		s.notifyRuntimeMayHaveWork(t.RuntimeID, "")
	}
}

// ReleaseProjectAdmission is the one release event: unpause, a run ending, a
// priority change or a WIP-limit change all end here.
func (s *TaskService) ReleaseProjectAdmission(ctx context.Context, projectID pgtype.UUID) error {
	if !projectID.Valid || s.TxStarter == nil {
		return nil
	}
	var admitted []db.AgentTaskQueue
	err := s.runInTx(ctx, func(q *db.Queries) error {
		if _, err := lockProjectAdmission(ctx, q, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var err error
		admitted, err = releaseLocked(ctx, q, projectID, nil)
		return err
	})
	if err != nil {
		return err
	}
	s.notifyAdmitted(admitted)
	return nil
}

// releaseAfterRun runs after a run's end committed. It is cheap when nothing is held.
func (s *TaskService) releaseAfterRun(tasks ...db.AgentTaskQueue) {
	if s.Queries == nil || s.TxStarter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen := map[pgtype.UUID]bool{}
	for _, t := range tasks {
		if !t.IssueID.Valid {
			continue
		}
		issue, err := s.Queries.GetIssue(ctx, t.IssueID)
		if err != nil || !issue.ProjectID.Valid || seen[issue.ProjectID] {
			continue
		}
		seen[issue.ProjectID] = true
		if has, err := s.Queries.ProjectHasHeldRuns(ctx, issue.ProjectID); err != nil || !has {
			continue
		}
		if err := s.ReleaseProjectAdmission(ctx, issue.ProjectID); err != nil {
			slog.Warn("release project admission failed", "project_id", util.UUIDToString(issue.ProjectID), "error", err)
		}
	}
}

// ExpireDueReservations is the lease-expiry event: an unclaimed reservation goes
// back to held and the slot goes to the next held run.
func (s *TaskService) ExpireDueReservations(ctx context.Context) (int, error) {
	due, err := s.Queries.ListExpiredReservations(ctx)
	if err != nil {
		return 0, fmt.Errorf("list expired reservations: %w", err)
	}
	expired := 0
	for _, d := range due {
		if !d.ProjectID.Valid {
			continue
		}
		var admitted []db.AgentTaskQueue
		won := false
		err := s.runInTx(ctx, func(q *db.Queries) error {
			if _, err := lockProjectAdmission(ctx, q, d.ProjectID); err != nil {
				return err
			}
			t, err := q.ExpireReservation(ctx, db.ExpireReservationParams{ID: d.ID, LeaseID: d.ReservationLeaseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // a claim won
			}
			if err != nil {
				return err
			}
			won = true
			// Next held run first; the expired run only takes a slot nobody else wants.
			first, err := releaseLocked(ctx, q, d.ProjectID, map[pgtype.UUID]bool{t.ID: true})
			if err != nil {
				return err
			}
			second, err := releaseLocked(ctx, q, d.ProjectID, nil)
			admitted = slices.Concat(first, second)
			return err
		})
		if err != nil {
			return expired, err
		}
		if won {
			expired++
		}
		s.notifyAdmitted(admitted)
	}
	return expired, nil
}

// ReconcileAdmission is the one pass after a restart: expire lapsed reservations,
// then release every project that still holds runs.
func (s *TaskService) ReconcileAdmission(ctx context.Context) error {
	if _, err := s.ExpireDueReservations(ctx); err != nil {
		return err
	}
	projects, err := s.Queries.ListProjectsWithHeldRuns(ctx)
	if err != nil {
		return fmt.Errorf("list projects with held runs: %w", err)
	}
	for _, p := range projects {
		if err := s.ReleaseProjectAdmission(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// RecordRunInput stores one trigger. A retry of the same event id is a no-op and
// reports false.
func (s *TaskService) RecordRunInput(ctx context.Context, in RunInput) (bool, error) {
	return recordRunInput(ctx, s.Queries, in)
}

func recordRunInput(ctx context.Context, q *db.Queries, in RunInput) (bool, error) {
	payload := in.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	_, err := q.InsertRunInput(ctx, db.InsertRunInputParams{
		WorkspaceID: in.WorkspaceID, IssueID: in.IssueID, AgentID: in.AgentID,
		EventID: in.EventID, Kind: in.Kind, Payload: payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// recordEnqueuedInput records the trigger of a freshly inserted run. The event id
// is the trigger comment when there is one, else the run itself.
func recordEnqueuedInput(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue, p db.CreateAgentTaskParams) error {
	if skip, _ := ctx.Value(skipRunInputKey{}).(bool); skip || !task.IssueID.Valid {
		return nil
	}
	event, kind := "task:"+util.UUIDToString(task.ID), "trigger"
	if p.TriggerCommentID.Valid {
		event, kind = "comment:"+util.UUIDToString(p.TriggerCommentID), "comment"
	}
	payload, _ := json.Marshal(map[string]any{
		"task_id":            util.UUIDToString(task.ID),
		"trigger_comment_id": util.UUIDToString(p.TriggerCommentID),
		"trigger_summary":    p.TriggerSummary.String,
	})
	_, err := recordRunInput(ctx, q, RunInput{WorkspaceID: issue.WorkspaceID, IssueID: task.IssueID, AgentID: task.AgentID, EventID: event, Kind: kind, Payload: payload})
	return err
}

// settleRunInputs is the acknowledgement. Only a completed run delivers its
// batch; every other end parks it. Runs in the same transaction as the end.
func settleRunInputs(ctx context.Context, q *db.Queries, tasks ...db.AgentTaskQueue) error {
	for _, t := range tasks {
		if !t.IssueID.Valid {
			continue
		}
		if t.Status == "completed" {
			if err := q.DeliverRunInputs(ctx, t.ID); err != nil {
				return fmt.Errorf("deliver run inputs: %w", err)
			}
			continue
		}
		reason := pgtype.Text{String: t.Status, Valid: true}
		if err := q.ParkRunInputs(ctx, db.ParkRunInputsParams{TaskID: t.ID, ParkReason: reason, FailureClass: t.FailureReason}); err != nil {
			return fmt.Errorf("park run inputs: %w", err)
		}
		if !t.DispatchedAt.Valid && !t.StartedAt.Valid {
			// Ended before any claim: the inputs it was holding are parked too.
			if err := q.ParkHeldRunInputs(ctx, db.ParkHeldRunInputsParams{IssueID: t.IssueID, AgentID: t.AgentID, ParkReason: reason, FailureClass: t.FailureReason}); err != nil {
				return fmt.Errorf("park held run inputs: %w", err)
			}
		}
	}
	return nil
}

// redeliverToRetry is the failed-run recovery path: the retry child gets the
// parent's batch again, same event ids, marked as a redelivery of the parent.
func redeliverToRetry(ctx context.Context, q *db.Queries, parent db.AgentTaskQueue) error {
	if !parent.IssueID.Valid {
		return nil
	}
	if err := q.ParkRunInputs(ctx, db.ParkRunInputsParams{TaskID: parent.ID, ParkReason: pgtype.Text{String: "failed", Valid: true}, FailureClass: parent.FailureReason}); err != nil {
		return err
	}
	_, err := q.ReheldParkedRunInputs(ctx, db.ReheldParkedRunInputsParams{IssueID: parent.IssueID, AgentID: parent.AgentID, FromTaskID: parent.ID})
	return err
}

// ResumeParkedInputs is the explicit resume: parked inputs return to held and a
// run is queued to carry them.
func (s *TaskService) ResumeParkedInputs(ctx context.Context, issueID, agentID pgtype.UUID) (int, error) {
	rows, err := s.Queries.ReheldParkedRunInputs(ctx, db.ReheldParkedRunInputsParams{IssueID: issueID, AgentID: agentID})
	if err != nil {
		return 0, fmt.Errorf("resume parked inputs: %w", err)
	}
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return len(rows), err
	}
	if len(rows) == 0 || issue.AssigneeType.String != "agent" || issue.AssigneeID != agentID {
		return len(rows), nil
	}
	_, err = s.EnqueueTaskForIssue(context.WithValue(ctx, skipRunInputKey{}, true), issue)
	if err != nil && !errors.Is(err, ErrDuplicatePendingTask) {
		return len(rows), err
	}
	return len(rows), nil
}

// DiscardParkedInputs drops the parked batch for good.
func (s *TaskService) DiscardParkedInputs(ctx context.Context, issueID, agentID pgtype.UUID) (int, error) {
	rows, err := s.Queries.DiscardParkedRunInputs(ctx, db.DiscardParkedRunInputsParams{IssueID: issueID, AgentID: agentID})
	return len(rows), err
}

// RunInputsForTask is the batch bound to a run: part of its initial prompt.
func (s *TaskService) RunInputsForTask(ctx context.Context, taskID pgtype.UUID) ([]db.RunInput, error) {
	return s.Queries.ListRunInputsForTask(ctx, taskID)
}

// SetProjectWIPLimit stores the project's WIP limit (nil removes it) and runs
// admission: a raised limit is a release event.
func (s *TaskService) SetProjectWIPLimit(ctx context.Context, workspaceID, projectID pgtype.UUID, limit *int32) (db.Project, error) {
	arg := db.SetProjectWIPLimitParams{ID: projectID, WorkspaceID: workspaceID}
	if limit != nil {
		arg.WipLimit = pgtype.Int4{Int32: *limit, Valid: true}
	}
	project, err := s.Queries.SetProjectWIPLimit(ctx, arg)
	if err != nil {
		return db.Project{}, err
	}
	if err := s.ReleaseProjectAdmission(ctx, project.ID); err != nil {
		slog.Warn("release project admission after WIP change failed", "project_id", util.UUIDToString(project.ID), "error", err)
	}
	return project, nil
}
