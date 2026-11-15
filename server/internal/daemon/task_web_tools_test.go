package daemon

import "testing"

func TestTaskDisableProviderWebToolsOnlyWhenElevated(t *testing.T) {
	for profile, want := range map[string]bool{"": false, "restricted": false, "root": false, "elevated": true} {
		if got := (Task{Profile: profile}).DisableProviderWebTools(); got != want {
			t.Errorf("profile %q: got %v, want %v", profile, got, want)
		}
	}
}
