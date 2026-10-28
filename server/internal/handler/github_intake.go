package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errGitHubDeliveryConflict = errors.New("github delivery GUID reused with different content")

type githubIntakeActor struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubIntakeObject struct {
	ID          int64             `json:"id"`
	NodeID      string            `json:"node_id"`
	Body        string            `json:"body"`
	User        githubIntakeActor `json:"user"`
	State       string            `json:"state"`
	CreatedAt   *time.Time        `json:"created_at"`
	UpdatedAt   *time.Time        `json:"updated_at"`
	SubmittedAt *time.Time        `json:"submitted_at"`
}

type githubIntakePayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID       int64             `json:"id"`
		Name     string            `json:"name"`
		FullName string            `json:"full_name"`
		Owner    githubIntakeActor `json:"owner"`
	} `json:"repository"`
	PullRequest struct {
		Number int32 `json:"number"`
		Head   struct {
			SHA  string              `json:"sha"`
			Repo *ghPRHeadRepository `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Issue struct {
		Number      int32           `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Review  githubIntakeObject `json:"review"`
	Comment githubIntakeObject `json:"comment"`
	Sender  githubIntakeActor  `json:"sender"`
}

func (payload githubIntakePayload) revision(event string) (githubIntakeObject, string, int32, string) {
	kind, revisionKind := ghsnapshot.ReviewEventKind(event, payload.Action)
	object, number := payload.Comment, payload.PullRequest.Number
	if kind == "" {
		return object, kind, number, "unsupported_event"
	}
	if revisionKind == "" {
		return object, kind, number, "unsupported_action"
	}
	switch event {
	case "pull_request_review":
		object = payload.Review
		if payload.Action != "dismissed" && object.State != "approved" && object.State != "commented" && object.State != "changes_requested" {
			return object, kind, number, "unsupported_review_state"
		}
	case "pull_request_review_comment", "issue_comment":
		if event == "issue_comment" {
			kind, number = "issue_comment", payload.Issue.Number
			if len(payload.Issue.PullRequest) == 0 || bytes.Equal(payload.Issue.PullRequest, []byte("null")) {
				return object, kind, number, "not_pull_request"
			}
		}
	}
	if number <= 0 || object.ID <= 0 || object.User.ID <= 0 || payload.Sender.ID <= 0 || payload.Installation.ID <= 0 {
		return object, kind, number, "invalid_revision"
	}
	if payload.Repository.ID <= 0 || payload.Repository.Name == "" || payload.Repository.Owner.Login == "" {
		return object, kind, number, "invalid_repository"
	}
	return object, kind, number, ""
}

func (h *Handler) handleGitHubWebhookIntake(ctx context.Context, event, delivery, signature string, body []byte) error {
	transaction, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	queries := h.Queries.WithTx(transaction)
	inserted, err := queries.InsertGitHubWebhookIntake(ctx, db.InsertGitHubWebhookIntakeParams{
		DeliveryGuid: delivery, Event: event, Signature: signature, Payload: body,
	})
	if err != nil {
		return err
	}
	if inserted == 0 {
		stored, err := queries.GetGitHubWebhookIntake(ctx, delivery)
		if err != nil {
			return err
		}
		if stored.Event != event || !bytes.Equal(stored.Payload, body) {
			return errGitHubDeliveryConflict
		}
		return transaction.Commit(ctx)
	}
	var payload githubIntakePayload
	state, reason := "deferred", "awaiting_worker"
	var revisions []db.InsertGitHubWebhookRevisionParams
	if json.Unmarshal(body, &payload) != nil {
		state, reason = "denied", "invalid_payload"
	} else {
		object, kind, number, invalid := payload.revision(event)
		if invalid != "" {
			state, reason = "denied", invalid
		} else {
			installations, err := queries.ListGitHubInstallationsByInstallationID(ctx, payload.Installation.ID)
			if err != nil {
				return err
			}
			if len(installations) == 0 {
				state, reason = "denied", "unknown_installation"
			}
			for _, installation := range installations {
				_, revisionKind := ghsnapshot.ReviewEventKind(event, payload.Action)
				if revisionKind == "withdrawal" {
					if err := queries.DenyPendingGitHubWebhookRevisions(ctx, db.DenyPendingGitHubWebhookRevisionsParams{
						WorkspaceID: installation.WorkspaceID, InstallationID: payload.Installation.ID,
						RepositoryID: payload.Repository.ID, ObjectKind: kind, ObjectID: object.ID, Reason: payload.Action,
					}); err != nil {
						return err
					}
				}
				pullID, revisionState, revisionReason, err := classifyGitHubIntake(ctx, queries, installation.WorkspaceID, payload, object, number)
				if err != nil {
					return err
				}
				if err := recordGitHubReviewDecision(ctx, queries, event, payload, object, pullID, revisionReason); err != nil {
					return err
				}
				if err := withdrawDismissedGitHubReview(ctx, queries, event, installation.WorkspaceID, payload, object, number); err != nil {
					return err
				}
				digest := sha256.Sum256([]byte(object.Body))
				revision := db.InsertGitHubWebhookRevisionParams{
					WorkspaceID: installation.WorkspaceID, InstallationID: payload.Installation.ID, RepositoryID: payload.Repository.ID,
					PullRequestID: pullID, PrNumber: number, ObjectKind: kind, ObjectID: object.ID, BodyHash: hex.EncodeToString(digest[:]),
					SourceDeliveryGuid: pgtype.Text{String: delivery, Valid: true}, Event: event, Action: payload.Action,
					SenderID: payload.Sender.ID, AuthorID: object.User.ID, AuthorLogin: object.User.Login, AuthorType: object.User.Type,
					HeadSha: payload.PullRequest.Head.SHA, ReviewState: object.State,
					ObjectCreatedAt: githubIntakeTimestamp(object.CreatedAt), ObjectUpdatedAt: githubIntakeTimestamp(object.UpdatedAt),
					ObjectSubmittedAt: githubIntakeTimestamp(object.SubmittedAt), State: revisionState, Reason: revisionReason,
				}
				if _, err := queries.InsertGitHubWebhookRevision(ctx, revision); err != nil {
					return err
				}
				revisions = append(revisions, revision)
			}
		}
	}
	if err := queries.SetGitHubWebhookIntakeState(ctx, db.SetGitHubWebhookIntakeStateParams{DeliveryGuid: delivery, State: state, Reason: reason}); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	slog.Info("github: durable intake", "delivery_guid", delivery, "event", event, "state", state, "reason", reason)
	for _, revision := range revisions {
		slog.Info("github: revision intake", "delivery_guid", delivery, "workspace_id", uuidToString(revision.WorkspaceID),
			"object_kind", revision.ObjectKind, "object_id", revision.ObjectID, "state", revision.State, "reason", revision.Reason)
	}
	return nil
}

func classifyGitHubIntake(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, payload githubIntakePayload, object githubIntakeObject, number int32) (pgtype.UUID, string, string, error) {
	var pullID pgtype.UUID
	if payload.Action == "dismissed" || payload.Action == "deleted" {
		return pullID, "denied", payload.Action, nil
	}
	if object.User.Type != "User" || payload.Sender.Type != "User" {
		return pullID, "denied", "bot_author", nil
	}
	workspace, err := queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return pullID, "", "", err
	}
	if !githubFeaturesEnabled(workspace) {
		return pullID, "denied", "integration_disabled", nil
	}
	var settings struct {
		Members map[string]string `json:"github_review_members"`
	}
	if json.Unmarshal(workspace.Settings, &settings) != nil {
		return pullID, "denied", "invalid_member_map", nil
	}
	memberID, err := util.ParseUUID(settings.Members[strconv.FormatInt(object.User.ID, 10)])
	if err != nil {
		return pullID, "denied", "unmapped_author", nil
	}
	if _, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: memberID, WorkspaceID: workspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pullID, "denied", "author_not_workspace_member", nil
		}
		return pullID, "", "", err
	}
	pull, err := queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: workspaceID, RepoOwner: payload.Repository.Owner.Login, RepoName: payload.Repository.Name, PrNumber: number,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pullID, "denied", "no_linked_pull_request", nil
	}
	if err != nil {
		return pullID, "", "", err
	}
	if pull.InstallationID != payload.Installation.ID {
		return pullID, "denied", "installation_mismatch", nil
	}
	headRepo := payload.PullRequest.Head.Repo
	if headRepo == nil {
		if !pull.HeadRepoSame || payload.Issue.Number != number || len(payload.Issue.PullRequest) == 0 {
			return pullID, "deferred", "missing_pull_request_metadata", nil
		}
	} else {
		if headRepo.ID <= 0 || headRepo.FullName == "" || headRepo.Fork == nil {
			return pullID, "deferred", "missing_pull_request_metadata", nil
		}
		if !headRepo.sameRepository(payload.Repository.ID, payload.Repository.Owner.Login, payload.Repository.Name) {
			return pullID, "denied", "fork_pull_request", nil
		}
		if !pull.HeadRepoSame {
			return pullID, "denied", "fork_or_unverified_head_repository", nil
		}
	}
	pullID = pull.ID
	issues, err := queries.ListIssueIDsForPullRequest(ctx, pullID)
	if err != nil {
		return pullID, "", "", err
	}
	for _, issueID := range issues {
		issue, err := queries.GetIssue(ctx, issueID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return pullID, "", "", err
		}
		if issue.WorkspaceID == workspaceID {
			if payload.Sender.ID != object.User.ID {
				return pullID, "quarantined", "foreign_editor", nil
			}
			return pullID, "deferred", "awaiting_admission", nil
		}
	}
	return pullID, "denied", "no_linked_issue", nil
}

