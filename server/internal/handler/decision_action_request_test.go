package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type actionView struct {
	ID            string `json:"id"`
	ActionType    string `json:"action_type"`
	ActionVersion int    `json:"action_version"`
	PayloadSHA256 string `json:"payload_sha256"`
	State         string `json:"state"`
}

type actionResponse struct {
	ID     string      `json:"id"`
	Action *actionView `json:"action"`
}

func (fixture singleCallFixture) withAction(action map[string]any) map[string]any {
	body := fixture.body()
	body["action"] = action
	return body
}

func (fixture singleCallFixture) envPatch(params map[string]any) map[string]any {
	return map[string]any{"type": "agent.env.patch", "version": 1, "params": params}
}

func (fixture singleCallFixture) respond(test *testing.T, body map[string]any, status int, into any) string {
	test.Helper()
	response := testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(status)
	if into != nil {
		response.JSON(into)
	}
	return response.Text()
}

func (fixture singleCallFixture) requestCounts(test *testing.T) (requests, actions int) {
	test.Helper()
	requests = dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID)
	actions = dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE requested_by_task_id = $1`, fixture.taskID)
	return requests, actions
}

func newActionFixture(test *testing.T) singleCallFixture {
	test.Helper()
	fixture := newSingleCallFixture(test)
	dbfx.Cleanup(test, `DELETE FROM decision_action WHERE requested_by_task_id = $1`, fixture.taskID)
	return fixture
}

func TestDecisionActionRequestEnvPatchStoresHashedPayload(test *testing.T) {
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{
		"type": "agent.env.patch", "version": 1,
		"params":       map[string]any{"agent_id": fixture.agentID, "set": map[string]string{"B": "2", "A": "1"}},
		"secret_slots": []map[string]any{{"slot": "KEY", "target": map[string]string{"agent_id": fixture.agentID, "env_key": "KEY"}}},
	})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	if created.Action == nil || created.Action.State != "pending" || created.Action.ActionType != "agent.env.patch" || created.Action.ActionVersion != 1 {
		test.Fatalf("action = %+v", created.Action)
	}
	var (
		requestID, params, preconditions, slots, hash string
		expiresAt, createdAt                          time.Time
		agentID, taskID                               string
	)
	dbfx.QueryRow(test, `SELECT decision_request_id::text, params::text, preconditions::text, secret_slots::text, payload_sha256, expires_at, created_at, requested_by_agent_id::text, requested_by_task_id::text FROM decision_action WHERE id = $1`, created.Action.ID).
		Scan(&requestID, &params, &preconditions, &slots, &hash, &expiresAt, &createdAt, &agentID, &taskID)
	if requestID != created.ID || agentID != fixture.agentID || taskID != fixture.taskID {
		test.Fatalf("provenance request=%s agent=%s task=%s", requestID, agentID, taskID)
	}
	if hash != created.Action.PayloadSHA256 {
		test.Fatalf("response hash %s, stored %s", created.Action.PayloadSHA256, hash)
	}
	if got := expiresAt.Sub(createdAt); got < 23*time.Hour+59*time.Minute || got > 24*time.Hour+time.Minute {
		test.Fatalf("expiry %s after creation, want the 24h default", got)
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(preconditions), &snapshot); err != nil || len(snapshot) != 2 || snapshot["agent_id"] != fixture.agentID || snapshot["custom_env_version"] != float64(0) {
		test.Fatalf("preconditions = %s, %v", preconditions, err)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'decision_action_requested' AND details->>'payload_sha256' = $2`, fixture.issueID, hash); n != 1 {
		test.Fatalf("decision_action_requested activity rows = %d, want 1", n)
	}
	// The stored rows re-hash to the digest the card will show.
	rehash, err := decisionaction.Payload{
		ActionType: "agent.env.patch", ActionVersion: 1, WorkspaceID: testWorkspaceID, DecisionRequestID: requestID,
		Params: json.RawMessage(params), Preconditions: json.RawMessage(preconditions),
		SecretSlots: []decisionaction.SecretSlot{{Slot: "KEY", Target: decisionaction.SlotTarget{AgentID: fixture.agentID, EnvKey: "KEY"}}},
		ExpiresAt:   expiresAt,
	}.Hash()
	if err != nil || rehash != hash {
		test.Fatalf("rehash = %s, %v; stored %s", rehash, err, hash)
	}
}

func TestDecisionActionRequestAgentCopySnapshotsSource(test *testing.T) {
	fixture := newActionFixture(test)
	var runtimeID string
	dbfx.QueryRow(test, `SELECT runtime_id::text FROM agent WHERE id = $1`, fixture.agentID).Scan(&runtimeID)
	body := fixture.withAction(map[string]any{
		"type": "agent.copy", "version": 1,
		"params": map[string]any{"source_agent_id": fixture.agentID, "name": "copy of source", "runtime_id": runtimeID},
	})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	if created.Action == nil {
		test.Fatal("response carries no action")
	}
	var preconditions map[string]string
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, created.Action.ID).Scan(&raw)
	if err := json.Unmarshal([]byte(raw), &preconditions); err != nil {
		test.Fatalf("preconditions %s: %v", raw, err)
	}
	// sha256 of the empty instructions the fixture agent carries.
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if preconditions["source_agent_id"] != fixture.agentID || preconditions["instructions_sha256"] != emptySHA || preconditions["settings_sha256"] == "" {
		test.Fatalf("preconditions = %v", preconditions)
	}
}

