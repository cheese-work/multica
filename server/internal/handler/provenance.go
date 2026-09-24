package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const provenanceExportBodyLimit = 256 << 10

// maxUUID bounds ListCommentThreadHistory's (created_at, id) keyset so the
// tuple comparison degenerates to created_at <= cutoff.
var maxUUID = pgtype.UUID{Bytes: [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, Valid: true}

type provenanceExportRequest struct {
	WorkspaceID string   `json:"workspace_id"`
	Issues      []string `json:"issues"`
	Threads     []string `json:"threads"`
	Cutoff      string   `json:"cutoff"`
}

type provenanceExportResponse struct {
	ExportID        string                        `json:"export_id"`
	RequestDigest   string                        `json:"request_digest"`
	ManifestDigest  string                        `json:"manifest_digest"`
	Cutoff          string                        `json:"cutoff"`
	GeneratedAt     string                        `json:"generated_at"`
	RedactionPolicy string                        `json:"redaction_policy"`
	Records         []service.ProvenanceRecord    `json:"records"`
	Exclusions      []service.ProvenanceExclusion `json:"exclusions"`
}

// ExportProvenance (CHE-755) returns a bounded, redacted, read-only package of
// issue and comment-thread rows created at or before a cutoff and unmodified
// since; a row modified after the cutoff is excluded, not reconstructed to an
// earlier state. Each record's revision_at_export is read at export time and
// is not verified as of the cutoff. It records an audit row before any of it
// leaves the server. Human owners/admins only.
func (h *Handler) ExportProvenance(w http.ResponseWriter, r *http.Request) {
	// No audit row is written for a rejected or failed attempt (row presence
	// means data left the server), so every such exit leaves a log line with
	// who asked, for which workspace, and why it was refused.
	actorID := requestUserID(r)
	logWS := r.Header.Get("X-Workspace-ID")
	reject := func(status int, reason, msg string, attrs ...any) {
		slog.Warn("provenance export: rejected", append([]any{
			"actor_id", actorID, "actor_source", r.Header.Get("X-Actor-Source"),
			"workspace_id", provenanceLogUUID(logWS), "reason", reason, "status", status,
		}, attrs...)...)
		writeError(w, status, msg)
	}
	fail := func(stage string, err error) {
		slog.Error("provenance export: failed", "actor_id", actorID, "workspace_id", provenanceLogUUID(logWS), "stage", stage, "error", err)
		writeError(w, http.StatusInternalServerError, "provenance export failed")
	}

	// Router applies RequireHumanActor too; this backstop keeps the handler
	// fail-closed if it is ever mounted without that middleware.
	if isMachineCredentialActor(r) {
		reject(http.StatusForbidden, "machine_credential", "this endpoint is only available to human actors")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, provenanceExportBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req provenanceExportRequest
	if err := dec.Decode(&req); err != nil {
		reject(http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	ctxWS := h.resolveWorkspaceID(r)
	logWS = ctxWS
	ctxWSUUID, ctxErr := util.ParseUUID(ctxWS)
	reqWSUUID, reqErr := util.ParseUUID(strings.TrimSpace(req.WorkspaceID))
	if ctxErr != nil || reqErr != nil || ctxWSUUID != reqWSUUID {
		reject(http.StatusBadRequest, "workspace_mismatch", "workspace_id must match the request workspace",
			"requested_workspace_id", provenanceLogUUID(req.WorkspaceID))
		return
	}
	cutoff, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Cutoff))
	if err != nil {
		reject(http.StatusBadRequest, "invalid_cutoff", "cutoff must be RFC3339")
		return
	}
	if cutoff.After(time.Now()) {
		reject(http.StatusBadRequest, "future_cutoff", "cutoff must not be in the future", "cutoff", cutoff.UTC().Format(time.RFC3339))
		return
	}
	sourceCount := len(req.Issues) + len(req.Threads)
	if sourceCount == 0 {
		reject(http.StatusBadRequest, "no_sources", "at least one issue or thread source is required")
		return
	}
	if sourceCount > service.ProvenanceMaxSources {
		reject(http.StatusBadRequest, "too_many_sources", fmt.Sprintf("at most %d sources per export", service.ProvenanceMaxSources),
			"source_count", sourceCount)
		return
	}

	workspaceID := util.UUIDToString(ctxWSUUID)
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		// requireWorkspaceRole already wrote the 404/403 and does not log.
		slog.Warn("provenance export: rejected", "actor_id", actorID, "actor_source", r.Header.Get("X-Actor-Source"),
			"workspace_id", workspaceID, "reason", "not_owner_or_admin")
		return
	}

	export, err := h.collectProvenance(r.Context(), ctxWSUUID, req, cutoff)
	if err != nil {
		fail("collect", err)
		return
	}

	requestDigest, err := service.ProvenanceRequestDigest(workspaceID, req.Issues, req.Threads, cutoff)
	if err != nil {
		fail("request_digest", err)
		return
	}
	manifest, manifestDigest, err := export.Manifest()
	if err != nil {
		fail("manifest", err)
		return
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		fail("manifest_marshal", err)
		return
	}
	actorUUID, err := util.ParseUUID(userID)
	if err != nil {
		reject(http.StatusUnauthorized, "invalid_actor_id", "user not authenticated")
		return
	}

	// The audit row is the precondition for releasing data: no row, no export.
	logRow, err := h.Queries.InsertProvenanceExportLog(r.Context(), db.InsertProvenanceExportLogParams{
		WorkspaceID:    ctxWSUUID,
		ActorType:      "member",
		ActorID:        actorUUID,
		RequestDigest:  requestDigest,
		ManifestDigest: manifestDigest,
		Cutoff:         pgtype.Timestamptz{Time: cutoff, Valid: true},
		SourceCount:    int32(sourceCount),
		IncludedCount:  int32(len(manifest.Records)),
		ExcludedCount:  int32(len(manifest.Exclusions)),
		Manifest:       manifestJSON,
	})
	if err != nil {
		fail("audit_insert", err)
		return
	}

	writeJSON(w, http.StatusOK, provenanceExportResponse{
		ExportID:        util.UUIDToString(logRow.ID),
		RequestDigest:   requestDigest,
		ManifestDigest:  manifestDigest,
		Cutoff:          cutoff.UTC().Format(time.RFC3339),
		GeneratedAt:     logRow.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		RedactionPolicy: service.ProvenanceRedactionPolicy,
		Records:         export.Records(),
		Exclusions:      export.Exclusions(),
	})
}

// provenanceLogUUID keeps caller-supplied workspace values out of logs unless
// they are a UUID, so a pasted secret in the field is never persisted.
func provenanceLogUUID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	id, err := util.ParseUUID(raw)
	if err != nil {
		return "invalid"
	}
	return util.UUIDToString(id)
}

