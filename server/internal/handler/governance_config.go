package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/util"
)

const governanceConfigBodyMaxBytes = 16 << 10

type governanceConfigResponse struct {
	governance.WorkspaceConfig
	MissingRequirements []string                    `json:"missing_requirements"`
	OperatingLimits     []governance.OperatingLimit `json:"operating_limits"`
	AuditHistory        []governance.LimitAudit     `json:"audit_history"`
	CredentialPresent   bool                        `json:"credential_present"`
}

type governanceConfigPatch struct {
	ExpectedVersion              *int64                 `json:"expected_version"`
	RequestID                    string                 `json:"request_id"`
	JevGovernanceEnabled         *bool                  `json:"jev_governance_enabled,omitempty"`
	JevFallbackAgentsEnabled     *bool                  `json:"jev_fallback_agents_enabled,omitempty"`
	GovernanceCorrectionsEnabled *bool                  `json:"governance_corrections_enabled,omitempty"`
	RuleMode                     *governance.RuleMode   `json:"rule_mode,omitempty"`
	Limits                       *governanceLimitsPatch `json:"limits,omitempty"`
	RollbackToVersion            *int64                 `json:"rollback_to_version,omitempty"`
}

type governanceLimitsPatch struct {
	MaxRefreshes              *int64                    `json:"max_refreshes,omitempty"`
	MaxEvaluations            *int64                    `json:"max_evaluations,omitempty"`
	AdmissionWindowSeconds    *int64                    `json:"admission_window_seconds,omitempty"`
	WorkspaceSpendCapMicroUSD *int64                    `json:"workspace_spend_cap_micro_usd,omitempty"`
	PricingPolicy             *governance.PricingPolicy `json:"pricing_policy,omitempty"`
}

type governanceConfigWrite struct {
	Version      int64
	ControlEpoch int64
	Settings     []byte
	Audit        governance.LimitAudit
}

func (h *Handler) GetGovernanceConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := h.governanceWorkspaceAccess(w, r, false)
	if !ok {
		return
	}
	config, audit, err := loadGovernanceConfig(r.Context(), h.DB, workspaceID)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, governanceConfigResponse{
		WorkspaceConfig:     config,
		MissingRequirements: config.MissingRequirements(),
		OperatingLimits:     governance.DescribeOperatingLimits(config, audit),
		AuditHistory:        audit,
		CredentialPresent:   false,
	})
}

