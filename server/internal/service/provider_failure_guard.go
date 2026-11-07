package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	providerFailureCooldown        = time.Hour
	providerFailureRecoveryTimeout = time.Second
	providerFailureChangedState    = "provider_server_error_to_http_401"
	// providerFailureSuccessState: same-provider success evidence (CHE-4311).
	providerFailureSuccessState = "provider_success_after_failure"
)

type ProviderFailureProbeEvidence struct {
	CheckedAt  time.Time
	StatusCode int
}

type ProviderFailureProbe interface {
	Probe(context.Context) (ProviderFailureProbeEvidence, error)
}

type HTTPProviderFailureProbe struct {
	url    string
	client *http.Client
}

var errProviderRecoveryProbeNotConfigured = errors.New("provider recovery probe is not configured")

func NewHTTPProviderFailureProbe(baseURL string) (*HTTPProviderFailureProbe, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("provider recovery base URL must be an http(s) URL without credentials, query, or fragment")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if basePath != "" && basePath != "/v1" {
		return nil, errors.New("provider recovery base URL path must be empty or /v1")
	}
	if basePath == "" {
		basePath = "/v1"
	}
	parsed.Path = path.Join(basePath, "models")
	parsed.RawPath = ""
	client := &http.Client{
		Timeout: providerFailureRecoveryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &HTTPProviderFailureProbe{url: parsed.String(), client: client}, nil
}

func (p *HTTPProviderFailureProbe) Probe(ctx context.Context) (ProviderFailureProbeEvidence, error) {
	if p == nil || p.client == nil || p.url == "" {
		return ProviderFailureProbeEvidence{}, errProviderRecoveryProbeNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, providerFailureRecoveryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return ProviderFailureProbeEvidence{}, fmt.Errorf("create provider recovery probe: %w", err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return ProviderFailureProbeEvidence{}, fmt.Errorf("request provider recovery probe: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 1024)
	return ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: response.StatusCode}, nil
}

type providerFailureRecoveryScope struct {
	Kind         string
	TriggerID    pgtype.UUID
	ConditionKey string
}

type providerFailureRecoveryAttempt struct {
	FailedTaskID pgtype.UUID
	FailedAt     time.Time
	ProbeAt      time.Time
	ProbeStatus  int32
	// SuccessEvidence: ProbeAt is the breaker's last_success_at, not an HTTP probe.
	SuccessEvidence bool
	Scope           providerFailureRecoveryScope
	SkipReason      string
	ReturnOwner     bool
	// Cap is set when the reservation was refused by a recovery cap. The caller
	// sends the notice after its transaction outcome (notifyProviderFailureAttemptCap).
	Cap *providerFailureOwnerDecision
}

func providerTriggerConditionKey(payload []byte) string {
	var trigger struct {
		HeadSHA string `json:"head_sha"`
	}
	if json.Unmarshal(payload, &trigger) != nil {
		return ""
	}
	return strings.TrimSpace(trigger.HeadSHA)
}

type providerFailureSnapshot struct {
	ID            pgtype.UUID
	Status        string
	FailureReason pgtype.Text
	TerminalAt    pgtype.Timestamptz
	IsRecovery    bool
}

func loadLatestProviderFailure(ctx context.Context, q *db.Queries, scope providerFailureRecoveryScope) (*providerFailureSnapshot, error) {
	if !scope.TriggerID.Valid || (scope.Kind != "autopilot_schedule" && scope.Kind != "issue_wakeup") {
		return nil, errors.New("invalid provider recovery trigger scope")
	}
	var snapshot providerFailureSnapshot
	var err error
	switch scope.Kind {
	case "autopilot_schedule":
		row, queryErr := q.FindLatestScheduledAutopilotTaskForTrigger(ctx, db.FindLatestScheduledAutopilotTaskForTriggerParams{
			TriggerID: scope.TriggerID, ConditionKey: scope.ConditionKey,
		})
		err = queryErr
		snapshot = providerFailureSnapshot{ID: row.ID, Status: row.Status, FailureReason: row.FailureReason, TerminalAt: row.TerminalAt, IsRecovery: row.IsRecovery}
	case "issue_wakeup":
		row, queryErr := q.FindLatestRecurringWakeupTask(ctx, db.FindLatestRecurringWakeupTaskParams{
			WakeupID: util.UUIDToString(scope.TriggerID), ConditionKey: scope.ConditionKey,
		})
		err = queryErr
		snapshot = providerFailureSnapshot{ID: row.ID, Status: row.Status, FailureReason: row.FailureReason, TerminalAt: row.TerminalAt, IsRecovery: row.IsRecovery}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load latest provider-trigger task: %w", err)
	}
	return &snapshot, nil
}

func (s *TaskService) prepareProviderFailureRecovery(ctx context.Context, q *db.Queries, scope providerFailureRecoveryScope) (*providerFailureRecoveryAttempt, error) {
	snapshot, err := loadLatestProviderFailure(ctx, q, scope)
	if err != nil || snapshot == nil {
		return nil, err
	}
	if snapshot.Status != "failed" || !snapshot.FailureReason.Valid || snapshot.FailureReason.String != "agent_error.provider_server_error" {
		return nil, nil
	}
	attempt := &providerFailureRecoveryAttempt{
		FailedTaskID: snapshot.ID,
		FailedAt:     snapshot.TerminalAt.Time.UTC(),
		Scope:        scope,
	}
	if snapshot.IsRecovery {
		attempt.SkipReason = "provider recovery already failed; a new external condition is required"
		attempt.ReturnOwner = true
		return attempt, nil
	}
	if at, ok, err := providerSuccessAfter(ctx, q, snapshot.ID, attempt.FailedAt); err != nil {
		return nil, err
	} else if ok {
		attempt.ProbeAt, attempt.SuccessEvidence = at, true
		return attempt, nil
	}
	if time.Since(attempt.FailedAt) < providerFailureCooldown {
		attempt.SkipReason = noProviderRecoveryEvidenceReason(attempt.FailedAt, time.Now().UTC())
		return attempt, nil
	}
	if s.ProviderFailureProbe == nil {
		attempt.SkipReason = "cooldown expiry alone is not recovery evidence; probe is not configured"
		return attempt, nil
	}
	evidence, err := s.ProviderFailureProbe.Probe(ctx)
	if err != nil {
		if errors.Is(err, errProviderRecoveryProbeNotConfigured) {
			attempt.SkipReason = "cooldown expiry alone is not recovery evidence; probe is not configured"
			return attempt, nil
		}
		attempt.SkipReason = "cooldown expiry alone is not recovery evidence; probe failed"
		return attempt, nil
	}
	attempt.ProbeAt = evidence.CheckedAt.UTC()
	attempt.ProbeStatus = int32(evidence.StatusCode)
	if evidence.StatusCode != http.StatusUnauthorized || !evidence.CheckedAt.After(attempt.FailedAt) {
		attempt.SkipReason = "fresh /v1/models probe did not establish a changed condition"
		return attempt, nil
	}
	return attempt, nil
}

// providerSuccessAfter is the CHE-1741 R1 evidence: the failed task's provider
// breaker is closed and its last_success_at is later than the failure. Unknown
// task, agent, provider or breaker row is "no evidence", never an error.
func providerSuccessAfter(ctx context.Context, q *db.Queries, failedTaskID pgtype.UUID, failedAt time.Time) (time.Time, bool, error) {
	task, err := q.GetAgentTask(ctx, failedTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("load failed provider task: %w", err)
	}
	agent, err := q.GetAgent(ctx, task.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("load failed provider task agent: %w", err)
	}
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok {
		return time.Time{}, false, nil
	}
	row, exists, err := providerBreakerRow(ctx, q, agent.WorkspaceID, provider)
	if err != nil || !exists || row.State != "closed" || !row.LastSuccessAt.Valid || !row.LastSuccessAt.Time.After(failedAt) {
		return time.Time{}, false, err
	}
	return row.LastSuccessAt.Time.UTC(), true, nil
}

func (s *TaskService) notifyProviderFailureOwner(
	ctx context.Context,
	q *db.Queries,
	attempt *providerFailureRecoveryAttempt,
	workspaceID, ownerID, issueID pgtype.UUID,
) {
	if s == nil || q == nil || attempt == nil || !attempt.ReturnOwner || !attempt.FailedTaskID.Valid || !workspaceID.Valid || !ownerID.Valid {
		return
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: ownerID, WorkspaceID: workspaceID}); err != nil {
		slog.Warn("provider recovery owner notice skipped: trigger owner is not a workspace member",
			"failed_task_id", util.UUIDToString(attempt.FailedTaskID),
			"owner_id", util.UUIDToString(ownerID),
			"error", err,
		)
		return
	}
	details, err := json.Marshal(map[string]string{
		"reason_code":    "provider_failure_cooldown",
		"failed_task_id": util.UUIDToString(attempt.FailedTaskID),
		"trigger_kind":   attempt.Scope.Kind,
		"trigger_id":     util.UUIDToString(attempt.Scope.TriggerID),
		"condition_key":  attempt.Scope.ConditionKey,
	})
	if err != nil {
		return
	}
	rows, err := q.CreateProviderFailureOwnerNotice(ctx, db.CreateProviderFailureOwnerNoticeParams{
		ID:          attempt.FailedTaskID,
		WorkspaceID: workspaceID,
		RecipientID: ownerID,
		IssueID:     issueID,
		Details:     details,
	})
	if err != nil {
		slog.Warn("provider recovery owner notice failed",
			"failed_task_id", util.UUIDToString(attempt.FailedTaskID),
			"owner_id", util.UUIDToString(ownerID),
			"error", err,
		)
		return
	}
	if rows == 1 {
		s.publishProviderFailureNotice(ctx, q, attempt.FailedTaskID, workspaceID)
	}
}

// publishProviderFailureNotice announces a freshly inserted notice to its recipient.
func (s *TaskService) publishProviderFailureNotice(ctx context.Context, q *db.Queries, failedTaskID, workspaceID pgtype.UUID) {
	if s.Bus == nil {
		return
	}
	item, err := q.GetInboxItem(ctx, failedTaskID)
	if err != nil {
		slog.Warn("provider recovery owner notice readback failed",
			"failed_task_id", util.UUIDToString(failedTaskID),
			"error", err,
		)
		return
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventInboxNew,
		WorkspaceID: util.UUIDToString(workspaceID),
		ActorType:   "system",
		Payload: map[string]any{
			"item": map[string]any{
				"id":             util.UUIDToString(item.ID),
				"workspace_id":   util.UUIDToString(item.WorkspaceID),
				"recipient_type": item.RecipientType,
				"recipient_id":   util.UUIDToString(item.RecipientID),
				"type":           item.Type,
				"severity":       item.Severity,
				"issue_id":       util.UUIDToPtr(item.IssueID),
				"title":          item.Title,
				"body":           item.Body.String,
				"actor_type":     item.ActorType.String,
				"actor_id":       util.UUIDToPtr(item.ActorID),
				"read":           item.Read,
				"archived":       item.Archived,
				"created_at":     item.CreatedAt.Time,
				"details":        json.RawMessage(item.Details),
			},
		},
	})
}

func noProviderRecoveryEvidenceReason(failedAt, now time.Time) string {
	if now.Sub(failedAt) < providerFailureCooldown {
		return "same trigger failed within the 60-minute cooldown"
	}
	return "cooldown expiry alone is not recovery evidence"
}

func (s *TaskService) reserveProviderFailureRecovery(ctx context.Context, q *db.Queries, attempt *providerFailureRecoveryAttempt, recoveryRunID, recoveryTaskID pgtype.UUID) (bool, error) {
	if attempt == nil || attempt.SkipReason != "" || !attempt.FailedTaskID.Valid || !recoveryRunID.Valid && !recoveryTaskID.Valid {
		return false, errors.New("invalid provider failure recovery reservation")
	}
	latest, err := loadLatestProviderFailure(ctx, q, attempt.Scope)
	if err != nil {
		return false, err
	}
	if latest == nil || latest.ID != attempt.FailedTaskID || latest.Status != "failed" || !latest.FailureReason.Valid || latest.FailureReason.String != "agent_error.provider_server_error" || latest.IsRecovery {
		return false, nil
	}
	decision, err := resolveProviderFailureOwner(ctx, q, attempt.FailedTaskID)
	if err != nil {
		return false, err
	}
	if decision.Capped() {
		attempt.Cap = &decision
		return false, nil
	}
	changed, status := providerFailureChangedState, pgtype.Int4{Int32: attempt.ProbeStatus, Valid: true}
	if attempt.SuccessEvidence {
		changed, status = providerFailureSuccessState, pgtype.Int4{}
	}
	rows, err := q.RecordProviderFailureRecovery(ctx, db.RecordProviderFailureRecoveryParams{
		FailedTaskID:     attempt.FailedTaskID,
		TriggerKind:      attempt.Scope.Kind,
		TriggerID:        attempt.Scope.TriggerID,
		ConditionKey:     attempt.Scope.ConditionKey,
		FailedAt:         pgtype.Timestamptz{Time: attempt.FailedAt, Valid: true},
		ProbeAt:          pgtype.Timestamptz{Time: attempt.ProbeAt, Valid: true},
		ProbeStatus:      status,
		ChangedCondition: changed,
		RecoveryRunID:    recoveryRunID,
		RecoveryTaskID:   recoveryTaskID,
	})
	if err != nil {
		return false, fmt.Errorf("reserve provider failure recovery: %w", err)
	}
	return rows == 1, nil
}

// applyProviderFailureReservation runs the reservation and apply inside
// one savepoint of the caller's transaction. A failed apply or a refused
// duplicate effect rolls back only the savepoint, so the caller's own writes
// and transaction stay usable; the caller decides whether to roll back.
func applyProviderFailureReservation(ctx context.Context, tx pgx.Tx, reservation db.RecordProviderFailureRecoveryParams, apply func(context.Context, pgx.Tx) error) (bool, error) {
	if tx == nil {
		return false, errors.New("provider failure recovery reservation requires a transaction")
	}
	if apply == nil {
		return false, errors.New("provider failure recovery reservation requires an apply function")
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin provider failure recovery savepoint: %w", err)
	}
	rollback := func(cause error) error {
		if rbErr := sp.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(cause, fmt.Errorf("rollback provider failure recovery savepoint: %w", rbErr))
		}
		return cause
	}

	rows, err := db.New(sp).RecordProviderFailureRecovery(ctx, reservation)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// A second re-arm or a duplicate recovery run/task/wakeup is a
			// refusal, not a fault: no effect was reserved, apply never runs.
			return false, rollback(nil)
		}
		return false, rollback(fmt.Errorf("reserve provider failure recovery: %w", err))
	}
	if rows == 0 {
		return false, rollback(nil)
	}
	if rows != 1 {
		return false, rollback(fmt.Errorf("reserve provider failure recovery: inserted %d rows, want at most one", rows))
	}
	if err := apply(ctx, sp); err != nil {
		return false, rollback(fmt.Errorf("apply provider failure recovery reservation: %w", err))
	}
	if err := sp.Commit(ctx); err != nil {
		return false, fmt.Errorf("release provider failure recovery savepoint: %w", err)
	}
	return true, nil
}

func providerFailureCooldownReason(attempt *providerFailureRecoveryAttempt) string {
	if attempt == nil {
		return ""
	}
	if attempt.SkipReason != "" {
		return fmt.Sprintf("provider_failure_cooldown: %s; failed_task_id=%s", attempt.SkipReason, util.UUIDToString(attempt.FailedTaskID))
	}
	if attempt.SuccessEvidence {
		return fmt.Sprintf("provider_failure_cooldown: same-provider success after failure; failed_task_id=%s", util.UUIDToString(attempt.FailedTaskID))
	}
	return fmt.Sprintf("provider_failure_cooldown: fresh unauthenticated /v1/models probe returned 401; failed_task_id=%s", util.UUIDToString(attempt.FailedTaskID))
}
