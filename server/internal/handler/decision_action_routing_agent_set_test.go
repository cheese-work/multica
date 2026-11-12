package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// routingRoster is the agents the routing-agent action tests name: two live
// agents of the workspace, an archived one and one of a foreign workspace.
type routingRoster struct{ a, b, archived, foreign string }

func newRoutingRoster(test *testing.T) routingRoster {
	test.Helper()
	runtime := dbfx.Runtime(test, "routing-set-"+uuid.NewString()[:8], testutil.Cols{"workspace_id": testWorkspaceID})
	agent := func(workspaceID string, extra testutil.Cols) string {
		cols := testutil.Cols{"workspace_id": workspaceID}
		for key, value := range extra {
			cols[key] = value
		}
		runtimeID := runtime
		if workspaceID != testWorkspaceID {
			runtimeID = ""
		}
		return dbfx.Agent(test, "routing-set-"+uuid.NewString()[:8], runtimeID, cols)
	}
	return routingRoster{
		a: agent(testWorkspaceID, nil), b: agent(testWorkspaceID, nil),
		archived: agent(testWorkspaceID, testutil.Cols{"archived_at": testutil.Raw("now()")}),
		foreign:  agent(foreignWorkspaceID(test), nil),
	}
}

// routingAfter is the canonical value the server stores for cfg: all four
// fields, the default statuses when none are named, an object for overrides.
func routingAfter(cfg RoutingAgentConfig) RoutingAgentConfig {
	if len(cfg.Statuses) == 0 {
		cfg.Statuses = defaultRoutingStatuses
	}
	if cfg.Overrides == nil {
		cfg.Overrides = map[string]string{}
	}
	return cfg
}

func routingDigest(test *testing.T, raw json.RawMessage) string {
	test.Helper()
	digest, err := decisionaction.SettingsDigest(testWorkspaceID, map[string]json.RawMessage{"routing_agent": raw})
	if err != nil {
		test.Fatal(err)
	}
	return digest
}

func liveRoutingDigest(test *testing.T) string {
	test.Helper()
	return routingDigest(test, storedSettings(test)["routing_agent"])
}

func afterRoutingDigest(test *testing.T, cfg RoutingAgentConfig) string {
	test.Helper()
	raw, err := json.Marshal(routingAfter(cfg))
	if err != nil {
		test.Fatal(err)
	}
	return routingDigest(test, raw)
}

func storedRouting(test *testing.T) RoutingAgentConfig {
	test.Helper()
	var settings map[string]any
	for key, raw := range storedSettings(test) {
		if key == "routing_agent" {
			settings = map[string]any{key: json.RawMessage(raw)}
		}
	}
	return routingAgentFromSettings(settings)
}

func routingParams(test *testing.T, cfg RoutingAgentConfig) map[string]any {
	test.Helper()
	return map[string]any{
		"workspace_id": testWorkspaceID, "routing_agent": cfg,
		"base_sha256": liveRoutingDigest(test), "after_sha256": afterRoutingDigest(test, cfg),
	}
}

// fileRouting files the action as the fixture's agent and expects status. A
// refused request stores nothing.
func fileRouting(test *testing.T, fixture singleCallFixture, status int, params map[string]any) (settingsRequest, string) {
	test.Helper()
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": routingAgentSetAction, "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return settingsRequest{}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return settingsRequest{pendingAction{fixture, created.ID, created.Action.ID}, params["base_sha256"].(string)}, body
}

func routingChangedCount(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE workspace_id = $1 AND action = 'routing_agent_changed'`, testWorkspaceID)
}

func TestRoutingAgentSetApplies(test *testing.T) {
	roster := newRoutingRoster(test)
	setWorkspaceSettings(test, `{"unlisted":"keep"}`)
	fixture := newActionFixture(test)
	cfg := RoutingAgentConfig{AgentID: roster.a, FallbackAgentID: roster.b, Overrides: map[string]string{"merged": roster.b}}
	request, _ := fileRouting(test, fixture, http.StatusCreated, routingParams(test, cfg))
	before := routingChangedCount(test)

	_, keyID := request.action.enrollPasskey(test)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	request.approveOwner(test, withVerifier(verify), http.StatusOK)

	if got := storedRouting(test); got.AgentID != roster.a || got.FallbackAgentID != roster.b || got.Overrides["merged"] != roster.b {
		test.Fatalf("stored routing agent = %+v", got)
	}
	if got := liveRoutingDigest(test); got != afterRoutingDigest(test, cfg) {
		test.Fatalf("stored digest %s, want the approved after digest", got)
	}
	if got := string(storedSettings(test)["unlisted"]); got != `"keep"` {
		test.Fatalf("unrelated key changed: %s", got)
	}
	if got := routingChangedCount(test); got != before+1 {
		test.Fatalf("routing_agent_changed rows = %d, want %d", got, before+1)
	}
	var raw string
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE workspace_id = $1 AND action = 'routing_agent_changed' ORDER BY created_at DESC LIMIT 1`, testWorkspaceID).Scan(&raw)
	var details struct{ From, To RoutingAgentConfig }
	if err := json.Unmarshal([]byte(raw), &details); err != nil || details.From.AgentID != "" || details.To.AgentID != roster.a {
		test.Fatalf("routing_agent_changed details %s (%v)", raw, err)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d, want 1", n)
	}
	var result string
	dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&result)
	mustContain(test, result, `"field": "workspace.routing_agent"`)
}

