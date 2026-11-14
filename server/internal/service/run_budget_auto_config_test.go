package service

import "testing"

// The auto-approver ships off: only an explicit enabled:true turns it on, and
// a partial or malformed setting never does.
func TestRunBudgetAutoConfigIsOffUnlessEnabled(t *testing.T) {
	cases := []struct {
		name     string
		settings string
		enabled  bool
		envelope int
	}{
		{"no settings", ``, false, runBudgetAutoDefaultEnvelope},
		{"empty document", `{}`, false, runBudgetAutoDefaultEnvelope},
		{"envelope only", `{"run_budget_auto":{"daily_envelope":3}}`, false, 3},
		{"explicitly on", `{"run_budget_auto":{"enabled":true}}`, true, runBudgetAutoDefaultEnvelope},
		{"explicitly off", `{"run_budget_auto":{"enabled":false}}`, false, runBudgetAutoDefaultEnvelope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := RunBudgetAutoConfigFromSettings([]byte(tc.settings))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Enabled != tc.enabled || cfg.DailyEnvelope != tc.envelope {
				t.Fatalf("cfg = %+v, want enabled %v envelope %d", cfg, tc.enabled, tc.envelope)
			}
		})
	}
	if _, err := RunBudgetAutoConfigFromSettings([]byte(`{"run_budget_auto":`)); err == nil {
		t.Fatal("malformed settings must return an error")
	}
}
