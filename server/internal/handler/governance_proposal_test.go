package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecodeGovernanceProposalResultConfidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "zero", value: `{"label":"no_correction","proposal_confidence":0}`},
		{name: "one", value: `{"label":"no_correction","proposal_confidence":1}`},
		{name: "missing", value: `{"label":"no_correction"}`, wantErr: true},
		{name: "null", value: `{"label":"no_correction","proposal_confidence":null}`, wantErr: true},
		{name: "boolean", value: `{"label":"no_correction","proposal_confidence":true}`, wantErr: true},
		{name: "string", value: `{"label":"no_correction","proposal_confidence":"0.5"}`, wantErr: true},
		{name: "malformed", value: `{"label":"no_correction","proposal_confidence":}`, wantErr: true},
		{name: "negative", value: `{"label":"no_correction","proposal_confidence":-0.01}`, wantErr: true},
		{name: "above one", value: `{"label":"no_correction","proposal_confidence":1.01}`, wantErr: true},
		{name: "overflow", value: `{"label":"no_correction","proposal_confidence":1e999}`, wantErr: true},
		{name: "NaN", value: `{"label":"no_correction","proposal_confidence":NaN}`, wantErr: true},
		{name: "positive infinity", value: `{"label":"no_correction","proposal_confidence":Infinity}`, wantErr: true},
		{name: "negative infinity", value: `{"label":"no_correction","proposal_confidence":-Infinity}`, wantErr: true},
		{name: "duplicate confidence", value: `{"label":"no_correction","proposal_confidence":0,"proposal_confidence":1}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeGovernanceProposalResult(strings.NewReader(test.value), []byte("[]"), []byte("[]"))
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestDecodeGovernanceProposalResultAllowsOnlyOfferedReferences(t *testing.T) {
	candidateMap := []byte(`[{"action_id":"action-a","target_id":"target-a"}]`)
	citationMap := []byte(`[{"citation_id":"citation-a"}]`)
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "offered correction", value: `{"label":"correction","action_id":"action-a","target_id":"target-a","citation_ids":["citation-a"],"proposal_confidence":0.5}`},
		{name: "known no correction", value: `{"label":"no_correction","citation_ids":["citation-a"],"proposal_confidence":0.5}`},
		{name: "known stale", value: `{"label":"stale","proposal_confidence":0.5}`},
		{name: "known insufficient evidence", value: `{"label":"insufficient_evidence","proposal_confidence":0.5}`},
		{name: "unoffered action", value: `{"label":"correction","action_id":"action-b","target_id":"target-a","citation_ids":["citation-a"],"proposal_confidence":0.5}`, wantErr: true},
		{name: "unoffered target", value: `{"label":"correction","action_id":"action-a","target_id":"target-b","citation_ids":["citation-a"],"proposal_confidence":0.5}`, wantErr: true},
		{name: "unoffered citation", value: `{"label":"correction","action_id":"action-a","target_id":"target-a","citation_ids":["citation-b"],"proposal_confidence":0.5}`, wantErr: true},
		{name: "unknown label", value: `{"label":"successor","proposal_confidence":0.5}`, wantErr: true},
		{name: "extra field", value: `{"label":"no_correction","proposal_confidence":0.5,"suggestion_reference":"anything"}`, wantErr: true},
		{name: "correction missing action", value: `{"label":"correction","target_id":"target-a","citation_ids":["citation-a"],"proposal_confidence":0.5}`, wantErr: true},
		{name: "correction missing citation", value: `{"label":"correction","action_id":"action-a","target_id":"target-a","proposal_confidence":0.5}`, wantErr: true},
		{name: "conflicting fields on stale", value: `{"label":"stale","action_id":"action-a","proposal_confidence":0.5}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeGovernanceProposalResult(strings.NewReader(test.value), candidateMap, citationMap)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestGovernanceProposalTokenBindingAndResultReplay(t *testing.T) {
	requireGovernanceProposalSchema(t)
	fixture := newGovernanceProposalFixture(t)

	rawToken := "mat_" + uuid.NewString()
	queries := db.New(testPool)
	createToken := func() (db.TaskToken, error) {
		return queries.CreateGovernanceProposalTaskToken(t.Context(), db.CreateGovernanceProposalTaskTokenParams{
			TokenHash: auth.HashToken(rawToken), UserID: proposalUUID(t, testUserID),
			ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
			WorkspaceID: proposalUUID(t, fixture.workspaceID), CaseID: proposalUUID(t, fixture.caseID),
			AttemptID: proposalUUID(t, fixture.attemptID), TaskID: proposalUUID(t, fixture.taskID),
			AgentID: proposalUUID(t, fixture.agentID),
		})
	}
	if _, err := createToken(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("default-off proposal token creation error = %v, want no rows", err)
	}
	fixture.fx.Exec(t, `UPDATE governance_workspace_config SET settings = '{"jev_governance_enabled":true}'::jsonb WHERE workspace_id = $1`, fixture.workspaceID)
	fixture.fx.Exec(t, `UPDATE governance_case SET state = 'parked' WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, fixture.caseID)
	if _, err := createToken(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("parked-case proposal token creation error = %v, want no rows", err)
	}
	fixture.fx.Exec(t, `UPDATE governance_case SET state = 'agent_attempt' WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, fixture.caseID)
	token, err := createToken()
	if err != nil {
		t.Fatalf("create enabled proposal token: %v", err)
	}
	if token.Purpose != middleware.GovernanceProposalTaskPurpose ||
		!proposalUUIDEquals(token.GovernanceCaseID, fixture.caseID) ||
		!proposalUUIDEquals(token.GovernanceAttemptID, fixture.attemptID) ||
		!proposalUUIDEquals(token.GovernanceAttemptFence, fixture.attemptFence) ||
		!token.GovernanceEvidenceEpoch.Valid || token.GovernanceEvidenceEpoch.Int32 != 0 {
		t.Fatalf("proposal token binding = purpose %q, case %s, attempt %s, fence %s, epoch %+v", token.Purpose,
			token.GovernanceCaseID, token.GovernanceAttemptID, token.GovernanceAttemptFence, token.GovernanceEvidenceEpoch)
	}

	router := chi.NewRouter()
	router.Use(middleware.Auth(queries, nil, nil, nil))
	router.Route("/api/governance/proposals/{caseId}/attempts/{attemptId}", func(r chi.Router) {
		r.Get("/evidence", testHandler.GetGovernanceProposalEvidence)
		r.Post("/heartbeat", testHandler.HeartbeatGovernanceProposalAttempt)
		r.Post("/result", testHandler.SubmitGovernanceProposalResult)
	})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawToken)
		req.Header.Set("X-Actor-Source", "member")
		req.Header.Set("X-Task-Token-Purpose", "agent_task")
		req.Header.Set("X-Workspace-ID", uuid.NewString())
		req.Header.Set("X-Agent-ID", uuid.NewString())
		req.Header.Set("X-Task-ID", uuid.NewString())
		req.Header.Set("X-Governance-Case-ID", uuid.NewString())
		req.Header.Set("X-Governance-Attempt-ID", uuid.NewString())
		req.Header.Set("X-Governance-Attempt-Fence", uuid.NewString())
		req.Header.Set("X-Governance-Evidence-Epoch", "99")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}
	basePath := "/api/governance/proposals/" + fixture.caseID + "/attempts/" + fixture.attemptID
	fixture.fx.Exec(t, `UPDATE governance_case SET state = 'parked' WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, fixture.caseID)
	for _, requestCase := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/evidence"},
		{method: http.MethodPost, path: "/heartbeat"},
		{method: http.MethodPost, path: "/result", body: `{"label":"no_correction","proposal_confidence":0}`},
	} {
		if response := request(requestCase.method, basePath+requestCase.path, requestCase.body); response.Code != http.StatusConflict {
			t.Fatalf("parked-case %s = %d: %s", requestCase.path, response.Code, response.Body.String())
		}
	}
	fixture.fx.Exec(t, `UPDATE governance_case SET state = 'agent_attempt' WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, fixture.caseID)
	if response := request(http.MethodGet, basePath+"/evidence", ""); response.Code != http.StatusOK {
		t.Fatalf("proposal evidence = %d: %s", response.Code, response.Body.String())
	}
	wrongAttemptPath := "/api/governance/proposals/" + fixture.caseID + "/attempts/" + uuid.NewString() + "/evidence"
	if response := request(http.MethodGet, wrongAttemptPath, ""); response.Code != http.StatusForbidden {
		t.Fatalf("wrong-attempt evidence = %d: %s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPost, basePath+"/heartbeat", ""); response.Code != http.StatusNoContent {
		t.Fatalf("proposal heartbeat = %d: %s", response.Code, response.Body.String())
	}
	result := `{"label":"correction","action_id":"action-a","target_id":"target-a","citation_ids":["citation-a"],"proposal_confidence":0}`
	if response := request(http.MethodPost, basePath+"/result", result); response.Code != http.StatusAccepted {
		t.Fatalf("proposal result = %d: %s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPost, basePath+"/heartbeat", ""); response.Code != http.StatusConflict {
		t.Fatalf("terminal proposal heartbeat = %d: %s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPost, basePath+"/result", result); response.Code != http.StatusConflict {
		t.Fatalf("proposal result replay = %d: %s", response.Code, response.Body.String())
	}

	fixture.fx.Exec(t, `UPDATE task_token SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, auth.HashToken(rawToken))
	if response := request(http.MethodGet, basePath+"/evidence", ""); response.Code != http.StatusUnauthorized ||
		response.Header().Get("Content-Type") != "application/problem+json" ||
		!strings.Contains(response.Body.String(), `"problem":"proposal_unavailable"`) {
		t.Fatalf("expired proposal token = %d (%s): %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
}

func TestGovernanceProposalTokenRequiresEvidenceEpoch(t *testing.T) {
	requireGovernanceProposalSchema(t)
	agentID := dbfx.Agent(t, "proposal-token-epoch-"+uuid.NewString(), testRuntimeID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": testRuntimeID})
	tokenHash := auth.HashToken("mat_" + uuid.NewString())
	dbfx.Cleanup(t, `DELETE FROM task_token WHERE token_hash = $1`, tokenHash)
	_, err := testPool.Exec(t.Context(), `
INSERT INTO task_token (
    token_hash, task_id, agent_id, workspace_id, user_id, expires_at,
    purpose, governance_case_id, governance_attempt_id, governance_attempt_fence,
    governance_evidence_epoch
) VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour', 'governance_proposal', $6, $7, $8, NULL)
`, tokenHash, taskID, agentID, testWorkspaceID, testUserID, uuid.NewString(), uuid.NewString(), uuid.NewString())
	if err == nil {
		t.Fatal("proposal token with a null evidence epoch was inserted")
	}
}

type governanceProposalFixture struct {
	fx           *testutil.Fixture
	workspaceID  string
	agentID      string
	taskID       string
	caseID       string
	attemptID    string
	attemptFence string
}

func newGovernanceProposalFixture(t *testing.T) governanceProposalFixture {
	t.Helper()
	workspaceID := dbfx.Workspace(t, "Governance proposal tests", "proposal-"+uuid.NewString())
	fx := testutil.New(testPool, workspaceID, testUserID)
	fx.Member(t, workspaceID, testUserID, "owner")
	fx.InsertNoID(t, "governance_workspace_config", testutil.Cols{"workspace_id": workspaceID}, "workspace_id = $1", workspaceID)
	runtimeID := fx.Runtime(t, "proposal-runtime-"+uuid.NewString())
	agentID := fx.Agent(t, "proposal-agent-"+uuid.NewString(), runtimeID)
	taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID})
	caseID := fx.Insert(t, "governance_case", testutil.Cols{
		"workspace_id": workspaceID, "subject_type": "issue", "subject_id": uuid.NewString(),
		"subject_revision": 1, "rule_id": uuid.NewString(), "generation": 0,
		"material_fingerprint": "proposal-" + uuid.NewString(), "state": "agent_attempt",
		"budget_root_id": uuid.NewString(), "control_epoch": 0,
	})
	attemptFence := uuid.NewString()
	attemptID := fx.Insert(t, "governance_attempt", testutil.Cols{
		"workspace_id": workspaceID, "case_id": caseID, "ordinal": 0, "kind": "agent",
		"candidate_id": agentID, "task_id": taskID, "input_digest": "proposal-input",
		"attempt_fence": attemptFence, "deadline_at": testutil.Raw("now() + interval '1 hour'"),
	})
	fx.Exec(t, `UPDATE governance_case SET current_attempt_id = $1 WHERE workspace_id = $2 AND id = $3`, attemptID, workspaceID, caseID)
	fx.Insert(t, "governance_evaluation", testutil.Cols{
		"workspace_id": workspaceID, "case_id": caseID, "attempt_id": attemptID,
		"trigger_identity": "proposal-test", "subject_revision_vector": testutil.Raw("'{}'::jsonb"),
		"snapshot": testutil.Raw("'{}'::jsonb"), "snapshot_digest": "proposal-snapshot",
		"snapshot_schema_version": 1, "required_complete": true,
		"candidate_map":    testutil.Raw("'[{\"action_id\":\"action-a\",\"target_id\":\"target-a\"}]'::jsonb"),
		"citation_map":     testutil.Raw("'[{\"citation_id\":\"citation-a\"}]'::jsonb"),
		"estimated_tokens": 0, "question_criteria_hash": "proposal-criteria",
	})
	return governanceProposalFixture{
		fx: fx, workspaceID: workspaceID, agentID: agentID, taskID: taskID,
		caseID: caseID, attemptID: attemptID, attemptFence: attemptFence,
	}
}

func requireGovernanceProposalSchema(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	var ready bool
	if err := testPool.QueryRow(t.Context(), `
SELECT to_regclass('governance_workspace_config') IS NOT NULL
   AND EXISTS (
       SELECT 1 FROM information_schema.columns
       WHERE table_schema = current_schema() AND table_name = 'task_token' AND column_name = 'purpose'
   )
   AND EXISTS (
       SELECT 1 FROM information_schema.columns
       WHERE table_schema = current_schema() AND table_name = 'governance_attempt' AND column_name = 'last_heartbeat_at'
   )
`).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Skip("governance proposal task-token migrations are not applied to the test database")
	}
}

func proposalUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("parse fixture UUID %q: %v", value, err)
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}
}

func proposalUUIDEquals(value pgtype.UUID, want string) bool {
	parsed, err := uuid.Parse(want)
	return err == nil && value.Valid && value.Bytes == parsed
}
