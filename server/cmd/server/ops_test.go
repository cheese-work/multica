package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func opsGet(t *testing.T, token, query string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/ops"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func opsTokenFor(t *testing.T, role string) string {
	t.Helper()
	ctx := context.Background()
	var userID string
	email := uuid.NewString() + "@multica.ai"
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "Ops "+role, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, testWorkspaceID, userID, role); err != nil {
		t.Fatal(err)
	}
	token, err := generateTestJWT(userID, email, "Ops "+role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// /ops is an operator view: owner and admin only, a plain member is refused.
func TestOpsRoleGate(t *testing.T) {
	for role, want := range map[string]int{"owner": 200, "admin": 200, "member": 403} {
		if status, body := opsGet(t, opsTokenFor(t, role), "?window=1h"); status != want {
			t.Errorf("%s GET /api/ops = %d %s, want %d", role, status, body, want)
		}
	}
}

func TestOpsWindowParam(t *testing.T) {
	token := opsTokenFor(t, "admin")
	for _, w := range []string{"1h", "24h", "7d", "30d", ""} {
		if status, body := opsGet(t, token, "?window="+w); status != 200 {
			t.Errorf("window %q = %d %s, want 200", w, status, body)
		}
	}
	for _, w := range []string{"2h", "90d", "garbage"} {
		if status, _ := opsGet(t, token, "?window="+w); status != 400 {
			t.Errorf("window %q = %d, want 400", w, status)
		}
	}
}

type opsBody struct {
	Window        string `json:"window"`
	BucketS       int    `json:"bucket_s"`
	ThresholdP95S int    `json:"threshold_p95_s"`
	Now           struct {
		Machines []struct {
			Machine string `json:"machine"`
			Queued  int    `json:"queued"`
			Running int    `json:"running"`
		} `json:"machines"`
		QueueWaitP95S float64 `json:"queue_wait_p95_s"`
	} `json:"now"`
	Runs []struct {
		Machine    string `json:"machine"`
		Source     string `json:"source"`
		RunningMax int    `json:"running_max"`
	} `json:"runs"`
	Requeues []struct {
		ClaimFailures int `json:"claim_failures"`
		StaleReclaims int `json:"stale_reclaims"`
	} `json:"requeues"`
	QueueWait []struct {
		Machine string  `json:"machine"`
		Samples int     `json:"samples"`
		P95S    float64 `json:"p95_s"`
	} `json:"queue_wait"`
	Failures []struct {
		Machine       string `json:"machine"`
		FailureReason string `json:"failure_reason"`
		TaskCount     int    `json:"task_count"`
	} `json:"failures"`
}

// A terminal task with no runtime is attributed to __unknown__ in charts 2
// and 3, the all-machines wait row keeps machine "", and the snapshot rows
// feed the "now" strip and chart 1.
func TestOpsPayloadAttributesUnknownMachine(t *testing.T) {
	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		testPool.Exec(bg, `DELETE FROM agent_task_queue WHERE agent_id = $1 AND runtime_id IS NULL`, agentID)
		testPool.Exec(bg, `DELETE FROM ops_snapshot WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(bg, `DELETE FROM agent_task_event WHERE workspace_id = $1`, testWorkspaceID)
	})
	for _, q := range []string{
		`INSERT INTO agent_task_queue (agent_id, status, created_at, started_at, completed_at, failure_reason)
		 VALUES ($1, 'failed', now() - interval '10 min', now() - interval '9 min', now() - interval '8 min', 'timeout')`,
		`INSERT INTO agent_task_queue (agent_id, status, created_at, started_at, completed_at)
		 VALUES ($1, 'completed', now() - interval '20 min', now() - interval '19 min', now() - interval '18 min')`,
	} {
		if _, err := testPool.Exec(ctx, q, agentID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO ops_snapshot (workspace_id, daemon_id, bucket_ts, queued, running)
		VALUES ($1, 'machine-a', date_bin('30 seconds', now(), TIMESTAMPTZ 'epoch'), 2, 3)`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO agent_task_event (workspace_id, task_id, kind, at)
		VALUES ($1, $2, 'stale_reclaim', now() - interval '5 min')`, testWorkspaceID, uuid.New()); err != nil {
		t.Fatal(err)
	}

	status, raw := opsGet(t, opsTokenFor(t, "admin"), "?window=1h")
	if status != 200 {
		t.Fatalf("GET /api/ops = %d %s", status, raw)
	}
	var body opsBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if body.Window != "1h" || body.BucketS != 60 || body.ThresholdP95S != 900 {
		t.Errorf("window/bucket/threshold = %q/%d/%d, want 1h/60/900", body.Window, body.BucketS, body.ThresholdP95S)
	}
	if len(body.Now.Machines) != 1 || body.Now.Machines[0].Machine != "machine-a" || body.Now.Machines[0].Queued != 2 || body.Now.Machines[0].Running != 3 {
		t.Errorf("now.machines = %+v, want machine-a queued 2 running 3", body.Now.Machines)
	}
	if body.Now.QueueWaitP95S < 59 || body.Now.QueueWaitP95S > 61 {
		t.Errorf("now.queue_wait_p95_s = %v, want ~60", body.Now.QueueWaitP95S)
	}
	var unknownFail, allWait, unknownWait int
	for _, f := range body.Failures {
		if f.Machine == "__unknown__" && f.FailureReason == "timeout" {
			unknownFail += f.TaskCount
		}
	}
	for _, w := range body.QueueWait {
		switch w.Machine {
		case "":
			allWait += w.Samples
		case "__unknown__":
			unknownWait += w.Samples
		}
	}
	if unknownFail != 1 {
		t.Errorf("failures __unknown__/timeout = %d, want 1 (%+v)", unknownFail, body.Failures)
	}
	if allWait != 2 || unknownWait != 2 {
		t.Errorf("queue wait samples all=%d unknown=%d, want 2 and 2", allWait, unknownWait)
	}
	var snap bool
	for _, r := range body.Runs {
		if r.Machine == "machine-a" && r.Source == "snapshot" && r.RunningMax == 3 {
			snap = true
		}
	}
	if !snap {
		t.Errorf("runs has no snapshot point for machine-a: %+v", body.Runs)
	}
	if len(body.Requeues) != 1 || body.Requeues[0].StaleReclaims != 1 {
		t.Errorf("requeues = %+v, want one stale_reclaim", body.Requeues)
	}
}
