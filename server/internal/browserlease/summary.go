package browserlease

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Lease summary ingest (CHE-4745, W3-16; plan W1 r3, F6; contract for the gateway's W2-10).
//
// At lease end the gateway posts one summary of what the lease did: counts and bytes only, never
// URLs, page content or credentials. Multica stores it on the lease row, write-once:
//
//	POST /internal/browser/leases/{lease_id}/summary   (install-token auth, internal listener only)
//	{"epoch":3,"requests":120,"denials":4,"bytes_in":1048576,"bytes_out":2048,
//	 "first_request_at":"2026-11-18T06:00:00Z","last_request_at":"2026-11-18T06:09:30Z"}
//
// 200 stored, or the same summary again. 400 invalid body. 401 no valid install token. 404 no such
// lease in the token's workspace. 409 lease_not_ended (retry once the lease has ended),
// stale_epoch (the lease was granted at another epoch) or summary_conflict (a different summary
// is already stored; the first one stays).
const SummaryPath = "/internal/browser/leases/{lease_id}/summary"

var (
	ErrLeaseNotFound   = errors.New("browserlease: lease not found")
	ErrSummaryConflict = errors.New("browserlease: a different summary is already stored")
	ErrInvalidSummary  = errors.New("browserlease: invalid summary")
)

// LeaseSummary is the stored and wire form. Epoch is the lease's grant epoch, the same fence the
// gateway uses for cleanup-ack: a summary for another epoch is refused.
type LeaseSummary struct {
	Epoch          int64      `json:"epoch"`
	Requests       int64      `json:"requests"`
	Denials        int64      `json:"denials"`
	BytesIn        int64      `json:"bytes_in"`
	BytesOut       int64      `json:"bytes_out"`
	FirstRequestAt *time.Time `json:"first_request_at,omitempty"`
	LastRequestAt  *time.Time `json:"last_request_at,omitempty"`
}

func (s LeaseSummary) validate() error {
	if s.Epoch <= 0 || s.Requests < 0 || s.Denials < 0 || s.BytesIn < 0 || s.BytesOut < 0 {
		return ErrInvalidSummary
	}
	if s.FirstRequestAt != nil && s.LastRequestAt != nil && s.LastRequestAt.Before(*s.FirstRequestAt) {
		return ErrInvalidSummary
	}
	return nil
}

// same reports whether two summaries say the same thing; instants compare as instants.
func (s LeaseSummary) same(o LeaseSummary) bool {
	eq := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == b
		}
		return a.Equal(*b)
	}
	return s.Epoch == o.Epoch && s.Requests == o.Requests && s.Denials == o.Denials &&
		s.BytesIn == o.BytesIn && s.BytesOut == o.BytesOut &&
		eq(s.FirstRequestAt, o.FirstRequestAt) && eq(s.LastRequestAt, o.LastRequestAt)
}

type SummaryService struct{ pool *pgxpool.Pool }

func NewSummaryService(pool *pgxpool.Pool) *SummaryService { return &SummaryService{pool: pool} }

// Store writes sum on the ended lease. The write is one conditional UPDATE, so two racing first
// posts cannot both win; the loser reads the winner back and is idempotent or conflicts.
func (s *SummaryService) Store(ctx context.Context, ws, lease pgtype.UUID, sum LeaseSummary) error {
	if err := sum.validate(); err != nil {
		return err
	}
	body, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	q := db.New(s.pool)
	_, err = q.SetBrowserLeaseSummary(ctx, db.SetBrowserLeaseSummaryParams{
		ID: lease, WorkspaceID: ws, Summary: body, Epoch: pgtype.Int8{Int64: sum.Epoch, Valid: true},
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	row, err := q.GetBrowserLease(ctx, db.GetBrowserLeaseParams{ID: lease, WorkspaceID: ws})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrLeaseNotFound
	case err != nil:
		return err
	case row.State != "ended":
		return ErrLeaseActive
	case !row.Epoch.Valid || row.Epoch.Int64 != sum.Epoch:
		return ErrStaleEpoch
	case row.Summary == nil:
		return errors.New("browserlease: summary write refused for an ended lease at its epoch")
	}
	var have LeaseSummary
	if err := json.Unmarshal(row.Summary, &have); err != nil {
		return err
	}
	if !have.same(sum) {
		return ErrSummaryConflict
	}
	return nil
}

// Mount registers the route on r, which serves internalPrefix. auth is required.
func (s *SummaryService) Mount(auth Authenticator) func(chi.Router) {
	if auth == nil {
		panic("browserlease: Mount needs an Authenticator")
	}
	return func(r chi.Router) {
		r.Post("/leases/{lease_id}/summary", s.handle(auth))
	}
}

// Routes serves the same route at its full path, for a handler under test.
func (s *SummaryService) Routes(auth Authenticator) http.Handler {
	r := chi.NewRouter()
	r.Route(internalPrefix, s.Mount(auth))
	return r
}

func (s *SummaryService) handle(auth Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, ok := auth(r)
		if !ok || !ws.Valid {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		lease, err := util.ParseUUID(chi.URLParam(r, "lease_id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		var sum LeaseSummary
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sum); err != nil || sum.validate() != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		if err := s.Store(r.Context(), ws, lease, sum); err != nil {
			status, code := summaryError(err)
			if status == http.StatusInternalServerError {
				slog.Error("browser lease summary", "error", err)
			}
			writeJSON(w, status, map[string]string{"error": code})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lease_id": util.UUIDToString(lease), "epoch": sum.Epoch})
	}
}

func summaryError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalidSummary):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, ErrLeaseNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrLeaseActive):
		return http.StatusConflict, "lease_not_ended"
	case errors.Is(err, ErrStaleEpoch):
		return http.StatusConflict, "stale_epoch"
	case errors.Is(err, ErrSummaryConflict):
		return http.StatusConflict, "summary_conflict"
	}
	return http.StatusInternalServerError, "internal"
}
