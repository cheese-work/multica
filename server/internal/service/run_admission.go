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

const (
	reservationLease = 2 * time.Minute
	// writeEventMaxAge is how long a finished write keeps its stored response.
	writeEventMaxAge = 7 * 24 * time.Hour
)

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
	projectID, err := q.GetTaskProjectID(ctx, task.ID)
	if err != nil {
		return false, fmt.Errorf("get task project for admission: %w", err)
	}
	if projectID.Valid {
		project, err := lockProjectAdmission(ctx, q, projectID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if err == nil {
			reason := ""
			if project.Status == "paused" {
				reason = "project paused"
			} else if limit := wipLimit(project.Settings); limit > 0 {
				n, err := q.CountProjectWIP(ctx, db.CountProjectWIPParams{ProjectID: projectID, ExcludedTaskID: task.ID})
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
	if !task.IssueID.Valid {
		return false, nil
	}
	if _, err := q.BindHeldRunInputs(ctx, db.BindHeldRunInputsParams{TaskID: task.ID, IssueID: task.IssueID, AgentID: task.AgentID}); err != nil {
		return false, fmt.Errorf("bind run inputs: %w", err)
	}
	return false, nil
}

// releaseLocked admits held runs of the project in focus order while a slot is
// free. The caller holds the project's admission lock. skip leaves out runs that
// just came back from an expired reservation. skipped counts held runs another
// transaction had locked, so the release did not see them; a closed (paused or
// inactive) project skips none: its held runs wait for its own release event.
func releaseLocked(ctx context.Context, q *db.Queries, projectID pgtype.UUID, skip map[pgtype.UUID]bool) (admitted []db.AgentTaskQueue, skipped int, err error) {
	project, err := q.GetProjectAdmission(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if ProjectInactive(project.Status) || project.Status == "paused" {
		return nil, 0, nil
	}
	heldCount, err := q.CountHeldProjectTasks(ctx, projectID)
	if err != nil {
		return nil, 0, fmt.Errorf("count held runs: %w", err)
	}
	held, err := q.ListHeldProjectTasks(ctx, projectID)
	if err != nil {
		return nil, 0, fmt.Errorf("list held runs: %w", err)
	}
	skipped = max(0, int(heldCount)-len(held))
	limit := wipLimit(project.Settings)
	count, err := q.CountProjectWIP(ctx, db.CountProjectWIPParams{ProjectID: projectID})
	if err != nil {
		return nil, 0, fmt.Errorf("count project wip: %w", err)
	}
	for _, h := range held {
		if skip[h.ID] {
			continue
		}
		if limit == 0 {
			t, err := q.UnholdTask(ctx, h.ID)
			if err != nil {
				return nil, 0, fmt.Errorf("admit held run: %w", err)
			}
			admitted = append(admitted, t)
			continue
		}
		if int(count) >= limit {
			break
		}
		t, err := q.ReserveHeldTask(ctx, db.ReserveHeldTaskParams{ID: h.ID, LeaseID: dbid.NewV7(), LeaseSecs: reservationLease.Seconds()})
		if err != nil {
			return nil, 0, fmt.Errorf("reserve held run: %w", err)
		}
		count++
		admitted = append(admitted, t)
	}
	return admitted, skipped, nil
}

func (s *TaskService) notifyAdmitted(tasks []db.AgentTaskQueue) {
	for _, t := range tasks {
		s.notifyRuntimeMayHaveWork(t.RuntimeID, "")
	}
}

// ReleaseProjectAdmission is the one release event: unpause, a run ending, a
// priority change or a WIP-limit change all end here. It also delivers the
// releases owed by ended runs (admission_release): it deletes exactly the rows
// it read at its start, and only when no held run was skipped behind a lock.
func (s *TaskService) ReleaseProjectAdmission(ctx context.Context, projectID pgtype.UUID) error {
	if !projectID.Valid || s.TxStarter == nil {
		return nil
	}
	var admitted []db.AgentTaskQueue
	err := s.runInTx(ctx, func(q *db.Queries) error {
		_, err := lockProjectAdmission(ctx, q, projectID)
		gone := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !gone {
			return err
		}
		owed, err := q.ListAdmissionReleaseTasks(ctx, projectID)
		if err != nil {
			return fmt.Errorf("list owed releases: %w", err)
		}
		var skipped int
		if !gone { // a deleted project has nothing to admit; its rows just go
			if admitted, skipped, err = releaseLocked(ctx, q, projectID, nil); err != nil {
				return err
			}
		}
		if s.afterReleaseRead != nil {
			s.afterReleaseRead()
		}
		if skipped > 0 || len(owed) == 0 {
			return nil
		}
		return q.DeleteAdmissionReleases(ctx, owed)
	})
	if err != nil {
		return err
	}
	s.notifyAdmitted(admitted)
	return nil
}

// ReleaseObligations delivers, for every project that owes a release, the
// release of the ended runs, and returns how many projects it released. One
// project's failure is logged and does not stop the others. Delivery is at
// least once: a second release of a project with nothing held changes nothing.
func (s *TaskService) ReleaseObligations(ctx context.Context) (int, error) {
	projects, err := s.Queries.ListAdmissionReleaseProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("list projects owing a release: %w", err)
	}
	released := 0
	var errs []error
	for _, p := range projects {
		if err := s.ReleaseProjectAdmission(ctx, p); err != nil {
			slog.Warn("release owed by an ended run failed; the next tick retries", "project_id", util.UUIDToString(p), "error", err)
			errs = append(errs, err)
			continue
		}
		released++
	}
	return released, errors.Join(errs...)
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
		projectID, err := s.Queries.GetTaskProjectID(ctx, t.ID)
		if err != nil || !projectID.Valid || seen[projectID] {
			continue
		}
		seen[projectID] = true
		if has, err := s.Queries.ProjectHasHeldRuns(ctx, projectID); err != nil || !has {
			continue
		}
		if err := s.releaseWithRetry(ctx, projectID); err != nil {
			slog.Warn("release project admission failed; the job delivers it from the ended run's admission_release row", "project_id", util.UUIDToString(projectID), "error", err)
		}
	}
}

// releaseWithRetry retries a release that failed on a transient database error.
func (s *TaskService) releaseWithRetry(ctx context.Context, projectID pgtype.UUID) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if err = s.ReleaseProjectAdmission(ctx, projectID); err == nil {
			return nil
		}
	}
	return err
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
			first, _, err := releaseLocked(ctx, q, d.ProjectID, map[pgtype.UUID]bool{t.ID: true})
			if err != nil {
				return err
			}
			second, _, err := releaseLocked(ctx, q, d.ProjectID, nil)
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

// PurgeWriteEvents drops finished write events past their age.
func (s *TaskService) PurgeWriteEvents(ctx context.Context) (int64, error) {
	n, err := s.Queries.DeleteOldWriteEvents(ctx, writeEventMaxAge.Seconds())
	if err != nil {
		return 0, fmt.Errorf("purge write events: %w", err)
	}
	return n, nil
}

// ReconcileAdmission is the restart-time admission pass: events lost with the
// process (a release that never ran, a lapsed lease) are delivered once. Nothing
// calls it on a timer; the tick job delivers due lease-expiry events and the
// releases owed by ended runs (ReleaseObligations).
func (s *TaskService) ReconcileAdmission(ctx context.Context) error {
	if _, err := s.ExpireDueReservations(ctx); err != nil {
		return err
	}
	parked, err := s.Queries.ParkOrphanedRunInputs(ctx)
	if err != nil {
		return fmt.Errorf("park orphaned run inputs: %w", err)
	}
	if len(parked) > 0 {
		slog.Warn("parked held run inputs without a run", "count", len(parked))
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

type eventIDKey struct{}

// WithEventID carries the stable id of the event that causes a run, so a
// redelivery of that event records the same input again as a no-op.
func WithEventID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, eventIDKey{}, id)
}

// recordEnqueuedInput records the trigger of a freshly inserted run. Event id,
// first match: the caller's event id, the trigger comment, the run itself.
// Evidence refs are not event ids: two assignments share one.
func recordEnqueuedInput(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue, p db.CreateAgentTaskParams) error {
	if skip, _ := ctx.Value(skipRunInputKey{}).(bool); skip || !task.IssueID.Valid {
		return nil
	}
	callerEvent, _ := ctx.Value(eventIDKey{}).(string)
	event, kind := "task:"+util.UUIDToString(task.ID), "trigger"
	switch {
	case callerEvent != "":
		event = "event:" + callerEvent
		if p.TriggerCommentID.Valid {
			kind = "comment"
		}
	case p.TriggerCommentID.Valid:
		event, kind = "comment:"+util.UUIDToString(p.TriggerCommentID), "comment"
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":                 kind,
		"event_id":             callerEvent,
		"task_id":              util.UUIDToString(task.ID),
		"trigger_comment_id":   util.UUIDToString(p.TriggerCommentID),
		"trigger_summary":      p.TriggerSummary.String,
		"handoff_note":         p.HandoffNote.String,
		"originator_user_id":   util.UUIDToString(p.OriginatorUserID),
		"originator_source":    p.OriginatorSource.String,
		"trigger_evidence":     p.TriggerEvidenceKind.String,
		"trigger_evidence_ref": util.UUIDToString(p.TriggerEvidenceRefID),
	})
	_, err := recordRunInput(ctx, q, RunInput{WorkspaceID: issue.WorkspaceID, IssueID: task.IssueID, AgentID: task.AgentID, EventID: event, Kind: kind, Payload: payload})
	return err
}

