package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Exact-snapshot approval of an `unsigned` or `quarantined` revision (CHE-2126
// plan 5.2 and 5.3, R3).
// An approver reads one snapshot (GET), then signs its hash (POST). The server
// reads again and approves only when the hash still matches. Settling then reads
// a third time: a body changed after approval returns the item to `quarantined`.

type githubSnapshotReader func(ctx context.Context, installationID int64, owner, repo string, number int32, objectKind string, objectID int64) (ghsnapshot.ApprovalSnapshot, error)

const githubApprovedBatch = 50

var errGitHubApprovalConflict = errors.New("github: revision no longer matches the approved snapshot")

type githubApprovalContext struct {
	revision db.GithubWebhookRevision
	pull     db.GithubPullRequest
	agent    db.Agent
	issues   []db.Issue
}

func (h *Handler) readGitHubSnapshot(ctx context.Context, revision db.GithubWebhookRevision, pull db.GithubPullRequest) (ghsnapshot.ApprovalSnapshot, error) {
	read := h.readApprovalSnapshot
	if read == nil {
		read = h.PRRefresh.ReadApprovalSnapshot
	}
	return read(ctx, pull.InstallationID, pull.RepoOwner, pull.RepoName, revision.PrNumber, revision.ObjectKind, revision.ObjectID)
}

func githubApprovable(state string) bool { return state == "unsigned" || state == "quarantined" }

// snapshotMatchesRevision is true when the live object is still the one the reconciler recorded.
func snapshotMatchesRevision(snapshot ghsnapshot.ApprovalSnapshot, revision db.GithubWebhookRevision) bool {
	return snapshot.ObjectKind == revision.ObjectKind && snapshot.ObjectID == revision.ObjectID &&
		snapshot.AuthorID == revision.AuthorID && snapshot.BodyHash == revision.BodyHash && snapshot.ReviewState == revision.ReviewState
}

// loadGitHubApproval authenticates a workspace member and loads the revision with the
// PR, its author agent and linked issues. The caller must pass can_route on at least
// one linked issue; it writes the error response and returns false otherwise.
func (h *Handler) loadGitHubApproval(w http.ResponseWriter, r *http.Request) (githubApprovalContext, pgtype.UUID, bool) {
	var loaded githubApprovalContext
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return loaded, pgtype.UUID{}, false
	}
	revisionID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "revision id")
	if !ok {
		return loaded, pgtype.UUID{}, false
	}
	ctx := r.Context()
	revision, err := h.Queries.GetGitHubWebhookRevision(ctx, db.GetGitHubWebhookRevisionParams{WorkspaceID: member.WorkspaceID, ID: revisionID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "revision not found")
		return loaded, pgtype.UUID{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load revision")
		return loaded, pgtype.UUID{}, false
	}
	if !githubApprovable(revision.State) {
		writeError(w, http.StatusConflict, "only an unsigned or quarantined revision can be approved")
		return loaded, pgtype.UUID{}, false
	}
	workspace, err := h.Queries.GetWorkspace(ctx, revision.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return loaded, pgtype.UUID{}, false
	}
	if !githubFeaturesEnabled(workspace) {
		writeError(w, http.StatusForbidden, "the GitHub integration is disabled")
		return loaded, pgtype.UUID{}, false
	}
	pull, err := h.Queries.GetGitHubPullRequestInWorkspace(ctx, db.GetGitHubPullRequestInWorkspaceParams{ID: revision.PullRequestID, WorkspaceID: revision.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pull request")
		return loaded, pgtype.UUID{}, false
	}
	allowed, err := h.newGitHubPolicyRun().workspaceAllowed(ctx, pull.InstallationID, revision.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check the installation allowlist")
		return loaded, pgtype.UUID{}, false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "the GitHub installation is not allowed")
		return loaded, pgtype.UUID{}, false
	}
	agent, err := service.GitHubPullRequestAuthor(ctx, h.Queries, pull)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "pull request has no unique author agent")
		return loaded, pgtype.UUID{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pull request author")
		return loaded, pgtype.UUID{}, false
	}
	issues, err := h.Queries.ListGitHubPullRequestIssues(ctx, db.ListGitHubPullRequestIssuesParams{WorkspaceID: revision.WorkspaceID, PullRequestID: pull.ID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load linked issues")
		return loaded, pgtype.UUID{}, false
	}
	for _, issue := range issues {
		if h.canRouteMember(ctx, member.UserID, issue, agent).Verdict == routeAllowed {
			return githubApprovalContext{revision: revision, pull: pull, agent: agent, issues: issues}, member.UserID, true
		}
	}
	writeError(w, http.StatusForbidden, "you may not route work on this pull request")
	return loaded, pgtype.UUID{}, false
}

