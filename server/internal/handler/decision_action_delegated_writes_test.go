package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	dwAction = "workspace.delegated_writes.set"
	dwCtx    = "workspace.context"
	dwSquad  = "squad.instructions"
)

// dwAgentIn creates an agent in the workspace for the test.
func dwAgentIn(test *testing.T, workspaceID string) string {
	test.Helper()
	name := fmt.Sprintf("dw-agent-%d", time.Now().UnixNano())
	runtime := dbfx.Runtime(test, name, testutil.Cols{"workspace_id": workspaceID})
	return dbfx.Agent(test, name, runtime, testutil.Cols{"workspace_id": workspaceID})
}

// dwConfig sorts both lists: the digest and the stored setting take that form.
func dwConfig(requires bool, agents, fields []string) DelegatedWritesConfig {
	return DelegatedWritesConfig{AllowedAgents: sortedSet(agents, true), AllowedFields: sortedSet(fields, false), RequiresHumanDecision: requires}
}

// seedDelegated stores the setting as the workspace has it before the request.
func seedDelegated(test *testing.T, cfg DelegatedWritesConfig) {
	test.Helper()
	raw, err := json.Marshal(map[string]any{delegatedWritesKey: cfg, "unlisted": "keep"})
	if err != nil {
		test.Fatal(err)
	}
	setWorkspaceSettings(test, string(raw))
}

// dwDigest is the digest a correct agent computes: the setting with its lists
// sorted and deduplicated, under the one settings key.
func dwDigest(test *testing.T, cfg DelegatedWritesConfig) string {
	test.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		test.Fatal(err)
	}
	digest, err := decisionaction.SettingsDigest(testWorkspaceID, map[string]json.RawMessage{delegatedWritesKey: raw})
	if err != nil {
		test.Fatal(err)
	}
	return digest
}