func TestRoutingAgentSetClearsTheSetting(test *testing.T) {
	roster := newRoutingRoster(test)
	setWorkspaceSettings(test, `{"routing_agent":{"agent_id":"`+roster.a+`","fallback_agent_id":"","statuses":["todo"],"overrides":{}}}`)
	request, _ := fileRouting(test, newActionFixture(test), http.StatusCreated, routingParams(test, RoutingAgentConfig{}))
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if got := storedRouting(test); got.AgentID != "" || len(got.Statuses) != len(defaultRoutingStatuses) {
		test.Fatalf("stored routing agent after a clear = %+v", got)
	}
}

func TestRoutingAgentSetRequestRefusals(test *testing.T) {
	roster := newRoutingRoster(test)
	for _, tc := range []struct {
		name   string
		status int
		want   string
		cfg    func(proposer string) RoutingAgentConfig
		edit   func(params map[string]any)
	}{
		{"agent of another workspace", http.StatusBadRequest, "not in this workspace", func(string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: roster.foreign} }, nil},
		{"fallback of another workspace", http.StatusBadRequest, "not in this workspace", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, FallbackAgentID: roster.foreign}
		}, nil},
		{"override target of another workspace", http.StatusBadRequest, "not in this workspace", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, Overrides: map[string]string{"merged": roster.foreign}}
		}, nil},
		{"archived agent", http.StatusBadRequest, "archived", func(string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: roster.archived} }, nil},
		{"archived fallback", http.StatusBadRequest, "archived", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, FallbackAgentID: roster.archived}
		}, nil},
		{"archived override target", http.StatusBadRequest, "archived", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, Overrides: map[string]string{"merged": roster.archived}}
		}, nil},
		{"agent is not an id", http.StatusBadRequest, "agent_id must be an agent id", func(string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: "nobody"} }, nil},
		{"invalid status override key", http.StatusBadRequest, "overrides must be keyed by", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, Overrides: map[string]string{" merged": roster.b}}
		}, nil},
		{"override of an unknown workflow", http.StatusBadRequest, "workflow not in this workspace", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, Overrides: map[string]string{uuid.NewString() + ":merged": roster.b}}
		}, nil},
		{"blank status", http.StatusBadRequest, "statuses must be status keys", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, Statuses: []string{" "}}
		}, nil},
		{"fallback without an agent", http.StatusBadRequest, "fallback_agent_id needs agent_id", func(string) RoutingAgentConfig { return RoutingAgentConfig{FallbackAgentID: roster.b} }, nil},
		{"fallback equals the agent", http.StatusBadRequest, "must differ", func(string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, FallbackAgentID: roster.a}
		}, nil},
		{"proposer names itself as the agent", http.StatusForbidden, "self_route_refused", func(proposer string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: proposer} }, nil},
		{"proposer names itself as the fallback", http.StatusForbidden, "self_route_refused", func(proposer string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, FallbackAgentID: proposer}
		}, nil},
		{"proposer names itself as an override target", http.StatusForbidden, "self_route_refused", func(proposer string) RoutingAgentConfig {
			return RoutingAgentConfig{AgentID: roster.a, Overrides: map[string]string{"merged": proposer}}
		}, nil},
		{"after digest is wrong", http.StatusBadRequest, "after_digest_mismatch", func(string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: roster.a} },
			func(params map[string]any) { params["after_sha256"] = strings.Repeat("0", 64) }},
		{"base digest is stale", http.StatusConflict, "precondition_drift", func(string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: roster.a} },
			func(params map[string]any) { params["base_sha256"] = strings.Repeat("0", 64) }},
		{"workspace of another workspace", http.StatusBadRequest, "target_workspace_mismatch", func(string) RoutingAgentConfig { return RoutingAgentConfig{AgentID: roster.a} },
			func(params map[string]any) { params["workspace_id"] = foreignWorkspaceID(test) }},
	} {
		test.Run(tc.name, func(test *testing.T) {
			setWorkspaceSettings(test, `{"unlisted":"keep"}`)
			fixture := newActionFixture(test)
			cfg := tc.cfg(fixture.agentID)
			params := routingParams(test, cfg)
			if strings.Contains(tc.want, "not in this workspace") || strings.Contains(tc.want, "archived") || strings.Contains(tc.want, "must") || strings.Contains(tc.want, "keyed") {
				params["after_sha256"] = strings.Repeat("0", 64) // the server refuses the value before it compares digests
			}
			if tc.edit != nil {
				tc.edit(params)
			}
			_, body := fileRouting(test, fixture, tc.status, params)
			mustContain(test, body, tc.want)
			if _, set := storedSettings(test)["routing_agent"]; set {
				test.Fatal("a refused request changed the routing agent")
			}
		})
	}
}