// readCurrentSnapshot reads the object and requires it to still be the recorded revision.
func (h *Handler) readCurrentSnapshot(w http.ResponseWriter, r *http.Request, loaded githubApprovalContext) (ghsnapshot.ApprovalSnapshot, bool) {
	snapshot, err := h.readGitHubSnapshot(r.Context(), loaded.revision, loaded.pull)
	if err != nil {
		slog.Warn("github: approval snapshot read failed", "revision_id", uuidToString(loaded.revision.ID), "reason", "github_api_error")
		writeError(w, http.StatusBadGateway, "could not read the GitHub object")
		return ghsnapshot.ApprovalSnapshot{}, false
	}
	if !snapshotMatchesRevision(snapshot, loaded.revision) {
		writeError(w, http.StatusConflict, "the GitHub object changed since it was recorded")
		return ghsnapshot.ApprovalSnapshot{}, false
	}
	return snapshot, true
}

type githubSnapshotResponse struct {
	RevisionID   string     `json:"revision_id"`
	ObjectKind   string     `json:"object_kind"`
	ObjectID     int64      `json:"object_id"`
	AuthorID     int64      `json:"author_id"`
	BodyHash     string     `json:"body_hash"`
	ReviewState  string     `json:"review_state"`
	EditorType   string     `json:"editor_type"`
	EditorID     int64      `json:"editor_id"`
	LastEditedAt *time.Time `json:"last_edited_at"`
	ReadAt       time.Time  `json:"read_at"`
	SnapshotHash string     `json:"snapshot_hash"`
}

// GetGitHubRevisionSnapshot (GET /api/github/intake/revisions/{id}/snapshot) reads the
// object now and returns the hash the approver would sign. It carries no GitHub text.
func (h *Handler) GetGitHubRevisionSnapshot(w http.ResponseWriter, r *http.Request) {
	loaded, _, ok := h.loadGitHubApproval(w, r)
	if !ok {
		return
	}
	snapshot, ok := h.readCurrentSnapshot(w, r, loaded)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, githubSnapshotResponse{
		RevisionID: uuidToString(loaded.revision.ID), ObjectKind: snapshot.ObjectKind, ObjectID: snapshot.ObjectID, AuthorID: snapshot.AuthorID,
		BodyHash: snapshot.BodyHash, ReviewState: snapshot.ReviewState, EditorType: snapshot.EditorType, EditorID: snapshot.EditorID,
		LastEditedAt: snapshot.LastEditedAt, ReadAt: snapshot.ReadAt, SnapshotHash: snapshot.Hash,
	})
}

