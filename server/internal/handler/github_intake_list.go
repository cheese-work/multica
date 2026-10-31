package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// githubIntakeScanCap bounds the rows read per call. Rows of other workspaces
// are skipped after the read, so the response says when the cap cut the scan.
const githubIntakeScanCap = 2000

type githubIntakeDelivery struct {
	Event          string `json:"event"`
	DeliveryGUID   string `json:"delivery_guid"`
	State          string `json:"state"`
	Reason         string `json:"reason"`
	ReceivedAt     string `json:"received_at"`
	RevisionState  string `json:"revision_state"`
	RevisionReason string `json:"revision_reason"`
	Tasks          int32  `json:"tasks"`
}

// ListGitHubIntake (GET /api/github/intake?since=<RFC3339>&limit=<1-500>) reads
// delivery metadata for the notify-only check (CHE-2669). Read-only; payload and
// signature are never selected.
func (h *Handler) ListGitHubIntake(w http.ResponseWriter, r *http.Request) {
	since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "since is required (RFC3339)")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 500 {
			writeError(w, http.StatusBadRequest, "limit must be 1-500")
			return
		}
	}
	workspaceID := parseUUID(h.resolveWorkspaceID(r))
	rows, err := h.Queries.ListGitHubWebhookIntakeScan(r.Context(), db.ListGitHubWebhookIntakeScanParams{
		ReceivedAt: pgtype.Timestamptz{Time: since, Valid: true}, Limit: githubIntakeScanCap,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read intake")
		return
	}
	out := make([]githubIntakeDelivery, 0, limit)
	for _, row := range rows {
		if len(out) == limit {
			break
		}
		revision, ok, err := h.githubIntakeRevision(r.Context(), workspaceID, row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read intake")
			return
		}
		if !ok {
			continue // no revision in this workspace: not ours to show
		}
		out = append(out, githubIntakeDelivery{
			Event: row.Event, DeliveryGUID: row.DeliveryGuid, State: row.State, Reason: row.Reason,
			ReceivedAt: timestampToString(row.ReceivedAt), RevisionState: revision.State,
			RevisionReason: revision.Reason, Tasks: revision.Tasks,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out, "truncated": len(rows) == githubIntakeScanCap && len(out) < limit})
}

// githubIntakeRevision finds the workspace's revision for a delivery by the same
// identity the worker uses, so a redelivery (new GUID, same content) resolves to
// its canonical revision. A payload that does not parse, or has no revision in
// this workspace, is not associated with it.
func (h *Handler) githubIntakeRevision(ctx context.Context, workspaceID pgtype.UUID, row db.ListGitHubWebhookIntakeScanRow) (db.FindGitHubWebhookRevisionForIntakeRow, bool, error) {
	var payload githubIntakePayload
	if json.Unmarshal(row.Payload, &payload) != nil {
		return db.FindGitHubWebhookRevisionForIntakeRow{}, false, nil
	}
	object, kind, _, invalid := payload.revision(row.Event)
	if invalid != "" {
		return db.FindGitHubWebhookRevisionForIntakeRow{}, false, nil
	}
	digest := sha256.Sum256([]byte(object.Body))
	revision, err := h.Queries.FindGitHubWebhookRevisionForIntake(ctx, db.FindGitHubWebhookRevisionForIntakeParams{
		WorkspaceID: workspaceID, InstallationID: payload.Installation.ID, RepositoryID: payload.Repository.ID,
		ObjectKind: kind, ObjectID: object.ID, BodyHash: hex.EncodeToString(digest[:]),
		Event: row.Event, Action: payload.Action, SenderID: payload.Sender.ID, AuthorID: object.User.ID, ReviewState: object.State,
		ObjectCreatedAt: githubIntakeTimestamp(object.CreatedAt), ObjectUpdatedAt: githubIntakeTimestamp(object.UpdatedAt),
		ObjectSubmittedAt: githubIntakeTimestamp(object.SubmittedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return revision, false, nil
	}
	return revision, err == nil, err
}
