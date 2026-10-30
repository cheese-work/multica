package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	githubReconcileInterval = 10 * time.Minute
	// Quiet period before a delivery or object counts as missed, so a webhook still in flight is not chased.
	githubReconcileGrace = 10 * time.Minute
	// GitHub keeps delivery history for 3 days; older failures fall to R3.
	githubDeliveryWindow        = 72 * time.Hour
	githubRedeliveryMaxAttempts = 5
	githubDeliveryMaxPages      = 20
	githubIntakeStaleAfter      = time.Hour
)

var githubIntakeEvents = map[string]string{
	"review": "pull_request_review", "review_comment": "pull_request_review_comment", "issue_comment": "issue_comment",
}

type githubReconcileAPI interface {
	ListHookDeliveries(ctx context.Context, cursor string) ([]ghsnapshot.DeliveryAttempt, string, error)
	RedeliverHook(ctx context.Context, deliveryID int64) error
	ListPullObjects(ctx context.Context, installationID int64, owner, repo string, number int32) (ghsnapshot.PullObjects, error)
}

// githubReconciler recovers intake that never reached the durable store (R2,
// R3) and alerts on items stuck in `deferred` or `unsigned`. R1, a stored
// delivery, is retried by githubIntakeWorker from its stored payload.
type githubReconciler struct {
	handler          *Handler
	api              githubReconcileAPI
	now              func() time.Time
	redeliverEnabled bool
	unsignedEnabled  bool
}

