package daemon

import (
	"encoding/json"
	"testing"
)

// A claim without a usable profile (older server, unknown value) is restricted.
func TestTaskEffectiveProfileDefaultsToRestricted(t *testing.T) {
	for body, want := range map[string]string{
		`{"id":"t"}`:                  "restricted",
		`{"id":"t","profile":""}`:     "restricted",
		`{"id":"t","profile":"root"}`: "restricted",
		`{"id":"t","profile":"elevated","scopes":["docker"]}`: "elevated",
	} {
		var task Task
		if err := json.Unmarshal([]byte(body), &task); err != nil {
			t.Fatal(err)
		}
		if got := task.EffectiveProfile(); got != want {
			t.Errorf("%s: profile = %q, want %q", body, got, want)
		}
	}
}