// ApproveGitHubRevisionSnapshot (POST /api/github/intake/revisions/{id}/approve) records
// the approver and the signed hash, then settles the revision.
func (h *Handler) ApproveGitHubRevisionSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SnapshotHash string `json:"snapshot_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SnapshotHash == "" {
		writeError(w, http.StatusBadRequest, "snapshot_hash is required")
		return
	}
	loaded, approver, ok := h.loadGitHubApproval(w, r)
	if !ok {
		return
	}
	snapshot, ok := h.readCurrentSnapshot(w, r, loaded)
	if !ok {
		return
	}
	if snapshot.Hash != req.SnapshotHash {
		writeError(w, http.StatusConflict, "the snapshot changed since you read it")
		return
	}
	if err := h.recordGitHubApproval(r.Context(), loaded.revision, approver, snapshot); err != nil {
		if errors.Is(err, errGitHubApprovalConflict) {
			writeError(w, http.StatusConflict, "the revision was already approved or settled")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to record approval")
		return
	}
	// A failed settle leaves the revision `deferred`; the reconciler retries it.
	if err := h.settleApprovedRevision(r.Context(), loaded.revision.WorkspaceID, loaded.revision.ID); err != nil {
		slog.Warn("github: approved revision not yet settled", "revision_id", uuidToString(loaded.revision.ID), "reason", "settle_error")
	}
	settled, err := h.Queries.GetGitHubWebhookRevision(r.Context(), db.GetGitHubWebhookRevisionParams{WorkspaceID: loaded.revision.WorkspaceID, ID: loaded.revision.ID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read revision")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"revision_id": uuidToString(settled.ID), "state": settled.State, "reason": settled.Reason})
}

func (h *Handler) recordGitHubApproval(ctx context.Context, revision db.GithubWebhookRevision, approver pgtype.UUID, snapshot ghsnapshot.ApprovalSnapshot) error {
	transaction, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	queries := h.Queries.WithTx(transaction)
	locked, err := queries.GetGitHubWebhookRevisionForUpdate(ctx, db.GetGitHubWebhookRevisionForUpdateParams{WorkspaceID: revision.WorkspaceID, ID: revision.ID})
	if err != nil {
		return err
	}
	if !githubApprovable(locked.State) {
		return errGitHubApprovalConflict
	}
	inserted, err := queries.InsertGitHubSnapshotApproval(ctx, db.InsertGitHubSnapshotApprovalParams{
		WorkspaceID: revision.WorkspaceID, RevisionID: revision.ID, ApproverUserID: approver, SnapshotHash: snapshot.Hash,
		ObjectKind: snapshot.ObjectKind, ObjectID: snapshot.ObjectID, AuthorID: snapshot.AuthorID, BodyHash: snapshot.BodyHash,
		ReviewState: snapshot.ReviewState, EditorType: snapshot.EditorType, EditorID: snapshot.EditorID,
		LastEditedAt: timestamptzFromPtr(snapshot.LastEditedAt), ReadAt: pgtype.Timestamptz{Time: snapshot.ReadAt, Valid: true},
	})
	if err != nil {
		return err
	}
	if inserted != 1 {
		return errGitHubApprovalConflict
	}
	if err := queries.SetGitHubWebhookRevisionState(ctx, db.SetGitHubWebhookRevisionStateParams{
		WorkspaceID: revision.WorkspaceID, ID: revision.ID, State: "deferred", Reason: "approved_snapshot",
	}); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func timestamptzFromPtr(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}

// settleApprovedRevision re-reads the approved object. A changed snapshot
// quarantines the revision; an unchanged one is routed like a signed revision, under
// the same can_route checks for both the approver and the GitHub author. A read or
// lookup error returns without a state change so the reconciler retries.
func (h *Handler) settleApprovedRevision(ctx context.Context, workspaceID, revisionID pgtype.UUID) error {
	revision, err := h.Queries.GetGitHubWebhookRevision(ctx, db.GetGitHubWebhookRevisionParams{WorkspaceID: workspaceID, ID: revisionID})
	if err != nil {
		return err
	}
	if revision.State != "deferred" || revision.Reason != "approved_snapshot" {
		return nil
	}
	approval, err := h.Queries.GetGitHubSnapshotApproval(ctx, db.GetGitHubSnapshotApprovalParams{WorkspaceID: workspaceID, RevisionID: revisionID})
	if err != nil {
		return err
	}
	pull, err := h.Queries.GetGitHubPullRequestInWorkspace(ctx, db.GetGitHubPullRequestInWorkspaceParams{ID: revision.PullRequestID, WorkspaceID: workspaceID})
	if err != nil {
		return err
	}
	// The master switch and the installation allowlist are read before the GitHub call
	// and again under the lock.
	workspace, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	enabled := githubFeaturesEnabled(workspace)
	// A binding excluded after the approval was stored must not cause a GitHub read.
	allowedBefore, err := h.newGitHubPolicyRun().workspaceAllowed(ctx, pull.InstallationID, workspaceID)
	if err != nil {
		return err
	}
	var snapshot ghsnapshot.ApprovalSnapshot
	if enabled && allowedBefore {
		if snapshot, err = h.readGitHubSnapshot(ctx, revision, pull); err != nil {
			return err
		}
	}

	transaction, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	queries := h.Queries.WithTx(transaction)
	locked, err := queries.GetGitHubWebhookRevisionForUpdate(ctx, db.GetGitHubWebhookRevisionForUpdateParams{WorkspaceID: workspaceID, ID: revisionID})
	if err != nil {
		return err
	}
	if locked.State != "deferred" || locked.Reason != "approved_snapshot" {
		return nil
	}
	workspace, err = queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	bindings, err := queries.ListGitHubInstallationsByInstallationIDForShare(ctx, pull.InstallationID)
	if err != nil {
		return err
	}
	// Policy off allows every installation, bound or not (as workspaceAllowed does).
	allowedNow := h.githubAllowlist.ids == nil
	for _, binding := range bindings {
		allowedNow = allowedNow || (binding.WorkspaceID == workspaceID && h.githubInstallationAllowed(binding))
	}
	state, reason := "quarantined", "approved_snapshot_changed"
	var publications []githubIntakePublication
	switch {
	case !githubFeaturesEnabled(workspace):
		state, reason = "denied", "integration_disabled"
	case !allowedNow:
		state, reason = "denied", "installation_not_allowed"
	case !enabled || !allowedBefore:
		return errors.New("github: integration or installation allowed since the snapshot read; retry")
	case snapshot.Hash == approval.SnapshotHash && snapshotMatchesRevision(snapshot, revision):
		state, reason, publications, err = h.routeApprovedRevision(ctx, queries, revision, pull, approval.ApproverUserID)
		if err != nil {
			return err
		}
	}
	if err := queries.SetGitHubWebhookRevisionState(ctx, db.SetGitHubWebhookRevisionStateParams{
		WorkspaceID: workspaceID, ID: revisionID, State: state, Reason: reason,
	}); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	slog.Info("github: approved revision settled", "revision_id", uuidToString(revisionID), "state", state, "reason", reason)
	h.publishGitHubIntake(ctx, publications)
	return nil
}

// routeApprovedRevision dispatches to each linked issue the approver may route on.
// Gate G stays closed: the intake worker's own notify-only projection applies.
func (h *Handler) routeApprovedRevision(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision, pull db.GithubPullRequest, approver pgtype.UUID) (string, string, []githubIntakePublication, error) {
	agent, err := service.GitHubPullRequestAuthor(ctx, queries, pull)
	if errors.Is(err, pgx.ErrNoRows) {
		return "denied", "no_unique_pr_author", nil, nil
	}
	if err != nil {
		return "", "", nil, err
	}
	issues, err := queries.ListGitHubPullRequestIssues(ctx, db.ListGitHubPullRequestIssuesParams{WorkspaceID: revision.WorkspaceID, PullRequestID: pull.ID})
	if err != nil {
		return "", "", nil, err
	}
	transactionHandler := *h
	transactionHandler.Queries = queries
	worker := githubIntakeWorker{handler: h}
	priority := map[string]int{"denied": 0, "dispatched": 1, "projected": 2, "unsigned": 3, "quarantined": 4, "deferred": 5}
	state, reason := "denied", "approver_not_routable"
	var publications []githubIntakePublication
	for _, issue := range issues {
		approverRoute := transactionHandler.canRouteMember(ctx, approver, issue, agent)
		if approverRoute.Verdict == routeDeferred {
			return "", "", nil, errors.New("github: approver route lookup failed")
		}
		if approverRoute.Verdict != routeAllowed {
			continue
		}
		route := transactionHandler.canRoute(ctx, revision.AuthorID, issue, agent)
		publication, err := worker.dispatch(ctx, queries, revision, githubIntakeTarget{IssueID: issue.ID, AgentID: agent.ID, UserID: route.UserID, Route: route})
		if err != nil {
			return "", "", nil, err
		}
		if priority[publication.state] >= priority[state] {
			state, reason = publication.state, publication.reason
		}
		if publication.comment.ID.Valid {
			publications = append(publications, *publication)
		}
	}
	if state == "deferred" {
		return "", "", nil, errors.New("github: approved revision route deferred")
	}
	return state, reason, publications, nil
}

// settleApproved retries approvals whose settle step failed or never ran.
func (reconciler githubReconciler) settleApproved(ctx context.Context) (int, error) {
	approved, err := reconciler.handler.Queries.ListApprovedGitHubRevisions(ctx, githubApprovedBatch)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, item := range approved {
		if err := reconciler.handler.settleApprovedRevision(ctx, item.WorkspaceID, item.ID); err != nil {
			slog.Warn("github: approved revision settle failed", "revision_id", uuidToString(item.ID), "reason", "settle_error")
			continue
		}
		settled++
	}
	return settled, nil
}
