package handler

import "testing"

// CHE-5093 (CHE-4678 S9 P4): the claim hands the daemon the script name and
// args, only for script-mode autopilots. A script autopilot whose stored config
// cannot be read still claims a script (with no name) so the daemon fails the
// task closed instead of starting a model session.
func TestClaimScriptFor(t *testing.T) {
	t.Parallel()
	if got := claimScriptFor("run_only", []byte(`{"name":"x","args":[]}`)); got != nil {
		t.Fatalf("run_only must carry no script, got %+v", got)
	}
	if got := claimScriptFor("create_issue", nil); got != nil {
		t.Fatalf("create_issue must carry no script, got %+v", got)
	}
	got := claimScriptFor("script", []byte(`{"name":"sweep-reconcile","args":["--once"]}`))
	if got == nil || got.Name != "sweep-reconcile" || len(got.Args) != 1 || got.Args[0] != "--once" {
		t.Fatalf("script config not passed through: %+v", got)
	}
	for _, raw := range [][]byte{nil, []byte(`{`), []byte(`null`)} {
		got := claimScriptFor("script", raw)
		if got == nil || got.Name != "" || len(got.Args) != 0 {
			t.Fatalf("unreadable config %q must yield an empty (fail-closed) script, got %+v", raw, got)
		}
	}
}