func storedDelegated(test *testing.T) DelegatedWritesConfig {
	test.Helper()
	var raw string
	dbfx.QueryRow(test, `SELECT settings->'delegated_governed_writes'::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw)
	var cfg DelegatedWritesConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		test.Fatalf("stored setting %q: %v", raw, err)
	}
	return cfg
}

func assertDelegated(test *testing.T, want DelegatedWritesConfig) {
	test.Helper()
	got := storedDelegated(test)
	if !slices.Equal(got.AllowedAgents, want.AllowedAgents) || !slices.Equal(got.AllowedFields, want.AllowedFields) || got.RequiresHumanDecision != want.RequiresHumanDecision {
		test.Fatalf("stored setting %+v, want %+v", got, want)
	}
}

// fileDelegated asks for the change on the live setting before. edit may break
// the params first. A refused request must store nothing.
func fileDelegated(test *testing.T, fixture singleCallFixture, status int, before, after DelegatedWritesConfig, edit func(params map[string]any)) (settingsRequest, string) {
	test.Helper()
	params := map[string]any{
		"workspace_id": testWorkspaceID, "delegated_governed_writes": after,
		"base_sha256": dwDigest(test, before), "after_sha256": dwDigest(test, after),
	}
	if edit != nil {
		edit(params)
	}
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": dwAction, "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return settingsRequest{}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return settingsRequest{pendingAction{fixture, created.ID, created.Action.ID}, dwDigest(test, before)}, body
}

func TestDelegatedWritesSetProposeAndApprove(test *testing.T) {
	fixture := newActionFixture(test)
	other := dwAgentIn(test, testWorkspaceID)
	before := dwConfig(true, []string{}, []string{})
	after := dwConfig(true, []string{other}, []string{dwCtx})
	seedDelegated(test, before)
	request, _ := fileDelegated(test, fixture, http.StatusCreated, before, after, nil)

	var preview map[string]any
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	if err := json.Unmarshal([]byte(raw), &preview); err != nil || preview["field"] != "workspace.delegated_writes" || preview["before_sha256"] != dwDigest(test, before) {
		test.Fatalf("preconditions %s (%v)", raw, err)
	}
	assertDelegated(test, before) // a request alone changes nothing

	_, keyID := request.action.enrollPasskey(test)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	request.approveOwner(test, withVerifier(verify), http.StatusOK)

	assertDelegated(test, after)
	if got := storedSettings(test); string(got["unlisted"]) != `"keep"` {
		test.Fatalf("an unnamed settings key changed: %v", got)
	}
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`, request.action.issueID).Scan(&raw)
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		test.Fatal(err)
	}
	if details["field"] != "workspace.delegated_writes" || details["decision_action_id"] != request.action.actionID || details["credential_id"] != keyID ||
		details["agent_id"] != fixture.agentID || details["after_sha256"] != dwDigest(test, after) {
		test.Fatalf("audit details %s", raw)
	}
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE workspace_id = $1 AND action = $2 ORDER BY created_at DESC LIMIT 1`, testWorkspaceID, delegatedWritesActivity).Scan(&raw)
	var change struct{ From, To DelegatedWritesConfig }
	if err := json.Unmarshal([]byte(raw), &change); err != nil || !slices.Equal(change.To.AllowedAgents, []string{other}) || len(change.From.AllowedAgents) != 0 {
		test.Fatalf("delegated_writes_changed details %s (%v)", raw, err)
	}
	dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result["readback_sha256"] != dwDigest(test, after) {
		test.Fatalf("result %s (%v)", raw, err)
	}
}

// The effective-authority rule: a request may not widen what its own proposer
// can write, and may not relax the human-decision requirement.
func TestDelegatedWritesSetRequestRefusals(test *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		want          string
		before, after func(proposer, other string) DelegatedWritesConfig
		edit          func(params map[string]any)
	}{
		{"an allowed proposer gets a new field", http.StatusForbidden, "authority_escalation",
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx}) },
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx, dwSquad}) }, nil},
		{"the proposer adds itself", http.StatusForbidden, "authority_escalation",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{dwCtx}) },
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx}) }, nil},
		{"requires_human_decision goes true to false", http.StatusForbidden, "requires_human_decision",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(false, []string{}, []string{}) }, nil},
		{"unknown field name", http.StatusBadRequest, "invalid_params",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig {
				return dwConfig(true, []string{}, []string{"workspace.owner"})
			}, nil},
		{"agent id of another workspace", http.StatusBadRequest, "invalid_params",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig {
				return dwConfig(true, []string{dwAgentIn(test, foreignWorkspaceID(test))}, []string{})
			}, nil},
		{"agent id that is not an id", http.StatusBadRequest, "invalid_params",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{"nobody"}, []string{}) }, nil},
		{"a list is missing", http.StatusBadRequest, "invalid_params",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(params map[string]any) {
				params["delegated_governed_writes"] = map[string]any{"allowed_agents": []string{}, "requires_human_decision": true}
			}},
		{"an unknown member of the setting", http.StatusBadRequest, "invalid_params",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(params map[string]any) {
				params["delegated_governed_writes"] = map[string]any{"allowed_agents": []string{}, "allowed_fields": []string{}, "owner": true}
			}},
		{"the after digest is wrong", http.StatusBadRequest, "after_digest_mismatch",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{dwCtx}) },
			func(params map[string]any) { params["after_sha256"] = strings.Repeat("0", 64) }},
		{"the base digest is stale", http.StatusConflict, "precondition_drift",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{dwCtx}) },
			func(params map[string]any) { params["base_sha256"] = strings.Repeat("0", 64) }},
		{"the workspace of another workspace", http.StatusBadRequest, "target_workspace_mismatch",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{dwCtx}) },
			func(params map[string]any) { params["workspace_id"] = foreignWorkspaceID(test) }},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newActionFixture(test)
			other := dwAgentIn(test, testWorkspaceID)
			before := tc.before(fixture.agentID, other)
			seedDelegated(test, before)
			_, body := fileDelegated(test, fixture, tc.status, before, tc.after(fixture.agentID, other), tc.edit)
			mustContain(test, body, tc.want)
			assertDelegated(test, before)
		})
	}
}

// Removing, narrowing and granting another agent never raise the proposer's
// own authority, so each is accepted and applied.
func TestDelegatedWritesSetAcceptedChanges(test *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after func(proposer, other string) DelegatedWritesConfig
	}{
		{"grant another agent, proposer unchanged",
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx}) },
			func(p, o string) DelegatedWritesConfig {
				return dwConfig(true, sortedPair(p, o), []string{dwCtx})
			}},
		{"grant another agent while the proposer holds nothing",
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) },
			func(_, o string) DelegatedWritesConfig { return dwConfig(true, []string{o}, []string{dwCtx}) }},
		{"narrow the fields",
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx, dwSquad}) },
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx}) }},
		{"the proposer removes itself",
			func(p, _ string) DelegatedWritesConfig { return dwConfig(true, []string{p}, []string{dwCtx}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{dwCtx}) }},
		{"requires_human_decision goes false to true",
			func(string, string) DelegatedWritesConfig { return dwConfig(false, []string{}, []string{}) },
			func(string, string) DelegatedWritesConfig { return dwConfig(true, []string{}, []string{}) }},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newActionFixture(test)
			other := dwAgentIn(test, testWorkspaceID)
			before, after := tc.before(fixture.agentID, other), tc.after(fixture.agentID, other)
			seedDelegated(test, before)
			request, _ := fileDelegated(test, fixture, http.StatusCreated, before, after, nil)
			request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
			assertDelegated(test, after)
		})
	}
}

func sortedPair(a, b string) []string {
	pair := []string{a, b}
	slices.Sort(pair)
	return pair
}

func TestDelegatedWritesSetApproveRefusals(test *testing.T) {
	newRequest := func(test *testing.T) (settingsRequest, DelegatedWritesConfig) {
		fixture := newActionFixture(test)
		other := dwAgentIn(test, testWorkspaceID)
		before := dwConfig(true, []string{fixture.agentID}, []string{dwCtx})
		seedDelegated(test, before)
		request, _ := fileDelegated(test, fixture, http.StatusCreated, before, dwConfig(true, sortedPair(fixture.agentID, other), []string{dwCtx}), nil)
		return request, before
	}
	test.Run("the live setting changed after the request", func(test *testing.T) {
		request, _ := newRequest(test)
		changed := dwConfig(true, []string{}, []string{dwCtx, dwSquad})
		seedDelegated(test, changed)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.action.assertUntouched(test)
		assertDelegated(test, changed)
	})
	test.Run("the proposer was archived before approval", func(test *testing.T) {
		request, before := newRequest(test)
		dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, request.action.agentID)
		// The lifecycle ends a request whose asking agent is archived, so the
		// approval is refused before Authorize runs; Authorize still refuses it
		// (see TestDelegatedWritesAuthorizeJudgesTheLiveSetting).
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "not_pending")
		request.action.assertUntouched(test)
		assertDelegated(test, before)
	})
	test.Run("the approver is not an owner", func(test *testing.T) {
		request, before := newRequest(test)
		makeApproverOwner(test, request.action, "admin")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		assertDelegated(test, before)
	})
	test.Run("the stored workspace is another workspace", func(test *testing.T) {
		request, before := newRequest(test)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{workspace_id}', to_jsonb($1::text)) WHERE id = $2`, foreignWorkspaceID(test), request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "target_workspace_mismatch")
		assertDelegated(test, before)
	})
	test.Run("a stored list member was altered", func(test *testing.T) {
		request, before := newRequest(test)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{delegated_governed_writes,allowed_fields}', '["workspace.context","squad.instructions"]') WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "payload_mismatch")
		assertDelegated(test, before)
	})
}

