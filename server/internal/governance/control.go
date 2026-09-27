package governance

import (
	"errors"
	"strings"
)

type RuleMode string

const (
	RuleModeOff        RuleMode = "off"
	RuleModeShadow     RuleMode = "shadow"
	RuleModeCorrection RuleMode = "correction"
)

type GateAction string

const (
	GateEvaluation GateAction = "evaluation"
	GateFallback   GateAction = "fallback"
	GateCorrection GateAction = "correction"
	GateRecovery   GateAction = "recovery"
)

type GateInput struct {
	Action                 GateAction
	TransportEnabled       bool
	CorrectionQualified    bool
	NativeObligationProven bool
}

type WorkspaceConfig struct {
	Version      int64             `json:"version"`
	ControlEpoch int64             `json:"control_epoch"`
	Settings     WorkspaceSettings `json:"settings"`
}

type WorkspaceSettings struct {
	JevGovernanceEnabled         bool            `json:"jev_governance_enabled"`
	JevFallbackAgentsEnabled     bool            `json:"jev_fallback_agents_enabled"`
	GovernanceCorrectionsEnabled bool            `json:"governance_corrections_enabled"`
	RuleMode                     RuleMode        `json:"rule_mode"`
	Limits                       OperatingLimits `json:"limits"`
}

type OperatingLimits struct {
	MaxRefreshes              *int64         `json:"max_refreshes,omitempty"`
	MaxEvaluations            *int64         `json:"max_evaluations,omitempty"`
	AdmissionWindowSeconds    *int64         `json:"admission_window_seconds,omitempty"`
	WorkspaceSpendCapMicroUSD *int64         `json:"workspace_spend_cap_micro_usd,omitempty"`
	PricingPolicy             *PricingPolicy `json:"pricing_policy,omitempty"`
}

type PricingPolicy struct {
	Version                        string `json:"version"`
	InputMicroUSDPerMillionTokens  *int64 `json:"input_micro_usd_per_million_tokens,omitempty"`
	OutputMicroUSDPerMillionTokens *int64 `json:"output_micro_usd_per_million_tokens,omitempty"`
}

func DefaultWorkspaceConfig() WorkspaceConfig {
	return WorkspaceConfig{Settings: WorkspaceSettings{RuleMode: RuleModeOff}}
}

func (config WorkspaceConfig) MissingRequirements() []string {
	missing := make([]string, 0, 6)
	limits := config.Settings.Limits
	if !positiveLimit(limits.MaxRefreshes) {
		missing = append(missing, "max_refreshes")
	}
	if !positiveLimit(limits.MaxEvaluations) {
		missing = append(missing, "max_evaluations")
	}
	if !positiveLimit(limits.AdmissionWindowSeconds) {
		missing = append(missing, "admission_window_seconds")
	}
	if !positiveLimit(limits.WorkspaceSpendCapMicroUSD) {
		missing = append(missing, "workspace_spend_cap_micro_usd")
	}
	if limits.PricingPolicy == nil || strings.TrimSpace(limits.PricingPolicy.Version) == "" ||
		!nonNegativeLimit(limits.PricingPolicy.InputMicroUSDPerMillionTokens) ||
		!nonNegativeLimit(limits.PricingPolicy.OutputMicroUSDPerMillionTokens) {
		missing = append(missing, "pricing_policy")
	}
	return missing
}

func (config WorkspaceConfig) Allows(input GateInput) bool {
	settings := config.Settings
	if !settings.JevGovernanceEnabled {
		return false
	}
	switch input.Action {
	case GateEvaluation:
		return input.TransportEnabled && settings.RuleMode != RuleModeOff && len(config.MissingRequirements()) == 0
	case GateFallback:
		return settings.JevFallbackAgentsEnabled && settings.RuleMode != RuleModeOff && len(config.MissingRequirements()) == 0
	case GateCorrection:
		return settings.GovernanceCorrectionsEnabled && settings.RuleMode == RuleModeCorrection && input.CorrectionQualified && len(config.MissingRequirements()) == 0
	case GateRecovery:
		return settings.GovernanceCorrectionsEnabled && settings.RuleMode == RuleModeCorrection && input.NativeObligationProven
	default:
		return false
	}
}

