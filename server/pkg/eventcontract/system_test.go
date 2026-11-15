package eventcontract

import (
	"slices"
	"testing"
)

func TestSystemTypes(t *testing.T) {
	want := []string{EventRuntimeOnline, EventRuntimeOffline, EventServerDeployed, EventAutopilotSkipped, EventAutopilotFailed}
	if !slices.Equal(SystemTypes, want) {
		t.Fatalf("SystemTypes = %v, want %v", SystemTypes, want)
	}
	for _, e := range want {
		if !IsSystem(e) {
			t.Errorf("IsSystem(%q) = false", e)
		}
	}
	// daemon.restarted is reserved: marked system, not emitted, not in SystemTypes.
	if !IsSystem(EventDaemonRestarted) || slices.Contains(SystemTypes, EventDaemonRestarted) {
		t.Errorf("daemon.restarted must be system but not emitted")
	}
	for _, e := range slices.Concat(WakeupTypes, LifecycleTypes) {
		if IsSystem(e) {
			t.Errorf("issue fact %q marked system", e)
		}
		if slices.Contains(WakeupTypes, e) && slices.Contains(SystemTypes, e) {
			t.Errorf("%q in both lists", e)
		}
	}
}