// RecordCoalescedTrigger stores the non-comment trigger (a status change, an
// assignment, a child-done or leader callback) that a queued run of (issue,
// agent) absorbed in place of a new run, so the run's batch carries it. source
// names the producer; commentID is its comment, if any. Event id, first match:
// the caller's event id, the trigger comment, the issue's write stamp.
func (s *TaskService) RecordCoalescedTrigger(ctx context.Context, issue db.Issue, agentID, commentID pgtype.UUID, source string) error {
	return recordCoalescedTrigger(ctx, s.Queries, issue, agentID, commentID, source)
}

// noteCoalesced records the trigger behind a duplicate-pending refusal and
// returns err unchanged. Call it after the failed transaction rolled back: the
// write goes through the pool.
func (s *TaskService) noteCoalesced(ctx context.Context, err error, issue db.Issue, agentID, commentID pgtype.UUID, source string) error {
	if errors.Is(err, ErrDuplicatePendingTask) {
		if rerr := s.RecordCoalescedTrigger(ctx, issue, agentID, commentID, source); rerr != nil {
			slog.Error("record coalesced trigger failed", "issue_id", util.UUIDToString(issue.ID), "source", source, "error", rerr)
		}
	}
	return err
}

// noteCoalescedIn is noteCoalesced inside the caller's transaction: the
// pre-check refused before any insert, so the transaction is still usable and
// the stored trigger commits or rolls back with the caller's write.
func (s *TaskService) noteCoalescedIn(ctx context.Context, q *db.Queries, issue db.Issue, agentID, commentID pgtype.UUID, source string) error {
	if err := recordCoalescedTrigger(ctx, q, issue, agentID, commentID, source); err != nil {
		return fmt.Errorf("record coalesced trigger: %w", err)
	}
	return ErrDuplicatePendingTask
}