// collectProvenance reads every source inside one read-only repeatable-read
// transaction: the database itself refuses writes, and all sources see the
// same snapshot.
func (h *Handler) collectProvenance(ctx context.Context, workspaceID pgtype.UUID, req provenanceExportRequest, cutoff time.Time) (*service.ProvenanceExport, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		return nil, fmt.Errorf("set read-only: %w", err)
	}
	q := h.Queries.WithTx(tx)

	ws, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	prefix := issuePrefixForWorkspace(ws)
	// source_task_id / origin_id have no FK, so a value can name a missing
	// task or one in another workspace; only a task this workspace owns counts
	// as provenance.
	export := service.NewProvenanceExport(cutoff, func(taskID pgtype.UUID) (bool, error) {
		_, err := q.GetAgentTaskInWorkspace(ctx, db.GetAgentTaskInWorkspaceParams{ID: taskID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("verify task provenance: %w", err)
		}
		return true, nil
	})

	for _, ref := range req.Issues {
		if err := collectIssueSource(ctx, q, export, workspaceID, prefix, strings.TrimSpace(ref)); err != nil {
			return nil, err
		}
	}
	for _, ref := range req.Threads {
		if err := collectThreadSource(ctx, q, export, workspaceID, strings.TrimSpace(ref)); err != nil {
			return nil, err
		}
	}
	return export, nil
}