func (h *Handler) PatchGovernanceConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID, actorID, ok := h.governanceWorkspaceAccess(w, r, true)
	if !ok {
		return
	}
	var patch governanceConfigPatch
	r.Body = http.MaxBytesReader(w, r.Body, governanceConfigBodyMaxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	requestUUID, err := uuid.Parse(patch.RequestID)
	if err != nil || patch.ExpectedVersion == nil || *patch.ExpectedVersion < 0 || !validGovernanceConfigPatch(patch) {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	encodedPatch, err := json.Marshal(patch)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	digest := sha256.Sum256(encodedPatch)
	result, err := h.writeGovernanceConfig(r.Context(), workspaceID, actorID, requestUUID, hex.EncodeToString(digest[:]), patch)
	if err != nil {
		switch {
		case errors.Is(err, errGovernanceConfigConflict):
			writeGovernanceProblem(w, r, http.StatusConflict, "revision_conflict")
		case errors.Is(err, errGovernanceConfigIdempotencyConflict):
			writeGovernanceProblem(w, r, http.StatusConflict, "idempotency_conflict")
		case errors.Is(err, errGovernanceConfigRollbackMissing):
			writeGovernanceProblem(w, r, http.StatusConflict, "rollback_unavailable")
		default:
			writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		}
		return
	}
	config, audit, err := loadGovernanceConfig(r.Context(), h.DB, workspaceID)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	config.Version = result.Version
	config.ControlEpoch = result.ControlEpoch
	if len(result.Settings) > 0 {
		if err := json.Unmarshal(result.Settings, &config.Settings); err != nil {
			writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, governanceConfigResponse{
		WorkspaceConfig:     config,
		MissingRequirements: config.MissingRequirements(),
		OperatingLimits:     governance.DescribeOperatingLimits(config, audit),
		AuditHistory:        audit,
		CredentialPresent:   false,
	})
}

func (h *Handler) governanceWorkspaceAccess(w http.ResponseWriter, r *http.Request, adminWrite bool) (pgtype.UUID, pgtype.UUID, bool) {
	if adminWrite && isMachineCredentialActor(r) {
		writeGovernanceProblem(w, r, http.StatusForbidden, "agent_denied")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	workspaceID, err := util.ParseUUID(workspaceIDFromURL(r, "id"))
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	actorID, err := util.ParseUUID(requestUserID(r))
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusUnauthorized, "unauthorized")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	member, err := h.getWorkspaceMember(r.Context(), uuidToString(actorID), uuidToString(workspaceID))
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusNotFound, "workspace_not_found")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	if adminWrite && !roleAllowed(member.Role, "owner", "admin") {
		writeGovernanceProblem(w, r, http.StatusForbidden, "permission_denied")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	return workspaceID, actorID, true
}

func (h *Handler) writeGovernanceConfig(ctx context.Context, workspaceID, actorID pgtype.UUID, requestID uuid.UUID, digest string, patch governanceConfigPatch) (governanceConfigWrite, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return governanceConfigWrite{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id)
		VALUES ($1)
		ON CONFLICT (workspace_id) DO NOTHING
	`, workspaceID); err != nil {
		return governanceConfigWrite{}, err
	}
	var currentVersion, currentEpoch int64
	var currentJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT config_version, control_epoch, settings
		FROM governance_workspace_config
		WHERE workspace_id = $1
		FOR UPDATE
	`, workspaceID).Scan(&currentVersion, &currentEpoch, &currentJSON); err != nil {
		return governanceConfigWrite{}, err
	}
	var priorDigest string
	var prior governanceConfigWrite
	var priorSettings, priorBefore []byte
	var priorAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT request_digest, config_version, control_epoch, settings_before, settings_after, created_at
		FROM governance_workspace_config_audit
		WHERE workspace_id = $1 AND request_id = $2
	`, workspaceID, requestID).Scan(&priorDigest, &prior.Version, &prior.ControlEpoch, &priorBefore, &priorSettings, &priorAt)
	if err == nil {
		if priorDigest != digest {
			return governanceConfigWrite{}, errGovernanceConfigIdempotencyConflict
		}
		prior.Settings = priorSettings
		prior.Audit = governance.LimitAudit{Version: prior.Version, RequestID: requestID.String(), ActorID: uuidToString(actorID), Previous: json.RawMessage(priorBefore), Effective: json.RawMessage(priorSettings), ChangedAt: priorAt.UTC().Format(time.RFC3339Nano)}
		if err := tx.Commit(ctx); err != nil {
			return governanceConfigWrite{}, err
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return governanceConfigWrite{}, err
	}
	if *patch.ExpectedVersion != currentVersion {
		return governanceConfigWrite{}, errGovernanceConfigConflict
	}
	if currentVersion == int64(^uint64(0)>>1) {
		return governanceConfigWrite{}, errGovernanceConfigConflict
	}
	beforeSettings := mustDecodeGovernanceSettings(currentJSON)
	var settings governance.WorkspaceSettings
	if err := json.Unmarshal(currentJSON, &settings); err != nil {
		return governanceConfigWrite{}, err
	}
	if settings.RuleMode == "" {
		settings.RuleMode = governance.RuleModeOff
	}
	if patch.RollbackToVersion != nil {
		err := tx.QueryRow(ctx, `
			SELECT settings_after
			FROM governance_workspace_config_audit
			WHERE workspace_id = $1 AND config_version = $2
		`, workspaceID, *patch.RollbackToVersion).Scan(&currentJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return governanceConfigWrite{}, errGovernanceConfigRollbackMissing
		}
		if err != nil {
			return governanceConfigWrite{}, err
		}
		if err := json.Unmarshal(currentJSON, &settings); err != nil {
			return governanceConfigWrite{}, err
		}
	} else {
		applyGovernanceConfigPatch(&settings, patch)
	}
	if err := governance.ValidateWorkspaceSettings(settings); err != nil {
		return governanceConfigWrite{}, errGovernanceConfigInvalid
	}
	beforeJSON, err := json.Marshal(beforeSettings)
	if err != nil {
		return governanceConfigWrite{}, err
	}
	afterJSON, err := json.Marshal(settings)
	if err != nil {
		return governanceConfigWrite{}, err
	}
	nextVersion := currentVersion + 1
	nextEpoch := currentEpoch
	if !bytes.Equal(beforeJSON, afterJSON) {
		if currentEpoch == int64(^uint64(0)>>1) {
			return governanceConfigWrite{}, errGovernanceConfigConflict
		}
		nextEpoch++
	}
	if _, err := tx.Exec(ctx, `
		UPDATE governance_workspace_config
		SET config_version = $2, control_epoch = $3, settings = $4, updated_by = $5, updated_at = now()
		WHERE workspace_id = $1 AND config_version = $6
	`, workspaceID, nextVersion, nextEpoch, afterJSON, actorID, currentVersion); err != nil {
		return governanceConfigWrite{}, err
	}
	rollbackVersion := any(nil)
	if patch.RollbackToVersion != nil {
		rollbackVersion = *patch.RollbackToVersion
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO governance_workspace_config_audit (
			workspace_id, config_version, request_id, request_digest, actor_id,
			settings_before, settings_after, control_epoch, rollback_of_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at
	`, workspaceID, nextVersion, requestID, digest, actorID, beforeJSON, afterJSON, nextEpoch, rollbackVersion).Scan(&createdAt); err != nil {
		return governanceConfigWrite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return governanceConfigWrite{}, err
	}
	return governanceConfigWrite{
		Version: nextVersion, ControlEpoch: nextEpoch, Settings: afterJSON,
		Audit: governance.LimitAudit{Version: nextVersion, RequestID: requestID.String(), ActorID: uuidToString(actorID), Previous: json.RawMessage(beforeJSON), Effective: json.RawMessage(afterJSON), ChangedAt: createdAt.UTC().Format(time.RFC3339Nano)},
	}, nil
}