// withdrawDismissedGitHubReview clears the stored review decision when GitHub
// dismisses that exact review, so the pr.review_* guards stop firing on it.
// The delivery is signature-checked, and the review id only matches a decision
// intake already admitted.
func withdrawDismissedGitHubReview(ctx context.Context, queries *db.Queries, event string, workspaceID pgtype.UUID, payload githubIntakePayload, object githubIntakeObject, number int32) error {
	if event != "pull_request_review" || payload.Action != "dismissed" {
		return nil
	}
	return queries.ClearGitHubPRReview(ctx, db.ClearGitHubPRReviewParams{
		WorkspaceID: workspaceID, InstallationID: payload.Installation.ID,
		RepoOwner: payload.Repository.Owner.Login, RepoName: payload.Repository.Name, PrNumber: number, ReviewID: object.ID,
	})
}

// recordGitHubReviewDecision stores the latest admitted approve,
// request-changes or comment review on its PR, where the pr.review_* edge
// guards read it. A later comment supersedes an earlier decision. Admitted means
// intake already checked the author: a mapped member of this workspace, on a
// same-repository PR linked to one of its issues.
func recordGitHubReviewDecision(ctx context.Context, queries *db.Queries, event string, payload githubIntakePayload, object githubIntakeObject, pullID pgtype.UUID, reason string) error {
	if event != "pull_request_review" || payload.Action != "submitted" || reason != "awaiting_admission" ||
		(object.State != "approved" && object.State != "changes_requested" && object.State != "commented") {
		return nil
	}
	submitted := githubIntakeTimestamp(object.SubmittedAt)
	if !submitted.Valid {
		submitted = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	return queries.RecordGitHubPRReview(ctx, db.RecordGitHubPRReviewParams{
		ID: pullID, ReviewState: object.State, HeadSha: payload.PullRequest.Head.SHA, ReviewID: object.ID, SubmittedAt: submitted,
	})
}

func githubIntakeTimestamp(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}