func collectIssueSource(ctx context.Context, q *db.Queries, export *service.ProvenanceExport, workspaceID pgtype.UUID, prefix, ref string) error {
	var (
		issue db.Issue
		err   error
	)
	id, uuidErr := util.ParseUUID(ref)
	parts := splitIdentifier(ref)
	wellFormed := len(ref) <= service.ProvenanceMaxRefLength && (uuidErr == nil || parts != nil)
	// An identifier only has to look like PREFIX-N, so its bytes are still
	// caller-controlled (e.g. a secret shaped like one); only a UUID is echoed.
	source := service.ProvenanceSourceRef("issue", ref, wellFormed && uuidErr == nil)
	switch {
	case !wellFormed:
		export.Exclude(source, "", service.ProvenanceMalformed, 0)
		return nil
	case uuidErr == nil:
		issue, err = q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
	case !strings.EqualFold(parts.prefix, prefix):
		export.Exclude(source, "", service.ProvenanceNotFoundOrDenied, 0)
		return nil
	default:
		issue, err = q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: workspaceID, Number: parts.number})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		export.Exclude(source, "", service.ProvenanceNotFoundOrDenied, 0)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}

	included, err := export.AddIssue(source, issue)
	if err != nil || !included {
		return err
	}
	rows, err := q.ListCommentsForIssueUpTo(ctx, db.ListCommentsForIssueUpToParams{
		IssueID:     issue.ID,
		WorkspaceID: workspaceID,
		Cutoff:      pgtype.Timestamptz{Time: export.Cutoff(), Valid: true},
		RowLimit:    service.ProvenanceMaxCommentsPerSource + 1,
	})
	if err != nil {
		return fmt.Errorf("list issue comments: %w", err)
	}
	return export.AddComments(source, rows)
}

func collectThreadSource(ctx context.Context, q *db.Queries, export *service.ProvenanceExport, workspaceID pgtype.UUID, ref string) error {
	id, uuidErr := util.ParseUUID(ref)
	wellFormed := len(ref) <= service.ProvenanceMaxRefLength && uuidErr == nil
	source := service.ProvenanceSourceRef("thread", ref, wellFormed)
	if !wellFormed {
		export.Exclude(source, "", service.ProvenanceMalformed, 0)
		return nil
	}

	anchor, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		export.Exclude(source, "", service.ProvenanceNotFoundOrDenied, 0)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load thread anchor: %w", err)
	}
	if !anchor.CreatedAt.Valid || anchor.CreatedAt.Time.After(export.Cutoff()) {
		export.Exclude(source, util.UUIDToString(anchor.ID), service.ProvenanceOutOfCutoff, 0)
		return nil
	}

	root, err := q.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: anchor.ID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		export.Exclude(source, "", service.ProvenanceNotFoundOrDenied, 0)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load thread root: %w", err)
	}
	if root.WorkspaceID != workspaceID || root.IssueID != anchor.IssueID {
		export.Exclude(source, "", service.ProvenanceNotFoundOrDenied, 0)
		return nil
	}

	history, err := q.ListCommentThreadHistory(ctx, db.ListCommentThreadHistoryParams{
		RowLimit:        service.ProvenanceMaxCommentsPerSource + 1,
		RootID:          root.ID,
		WorkspaceID:     workspaceID,
		IssueID:         root.IssueID,
		AnchorCreatedAt: pgtype.Timestamptz{Time: export.Cutoff(), Valid: true},
		AnchorID:        maxUUID,
	})
	if err != nil {
		return fmt.Errorf("list thread history: %w", err)
	}
	rows := make([]db.Comment, len(history))
	for i, row := range history {
		rows[i] = db.Comment(row)
	}
	return export.AddComments(source, rows)
}
