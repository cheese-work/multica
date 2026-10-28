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
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type githubIntakeTarget struct {
	IssueID pgtype.UUID
	AgentID pgtype.UUID
	UserID  pgtype.UUID
	Content string
	Route   routeDecision
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
	state   string
	reason  string
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
	pullID, state, reason, err := classifyGitHubIntake(ctx, queries, revision.WorkspaceID, payload, object, number)
	if err != nil || state != "deferred" || reason != "awaiting_admission" {
		return githubIntakeDecision{State: state, Reason: reason}, err
	}
	admission := handler.PRRefresh.CheckReviewRevision(ctx, revision.InstallationID, ghsnapshot.ReviewRevision{
		Event: revision.Event, Action: revision.Action, ObjectID: revision.ObjectID, NodeID: object.NodeID,
		BodyHash: revision.BodyHash, AuthorID: revision.AuthorID, SenderID: revision.SenderID,
		AuthorType: revision.AuthorType, SenderType: payload.Sender.Type, ReviewState: revision.ReviewState,
	}, revision.AuthorID)
	if admission.State != "admitted" {
		return githubIntakeDecision{State: admission.State, Reason: admission.Reason}, nil
	}
	pull, err := queries.GetGitHubPullRequestInWorkspace(ctx, db.GetGitHubPullRequestInWorkspaceParams{ID: pullID, WorkspaceID: revision.WorkspaceID})
	if err != nil {
		return githubIntakeDecision{}, err
	}
	agent, err := githubIntakeAuthor(ctx, queries, pull)
	if errors.Is(err, pgx.ErrNoRows) {
		return githubIntakeDecision{State: "denied", Reason: "no_unique_pr_author"}, nil
	}
	if err != nil {
		return githubIntakeDecision{}, err
	}
	issues, err := queries.ListGitHubPullRequestIssues(ctx, db.ListGitHubPullRequestIssuesParams{WorkspaceID: revision.WorkspaceID, PullRequestID: pull.ID})
	if err != nil {
		return githubIntakeDecision{}, err
	}
	transactionHandler := *handler
	transactionHandler.Queries = queries
	decision := githubIntakeDecision{State: "dispatched", Reason: admission.Reason}
	for _, issue := range issues {
		route := transactionHandler.canRoute(ctx, revision.AuthorID, issue, agent)
		decision.Targets = append(decision.Targets, githubIntakeTarget{IssueID: issue.ID, AgentID: agent.ID, UserID: route.UserID, Route: route})
	}
	if len(decision.Targets) == 0 {
		decision.State, decision.Reason = "denied", "no_linked_issue"
	}
	return decision, nil
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
		if publication.task.ID.Valid {
			worker.handler.TaskService.PublishTaskEnqueued(ctx, publication.task)
		}
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
	if err := queries.SupersedePendingGitHubWebhookRevisions(ctx, db.SupersedePendingGitHubWebhookRevisionsParams{
		InstallationID: payload.Installation.ID, RepositoryID: payload.Repository.ID, ObjectKind: kind, ObjectID: object.ID,
	}); err != nil {
		return "", "", nil, err
	}
	digest := sha256.Sum256([]byte(object.Body))
	revisions, err := queries.ListGitHubWebhookRevisionsForWork(ctx, db.ListGitHubWebhookRevisionsForWorkParams{
		InstallationID: payload.Installation.ID, RepositoryID: payload.Repository.ID,
		ObjectKind: kind, ObjectID: object.ID, BodyHash: hex.EncodeToString(digest[:]),
		Event: intake.Event, Action: payload.Action, SenderID: payload.Sender.ID, AuthorID: object.User.ID, ReviewState: object.State,
		ObjectCreatedAt: githubIntakeTimestamp(object.CreatedAt), ObjectUpdatedAt: githubIntakeTimestamp(object.UpdatedAt),
		ObjectSubmittedAt: githubIntakeTimestamp(object.SubmittedAt),
	})
	if err != nil {
		return "", "", nil, err
	}
	state, reason := "denied", "no_revision"
	priority := map[string]int{"denied": 0, "dispatched": 1, "projected": 2, "unsigned": 3, "quarantined": 4, "deferred": 5}
	var publications []githubIntakePublication
	for _, revision := range revisions {
		if !revision.SourceDeliveryGuid.Valid || revision.SourceDeliveryGuid.String == "" {
			// No signed delivery backs this revision (R3). Settle it as unsigned; retrying cannot create one.
			if err := queries.SetGitHubWebhookRevisionState(ctx, db.SetGitHubWebhookRevisionStateParams{
				WorkspaceID: revision.WorkspaceID, ID: revision.ID, State: "unsigned", Reason: "no_signed_delivery",
			}); err != nil {
				return "", "", nil, err
			}
			if priority["unsigned"] >= priority[state] {
				state, reason = "unsigned", "no_signed_delivery"
			}
			continue
		}
		decision := githubIntakeDecision{State: revision.State, Reason: revision.Reason}
		needsAdmission := revision.State == "deferred"
		if revision.State == "dispatched" {
			recorded, err := queries.HasGitHubWebhookRevisionDispatch(ctx, db.HasGitHubWebhookRevisionDispatchParams{
				WorkspaceID: revision.WorkspaceID, RevisionID: revision.ID,
			})
			if err != nil {
				return "", "", nil, err
			}
			needsAdmission = !recorded
		}
		if needsAdmission {
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
				decision.State, decision.Reason = "denied", "no_linked_issue"
				for _, target := range decision.Targets {
					publication, err := worker.dispatch(ctx, queries, revision, target)
					if err != nil {
						return "", "", nil, err
					}
					if priority[publication.state] >= priority[decision.State] {
						decision.State, decision.Reason = publication.state, publication.reason
					}
					if publication.comment.ID.Valid {
						publications = append(publications, *publication)
					}
				}
			}
			if err := queries.SetGitHubWebhookRevisionState(ctx, db.SetGitHubWebhookRevisionStateParams{
				WorkspaceID: revision.WorkspaceID, ID: revision.ID, State: decision.State, Reason: decision.Reason,
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
	receipt, err := queries.GetGitHubWebhookDispatch(ctx, db.GetGitHubWebhookDispatchParams{
		WorkspaceID: revision.WorkspaceID, RevisionID: revision.ID, IssueID: target.IssueID,
	})
	if err == nil {
		return &githubIntakePublication{state: receipt.State, reason: receipt.Reason}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	issue, err := queries.GetGitHubRouteIssue(ctx, db.GetGitHubRouteIssueParams{
		ID: target.IssueID, WorkspaceID: revision.WorkspaceID, PullRequestID: revision.PullRequestID, InstallationID: revision.InstallationID,
	})
	route := target.Route
	if errors.Is(err, pgx.ErrNoRows) {
		route = routeDeny("target_binding_revoked")
	} else if err != nil {
		return nil, err
	} else if route.Verdict == "" || route.Verdict == routeAllowed {
		agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: target.AgentID, WorkspaceID: revision.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			route = routeDeny("target_agent_not_found")
		} else if err != nil {
			return nil, err
		} else {
			transactionHandler := *worker.handler
			transactionHandler.Queries = queries
			route = transactionHandler.canRoute(ctx, revision.AuthorID, issue, agent)
			if route.Verdict == routeAllowed && target.UserID.Valid && route.UserID != target.UserID {
				route = routeDeny("github_mapping_changed")
			}
		}
	}
	publication := &githubIntakePublication{state: string(route.Verdict), reason: route.Reason, issue: issue}
	if route.Verdict == routeDeferred {
		return publication, nil
	}
	if route.Verdict != routeAllowed {
		if err := recordGitHubRouteResult(ctx, queries, revision, target.IssueID, publication); err != nil {
			return nil, err
		}
		return publication, nil
	}
	key := fmt.Sprintf("github.intake:%s:%s", uuidToString(revision.ID), uuidToString(issue.ID))
	commentID := pgtype.UUID{Bytes: uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)), Valid: true}
	if !worker.dispatchEnabled {
		content, err := githubNotifyContent(ctx, queries, revision)
		if err != nil {
			return nil, err
		}
		created, err := queries.CreateGitHubNotifyComment(ctx, db.CreateGitHubNotifyCommentParams{CommentID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Content: content})
		if err != nil {
			return nil, err
		}
		publication.comment = db.CreateCommentRow(created)
		publication.state, publication.reason = "projected", "gate_g_closed"
	} else {
		params, err := worker.handler.TaskService.PrepareTaskForMention(ctx, issue, target.AgentID, commentID, route.UserID, service.OriginDerived)
		if err != nil {
			return nil, err
		}
		publication.comment, err = queries.CreateComment(ctx, db.CreateCommentParams{
			ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system",
			AuthorID: pgtype.UUID{Valid: true}, Content: target.Content, Type: "comment",
		})
		if err != nil {
			return nil, err
		}
		publication.task, err = worker.handler.TaskService.EnqueuePreparedTaskForMentionInTx(ctx, queries, issue, params, service.OriginDerived)
		if err != nil {
			return nil, err
		}
		publication.state, publication.reason = "dispatched", "admitted"
	}
	if err := recordGitHubRouteResult(ctx, queries, revision, issue.ID, publication); err != nil {
		return nil, err
	}
	return publication, nil
}

func recordGitHubRouteResult(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision, issueID pgtype.UUID, publication *githubIntakePublication) error {
	inserted, err := queries.RecordGitHubWebhookDispatch(ctx, db.RecordGitHubWebhookDispatchParams{
		WorkspaceID: revision.WorkspaceID, ObjectKind: revision.ObjectKind, ObjectID: revision.ObjectID,
		BodyHash: revision.BodyHash, IssueID: issueID, CommentID: publication.comment.ID, TaskID: publication.task.ID,
		RevisionID: revision.ID, State: publication.state, Reason: publication.reason,
	})
	if err != nil {
		return err
	}
	if inserted != 1 {
		return errors.New("github: route receipt requires its committed effects")
	}
	return nil
}