func ValidateWorkspaceSettings(settings WorkspaceSettings) error {
	if settings.RuleMode != RuleModeOff && settings.RuleMode != RuleModeShadow && settings.RuleMode != RuleModeCorrection {
		return errors.New("invalid governance rule mode")
	}
	for _, value := range []*int64{
		settings.Limits.MaxRefreshes,
		settings.Limits.MaxEvaluations,
		settings.Limits.AdmissionWindowSeconds,
		settings.Limits.WorkspaceSpendCapMicroUSD,
	} {
		if value != nil && *value <= 0 {
			return errors.New("governance limits must be positive")
		}
	}
	if policy := settings.Limits.PricingPolicy; policy != nil {
		if strings.TrimSpace(policy.Version) == "" ||
			policy.InputMicroUSDPerMillionTokens != nil && *policy.InputMicroUSDPerMillionTokens < 0 ||
			policy.OutputMicroUSDPerMillionTokens != nil && *policy.OutputMicroUSDPerMillionTokens < 0 {
			return errors.New("invalid governance pricing policy")
		}
	}
	return nil
}

type LimitClassification string

const (
	LimitSafetyCap         LimitClassification = "immutable_safety_cap"
	LimitDeploymentSetting LimitClassification = "deployment_setting"
	LimitWorkspaceSetting  LimitClassification = "workspace_setting"
)

type LimitAudit struct {
	Version   int64  `json:"version"`
	RequestID string `json:"request_id"`
	ActorID   string `json:"actor_id"`
	Previous  any    `json:"previous,omitempty"`
	Effective any    `json:"effective,omitempty"`
	ChangedAt string `json:"changed_at"`
}

type OperatingLimit struct {
	Key             string              `json:"key"`
	Classification  LimitClassification `json:"classification"`
	Rationale       string              `json:"rationale"`
	Owner           string              `json:"owner"`
	AllowedRange    string              `json:"allowed_range"`
	EffectiveValue  any                 `json:"effective_value"`
	EffectiveSource string              `json:"effective_source"`
	AuditHistory    []LimitAudit        `json:"audit_history"`
	RollbackTo      []int64             `json:"rollback_to_versions"`
}

func DescribeOperatingLimits(config WorkspaceConfig, audit []LimitAudit) []OperatingLimit {
	settings := config.Settings.Limits
	return []OperatingLimit{
		limit("config_request_bytes", LimitSafetyCap, "Bounds configuration input parsing.", "Multica", "1..16384 bytes", int64(16<<10), "application", audit),
		limit("max_refreshes", LimitWorkspaceSetting, "Bounds same-case evidence refreshes.", "workspace owner/admin", "positive integer within deployment cap", pointerValue(settings.MaxRefreshes), effectiveSource(settings.MaxRefreshes, "workspace"), audit),
		limit("max_evaluations", LimitWorkspaceSetting, "Bounds total Jev evaluations per work item.", "workspace owner/admin", "positive integer within deployment cap", pointerValue(settings.MaxEvaluations), effectiveSource(settings.MaxEvaluations, "workspace"), audit),
		limit("admission_window_seconds", LimitWorkspaceSetting, "Defines the workspace admission accounting window.", "workspace owner/admin", "positive integer within deployment policy", pointerValue(settings.AdmissionWindowSeconds), effectiveSource(settings.AdmissionWindowSeconds, "workspace"), audit),
		limit("workspace_spend_cap_micro_usd", LimitWorkspaceSetting, "Caps workspace spend in the admission window.", "workspace owner/admin", "positive integer within deployment cap", pointerValue(settings.WorkspaceSpendCapMicroUSD), effectiveSource(settings.WorkspaceSpendCapMicroUSD, "workspace"), audit),
		limit("pricing_policy_version", LimitDeploymentSetting, "Pins the price schedule used for cost estimation.", "deployment operator", "non-empty version identifier", pricingValue(settings.PricingPolicy, func(policy *PricingPolicy) any { return policy.Version }), effectivePricingSource(settings.PricingPolicy), audit),
		limit("input_price_micro_usd_per_million_tokens", LimitDeploymentSetting, "Prices billable input tokens under the pinned schedule.", "deployment operator", "non-negative integer", pricingValue(settings.PricingPolicy, func(policy *PricingPolicy) any { return pointerValue(policy.InputMicroUSDPerMillionTokens) }), effectivePricingSource(settings.PricingPolicy), audit),
		limit("output_price_micro_usd_per_million_tokens", LimitDeploymentSetting, "Prices billable output tokens under the pinned schedule.", "deployment operator", "non-negative integer", pricingValue(settings.PricingPolicy, func(policy *PricingPolicy) any { return pointerValue(policy.OutputMicroUSDPerMillionTokens) }), effectivePricingSource(settings.PricingPolicy), audit),
	}
}

func ResolveOperatingLimit(key string, workspaceOverride, ruleOverride int64) (OperatingLimit, bool) {
	for _, definition := range DescribeOperatingLimits(DefaultWorkspaceConfig(), nil) {
		if definition.Key != key {
			continue
		}
		if definition.Classification == LimitSafetyCap {
			return definition, true
		}
		if ruleOverride != 0 {
			return definition, true
		}
		if workspaceOverride != 0 {
			definition.EffectiveValue = workspaceOverride
			definition.EffectiveSource = "workspace"
		}
		return definition, true
	}
	return OperatingLimit{}, false
}

