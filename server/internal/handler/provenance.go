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
// issue and comment-thread rows as they stood at a cutoff, and records an
// audit row before any of it leaves the server. Human owners/admins only.
func (h *Handler) ExportProvenance(w http.ResponseWriter, r *http.Request) {
	// Router applies RequireHumanActor too; this backstop keeps the handler
	// fail-closed if it is ever mounted without that middleware.
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
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
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctxWS := h.resolveWorkspaceID(r)
	ctxWSUUID, ctxErr := util.ParseUUID(ctxWS)
	reqWSUUID, reqErr := util.ParseUUID(strings.TrimSpace(req.WorkspaceID))
	if ctxErr != nil || reqErr != nil || ctxWSUUID != reqWSUUID {
		writeError(w, http.StatusBadRequest, "workspace_id must match the request workspace")
		return
	}
	cutoff, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Cutoff))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cutoff must be RFC3339")
		return
	}
	if cutoff.After(time.Now()) {
		writeError(w, http.StatusBadRequest, "cutoff must not be in the future")
		return
	}
	if len(req.Issues) == 0 && len(req.Threads) == 0 {
		writeError(w, http.StatusBadRequest, "at least one issue or thread source is required")
		return
	}
	if len(req.Issues)+len(req.Threads) > service.ProvenanceMaxSources {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d sources per export", service.ProvenanceMaxSources))
		return
	}

	workspaceID := util.UUIDToString(ctxWSUUID)
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	export, err := h.collectProvenance(r.Context(), ctxWSUUID, req, cutoff)
	if err != nil {
		slog.Error("provenance export: collect failed", "workspace_id", workspaceID, "error", err)
		writeError(w, http.StatusInternalServerError, "provenance export failed")
		return
	}

	requestDigest, err := service.ProvenanceRequestDigest(workspaceID, req.Issues, req.Threads, cutoff)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provenance export failed")
		return
	}
	manifest, manifestDigest, err := export.Manifest()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provenance export failed")
		return
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provenance export failed")
		return
	}
	actorUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user not authenticated")
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
		SourceCount:    int32(len(req.Issues) + len(req.Threads)),
		IncludedCount:  int32(len(manifest.Records)),
		ExcludedCount:  int32(len(manifest.Exclusions)),
		Manifest:       manifestJSON,
	})
	if err != nil {
		slog.Error("provenance export: audit log insert failed", "workspace_id", workspaceID, "error", err)
		writeError(w, http.StatusInternalServerError, "provenance export failed")
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
