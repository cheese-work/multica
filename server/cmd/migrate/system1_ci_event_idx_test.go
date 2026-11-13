package main

import (
	"strings"
	"testing"
)

// CHE-4276: one rule row per CI run attempt, enforced by the database.
const system1CIEventSubjectVersion = "9992_system1_observation_ci_event_subject_idx"

func TestSystem1CIEventSubjectIndexIsRecoverableAndUnique(t *testing.T) {
	t.Parallel()
	const index = "system1_observation_ci_event_subject"
	if got := concurrentIndexCleanups[system1CIEventSubjectVersion]; got != index {
		t.Errorf("cleanup index = %q, want %q", got, index)
	}
	req, ok := requiredConcurrentIndexes[system1CIEventSubjectVersion]
	if !ok || req.IndexRegclass != index || req.TableRegclass != "system1_observation" || !req.Unique ||
		!equalIndexColumns(req.Columns, []string{"workspace_id", "subject_id"}) || !strings.Contains(req.Predicate, "ci_event") {
		t.Errorf("requirement = %+v", req)
	}
	up := stripSQLComments(readWakeupMigration(t, system1CIEventSubjectVersion, "up"))
	if strings.Count(up, ";") != 1 || !strings.Contains(up, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "+index) {
		t.Errorf("up must be one concurrent re-runnable build:\n%s", up)
	}
	down := stripSQLComments(readWakeupMigration(t, system1CIEventSubjectVersion, "down"))
	if strings.Count(down, ";") != 1 || !strings.Contains(down, "DROP INDEX CONCURRENTLY IF EXISTS "+index) {
		t.Errorf("down must be one concurrent drop:\n%s", down)
	}
}