func TestRoutingAgentSetProposerMayKeepWhatIsLive(test *testing.T) {
	roster := newRoutingRoster(test)
	fixture := newActionFixture(test)
	// The proposer is already the routing agent: a change that leaves it named,
	// here by adding a fallback, is not a new self-grant.
	setWorkspaceSettings(test, `{"routing_agent":{"agent_id":"`+fixture.agentID+`","fallback_agent_id":"","statuses":["todo"],"overrides":{}}}`)
	cfg := RoutingAgentConfig{AgentID: fixture.agentID, FallbackAgentID: roster.b, Statuses: []string{"todo"}}
	request, _ := fileRouting(test, fixture, http.StatusCreated, routingParams(test, cfg))
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if got := storedRouting(test); got.AgentID != fixture.agentID || got.FallbackAgentID != roster.b {
		test.Fatalf("stored routing agent = %+v", got)
	}
}

func TestRoutingAgentSetApproveRefusals(test *testing.T) {
	roster := newRoutingRoster(test)
	cfg := RoutingAgentConfig{AgentID: roster.a}
	file := func(test *testing.T) (settingsRequest, singleCallFixture) {
		setWorkspaceSettings(test, `{"unlisted":"keep"}`)
		fixture := newActionFixture(test)
		request, _ := fileRouting(test, fixture, http.StatusCreated, routingParams(test, cfg))
		return request, fixture
	}
	notApplied := func(test *testing.T, request settingsRequest) {
		test.Helper()
		request.action.assertUntouched(test)
		if _, set := storedSettings(test)["routing_agent"]; set {
			test.Fatal("a refused approval wrote the routing agent")
		}
		if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
			test.Fatalf("config_change_applied rows = %d, want 0", n)
		}
	}
	test.Run("the setting changed after the request", func(test *testing.T) {
		request, _ := file(test)
		dbfx.Exec(test, `UPDATE workspace SET settings = jsonb_set(settings, '{routing_agent}', $2::jsonb) WHERE id = $1`, testWorkspaceID,
			`{"agent_id":"`+roster.b+`","fallback_agent_id":"","statuses":["todo"],"overrides":{}}`)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.action.assertUntouched(test)
		if got := storedRouting(test); got.AgentID != roster.b {
			test.Fatalf("a refused approval changed the routing agent: %+v", got)
		}
	})
	test.Run("approver is not an owner", func(test *testing.T) {
		request, _ := file(test)
		makeApproverOwner(test, request.action, "admin")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		notApplied(test, request)
	})
	test.Run("the named agent was archived after the request", func(test *testing.T) {
		request, _ := file(test)
		dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, roster.a)
		test.Cleanup(func() { dbfx.Exec(test, `UPDATE agent SET archived_at = NULL WHERE id = $1`, roster.a) })
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusBadRequest), "archived agent")
		notApplied(test, request)
	})
	test.Run("the proposer was archived after the request, before the sweeper", func(test *testing.T) {
		request, fixture := file(test)
		dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, fixture.agentID)
		test.Cleanup(func() { dbfx.Exec(test, `UPDATE agent SET archived_at = NULL WHERE id = $1`, fixture.agentID) })
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "not_pending")
		notApplied(test, request)
	})
	test.Run("the stored params name the proposer", func(test *testing.T) {
		request, fixture := file(test)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{routing_agent,agent_id}', to_jsonb($1::text)) WHERE id = $2`, fixture.agentID, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "payload_mismatch")
		notApplied(test, request)
	})
}

func TestSettingsPatchStillRefusesTheRoutingAgentKey(test *testing.T) {
	roster := newRoutingRoster(test)
	_, body := requestSettings(test, http.StatusForbidden, map[string]any{"routing_agent": map[string]any{"agent_id": roster.a}}, nil)
	mustContain(test, body, "denylisted")
	if _, set := storedSettings(test)["routing_agent"]; set {
		test.Fatal("the generic patch wrote the routing agent")
	}
}
