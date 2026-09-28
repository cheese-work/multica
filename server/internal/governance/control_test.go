package governance

import (
	"strings"
	"testing"
)

func TestDefaultWorkspaceConfigFailsClosed(t *testing.T) {
	config := DefaultWorkspaceConfig()
	if config.Settings.JevGovernanceEnabled || config.Settings.JevFallbackAgentsEnabled || config.Settings.GovernanceCorrectionsEnabled {
		t.Fatal("new governance controls must default off")
	}
	if config.Settings.RuleMode != RuleModeOff {
		t.Fatalf("rule mode = %q, want off", config.Settings.RuleMode)
	}
	if config.Settings.Limits.MaxRefreshes != nil || config.Settings.Limits.MaxEvaluations != nil ||
		config.Settings.Limits.AdmissionWindowSeconds != nil || config.Settings.Limits.WorkspaceSpendCapMicroUSD != nil ||
		config.Settings.Limits.PricingPolicy != nil {
		t.Fatal("production operating limits must remain unset")
	}
	if missing := config.MissingRequirements(); len(missing) == 0 {
		t.Fatal("default configuration must report missing production requirements")
	}
	for _, action := range []GateAction{GateEvaluation, GateFallback, GateCorrection, GateRecovery} {
		if config.Allows(GateInput{Action: action, TransportEnabled: true, CorrectionQualified: true, NativeObligationProven: true}) {
			t.Errorf("default configuration allowed %q", action)
		}
	}
}

func TestWorkspaceGovernanceGateMatrix(t *testing.T) {
	for _, mode := range []RuleMode{RuleModeOff, RuleModeShadow, RuleModeCorrection} {
		for _, master := range []bool{false, true} {
			for _, transport := range []bool{false, true} {
				for _, fallback := range []bool{false, true} {
					for _, corrections := range []bool{false, true} {
						config := completeWorkspaceConfig()
						config.Settings.RuleMode = mode
						config.Settings.JevGovernanceEnabled = master
						config.Settings.JevFallbackAgentsEnabled = fallback
						config.Settings.GovernanceCorrectionsEnabled = corrections
						for _, qualified := range []bool{false, true} {
							for _, obligation := range []bool{false, true} {
								cases := []struct {
									action GateAction
									want   bool
								}{
									{action: GateEvaluation, want: master && transport && mode != RuleModeOff},
									{action: GateFallback, want: master && fallback && mode != RuleModeOff},
									{action: GateCorrection, want: master && corrections && mode == RuleModeCorrection && qualified},
									{action: GateRecovery, want: master && corrections && mode == RuleModeCorrection && obligation},
								}
								for _, tc := range cases {
									got := config.Allows(GateInput{
										Action:                 tc.action,
										TransportEnabled:       transport,
										CorrectionQualified:    qualified,
										NativeObligationProven: obligation,
									})
									if got != tc.want {
										t.Errorf("mode=%s master=%v transport=%v fallback=%v corrections=%v qualified=%v obligation=%v action=%s: got %v, want %v",
											mode, master, transport, fallback, corrections, qualified, obligation, tc.action, got, tc.want)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestJevTransportFlagDoesNotEnableGovernanceMaster(t *testing.T) {
	config := completeWorkspaceConfig()
	if config.Settings.JevGovernanceEnabled {
		t.Fatal("fixture unexpectedly enables the master gate")
	}
	if config.Allows(GateInput{Action: GateEvaluation, TransportEnabled: true}) {
		t.Fatal("legacy Jev transport flag enabled governance evaluation")
	}
}

func TestValidateWorkspaceSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings WorkspaceSettings
		valid    bool
	}{
		{name: "default off with missing production limits", settings: DefaultWorkspaceConfig().Settings, valid: true},
		{name: "known rule mode", settings: completeWorkspaceConfig().Settings, valid: true},
		{name: "unknown rule mode", settings: WorkspaceSettings{RuleMode: "automatic"}, valid: false},
		{name: "nonpositive refresh limit", settings: WorkspaceSettings{RuleMode: RuleModeOff, Limits: OperatingLimits{MaxRefreshes: int64Pointer(0)}}, valid: false},
		{name: "negative pricing", settings: WorkspaceSettings{RuleMode: RuleModeOff, Limits: OperatingLimits{PricingPolicy: &PricingPolicy{Version: "v1", InputMicroUSDPerMillionTokens: int64Pointer(-1)}}}, valid: false},
		{name: "blank pricing version", settings: WorkspaceSettings{RuleMode: RuleModeOff, Limits: OperatingLimits{PricingPolicy: &PricingPolicy{InputMicroUSDPerMillionTokens: int64Pointer(0)}}}, valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateWorkspaceSettings(test.settings)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateWorkspaceSettings() error = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestOperatingLimitCatalogIsClassifiedAndSafeCapsAreImmutable(t *testing.T) {
	config := completeWorkspaceConfig()
	limits := DescribeOperatingLimits(config, nil)
	if len(limits) < 5 {
		t.Fatalf("limit catalog has %d entries, want at least 5", len(limits))
	}
	seen := map[string]bool{}
	for _, limit := range limits {
		if limit.Key == "" || seen[limit.Key] || limit.Rationale == "" || limit.Owner == "" || limit.AllowedRange == "" {
			t.Fatalf("incomplete or duplicate limit description: %+v", limit)
		}
		seen[limit.Key] = true
		if limit.EffectiveSource == "" {
			t.Errorf("limit %q has no effective source", limit.Key)
		}
	}
	configBytes, ok := ResolveOperatingLimit("config_request_bytes", 1, 2)
	if !ok || configBytes.EffectiveValue != int64(16<<10) || configBytes.Classification != LimitSafetyCap {
		t.Fatalf("rule input changed immutable config safety cap: %+v, found=%v", configBytes, ok)
	}
}

func TestGovernanceSafeProblemUsesStableNonSensitiveFields(t *testing.T) {
	problem, ok := SafeProblemFor("revision_conflict")
	if !ok {
		t.Fatal("revision conflict has no safe problem contract")
	}
	for name, value := range map[string]string{
		"problem":            problem.Problem,
		"cause":              problem.Cause,
		"permitted fix":      problem.PermittedFix,
		"documentation link": problem.DocumentationLink,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if problem.CorrelationID != "" {
		t.Fatal("static problem catalog must not invent a correlation ID")
	}
	if strings.Contains(strings.ToLower(problem.Cause+problem.PermittedFix), "api_key") {
		t.Fatal("safe problem contract contains secret-like detail")
	}
}

func completeWorkspaceConfig() WorkspaceConfig {
	zero := int64(0)
	return WorkspaceConfig{Settings: WorkspaceSettings{
		RuleMode: RuleModeShadow,
		Limits: OperatingLimits{
			MaxRefreshes:              int64Pointer(2),
			MaxEvaluations:            int64Pointer(5),
			AdmissionWindowSeconds:    int64Pointer(60),
			WorkspaceSpendCapMicroUSD: int64Pointer(1000),
			PricingPolicy: &PricingPolicy{
				Version:                        "synthetic-test-v1",
				InputMicroUSDPerMillionTokens:  &zero,
				OutputMicroUSDPerMillionTokens: &zero,
			},
		},
	}}
}

func int64Pointer(value int64) *int64 {
	return &value
}
