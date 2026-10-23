package service

import (
	"context"
	"testing"
)

// A transient gateway failure the platform will retry must not wake the
// coordinator with an LLM run; a failure with no retry pending still does.
func TestFinalDelegatedFailureCoordinatorWakeByRetryState(t *testing.T) {
	cases := []struct {
		name        string
		reason      string
		errMsg      string
		attempt     int32
		maxAttempts int32
		wantWakeup  bool
	}{
		{"retry pending", "agent_error.provider_network", "API Error: Connection closed mid-response", 1, 3, false},
		{"retry exhausted", "agent_error.provider_network", "API Error: Connection closed mid-response", 3, 3, true},
		{"not retried 503", "agent_error.provider_server_error", "API Error: 503 service unavailable", 1, 2, true},
		{"process failure", "agent_error.process_failure", "exit status 1", 1, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, svc := seedDelegatedFailureFixture(t)
			ctx := context.Background()
			failedID := f.insertWorkerTask(t, "running", "comment", tc.attempt, tc.maxAttempts)

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
