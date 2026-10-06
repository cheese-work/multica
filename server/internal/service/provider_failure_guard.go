package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	providerFailureCooldown        = time.Hour
	providerFailureRecoveryTimeout = time.Second
	providerFailureChangedState    = "provider_server_error_to_http_401"
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
		return ProviderFailureProbeEvidence{}, errors.New("provider recovery probe is not configured")
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
	Scope        providerFailureRecoveryScope
	SkipReason   string
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
	rows, err := q.RecordProviderFailureRecovery(ctx, db.RecordProviderFailureRecoveryParams{
		FailedTaskID:     attempt.FailedTaskID,
		TriggerKind:      attempt.Scope.Kind,
		TriggerID:        attempt.Scope.TriggerID,
		ConditionKey:     attempt.Scope.ConditionKey,
		FailedAt:         pgtype.Timestamptz{Time: attempt.FailedAt, Valid: true},
		ProbeAt:          pgtype.Timestamptz{Time: attempt.ProbeAt, Valid: true},
		ProbeStatus:      attempt.ProbeStatus,
		ChangedCondition: providerFailureChangedState,
		RecoveryRunID:    recoveryRunID,
		RecoveryTaskID:   recoveryTaskID,
	})
	if err != nil {
		return false, fmt.Errorf("reserve provider failure recovery: %w", err)
	}
	return rows == 1, nil
}

func providerFailureCooldownReason(attempt *providerFailureRecoveryAttempt) string {
	if attempt == nil {
		return ""
	}
	if attempt.SkipReason != "" {
		return fmt.Sprintf("provider_failure_cooldown: %s; failed_task_id=%s", attempt.SkipReason, util.UUIDToString(attempt.FailedTaskID))
	}
	return fmt.Sprintf("provider_failure_cooldown: fresh unauthenticated /v1/models probe returned 401; failed_task_id=%s", util.UUIDToString(attempt.FailedTaskID))
}
