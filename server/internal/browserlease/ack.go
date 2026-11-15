package browserlease

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Handover fence (CHE-4736, W3-7; plan W1 r3, "Lease handover (fenced)").
//
// An ended lease keeps its instance until the gateway sends cleanup-ack(E) for the instance's
// current epoch: the gateway reset the instance and its S0 probe came back empty. Only then can
// the next queued lease be granted. A gateway that cannot prove the instance empty reports
// quarantine instead, and Multica grants nothing until a later ack clears it.
//
// W3-3 (mint and queue) must call CanGrant before every grant. W3-17 mounts Routes on the
// internal listener with the install-token authenticator; the public and tailnet routers do
// not mount it.
const (
	CleanupAckPath = "/internal/browser/instances/{instance_id}/cleanup-ack"
	QuarantinePath = "/internal/browser/instances/{instance_id}/quarantine"
)

const (
	phaseIdleClean   = "idle_clean"
	phaseQuarantined = "quarantined"
)

var (
	ErrInstanceNotFound = errors.New("browserlease: instance not found")
	ErrStaleEpoch       = errors.New("browserlease: epoch is not the instance's current epoch")
	ErrLeaseActive      = errors.New("browserlease: lease has not ended")
	ErrQuarantined      = errors.New("browserlease: instance is quarantined")
	ErrNoPendingLease   = errors.New("browserlease: no lease is waiting for cleanup-ack")
)

// Authenticator resolves the workspace of a gateway request, or reports it unauthenticated.
type Authenticator func(r *http.Request) (workspaceID pgtype.UUID, ok bool)

type AckResult struct {
	InstanceID pgtype.UUID
	Epoch      int64
	Phase      string
}

type AckService struct{ pool *pgxpool.Pool }

func NewAckService(pool *pgxpool.Pool) *AckService { return &AckService{pool: pool} }

// withLockedInstance runs fn in a transaction holding the instance row lock, after checking that
// epoch is the instance's current one.
func (s *AckService) withLockedInstance(ctx context.Context, ws, instance pgtype.UUID, epoch int64,
	fn func(q *db.Queries, inst db.BrowserInstance) (string, error)) (AckResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AckResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	q := db.New(tx)
	inst, err := q.LockBrowserInstance(ctx, db.LockBrowserInstanceParams{ID: instance, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return AckResult{}, ErrInstanceNotFound
	}
	if err != nil {
		return AckResult{}, err
	}
	if inst.Epoch != epoch {
		return AckResult{}, ErrStaleEpoch
	}
	phase, err := fn(q, inst)
	if err != nil {
		return AckResult{}, err
	}
	if phase != inst.Phase {
		if _, err := q.SetBrowserInstancePhase(ctx, db.SetBrowserInstancePhaseParams{ID: instance, WorkspaceID: ws, Phase: phase}); err != nil {
			return AckResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AckResult{}, err
	}
	return AckResult{InstanceID: instance, Epoch: epoch, Phase: phase}, nil
}

// CleanupAck records that the gateway reset instance and proved it empty for epoch. It stamps
// the ended lease holding the instance and sets the phase idle_clean, which also clears a
// quarantine. A repeat for an already acked epoch succeeds. An ack for a lease that has not
// ended, or for a quarantined instance with nothing waiting, is refused.
func (s *AckService) CleanupAck(ctx context.Context, ws, instance pgtype.UUID, epoch int64) (AckResult, error) {
	return s.withLockedInstance(ctx, ws, instance, epoch, func(q *db.Queries, inst db.BrowserInstance) (string, error) {
		holder, err := q.GetBrowserInstanceHolder(ctx, db.GetBrowserInstanceHolderParams{InstanceID: inst.ID, WorkspaceID: inst.WorkspaceID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			switch inst.Phase {
			case phaseIdleClean:
				return phaseIdleClean, nil // retry of an ack that landed
			case phaseQuarantined:
				return "", ErrQuarantined
			}
			return "", ErrNoPendingLease
		case err != nil:
			return "", err
		case holder.State != "ended":
			return "", ErrLeaseActive
		}
		if _, err := q.AckBrowserLeaseCleanup(ctx, db.AckBrowserLeaseCleanupParams{InstanceID: inst.ID, WorkspaceID: inst.WorkspaceID, Epoch: pgtype.Int8{Int64: epoch, Valid: true}}); err != nil {
			return "", err // the holder is ended and unacked at this epoch under the row lock
		}
		return phaseIdleClean, nil
	})
}

// Quarantine records that the gateway could not prove instance empty for epoch (failed step,
// non-empty probe, restart from a state that was not idle_clean). Grants stop until CleanupAck.
func (s *AckService) Quarantine(ctx context.Context, ws, instance pgtype.UUID, epoch int64) (AckResult, error) {
	res, err := s.withLockedInstance(ctx, ws, instance, epoch, func(*db.Queries, db.BrowserInstance) (string, error) {
		return phaseQuarantined, nil
	})
	if err == nil {
		slog.Warn("browser instance quarantined", "instance_id", util.UUIDToString(instance), "epoch", epoch)
	}
	return res, err
}

// CanGrant reports whether the next queued lease may be granted instance: it is idle_clean and
// no lease holds it (none granted, none ended without cleanup-ack).
func (s *AckService) CanGrant(ctx context.Context, ws, instance pgtype.UUID) (bool, error) {
	q := db.New(s.pool)
	inst, err := q.GetBrowserInstance(ctx, db.GetBrowserInstanceParams{ID: instance, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrInstanceNotFound
	}
	if err != nil {
		return false, err
	}
	if inst.Phase != phaseIdleClean {
		return false, nil
	}
	_, err = q.GetBrowserInstanceHolder(ctx, db.GetBrowserInstanceHolderParams{InstanceID: instance, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// Routes serves the two gateway-facing POST routes. auth is required: a state-changing route is
// never served unauthenticated.
func (s *AckService) Routes(auth Authenticator) http.Handler {
	if auth == nil {
		panic("browserlease: Routes needs an Authenticator")
	}
	r := chi.NewRouter()
	r.Post(CleanupAckPath, s.handle(auth, s.CleanupAck))
	r.Post(QuarantinePath, s.handle(auth, s.Quarantine))
	return r
}

type epochRequest struct {
	Epoch int64 `json:"epoch"`
}

func (s *AckService) handle(auth Authenticator, op func(context.Context, pgtype.UUID, pgtype.UUID, int64) (AckResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, ok := auth(r)
		if !ok || !ws.Valid {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		instance, err := util.ParseUUID(chi.URLParam(r, "instance_id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		var req epochRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || req.Epoch <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		res, err := op(r.Context(), ws, instance, req.Epoch)
		if status, code := ackError(err); err != nil {
			if status == http.StatusInternalServerError {
				slog.Error("browser lease ack", "error", err)
			}
			writeJSON(w, status, map[string]string{"error": code})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"instance_id": util.UUIDToString(res.InstanceID), "epoch": res.Epoch, "phase": res.Phase,
		})
	}
}

func ackError(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, ErrInstanceNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrStaleEpoch):
		return http.StatusConflict, "stale_epoch"
	case errors.Is(err, ErrLeaseActive):
		return http.StatusConflict, "lease_not_ended"
	case errors.Is(err, ErrQuarantined):
		return http.StatusConflict, "quarantined"
	case errors.Is(err, ErrNoPendingLease):
		return http.StatusConflict, "no_pending_lease"
	}
	return http.StatusInternalServerError, "internal"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