func limit(key string, classification LimitClassification, rationale, owner, allowedRange string, value any, source string, audit []LimitAudit) OperatingLimit {
	versions := make([]int64, 0, len(audit))
	for _, entry := range audit {
		if entry.Version > 0 {
			versions = append(versions, entry.Version)
		}
	}
	return OperatingLimit{
		Key: key, Classification: classification, Rationale: rationale, Owner: owner,
		AllowedRange: allowedRange, EffectiveValue: value, EffectiveSource: source,
		AuditHistory: append([]LimitAudit(nil), audit...), RollbackTo: versions,
	}
}

func effectiveSource(value *int64, source string) string {
	if value == nil {
		return "unset"
	}
	return source
}

func effectivePricingSource(policy *PricingPolicy) string {
	if policy == nil {
		return "unset"
	}
	return "deployment"
}

func pricingValue(policy *PricingPolicy, value func(*PricingPolicy) any) any {
	if policy == nil {
		return nil
	}
	return value(policy)
}

func pointerValue(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func positiveLimit(value *int64) bool {
	return value != nil && *value > 0
}

func nonNegativeLimit(value *int64) bool {
	return value != nil && *value >= 0
}

type SafeProblem struct {
	Problem           string `json:"problem"`
	Cause             string `json:"cause"`
	PermittedFix      string `json:"permitted_fix"`
	Retryable         bool   `json:"retryable"`
	CorrelationID     string `json:"correlation_id,omitempty"`
	DocumentationLink string `json:"documentation_link"`
}

func SafeProblemFor(code string) (SafeProblem, bool) {
	problems := map[string]SafeProblem{
		"revision_conflict": {
			Problem: "configuration_revision_conflict", Cause: "The workspace configuration changed after it was read.",
			PermittedFix: "Reload the current configuration and retry with its version.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#revision_conflict",
		},
		"invalid_request": {
			Problem: "invalid_governance_configuration", Cause: "The request does not match the supported configuration contract.",
			PermittedFix: "Correct the request fields and submit a new request.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#invalid_request",
		},
		"agent_denied": {
			Problem: "human_admin_required", Cause: "This operation requires a human workspace owner or admin.",
			PermittedFix: "Have a workspace owner or admin make this change.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#agent_denied",
		},
		"permission_denied": {
			Problem: "workspace_admin_required", Cause: "The current workspace role cannot change governance configuration.",
			PermittedFix: "Have a workspace owner or admin make this change.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#permission_denied",
		},
		"unauthorized": {
			Problem: "authentication_required", Cause: "The request has no valid authenticated workspace actor.",
			PermittedFix: "Authenticate and retry the request.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#unauthorized",
		},
		"workspace_not_found": {
			Problem: "workspace_unavailable", Cause: "The workspace is unavailable to the current actor.",
			PermittedFix: "Verify workspace access and retry.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#workspace_not_found",
		},
		"idempotency_conflict": {
			Problem: "idempotency_key_conflict", Cause: "The request identifier was already used with different configuration data.",
			PermittedFix: "Use a new request identifier for the corrected request.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#idempotency_conflict",
		},
		"rollback_unavailable": {
			Problem: "configuration_rollback_unavailable", Cause: "The requested prior configuration version is not available.",
			PermittedFix: "Choose a version present in the workspace audit history.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#rollback_unavailable",
		},
		"stale_control_epoch": {
			Problem: "governance_control_epoch_stale", Cause: "Governance control changed while this operation was in progress.",
			PermittedFix: "Reload the current configuration and retry only if the operation remains permitted.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#stale_control_epoch",
		},
		"configuration_unavailable": {
			Problem: "governance_configuration_unavailable", Cause: "The workspace configuration could not be loaded safely.",
			PermittedFix: "Retry later; governance remains disabled until configuration is available.", Retryable: true,
			DocumentationLink: "/docs/governance/errors#configuration_unavailable",
		},
		"credential_unavailable": {
			Problem: "credential_storage_unavailable", Cause: "Credential storage is not configured on this server.",
			PermittedFix: "Configure the server's credential encryption key, then retry.", Retryable: true,
			DocumentationLink: "/docs/governance/errors#credential_unavailable",
		},
		"credential_not_found": {
			Problem: "credential_not_found", Cause: "No credential is stored for this workspace and purpose.",
			PermittedFix: "Write a credential before deleting or relying on it.", Retryable: false,
			DocumentationLink: "/docs/governance/errors#credential_not_found",
		},
	}
	problem, ok := problems[code]
	return problem, ok
}
