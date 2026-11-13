package main

import "testing"

// Migration 971 builds the unique arbiter for InsertDMProgressDataIncident's
// ON CONFLICT. An interrupted build leaves an INVALID index that IF NOT EXISTS
// skips on retry, so the version needs both the invalid-index cleanup and the
// post-run validity check.
func TestDMProgressIncidentIndexIsRecoverable(t *testing.T) {
	t.Parallel()
	const version = "971_dm_progress_data_incident_follow_kind_idx"
	const index = "idx_dm_progress_data_incident_follow_kind"
	if got := concurrentIndexCleanups[version]; got != index {
		t.Errorf("cleanup index = %q, want %q", got, index)
	}
	req, ok := requiredConcurrentIndexes[version]
	if !ok {
		t.Fatalf("%s missing from requiredConcurrentIndexes", version)
	}
	if req.IndexRegclass != index || req.TableRegclass != "dm_progress_data_incident" || !req.Unique || !equalIndexColumns(req.Columns, []string{"follow_id", "kind"}) {
		t.Errorf("requirement = %+v", req)
	}
}
