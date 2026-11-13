package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// CHE-4383 B4a over the real router: only the assigned labeller's task token writes a label; identity comes from the token.
func TestSystem1LabelWriteRoute(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	var previous []byte
	if err := testPool.QueryRow(ctx, `SELECT coalesce(settings,'{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	var labeller, other, runtime string
	if err := testPool.QueryRow(ctx, `SELECT id::text, runtime_id::text FROM agent WHERE workspace_id=$1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&labeller, &runtime); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO agent (workspace_id, name, runtime_id, runtime_mode, owner_id) SELECT workspace_id, 'label-route-other', runtime_id, runtime_mode, owner_id FROM agent WHERE id=$1 RETURNING id::text`, labeller).Scan(&other); err != nil {
		t.Fatal(err)
	}
	var task, task2, observation string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority) VALUES ($1,$2,'running',0) RETURNING id::text`, labeller, runtime).Scan(&task); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority) VALUES ($1,$2,'running',0) RETURNING id::text`, labeller, runtime).Scan(&task2); err != nil {
		t.Fatal(err)
	}
	var otherTask string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority) VALUES ($1,$2,'running',0) RETURNING id::text`, other, runtime).Scan(&otherTask); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, inputs) VALUES ($1,'decision',gen_random_uuid(),'reversible','{"topic":"test_first_waiver"}') RETURNING id::text`, testWorkspaceID).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
		testPool.Exec(ctx, `DELETE FROM system1_label WHERE observation_id=$1`, observation)
		testPool.Exec(ctx, `DELETE FROM system1_observation WHERE id=$1`, observation)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = ANY($1::uuid[])`, []string{task, task2, otherTask})
		testPool.Exec(ctx, `DELETE FROM agent WHERE id=$1`, other)
	})
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings=jsonb_set(coalesce(settings,'{}'::jsonb),'{system1_labellers}',jsonb_build_object('test_first_waiver',$2::text)) WHERE id=$1`, testWorkspaceID, labeller); err != nil {
		t.Fatal(err)
	}
	token := mintAgentTaskToken(t, labeller, task, testUserID)
	token2 := mintAgentTaskToken(t, labeller, task2, testUserID)
	otherToken := mintAgentTaskToken(t, other, otherTask, testUserID)

	send := func(method, path, bearer string, payload map[string]any, forgedAgent string) (int, map[string]any) {
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest(method, testServer.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		if forgedAgent != "" {
			req.Header.Set("X-Agent-ID", forgedAgent)
			req.Header.Set("X-Actor-Source", "task_token")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	post := func(bearer, label, forgedAgent string) (int, map[string]any) {
		return send(http.MethodPost, "/api/system1/labels", bearer, map[string]any{"observation_id": observation, "kind": "burn_in", "label": label}, forgedAgent)
	}
	rows := func() (n int) {
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM system1_label WHERE observation_id=$1`, observation).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}

	code, created := post(token, "waive", "")
	if code != http.StatusCreated {
		t.Fatalf("assigned labeller status=%d body=%v, want 201", code, created)
	}
	// A forged agent header loses to the token: same identity, same row.
	code, replay := post(token, "waive", other)
	if code != http.StatusOK || replay["id"] != created["id"] {
		t.Fatalf("replay status=%d id=%v, want 200 and %v", code, replay["id"], created["id"])
	}
	if code, _ := post(token, "refuse", ""); code != http.StatusConflict {
		t.Fatalf("different body, same key: status=%d, want 409", code)
	}
	// A member session never writes a label, whatever headers it forges.
	if code, _ := post(testToken, "waive", other); code != http.StatusForbidden {
		t.Fatalf("member session status=%d, want 403", code)
	}
	// An agent that is not the assigned labeller is refused, forged header or not.
	if code, _ := post(otherToken, "waive", labeller); code != http.StatusForbidden {
		t.Fatalf("unassigned agent status=%d, want 403", code)
	}
	if code, _ := post(token, "bogus", ""); code != http.StatusBadRequest {
		t.Fatalf("label outside the closed set: status=%d, want 400", code)
	}
	// An observation of another workspace reads as missing.
	var foreign string
	if err := testPool.QueryRow(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, inputs) VALUES (gen_random_uuid(),'decision',gen_random_uuid(),'reversible','{"topic":"test_first_waiver"}') RETURNING id::text`).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM system1_observation WHERE id=$1`, foreign) })
	if code, _ := send(http.MethodPost, "/api/system1/labels", token, map[string]any{"observation_id": foreign, "kind": "burn_in", "label": "waive"}, ""); code != http.StatusNotFound {
		t.Fatalf("cross-workspace observation status=%d, want 404", code)
	}
	if n := rows(); n != 1 {
		t.Fatalf("label rows=%d after refusals, want 1", n)
	}

	// Correction: a second task needs supersedes, and only the live row can be superseded.
	if code, _ := post(token2, "refuse", ""); code != http.StatusConflict {
		t.Fatalf("second label without supersedes status=%d, want 409", code)
	}
	correct := func(bearer, label string, supersedes any) (int, map[string]any) {
		return send(http.MethodPost, "/api/system1/labels", bearer, map[string]any{"observation_id": observation, "kind": "burn_in", "label": label, "supersedes": supersedes}, "")
	}
	code, fixed := correct(token2, "refuse", created["id"])
	if code != http.StatusCreated {
		t.Fatalf("correction status=%d body=%v, want 201", code, fixed)
	}
	var liveID string
	var superseded bool
	if err := testPool.QueryRow(ctx, `SELECT (SELECT id::text FROM system1_label WHERE observation_id=$1 AND source='labeller' AND superseded_at IS NULL),
 (SELECT superseded_at IS NOT NULL FROM system1_label WHERE id=$2)`, observation, created["id"]).Scan(&liveID, &superseded); err != nil || liveID != fixed["id"] || !superseded {
		t.Fatalf("live=%s superseded=%v err=%v, want live %v and the first row superseded", liveID, superseded, err, fixed["id"])
	}
	if code, _ := correct(token, "refuse", created["id"]); code != http.StatusConflict {
		t.Fatalf("superseding a superseded row status=%d, want 409", code)
	}
	if n := rows(); n != 2 {
		t.Fatalf("label rows=%d after correction, want 2", n)
	}

	// The labeller is assigned by the owner only; a task token carries the owner's user id.
	set := func(bearer, scope, agent string) int {
		code, _ := send(http.MethodPut, "/api/system1/labellers", bearer, map[string]any{"scope": scope, "agent_id": agent}, "")
		return code
	}
	if code := set(token, "test_first_waiver", other); code != http.StatusForbidden {
		t.Fatalf("task token set labeller status=%d, want 403", code)
	}
	if code := set(testToken, "unknown_scope", other); code != http.StatusBadRequest {
		t.Fatalf("unknown scope status=%d, want 400", code)
	}
	if code := set(testToken, "test_first_waiver", "00000000-0000-0000-0000-000000000001"); code != http.StatusBadRequest {
		t.Fatalf("agent outside the workspace status=%d, want 400", code)
	}
	if code := set(testToken, "test_first_waiver", other); code != http.StatusOK {
		t.Fatalf("owner set labeller status=%d, want 200", code)
	}
	if code, _ := correct(token2, "waive", fixed["id"]); code != http.StatusForbidden {
		t.Fatalf("former labeller status=%d after reassignment, want 403", code)
	}
	if code := set(testToken, "test_first_waiver", ""); code != http.StatusOK {
		t.Fatalf("owner clear labeller status=%d, want 200", code)
	}
	if code, _ := post(otherToken, "waive", ""); code != http.StatusForbidden {
		t.Fatalf("cleared assignment status=%d, want 403", code)
	}
}