func TestDecisionActionRequestRefusalsWriteNothing(test *testing.T) {
	fixture := newActionFixture(test)
	good := func() map[string]any {
		return map[string]any{"agent_id": fixture.agentID, "set": map[string]string{"A": "1"}}
	}
	withField := func(params map[string]any, key string, value any) map[string]any { params[key] = value; return params }
	slot := []map[string]any{{"slot": "A", "target": map[string]string{"agent_id": fixture.agentID, "env_key": "A"}}}
	foreign := "00000000-0000-4000-8000-000000000001"
	cases := []struct {
		name   string
		action map[string]any
		status int
		code   string
	}{
		{"unknown type", map[string]any{"type": "agent.delete", "version": 1, "params": good()}, http.StatusBadRequest, "unknown_action"},
		{"unknown version", map[string]any{"type": "agent.env.patch", "version": 2, "params": good()}, http.StatusBadRequest, "unknown_action"},
		{"unknown param", fixture.envPatch(withField(good(), "owner", "x")), http.StatusBadRequest, "invalid_params"},
		{"secret in params", map[string]any{"type": "agent.env.patch", "version": 1, "params": good(), "secret_slots": slot}, http.StatusBadRequest, "secret_in_params"},
		{"expiry beyond maximum", withField2(fixture.envPatch(good()), "expires_in_seconds", 73*3600), http.StatusBadRequest, "invalid_params"},
		{"target outside workspace", fixture.envPatch(map[string]any{"agent_id": foreign, "set": map[string]string{"A": "1"}}), http.StatusForbidden, "target_workspace"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			response := fixture.respond(test, fixture.withAction(tc.action), tc.status, nil)
			if !strings.Contains(response, tc.code) {
				test.Fatalf("body %q lacks code %q", response, tc.code)
			}
			if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
				test.Fatalf("requests=%d actions=%d, want nothing written", requests, actions)
			}
		})
	}
}

func withField2(action map[string]any, key string, value any) map[string]any {
	action[key] = value
	return action
}

func TestDecisionActionRequestReplayReturnsTheSameAction(test *testing.T) {
	fixture := newActionFixture(test)
	body := fixture.withAction(fixture.envPatch(map[string]any{"agent_id": fixture.agentID, "set": map[string]string{"A": "1"}}))
	var first, second actionResponse
	fixture.respond(test, body, http.StatusCreated, &first)
	fixture.respond(test, body, http.StatusOK, &second)
	if first.Action == nil || second.Action == nil || first.Action.ID != second.Action.ID || first.Action.PayloadSHA256 != second.Action.PayloadSHA256 {
		test.Fatalf("first=%+v second=%+v", first.Action, second.Action)
	}
	if requests, actions := fixture.requestCounts(test); requests != 1 || actions != 1 {
		test.Fatalf("requests=%d actions=%d, want one each", requests, actions)
	}
}

func TestDecisionRequestWithoutActionIsUnchanged(test *testing.T) {
	fixture := newActionFixture(test)
	var created actionResponse
	fixture.respond(test, fixture.body(), http.StatusCreated, &created)
	if created.Action != nil {
		test.Fatalf("action = %+v, want none", created.Action)
	}
	if requests, actions := fixture.requestCounts(test); requests != 1 || actions != 0 {
		test.Fatalf("requests=%d actions=%d", requests, actions)
	}
}

func TestActionRequestClassNeverLowersTheAgentsClass(test *testing.T) {
	for _, tc := range []struct{ agent, min, want string }{
		{"reversible", "reversible", "reversible"},
		{"reversible", "deploy", "deploy"},
		{"spend", "reversible", "spend"},
		{"spend", "deploy", "spend"},
	} {
		if got := actionRequestClass(tc.agent, tc.min); got != tc.want {
			test.Errorf("actionRequestClass(%s, %s) = %s, want %s", tc.agent, tc.min, got, tc.want)
		}
	}
}

// The copy snapshot must change whenever a setting the copy would carry over
// changes, so a signed hash never covers less than the copy executes.
func TestAgentCopySettingsDigestCoversEveryCopiedSetting(test *testing.T) {
	fixture := newActionFixture(test)
	agentID := parseUUID(fixture.agentID)
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, fixture.agentID)
	dbfx.Exec(test, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, fixture.agentID)
	digest := func() string {
		source, err := testHandler.Queries.GetAgent(test.Context(), agentID)
		if err != nil {
			test.Fatal(err)
		}
		value, err := agentCopySettingsDigest(test.Context(), testHandler.Queries, source)
		if err != nil {
			test.Fatal(err)
		}
		return value
	}
	seen := map[string]string{"start": digest()}
	if again := digest(); again != seen["start"] {
		test.Fatalf("digest is not stable: %s then %s", seen["start"], again)
	}
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'public_to' WHERE id = $1`, fixture.agentID)
	seen["permission mode"] = digest()
	dbfx.Exec(test, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id, created_by) VALUES ($1, 'workspace', $2, $3)`, fixture.agentID, testWorkspaceID, testUserID)
	seen["invocation target"] = digest()
	var skillID string
	dbfx.QueryRow(test, `INSERT INTO skill (workspace_id, name, description, content, config, created_by) VALUES ($1, 'digest skill', '', '', '{}'::jsonb, $2) RETURNING id::text`, testWorkspaceID, testUserID).Scan(&skillID)
	test.Cleanup(func() {
		testPool.Exec(test.Context(), `DELETE FROM agent_skill WHERE skill_id = $1`, skillID)
		testPool.Exec(test.Context(), `DELETE FROM skill WHERE id = $1`, skillID)
	})
	dbfx.Exec(test, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, fixture.agentID, skillID)
	seen["skill assignment"] = digest()
	distinct := map[string]string{}
	for step, value := range seen {
		if earlier, ok := distinct[value]; ok {
			test.Errorf("%q left the digest equal to %q", step, earlier)
		}
		distinct[value] = step
	}
}