func recordCoalescedTrigger(ctx context.Context, q *db.Queries, issue db.Issue, agentID, commentID pgtype.UUID, source string) error {
	if skip, _ := ctx.Value(skipRunInputKey{}).(bool); skip {
		return nil
	}
	callerEvent, _ := ctx.Value(eventIDKey{}).(string)
	var event string
	switch {
	case callerEvent != "":
		event = "event:" + callerEvent
	case commentID.Valid:
		event = "comment:" + util.UUIDToString(commentID)
	default:
		event = fmt.Sprintf("%s:%s:%d", source, util.UUIDToString(issue.ID), issue.UpdatedAt.Time.UnixNano())
	}
	kind := "trigger"
	if commentID.Valid {
		kind = "comment"
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":               kind,
		"source":             source,
		"coalesced":          true,
		"event_id":           callerEvent,
		"issue_status":       issue.Status,
		"trigger_comment_id": util.UUIDToString(commentID),
	})
	_, err := q.InsertCoalescedRunInput(ctx, db.InsertCoalescedRunInputParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: agentID, EventID: event, Kind: kind, Payload: payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// settleAdmissionRelease records, in the run's terminal transaction, the release
// the ended run owes its project. A failed project lookup aborts the end. A run
// without a project owes nothing. The row is deleted by the release that
// delivers it, so a failed or skipped fast path is retried by the job.
func settleAdmissionRelease(ctx context.Context, q *db.Queries, tasks ...db.AgentTaskQueue) error {
	for _, t := range tasks {
		projectID, err := q.GetTaskProjectID(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("get task project for release: %w", err)
		}
		if !projectID.Valid {
			continue
		}
		if err := q.InsertAdmissionRelease(ctx, db.InsertAdmissionReleaseParams{TaskID: t.ID, ProjectID: projectID}); err != nil {
			return fmt.Errorf("record owed release: %w", err)
		}
	}
	return nil
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
// run is queued to carry them, in one transaction. Nothing is re-held for an
// agent the issue is no longer assigned to; a refused carrier run rolls the
// re-hold back, so no input is left held without a run.
func (s *TaskService) ResumeParkedInputs(ctx context.Context, issueID, agentID pgtype.UUID) (int, error) {
	var count int
	var after func()
	err := s.runInTx(ctx, func(q *db.Queries) error {
		issue, err := q.GetIssue(ctx, issueID)
		if err != nil {
			return err
		}
		if issue.AssigneeType.String != "agent" || issue.AssigneeID != agentID {
			return nil
		}
		rows, err := q.ReheldParkedRunInputs(ctx, db.ReheldParkedRunInputsParams{IssueID: issueID, AgentID: agentID})
		if err != nil {
			return fmt.Errorf("resume parked inputs: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		// A pending run already carries the held inputs when it is claimed.
		_, after, err = s.TxScoped(q).EnqueueIssueRunInTx(context.WithValue(ctx, skipRunInputKey{}, true), q, issue, pgtype.UUID{}, pgtype.UUID{}, nil)
		if err != nil && !errors.Is(err, ErrDuplicatePendingTask) {
			return err
		}
		count = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if after != nil {
		after()
	}
	return count, nil
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