// CHE-4472 security finding: the generic workspace settings save must never change
// system1_labellers. Only PUT /api/system1/labellers (owner, human) assigns a labeller.
func TestSystem1LabellersNotWritableThroughWorkspaceSettings(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	var previous []byte
	if err := testPool.QueryRow(ctx, `SELECT coalesce(settings,'{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	var labeller, runtime string
	if err := testPool.QueryRow(ctx, `SELECT id::text, runtime_id::text FROM agent WHERE workspace_id=$1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&labeller, &runtime); err != nil {
		t.Fatal(err)
	}
	var task string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority) VALUES ($1,$2,'running',0) RETURNING id::text`, labeller, runtime).Scan(&task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id=$1`, task)
	})
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings=jsonb_set(coalesce(settings,'{}'::jsonb),'{system1_labellers}','{"test_first_waiver":"keep"}'::jsonb) WHERE id=$1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	taskToken := mintAgentTaskToken(t, labeller, task, testUserID)

	patch := func(bearer string, settings any) int {
		body, _ := json.Marshal(map[string]any{"settings": settings})
		req, err := http.NewRequest(http.MethodPatch, testServer.URL+"/api/workspaces/"+testWorkspaceID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	stored := func() string {
		var v string
		if err := testPool.QueryRow(ctx, `SELECT coalesce(settings->'system1_labellers','null'::jsonb)::text FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	const want = `{"test_first_waiver": "keep"}`
	for name, tc := range map[string]struct {
		bearer   string
		settings any
	}{
		"task token replaces the map":      {taskToken, map[string]any{"system1_labellers": map[string]any{"test_first_waiver": labeller}}},
		"admin session replaces the map":   {testToken, map[string]any{"system1_labellers": map[string]any{"test_first_waiver": labeller}}},
		"admin session omits the key":      {testToken, map[string]any{"other": 1}},
		"admin session sends a non-object": {testToken, "x"},
	} {
		patch(tc.bearer, tc.settings)
		if got := stored(); got != want {
			t.Fatalf("%s: system1_labellers=%s, want %s unchanged", name, got, want)
		}
	}
}
