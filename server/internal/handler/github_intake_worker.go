package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type githubIntakeTarget struct {
	IssueID pgtype.UUID
	AgentID pgtype.UUID
	UserID  pgtype.UUID
	Content string
}

type githubIntakeDecision struct {
	State   string
	Reason  string
	Targets []githubIntakeTarget
}

type githubIntakeWorker struct {
	handler         *Handler
	admit           func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error)
	dispatchEnabled bool
}

type githubIntakePublication struct {
	comment db.CreateCommentRow
	issue   db.Issue
	task    db.AgentTaskQueue
}

func (handler *Handler) RunGitHubIntakeWorker(ctx context.Context) {
	worker := githubIntakeWorker{handler: handler, admit: handler.admitGitHubIntake}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		for batch := 0; batch < 25 && ctx.Err() == nil; batch++ {
			processed, err := worker.runOnce(ctx)
			if err != nil {
				slog.Warn("github: intake worker pass failed", "reason", "database_error")
			}
			if err != nil || !processed {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (handler *Handler) admitGitHubIntake(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision, payload githubIntakePayload) (githubIntakeDecision, error) {
	object, _, number, _ := payload.revision(revision.Event)
	_, state, reason, err := classifyGitHubIntake(ctx, queries, revision.WorkspaceID, payload, object, number)
	return githubIntakeDecision{State: state, Reason: reason}, err
}

func (worker githubIntakeWorker) runOnce(ctx context.Context) (bool, error) {
	transaction, err := worker.handler.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	queries := worker.handler.Queries.WithTx(transaction)
	intake, err := queries.TakeGitHubWebhookIntake(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	processing, err := transaction.Begin(ctx)
	if err != nil {
		return false, err
	}
	state, reason, publications, processErr := worker.process(ctx, worker.handler.Queries.WithTx(processing), intake)
	if processErr != nil {
		if err := processing.Rollback(context.WithoutCancel(ctx)); err != nil {
			return false, err
		}
		state, reason, publications = "deferred", "processing_error", nil
	} else if err := processing.Commit(ctx); err != nil {
		return false, err
	}
	if state == "deferred" {
		err = queries.ScheduleGitHubWebhookIntakeRetry(ctx, db.ScheduleGitHubWebhookIntakeRetryParams{
			DeliveryGuid: intake.DeliveryGuid, Reason: reason, DelaySeconds: min(int64(5)<<min(intake.Attempts, 6), 300),
		})
	} else {
		err = queries.SetGitHubWebhookIntakeState(ctx, db.SetGitHubWebhookIntakeStateParams{DeliveryGuid: intake.DeliveryGuid, State: state, Reason: reason})
	}
	if err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	slog.Info("github: intake processed", "delivery_guid", intake.DeliveryGuid, "state", state, "reason", reason,
		"attempts", intake.Attempts, "age_seconds", int64(time.Since(intake.ReceivedAt.Time).Seconds()))
	for _, publication := range publications {
		issue := publication.issue
		worker.handler.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
			"comment": commentToResponse(publication.comment.Comment(), nil, nil), "issue_title": issue.Title,
			"issue_assignee_type": textToPtr(issue.AssigneeType), "issue_assignee_id": uuidToPtr(issue.AssigneeID),
			"issue_status": issue.Status, "issue_revision": publication.comment.IssueRevision,
		})
		worker.handler.TaskService.PublishTaskEnqueued(ctx, publication.task)
	}
	return true, nil
}

func (worker githubIntakeWorker) process(ctx context.Context, queries *db.Queries, intake db.GithubWebhookIntake) (string, string, []githubIntakePublication, error) {
	var payload githubIntakePayload
	if json.Unmarshal(intake.Payload, &payload) != nil {
		return "denied", "invalid_payload", nil, nil
	}
	object, kind, _, invalid := payload.revision(intake.Event)
	if invalid != "" {
		return "denied", invalid, nil, nil
	}
	digest := sha256.Sum256([]byte(object.Body))
	revisions, err := queries.ListGitHubWebhookRevisionsForWork(ctx, db.ListGitHubWebhookRevisionsForWorkParams{
		InstallationID: payload.Installation.ID, RepositoryID: payload.Repository.ID,
		ObjectKind: kind, ObjectID: object.ID, BodyHash: hex.EncodeToString(digest[:]),
	})
	if err != nil {
		return "", "", nil, err
	}
	state, reason := "denied", "no_revision"
	priority := map[string]int{"denied": 0, "dispatched": 1, "projected": 2, "unsigned": 3, "quarantined": 4, "deferred": 5}
	var publications []githubIntakePublication
	for _, revision := range revisions {
		decision := githubIntakeDecision{State: revision.State, Reason: revision.Reason}
		if revision.State == "deferred" || revision.State == "dispatched" {
			if worker.admit == nil {
				return "", "", nil, errors.New("github: admission unavailable")
			}
			decision, err = worker.admit(ctx, queries, revision, payload)
			if err != nil {
				return "", "", nil, err
			}
			if _, valid := priority[decision.State]; !valid || decision.Reason == "" {
				return "", "", nil, errors.New("github: invalid admission decision")
			}
			if decision.State == "dispatched" {
				if !worker.dispatchEnabled {
					decision.State, decision.Reason = "projected", "gate_g_closed"
				} else {
					if len(decision.Targets) == 0 {
						return "", "", nil, errors.New("github: dispatch has no target")
					}
					for _, target := range decision.Targets {
						publication, err := worker.dispatch(ctx, queries, revision, target)
						if err != nil {
							return "", "", nil, err
						}
						if publication != nil {
							publications = append(publications, *publication)
						}
					}
				}
			}
			if err := queries.SetGitHubWebhookRevisionState(ctx, db.SetGitHubWebhookRevisionStateParams{
				WorkspaceID: revision.WorkspaceID, ObjectKind: revision.ObjectKind, ObjectID: revision.ObjectID,
				BodyHash: revision.BodyHash, State: decision.State, Reason: decision.Reason,
			}); err != nil {
				return "", "", nil, err
			}
		}
		if priority[decision.State] >= priority[state] {
			state, reason = decision.State, decision.Reason
		}
	}
	return state, reason, publications, nil
}

func (worker githubIntakeWorker) dispatch(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision, target githubIntakeTarget) (*githubIntakePublication, error) {
	issue, err := queries.GetIssue(ctx, target.IssueID)
	if err != nil {
		return nil, err
	}
	if issue.WorkspaceID != revision.WorkspaceID {
		return nil, errors.New("github: cross-workspace dispatch refused")
	}
	if _, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: target.AgentID, WorkspaceID: revision.WorkspaceID}); err != nil {
		return nil, err
	}
	if _, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: target.UserID, WorkspaceID: revision.WorkspaceID}); err != nil {
		return nil, err
	}
	seen, err := queries.HasGitHubWebhookDispatch(ctx, db.HasGitHubWebhookDispatchParams{
		WorkspaceID: revision.WorkspaceID, ObjectKind: revision.ObjectKind, ObjectID: revision.ObjectID,
		BodyHash: revision.BodyHash, IssueID: issue.ID,
	})
	if err != nil || seen {
		return nil, err
	}
	key := fmt.Sprintf("github.intake:%s:%s:%d:%s:%s", uuidToString(revision.WorkspaceID), revision.ObjectKind,
		revision.ObjectID, revision.BodyHash, uuidToString(issue.ID))
	commentID := pgtype.UUID{Bytes: uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)), Valid: true}
	params, err := worker.handler.TaskService.PrepareTaskForMention(ctx, issue, target.AgentID, commentID, target.UserID, service.OriginDerived)
	if err != nil {
		return nil, err
	}
	created, err := queries.CreateComment(ctx, db.CreateCommentParams{
		ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system",
		AuthorID: pgtype.UUID{Valid: true}, Content: target.Content, Type: "comment",
	})
	if err != nil {
		return nil, err
	}
	task, err := worker.handler.TaskService.EnqueuePreparedTaskForMentionInTx(ctx, queries, issue, params, service.OriginDerived)
	if err != nil {
		return nil, err
	}
	inserted, err := queries.RecordGitHubWebhookDispatch(ctx, db.RecordGitHubWebhookDispatchParams{
		WorkspaceID: revision.WorkspaceID, ObjectKind: revision.ObjectKind, ObjectID: revision.ObjectID,
		BodyHash: revision.BodyHash, IssueID: issue.ID, CommentID: commentID, TaskID: task.ID,
	})
	if err != nil {
		return nil, err
	}
	if inserted != 1 {
		return nil, errors.New("github: dispatch receipt requires its comment and run")
	}
	return &githubIntakePublication{comment: created, issue: issue, task: task}, nil
}