// RunGitHubIntakeReconciler always raises stale alerts. R2 redelivery replays
// webhooks and R3 reads every open linked PR, so each stays off until its
// environment variable is set: GITHUB_INTAKE_REDELIVER=1, GITHUB_INTAKE_UNSIGNED=1.
func (handler *Handler) RunGitHubIntakeReconciler(ctx context.Context) {
	reconciler := githubReconciler{
		handler: handler, api: handler.PRRefresh, now: time.Now,
		redeliverEnabled: os.Getenv("GITHUB_INTAKE_REDELIVER") == "1" && handler.PRRefresh.Enabled(),
		unsignedEnabled:  os.Getenv("GITHUB_INTAKE_UNSIGNED") == "1" && handler.PRRefresh.Enabled(),
	}
	ticker := time.NewTicker(githubReconcileInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		reconciler.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (reconciler githubReconciler) runOnce(ctx context.Context) {
	if _, err := reconciler.alertStale(ctx); err != nil {
		slog.Warn("github: stale intake scan failed", "reason", "database_error")
	}
	if reconciler.redeliverEnabled {
		if _, err := reconciler.redeliver(ctx); err != nil {
			slog.Warn("github: delivery redelivery pass failed", "reason", "github_api_error")
		}
	}
	if reconciler.unsignedEnabled {
		if _, err := reconciler.recordUnsigned(ctx); err != nil {
			slog.Warn("github: unsigned revision scan failed", "reason", "github_api_error")
		}
	}
}

// redeliver is R2: a failed delivery in GitHub's log with no stored intake is
// requested again. The redelivery keeps its GUID, is signed by GitHub and takes
// the normal webhook path, so a repeat request is idempotent.
func (reconciler githubReconciler) redeliver(ctx context.Context) (int, error) {
	attempts := map[string][]ghsnapshot.DeliveryAttempt{}
	var order []string
	cursor := ""
	complete := false
	for page := 0; page < githubDeliveryMaxPages; page++ {
		entries, next, err := reconciler.api.ListHookDeliveries(ctx, cursor)
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			if entry.GUID == "" {
				continue
			}
			if _, seen := attempts[entry.GUID]; !seen {
				order = append(order, entry.GUID)
			}
			attempts[entry.GUID] = append(attempts[entry.GUID], entry)
		}
		if next == "" || len(entries) == 0 || reconciler.now().Sub(entries[len(entries)-1].DeliveredAt) > githubDeliveryWindow {
			complete = true
			break
		}
		cursor = next
	}
	if !complete {
		// Attempts beyond the page cap are unseen, so an attempt count would be too low and could pass the cap.
		return 0, errors.New("github: delivery log pagination limit exceeded")
	}
	requested := 0
	for _, guid := range order {
		newest, redeliverable := redeliverableAttempt(attempts[guid], reconciler.now())
		if !redeliverable {
			continue
		}
		if _, err := reconciler.handler.Queries.GetGitHubWebhookIntake(ctx, guid); err == nil {
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return requested, err
		}
		if err := reconciler.api.RedeliverHook(ctx, newest.ID); err != nil {
			return requested, err
		}
		slog.Info("github: delivery redelivery requested", "delivery_guid", guid, "attempts", len(attempts[guid]))
		requested++
	}
	return requested, nil
}

func redeliverableAttempt(attempts []ghsnapshot.DeliveryAttempt, now time.Time) (ghsnapshot.DeliveryAttempt, bool) {
	newest := attempts[0]
	for _, attempt := range attempts {
		if attempt.Succeeded() {
			return newest, false
		}
		if attempt.DeliveredAt.After(newest.DeliveredAt) {
			newest = attempt
		}
	}
	supported := false
	for _, event := range githubIntakeEvents {
		supported = supported || event == newest.Event
	}
	age := now.Sub(newest.DeliveredAt)
	return newest, supported && len(attempts) < githubRedeliveryMaxAttempts && age >= githubReconcileGrace && age <= githubDeliveryWindow
}

// recordUnsigned is R3: a review or comment on an open, same-repository linked
// PR whose newest stored revision has a different body is recorded as
// `unsigned`. The worker never dispatches it; exact-snapshot approval is a
// separate slice.
func (reconciler githubReconciler) recordUnsigned(ctx context.Context) (int, error) {
	queries := reconciler.handler.Queries
	pulls, err := queries.ListGitHubReconcilePullRequests(ctx)
	if err != nil {
		return 0, err
	}
	recorded := 0
	enabled := map[pgtype.UUID]bool{}
	for _, pull := range pulls {
		allowed, known := enabled[pull.WorkspaceID]
		if !known {
			workspace, err := queries.GetWorkspace(ctx, pull.WorkspaceID)
			if err != nil {
				return recorded, err
			}
			allowed = githubFeaturesEnabled(workspace)
			enabled[pull.WorkspaceID] = allowed
		}
		if !allowed {
			continue
		}
		listed, err := reconciler.api.ListPullObjects(ctx, pull.InstallationID, pull.RepoOwner, pull.RepoName, pull.PrNumber)
		if err != nil {
			slog.Warn("github: unsigned revision scan skipped a pull request", "pr_number", pull.PrNumber, "reason", "github_api_error")
			continue
		}
		if !listed.Open || !listed.SameRepo {
			continue
		}
		for _, object := range listed.Objects {
			if object.AuthorType != "User" || reconciler.now().Sub(latestObjectTime(object)) < githubReconcileGrace {
				continue
			}
			digest := sha256.Sum256([]byte(object.Body))
			bodyHash := hex.EncodeToString(digest[:])
			// Only the newest stored revision counts: an object edited back to an earlier body is a new revision.
			action := "reconciled"
			latest, err := queries.GetLatestGitHubWebhookRevision(ctx, db.GetLatestGitHubWebhookRevisionParams{
				WorkspaceID: pull.WorkspaceID, InstallationID: pull.InstallationID, RepositoryID: listed.RepositoryID,
				ObjectKind: object.Kind, ObjectID: object.ID,
			})
			switch {
			case err == nil && revisionMatchesObject(latest, object, bodyHash):
				continue
			case err == nil:
				// The identity index rejects metadata equal to an older reconciled row; naming the superseded revision keeps it distinct.
				action += ":" + uuidToString(latest.ID)
			case !errors.Is(err, pgx.ErrNoRows):
				return recorded, err
			}
			inserted, err := queries.InsertGitHubWebhookRevision(ctx, db.InsertGitHubWebhookRevisionParams{
				WorkspaceID: pull.WorkspaceID, InstallationID: pull.InstallationID, RepositoryID: listed.RepositoryID,
				PullRequestID: pull.ID, PrNumber: pull.PrNumber, ObjectKind: object.Kind, ObjectID: object.ID, BodyHash: bodyHash,
				Event: githubIntakeEvents[object.Kind], Action: action, SenderID: object.AuthorID, AuthorID: object.AuthorID,
				AuthorLogin: object.AuthorLogin, AuthorType: object.AuthorType, HeadSha: listed.HeadSHA, ReviewState: object.ReviewState,
				ObjectCreatedAt: githubIntakeTimestamp(object.CreatedAt), ObjectUpdatedAt: githubIntakeTimestamp(object.UpdatedAt),
				ObjectSubmittedAt: githubIntakeTimestamp(object.SubmittedAt), State: "unsigned", Reason: "no_signed_event",
			})
			if err != nil {
				return recorded, err
			}
			if inserted == 1 {
				slog.Info("github: unsigned revision recorded", "workspace_id", uuidToString(pull.WorkspaceID),
					"object_kind", object.Kind, "object_id", object.ID)
				recorded++
			}
		}
	}
	return recorded, nil
}

// revisionMatchesObject compares the REST object with a stored revision. A
// timestamp missing on either side is not compared, and times compare at
// database (microsecond) precision, since signed payloads and
// the REST API do not carry the same set for every object kind.
func revisionMatchesObject(revision db.GithubWebhookRevision, object ghsnapshot.PullObject, bodyHash string) bool {
	same := func(stored pgtype.Timestamptz, current *time.Time) bool {
		return !stored.Valid || current == nil || stored.Time.Equal(current.Truncate(time.Microsecond))
	}
	return revision.BodyHash == bodyHash && revision.ReviewState == object.ReviewState &&
		same(revision.ObjectCreatedAt, object.CreatedAt) && same(revision.ObjectUpdatedAt, object.UpdatedAt) &&
		same(revision.ObjectSubmittedAt, object.SubmittedAt)
}

func latestObjectTime(object ghsnapshot.PullObject) time.Time {
	var latest time.Time
	for _, value := range []*time.Time{object.CreatedAt, object.UpdatedAt, object.SubmittedAt} {
		if value != nil && value.After(latest) {
			latest = *value
		}
	}
	return latest
}

// alertStale logs one error per `deferred` intake or `unsigned` revision older
// than an hour (Cheese decision 4: no loss). The scan repeats each pass, so the
// alert keeps firing until the item settles. Logs carry IDs, states and reason
// codes only, never GitHub text.
func (reconciler githubReconciler) alertStale(ctx context.Context) (int, error) {
	items, err := reconciler.handler.Queries.ListStaleGitHubWebhookItems(ctx, int64(githubIntakeStaleAfter/time.Second))
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		slog.Error("github: intake item stale", "alert", "github_intake_stale", "item", item.Item, "ref", item.Ref,
			"state", item.State, "reason", item.Reason, "age_seconds", item.AgeSeconds)
	}
	return len(items), nil
}
