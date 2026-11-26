package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func readAgentAppSlugs(t *testing.T, agentID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT toolkit_slug FROM agent_app WHERE agent_id = $1 ORDER BY toolkit_slug`, agentID)
	if err != nil {
		t.Fatalf("read agent_app: %v", err)
	}
	defer rows.Close()
	slugs := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatalf("scan agent_app: %v", err)
		}
		slugs = append(slugs, slug)
	}
	return slugs
}

// A failed audit write must roll back the connected-app writes of the same
// UpdateAgent request: the allowlist sync shares the agent update transaction.
func TestUpdateAgent_AuditFailureRollsBackConnectedApps(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withComposioMCPAppsFlag(t, testHandler, true)
	agentID, ownerID := allowlistFixture(t)

	seed := httptest.NewRecorder()
	testHandler.UpdateAgent(seed, withURLParam(newRequestAs(
		ownerID, http.MethodPut, "/api/agents/"+agentID,
		map[string]any{"composio_toolkit_allowlist": []string{"notion"}},
	), "id", agentID))
	if seed.Code != http.StatusOK {
		t.Fatalf("seed UpdateAgent: %d %s", seed.Code, seed.Body.String())
	}
	if got := readAgentAppSlugs(t, agentID); !reflect.DeepEqual(got, []string{"notion"}) {
		t.Fatalf("seeded agent_app = %v; want [notion]", got)
	}

	failing := *testHandler
	failing.TxStarter = failingQueryStarter{inner: testHandler.TxStarter, name: "InsertWorkspaceAuditEvent"}
	w := httptest.NewRecorder()
	failing.UpdateAgent(w, withURLParam(newRequestAs(
		ownerID, http.MethodPut, "/api/agents/"+agentID,
		map[string]any{"composio_toolkit_allowlist": []string{"github"}},
	), "id", agentID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateAgent with failing audit: %d %s; want 500", w.Code, w.Body.String())
	}

	stored, isNull := readAllowlistColumn(t, agentID)
	if isNull || !reflect.DeepEqual(stored, []string{"notion"}) {
		t.Errorf("allowlist after failed update = %v (null=%v); want [notion]", stored, isNull)
	}
	if got := readAgentAppSlugs(t, agentID); !reflect.DeepEqual(got, []string{"notion"}) {
		t.Errorf("agent_app after failed update = %v; want [notion] (app writes must roll back with the agent)", got)
	}
}
