package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// hasPredictionKey walks decoded JSON and reports any key that names a prediction.
func hasPredictionKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if strings.Contains(strings.ToLower(k), "predict") || hasPredictionKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if hasPredictionKey(child) {
				return true
			}
		}
	}
	return false
}

// CHE-4383 B4b over the real router: the assigned labeller's task token reads a
// blind queue (no prediction field), and the sample (ids and seed) is stored when served.
func TestSystem1LabelQueueRoute(t *testing.T) {
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
	if err := testPool.QueryRow(ctx, `INSERT INTO agent (workspace_id, name, runtime_id, runtime_mode, owner_id) SELECT workspace_id, 'queue-route-other', runtime_id, runtime_mode, owner_id FROM agent WHERE id=$1 RETURNING id::text`, labeller).Scan(&other); err != nil {
		t.Fatal(err)
	}
	newTask := func(agent string) string {
		var id string
		if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority) VALUES ($1,$2,'running',0) RETURNING id::text`, agent, runtime).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	task, task2, otherTask := newTask(labeller), newTask(labeller), newTask(other)

	var observations []string
	newObservation := func() string {
		var id string
		if err := testPool.QueryRow(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, inputs, predicted_key, predicted_confidence, predicted_at)
 VALUES ($1,'decision',gen_random_uuid(),'reversible',
 '{"topic":"test_first_waiver","option_keys":["refuse","waive"],"waiver":{"repo":"cheese-work/multica","head_sha":"0123456789012345678901234567890123456789","change_kind":"docs"}}',
 'PREDICTED_SENTINEL', 0.9, now()) RETURNING id::text`, testWorkspaceID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO system1_prediction (workspace_id, observation_id, scope, predictor_id, predictor_version, predicted_key) VALUES ($1,$2,'test_first_waiver','PREDICTOR_SENTINEL','1','refuse')`, testWorkspaceID, id); err != nil {
			t.Fatal(err)
		}
		observations = append(observations, id)
		return id
	}
	first := newObservation()
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
		testPool.Exec(ctx, `DELETE FROM system1_prediction WHERE observation_id = ANY($1::uuid[])`, observations)
		testPool.Exec(ctx, `DELETE FROM system1_label WHERE observation_id = ANY($1::uuid[])`, observations)
		testPool.Exec(ctx, `DELETE FROM system1_label_sample WHERE task_id = ANY($1::uuid[])`, []string{task, task2, otherTask})
		testPool.Exec(ctx, `DELETE FROM system1_observation WHERE id = ANY($1::uuid[])`, observations)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = ANY($1::uuid[])`, []string{task, task2, otherTask})
		testPool.Exec(ctx, `DELETE FROM agent WHERE id=$1`, other)
	})
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings=jsonb_set(coalesce(settings,'{}'::jsonb),'{system1_labellers}',jsonb_build_object('test_first_waiver',$2::text)) WHERE id=$1`, testWorkspaceID, labeller); err != nil {
		t.Fatal(err)
	}
	token := mintAgentTaskToken(t, labeller, task, testUserID)
	token2 := mintAgentTaskToken(t, labeller, task2, testUserID)
	otherToken := mintAgentTaskToken(t, other, otherTask, testUserID)

	get := func(bearer, query string) (int, []byte) {
		req, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/system1/label-queue?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf strings.Builder
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		buf.Write(raw)
		return resp.StatusCode, []byte(buf.String())
	}
	type queue struct {
		SampleID string `json:"sample_id"`
		Seed     int64  `json:"seed"`
		Items    []struct {
			ObservationID string         `json:"observation_id"`
			Features      map[string]any `json:"features"`
		} `json:"items"`
	}
	decode := func(body []byte) (q queue) {
		if err := json.Unmarshal(body, &q); err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		return
	}
	const q = "scope=test_first_waiver&kind=burn_in"

	code, body := get(token, q)
	if code != http.StatusOK {
		t.Fatalf("assigned labeller status=%d body=%s, want 200", code, body)
	}
	served := decode(body)
	if len(served.Items) != 1 || served.Items[0].ObservationID != first {
		t.Fatalf("items=%+v, want only %s", served.Items, first)
	}
	// Blind: no prediction field and no prediction value anywhere in the payload.
	var generic any
	_ = json.Unmarshal(body, &generic)
	if hasPredictionKey(generic) || strings.Contains(string(body), "SENTINEL") {
		t.Fatalf("queue payload leaks a prediction: %s", body)
	}
	// The asker's recommended-first option order would leak the asker's lean; it is not served.
	if strings.Contains(string(body), "option_keys") {
		t.Fatalf("queue payload serves option_keys: %s", body)
	}
	if served.Items[0].Features["waiver"] == nil {
		t.Fatalf("typed waiver features missing: %s", body)
	}

	// The stored sample matches the served ids and seed.
	var storedSeed int64
	var storedIDs []string
	if err := testPool.QueryRow(ctx, `SELECT seed, observation_ids::text[] FROM system1_label_sample WHERE task_id=$1 AND scope='test_first_waiver' AND kind='burn_in'`, task).Scan(&storedSeed, &storedIDs); err != nil {
		t.Fatalf("stored sample: %v", err)
	}
	if storedSeed != served.Seed || len(storedIDs) != 1 || storedIDs[0] != first {
		t.Fatalf("stored seed=%d ids=%v, served seed=%d ids=[%s]", storedSeed, storedIDs, served.Seed, first)
	}

	// The same task asking again gets the stored sample, even after new observations arrive.
	newObservation()
	code, body = get(token, q)
	if again := decode(body); code != http.StatusOK || again.SampleID != served.SampleID || len(again.Items) != 1 {
		t.Fatalf("repeat status=%d body=%s, want the stored sample %s", code, body, served.SampleID)
	}
	var n int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM system1_label_sample WHERE task_id=$1`, task).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sample rows=%d err=%v, want 1", n, err)
	}

	// Refusals: a member session, an unassigned agent, an unknown scope and a bad kind.
	if code, _ := get(testToken, q); code != http.StatusForbidden {
		t.Fatalf("member session status=%d, want 403", code)
	}
	if code, _ := get(otherToken, q); code != http.StatusForbidden {
		t.Fatalf("unassigned agent status=%d, want 403", code)
	}
	if code, _ := get(token, "scope=nope&kind=burn_in"); code != http.StatusBadRequest {
		t.Fatalf("unknown scope status=%d, want 400", code)
	}
	if code, _ := get(token, "scope=test_first_waiver&kind=nope"); code != http.StatusBadRequest {
		t.Fatalf("bad kind status=%d, want 400", code)
	}

	// A labelled observation leaves the queue; above 20 eligible, a sample of 20 is served.
	if _, err := testPool.Exec(ctx, `INSERT INTO system1_label (workspace_id, observation_id, scope, kind, source, label, task_id, agent_id) VALUES ($1,$2,'test_first_waiver','burn_in','labeller','waive',$3,$4)`, testWorkspaceID, first, task, labeller); err != nil {
		t.Fatal(err)
	}
	for range 24 {
		newObservation()
	}
	code, body = get(token2, q)
	if code != http.StatusOK {
		t.Fatalf("second task status=%d body=%s", code, body)
	}
	capped := decode(body)
	if len(capped.Items) != 20 {
		t.Fatalf("served %d items, want the cap of 20", len(capped.Items))
	}
	for _, item := range capped.Items {
		if item.ObservationID == first {
			t.Fatalf("labelled observation %s is still in the queue", first)
		}
	}
	if err := testPool.QueryRow(ctx, `SELECT cardinality(observation_ids) FROM system1_label_sample WHERE task_id=$1`, task2).Scan(&n); err != nil || n != 20 {
		t.Fatalf("stored sample size=%d err=%v, want 20", n, err)
	}
}