func loadGovernanceConfig(ctx context.Context, database dbExecutor, workspaceID pgtype.UUID) (governance.WorkspaceConfig, []governance.LimitAudit, error) {
	config := governance.DefaultWorkspaceConfig()
	var settingsJSON []byte
	err := database.QueryRow(ctx, `
		SELECT config_version, control_epoch, settings
		FROM governance_workspace_config
		WHERE workspace_id = $1
		FOR SHARE
	`, workspaceID).Scan(&config.Version, &config.ControlEpoch, &settingsJSON)
	if err == nil {
		if err := json.Unmarshal(settingsJSON, &config.Settings); err != nil {
			return governance.WorkspaceConfig{}, nil, err
		}
		if config.Settings.RuleMode == "" {
			config.Settings.RuleMode = governance.RuleModeOff
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return governance.WorkspaceConfig{}, nil, err
	}
	rows, err := database.Query(ctx, `
		SELECT config_version, request_id::text, actor_id::text, settings_before, settings_after, created_at
		FROM governance_workspace_config_audit
		WHERE workspace_id = $1
		ORDER BY config_version DESC
		LIMIT 50
	`, workspaceID)
	if err != nil {
		return governance.WorkspaceConfig{}, nil, err
	}
	defer rows.Close()
	audit := make([]governance.LimitAudit, 0)
	for rows.Next() {
		var entry governance.LimitAudit
		var previous, effective []byte
		var changedAt time.Time
		if err := rows.Scan(&entry.Version, &entry.RequestID, &entry.ActorID, &previous, &effective, &changedAt); err != nil {
			return governance.WorkspaceConfig{}, nil, err
		}
		entry.Previous = json.RawMessage(previous)
		entry.Effective = json.RawMessage(effective)
		entry.ChangedAt = changedAt.UTC().Format(time.RFC3339Nano)
		audit = append(audit, entry)
	}
	return config, audit, rows.Err()
}

func loadGovernanceControl(ctx context.Context, database dbExecutor, workspaceID pgtype.UUID) (governance.WorkspaceConfig, error) {
	config := governance.DefaultWorkspaceConfig()
	var settingsJSON []byte
	err := database.QueryRow(ctx, `
		SELECT config_version, control_epoch, settings
		FROM governance_workspace_config
		WHERE workspace_id = $1
		FOR SHARE
	`, workspaceID).Scan(&config.Version, &config.ControlEpoch, &settingsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return config, nil
	}
	if err != nil {
		return governance.WorkspaceConfig{}, err
	}
	if err := json.Unmarshal(settingsJSON, &config.Settings); err != nil {
		return governance.WorkspaceConfig{}, err
	}
	if config.Settings.RuleMode == "" {
		config.Settings.RuleMode = governance.RuleModeOff
	}
	return config, nil
}

func validGovernanceConfigPatch(patch governanceConfigPatch) bool {
	if patch.RequestID == "" || patch.RollbackToVersion != nil && *patch.RollbackToVersion < 1 {
		return false
	}
	settingsProvided := patch.JevGovernanceEnabled != nil || patch.JevFallbackAgentsEnabled != nil ||
		patch.GovernanceCorrectionsEnabled != nil || patch.RuleMode != nil || patch.Limits != nil
	if patch.RollbackToVersion != nil {
		return !settingsProvided
	}
	if !settingsProvided {
		return false
	}
	if patch.RuleMode != nil && *patch.RuleMode != governance.RuleModeOff && *patch.RuleMode != governance.RuleModeShadow && *patch.RuleMode != governance.RuleModeCorrection {
		return false
	}
	if patch.Limits != nil {
		for _, value := range []*int64{patch.Limits.MaxRefreshes, patch.Limits.MaxEvaluations, patch.Limits.AdmissionWindowSeconds, patch.Limits.WorkspaceSpendCapMicroUSD} {
			if value != nil && *value <= 0 {
				return false
			}
		}
		if patch.Limits.MaxRefreshes == nil && patch.Limits.MaxEvaluations == nil && patch.Limits.AdmissionWindowSeconds == nil && patch.Limits.WorkspaceSpendCapMicroUSD == nil && patch.Limits.PricingPolicy == nil {
			return false
		}
		if policy := patch.Limits.PricingPolicy; policy != nil &&
			(strings.TrimSpace(policy.Version) == "" ||
				policy.InputMicroUSDPerMillionTokens != nil && *policy.InputMicroUSDPerMillionTokens < 0 ||
				policy.OutputMicroUSDPerMillionTokens != nil && *policy.OutputMicroUSDPerMillionTokens < 0) {
			return false
		}
	}
	return true
}

func applyGovernanceConfigPatch(settings *governance.WorkspaceSettings, patch governanceConfigPatch) {
	if patch.JevGovernanceEnabled != nil {
		settings.JevGovernanceEnabled = *patch.JevGovernanceEnabled
	}
	if patch.JevFallbackAgentsEnabled != nil {
		settings.JevFallbackAgentsEnabled = *patch.JevFallbackAgentsEnabled
	}
	if patch.GovernanceCorrectionsEnabled != nil {
		settings.GovernanceCorrectionsEnabled = *patch.GovernanceCorrectionsEnabled
	}
	if patch.RuleMode != nil {
		settings.RuleMode = *patch.RuleMode
	}
	if patch.Limits != nil {
		limits := &settings.Limits
		if patch.Limits.MaxRefreshes != nil {
			limits.MaxRefreshes = patch.Limits.MaxRefreshes
		}
		if patch.Limits.MaxEvaluations != nil {
			limits.MaxEvaluations = patch.Limits.MaxEvaluations
		}
		if patch.Limits.AdmissionWindowSeconds != nil {
			limits.AdmissionWindowSeconds = patch.Limits.AdmissionWindowSeconds
		}
		if patch.Limits.WorkspaceSpendCapMicroUSD != nil {
			limits.WorkspaceSpendCapMicroUSD = patch.Limits.WorkspaceSpendCapMicroUSD
		}
		if patch.Limits.PricingPolicy != nil {
			policy := *patch.Limits.PricingPolicy
			limits.PricingPolicy = &policy
		}
	}
}

func mustDecodeGovernanceSettings(data []byte) governance.WorkspaceSettings {
	var settings governance.WorkspaceSettings
	_ = json.Unmarshal(data, &settings)
	if settings.RuleMode == "" {
		settings.RuleMode = governance.RuleModeOff
	}
	return settings
}

func writeGovernanceProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	problem, ok := governance.SafeProblemFor(code)
	if !ok {
		problem, _ = governance.SafeProblemFor("configuration_unavailable")
	}
	id := strings.TrimSpace(middleware.GetReqID(r.Context()))
	if id == "" {
		candidate := strings.TrimSpace(r.Header.Get(middleware.RequestIDHeader))
		if _, err := uuid.Parse(candidate); err == nil {
			id = candidate
		} else {
			id = uuid.NewString()
		}
	}
	problem.CorrelationID = id
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set(middleware.RequestIDHeader, id)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

var (
	errGovernanceConfigConflict            = errors.New("governance config version conflict")
	errGovernanceConfigIdempotencyConflict = errors.New("governance config request id conflict")
	errGovernanceConfigRollbackMissing     = errors.New("governance config rollback version unavailable")
	errGovernanceConfigInvalid             = errors.New("governance config invalid")
)
