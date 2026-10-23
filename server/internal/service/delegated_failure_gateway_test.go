package service

import (
	"context"
	"testing"
)

// A transient gateway failure (provider 5xx, capacity) must not wake the
// coordinator with an LLM run; any other final failure still does.
func TestFinalDelegatedFailureCoordinatorWakeByReason(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		errMsg     string
		wantWakeup bool
	}{
		{"provider 503", "agent_error.provider_server_error", "API Error: 503 service unavailable", false},
		{"provider capacity", "agent_error.provider_capacity_or_rate_limit", "429 rate limit exceeded", false},
		{"process failure", "agent_error.process_failure", "exit status 1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, svc := seedDelegatedFailureFixture(t)
			ctx := context.Background()
			failedID := f.insertWorkerTask(t, "running", "comment", 1, 2)

			if _, err := svc.FailTask(ctx, failedID, tc.errMsg, "", "", "", tc.reason, false, "", ""); err != nil {
				t.Fatalf("FailTask: %v", err)
			}

			var coordinatorRuns, coordinatorComments int
			if err := f.pool.QueryRow(ctx, `
				SELECT count(*) FROM agent_task_queue
				WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).
				Scan(&coordinatorRuns); err != nil {
				t.Fatalf("count coordinator runs: %v", err)
			}
			if err := f.pool.QueryRow(ctx, `
				SELECT count(*) FROM comment
				WHERE issue_id = $1 AND author_type = 'system' AND type = 'progress_update' AND source_task_id = $2`, f.issueID, failedID).
				Scan(&coordinatorComments); err != nil {
				t.Fatalf("count coordinator comments: %v", err)
			}
			want := 0
			if tc.wantWakeup {
				want = 1
			}
			if coordinatorRuns != want || coordinatorComments != want {
				t.Fatalf("coordinator runs=%d comments=%d, want %d each", coordinatorRuns, coordinatorComments, want)
			}
		})
	}
}