// The guard reads the setting it is given, so a re-run at approval judges the
// live one: the same change passes against one live setting and fails against
// another.
func TestDelegatedWritesAuthorizeJudgesTheLiveSetting(test *testing.T) {
	fixture := newActionFixture(test)
	proposer := parseUUID(fixture.agentID)
	wide := dwConfig(true, []string{fixture.agentID}, []string{dwCtx, dwSquad})
	params, err := json.Marshal(map[string]any{"workspace_id": testWorkspaceID, "delegated_governed_writes": wide})
	if err != nil {
		test.Fatal(err)
	}
	archived := dwAgentIn(test, testWorkspaceID)
	dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, archived)
	for _, tc := range []struct {
		name     string
		live     DelegatedWritesConfig
		proposer pgtype.UUID
		want     string
	}{
		{"live grants the proposer less than the change", dwConfig(true, []string{fixture.agentID}, []string{dwCtx}), proposer, "authority_escalation"},
		{"live already grants the proposer all of it", wide, proposer, ""},
		{"the proposer is archived", dwConfig(true, []string{}, []string{}), parseUUID(archived), "proposer_revoked"},
		{"the proposer is gone", dwConfig(true, []string{}, []string{}), parseUUID(foreignWorkspaceID(test)), "proposer_revoked"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			seedDelegated(test, tc.live)
			read, err := readDelegatedWrites(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), params)
			if err != nil {
				test.Fatal(err)
			}
			err = authorizeDelegatedWrites(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), tc.proposer, params, read)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				test.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
