package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/webhookguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The inbound workflow webhook (CHE-2031). The check order and the admission
// transaction are specified in docs/engineering/workflow-webhook-guard.md; the
// numbers in the comments below are that spec's steps.

const (
	// webhookQuotaWindow is the window rate_per_min counts nonces over.
	webhookQuotaWindow = time.Minute
	// webhookRetryAfter is what an over-quota or unavailable answer tells the
	// caller to wait, in seconds.
	webhookRetryAfter = "30"
)

var errWebhookLimiterMissing = errors.New("workflow webhook limiter not configured")

// webhookDummySecret stands in for the secret of an unknown or disabled
// endpoint, so those calls cost the same HMAC work as a real one.
var webhookDummySecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

type webhookOutcome int

const (
	webhookAccepted webhookOutcome = iota
	webhookIgnored
	webhookReplay
	webhookOverQuota
	webhookGone
)

func (h *Handler) HandleWorkflowWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := h.clientIPForRateLimit(r)

	// 1. Per-IP gates fail closed: a missing or erroring limiter refuses the
	//    call instead of letting it through unmetered.
	allowed, err := failClosedAllow(ctx, h.WebhookAbsoluteIPRateLimiter, ip)
	if err != nil {
		writeWebhookUnavailable(w, "absolute limiter", err)
		return
	}
	if !allowed {
		writeWebhookRateLimit(w, r, h.WebhookAbsoluteIPRateLimiter, ip, "absolute_ip", h.Metrics)
		return
	}
	allowed, err = failClosedCheck(ctx, h.WebhookIPRateLimiter, ip)
	if err != nil {
		writeWebhookUnavailable(w, "bad-credential limiter", err)
		return
	}
	if !allowed {
		writeWebhookRateLimit(w, r, h.WebhookIPRateLimiter, ip, "bad_credential_ip", h.Metrics)
		return
	}
	if h.VCSSecretBox == nil {
		writeWebhookUnavailable(w, "secret box", errors.New("MULTICA_VCS_SECRET_KEY unset"))
		return
	}

	// 2. Header shape and body cap, before any database read.
	timestamp, nonce, signature := r.Header.Get("X-Multica-Timestamp"), r.Header.Get("X-Multica-Nonce"), r.Header.Get("X-Multica-Signature")
	if webhookguard.CheckHeaders(timestamp, nonce, signature) != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webhookguard.MaxBodyBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	// 3-5. Endpoint, timestamp window and signature. Every failure gets the
	//      same answer and charges the caller's bad-credential debt.
	secrets, known, err := h.webhookSecrets(ctx, chi.URLParam(r, "endpoint_id"))
	if err != nil {
		writeWebhookUnavailable(w, "load endpoint", err)
		return
	}
	if verr := webhookguard.Verify(secrets, time.Now(), timestamp, nonce, signature, body); verr != nil || !known {
		h.WebhookIPRateLimiter.Allow(ctx, ip)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// 6. Strict payload parse.
	payload, err := webhookguard.ParsePayload(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	// 7. Admission.
	endpointID, _ := util.ParseUUID(chi.URLParam(r, "endpoint_id"))
	sum := sha256.Sum256(body)
	outcome, err := h.admitWorkflowWebhook(ctx, endpointID, nonce, payload, sum[:], len(body))
	if err != nil {
		writeWebhookUnavailable(w, "admission", err)
		return
	}
	switch outcome {
	case webhookAccepted:
		writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
	case webhookIgnored:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
	case webhookReplay:
		writeError(w, http.StatusConflict, "replay")
	case webhookOverQuota:
		w.Header().Set("Retry-After", webhookRetryAfter)
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
	default:
		h.WebhookIPRateLimiter.Allow(ctx, ip)
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
}

func writeWebhookUnavailable(w http.ResponseWriter, what string, err error) {
	slog.Error("workflow webhook unavailable", "stage", what, "error", err)
	w.Header().Set("Retry-After", webhookRetryAfter)
	writeError(w, http.StatusServiceUnavailable, "unavailable")
}

func failClosedAllow(ctx context.Context, limiter WebhookRateLimiter, key string) (bool, error) {
	if limiter == nil {
		return false, errWebhookLimiterMissing
	}
	return slidingWindowLimiterAllow(ctx, limiter, key)
}

func failClosedCheck(ctx context.Context, limiter WebhookRateLimiter, key string) (bool, error) {
	if limiter == nil {
		return false, errWebhookLimiterMissing
	}
	return slidingWindowLimiterCheckWithError(ctx, limiter, key)
}

// webhookSecrets returns the secrets a signature may match: the current one and
// a previous one still inside its rotation overlap. An unknown, malformed or
// disabled endpoint yields the dummy secret and known=false.
func (h *Handler) webhookSecrets(ctx context.Context, rawID string) (secrets [][]byte, known bool, err error) {
	id, perr := util.ParseUUID(rawID)
	if perr != nil {
		return [][]byte{webhookDummySecret}, false, nil
	}
	ep, err := h.Queries.GetWorkflowWebhookEndpoint(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ep.DisabledAt.Valid) {
		return [][]byte{webhookDummySecret}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	current, err := h.VCSSecretBox.Open(ep.SecretSealed)
	if err != nil {
		return nil, false, err
	}
	secrets = [][]byte{current}
	if len(ep.PreviousSecretSealed) > 0 && ep.PreviousExpiresAt.Valid && ep.PreviousExpiresAt.Time.After(time.Now()) {
		previous, err := h.VCSSecretBox.Open(ep.PreviousSecretSealed)
		if err != nil {
			return nil, false, err
		}
		secrets = append(secrets, previous)
	}
	return secrets, true, nil
}

// admitWorkflowWebhook is the admission transaction: the endpoint row lock
// serialises callers, the quota counts committed nonces, and the nonce, the
// event and the engine prompt commit together or not at all.
func (h *Handler) admitWorkflowWebhook(ctx context.Context, endpointID pgtype.UUID, nonce string, p webhookguard.Payload, bodySum []byte, bodyBytes int) (webhookOutcome, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)

	ep, err := qtx.LockWorkflowWebhookEndpoint(ctx, endpointID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ep.DisabledAt.Valid) {
		return webhookGone, nil
	}
	if err != nil {
		return 0, err
	}
	used, err := qtx.CountWorkflowWebhookNoncesSince(ctx, db.CountWorkflowWebhookNoncesSinceParams{
		EndpointID: ep.ID, SeenAt: pgtype.Timestamptz{Time: time.Now().Add(-webhookQuotaWindow), Valid: true},
	})
	if err != nil {
		return 0, err
	}
	if used >= int64(ep.RatePerMin) {
		return webhookOverQuota, nil
	}
	fresh, err := qtx.InsertWorkflowWebhookNonce(ctx, db.InsertWorkflowWebhookNonceParams{EndpointID: ep.ID, Nonce: nonce})
	if err != nil {
		return 0, err
	}
	if fresh == 0 {
		return webhookReplay, nil
	}
	issue, eligible, err := h.webhookTarget(ctx, qtx, ep, p.Issue)
	if err != nil {
		return 0, err
	}
	if eligible {
		ref := pgtype.Text{String: p.Ref, Valid: p.Ref != ""}
		if _, err := qtx.InsertWorkflowWebhookEvent(ctx, db.InsertWorkflowWebhookEventParams{
			EndpointID: ep.ID, WorkspaceID: ep.WorkspaceID, IssueID: issue.ID, Name: ep.Name,
			Result: string(p.Result), Ref: ref, BodySha256: bodySum, BodyBytes: int32(bodyBytes),
		}); err != nil {
			return 0, err
		}
		if err := qtx.WakeWorkflowEdges(ctx, issue.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if eligible {
		return webhookAccepted, nil
	}
	return webhookIgnored, nil
}

// webhookTarget resolves the identifier inside the endpoint's workspace and
// reports whether the issue waits on this endpoint: it follows the endpoint's
// own workflow, sits on a step with a webhook(name) edge for this endpoint, and
// is open.
func (h *Handler) webhookTarget(ctx context.Context, q *db.Queries, ep db.WorkflowWebhookEndpoint, identifier string) (db.Issue, bool, error) {
	parts := splitIdentifier(identifier)
	if parts == nil {
		return db.Issue{}, false, nil
	}
	ws, err := q.GetWorkspace(ctx, ep.WorkspaceID)
	if err != nil {
		return db.Issue{}, false, err
	}
	if !strings.EqualFold(parts.prefix, issuePrefixForWorkspace(ws)) {
		return db.Issue{}, false, nil
	}
	issue, err := q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: ep.WorkspaceID, Number: parts.number})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, false, nil
	}
	if err != nil {
		return db.Issue{}, false, err
	}
	if issue.TriageState.Valid {
		return issue, false, nil
	}
	category, err := issuestatus.CategoryWithError(ctx, q, issue.WorkspaceID, issue.Status)
	if err != nil {
		return issue, false, err
	}
	if category == issuestatus.CategoryClosed {
		return issue, false, nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if errors.Is(err, issueworkflow.ErrOverrideMissing) || (err == nil && (def == nil || def.ID != ep.WorkflowID)) {
		return issue, false, nil
	}
	if err != nil {
		return issue, false, err
	}
	step, ok := def.Step(issue.Status)
	if !ok {
		return issue, false, nil
	}
	for _, e := range step.Edges {
		if g, err := issueworkflow.ParseGuard(e.Guard); err == nil && g.Kind == issueworkflow.GuardWebhook && g.Args[0] == ep.Name {
			return issue, true, nil
		}
	}
	return issue, false, nil
}

// guardedWebhooks marks each webhook(name) guard whose endpoint has a success
// event for this issue received since it entered the step.
func (h *Handler) guardedWebhooks(ctx context.Context, issue db.Issue, workflowID pgtype.UUID, edges []issueworkflow.Edge, entered time.Time) (map[string]bool, error) {
	var out map[string]bool
	for _, e := range edges {
		g, err := issueworkflow.ParseGuard(e.Guard)
		if err != nil || g.Kind != issueworkflow.GuardWebhook {
			continue
		}
		ep, err := h.Queries.GetWorkflowWebhookEndpointByName(ctx, db.GetWorkflowWebhookEndpointByNameParams{WorkflowID: workflowID, Name: g.Args[0]})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, err = h.Queries.GetLatestWorkflowWebhookSuccess(ctx, db.GetLatestWorkflowWebhookSuccessParams{
			EndpointID: ep.ID, IssueID: issue.ID, ReceivedAt: pgtype.Timestamptz{Time: entered, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[g.Args[0]] = true
	}
	return out, nil
}
