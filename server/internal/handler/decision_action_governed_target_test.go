package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	squadNoopAction     = "test.squad.noop"
	workspaceNoopAction = "test.workspace.noop"
)

// registerGovernedTargetActions adds one spend-class, owner-only noop action per
// new target; the real actions of steps 4 to 7 reuse the same plumbing.
func registerGovernedTargetActions(test *testing.T) {
	test.Helper()
	normalize := func(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
		return raw, slots, nil
	}
	decisionaction.Register(&decisionaction.Definition{Type: squadNoopAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetSquad, OwnerOnly: true, Normalize: normalize})
	decisionaction.Register(&decisionaction.Definition{Type: workspaceNoopAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetWorkspace, OwnerOnly: true, Normalize: normalize})
	decisionActionExecutors[squadNoopAction] = noopAssertingTarget("squad_id", func(execution decisionActionExecution) pgtype.UUID { return execution.Squad.ID })
	decisionActionExecutors[workspaceNoopAction] = noopAssertingTarget("workspace_id", func(execution decisionActionExecution) pgtype.UUID { return execution.Workspace.ID })
	test.Cleanup(func() {
		delete(decisionActionExecutors, squadNoopAction)
		delete(decisionActionExecutors, workspaceNoopAction)
	})
}

// noopAssertingTarget fails the execution unless the executor was handed the
// locked target row the action's params name.
func noopAssertingTarget(paramKey string, loaded func(decisionActionExecution) pgtype.UUID) decisionActionExecutor {
	return func(_ context.Context, execution decisionActionExecution) (json.RawMessage, error) {
		var params map[string]string
		if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
			return nil, err
		}
		if got := util.UUIDToString(loaded(execution)); got != params[paramKey] {
			return nil, fmt.Errorf("executor received target %q; want %s %q", got, paramKey, params[paramKey])
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
}

// foreignWorkspaceID creates a second workspace the test fixtures do not belong to.
func foreignWorkspaceID(test *testing.T) string {
	test.Helper()
	dbfx.Exec(test, `DELETE FROM workspace WHERE slug = 'gov-target-foreign-test'`)
	var id string
	dbfx.QueryRow(test, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Gov foreign', 'gov-target-foreign-test', '', 'GTF') RETURNING id`).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM squad WHERE workspace_id = $1`, id)
	dbfx.Cleanup(test, `DELETE FROM workspace WHERE id = $1`, id)
	return id
}

// squadIn creates a squad in the workspace; its leader is the fixture agent.
func squadIn(test *testing.T, fixture singleCallFixture, workspaceID string) string {
	test.Helper()
	var id string
	dbfx.QueryRow(test, `INSERT INTO squad (workspace_id, name, description, leader_id, creator_id) VALUES ($1, 'target squad ' || gen_random_uuid()::text, '', $2, $3) RETURNING id`, workspaceID, fixture.agentID, testUserID).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM squad WHERE id = $1`, id)
	return id
}

type governedTarget struct {
	name       string
	actionType string
	paramKey   string
	// own is the target id inside the fixture workspace; foreign is one outside it.
	own, foreign func(*testing.T, singleCallFixture) string
}

func governedTargets() []governedTarget {
	return []governedTarget{
		{
			name: "squad", actionType: squadNoopAction, paramKey: "squad_id",
			own: func(test *testing.T, fixture singleCallFixture) string {
				return squadIn(test, fixture, testWorkspaceID)
			},
			foreign: func(test *testing.T, fixture singleCallFixture) string {
				return squadIn(test, fixture, foreignWorkspaceID(test))
			},
		},
		{
			name: "workspace", actionType: workspaceNoopAction, paramKey: "workspace_id",
			own:     func(*testing.T, singleCallFixture) string { return testWorkspaceID },
			foreign: func(test *testing.T, _ singleCallFixture) string { return foreignWorkspaceID(test) },
		},
	}
}

func (target governedTarget) action(id string) map[string]any {
	return map[string]any{"type": target.actionType, "version": 1, "params": map[string]any{target.paramKey: id}}
}

func (target governedTarget) refusalCode() string { return "target_" + target.name + "_mismatch" }

func newPendingGovernedAction(test *testing.T, target governedTarget) (pendingAction, string) {
	test.Helper()
	registerGovernedTargetActions(test)
	fixture := newActionFixture(test)
	id := target.own(test, fixture)
	var created actionResponse
	fixture.respond(test, fixture.withAction(target.action(id)), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return pendingAction{fixture, created.ID, created.Action.ID}, id
}

func TestDecisionActionRequestStoresAGovernedTarget(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			action, id := newPendingGovernedAction(test, target)
			var raw string
			dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, action.actionID).Scan(&raw)
			var preconditions map[string]string
			if err := json.Unmarshal([]byte(raw), &preconditions); err != nil {
				test.Fatal(err)
			}
			if preconditions[target.paramKey] != id {
				test.Fatalf("preconditions %s; want %s %s", raw, target.paramKey, id)
			}
		})
	}
}

func TestDecisionActionRequestRefusesAForeignGovernedTarget(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			registerGovernedTargetActions(test)
			fixture := newActionFixture(test)
			body := fixture.respond(test, fixture.withAction(target.action(target.foreign(test, fixture))), http.StatusBadRequest, nil)
			if !strings.Contains(body, target.refusalCode()) {
				test.Fatalf("body %q; want %s", body, target.refusalCode())
			}
			if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
				test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
			}
		})
	}
}

func TestDecisionActionRequestRefusesAMissingGovernedTarget(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			registerGovernedTargetActions(test)
			fixture := newActionFixture(test)
			for name, id := range map[string]string{"unknown id": "00000000-0000-4000-8000-000000000001", "not a uuid": "nope"} {
				body := fixture.respond(test, fixture.withAction(target.action(id)), http.StatusBadRequest, nil)
				if !strings.Contains(body, target.refusalCode()) {
					test.Fatalf("%s: body %q; want %s", name, body, target.refusalCode())
				}
			}
		})
	}
}

func TestApproveGovernedTargetExecutesForAnOwner(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			action, _ := newPendingGovernedAction(test, target)
			makeApproverOwner(test, action, "owner")
			action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusOK)
			if state := action.state(test); state != "executed" {
				test.Fatalf("action state = %s, want executed", state)
			}
		})
	}
}

func TestApproveGovernedTargetRefusesATargetOutsideTheWorkspace(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			action, _ := newPendingGovernedAction(test, target)
			makeApproverOwner(test, action, "owner")
			foreign := target.foreign(test, action.singleCallFixture)
			dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, ARRAY[$1::text], to_jsonb($2::text)) WHERE id = $3`, target.paramKey, foreign, action.actionID)
			body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
			if !strings.Contains(body, target.refusalCode()) {
				test.Fatalf("body %q; want %s", body, target.refusalCode())
			}
			action.assertUntouched(test)
		})
	}
}

func TestApproveSquadRefusesASquadMovedOutOfTheWorkspace(test *testing.T) {
	target := governedTargets()[0]
	action, squadID := newPendingGovernedAction(test, target)
	makeApproverOwner(test, action, "owner")
	dbfx.Exec(test, `UPDATE squad SET workspace_id = $1 WHERE id = $2`, foreignWorkspaceID(test), squadID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
	if !strings.Contains(body, target.refusalCode()) {
		test.Fatalf("body %q; want %s", body, target.refusalCode())
	}
	action.assertUntouched(test)
}

func TestApproveGovernedTargetNeedsAnOwner(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			refused := func(test *testing.T, action pendingAction, handler *Handler) {
				test.Helper()
				body := action.approve(test, handler, action.claim(test, map[string]string{}), http.StatusForbidden)
				if !strings.Contains(body, "class_needs_owner") {
					test.Fatalf("body %q; want class_needs_owner", body)
				}
				action.assertUntouched(test)
			}
			test.Run("non-owner is refused before the passkey is checked", func(test *testing.T) {
				action, _ := newPendingGovernedAction(test, target)
				makeApproverOwner(test, action, "admin")
				verified := false
				refused(test, action, withVerifier(func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
					verified = true
					return pgtype.UUID{}, nil
				}))
				if verified {
					test.Fatal("the passkey was verified for a non-owner approver")
				}
			})
			test.Run("server-opened request, non-owner", func(test *testing.T) {
				action, _ := newPendingGovernedAction(test, target)
				serverOpened(test, action)
				makeApproverOwner(test, action, "member")
				refused(test, action, withVerifier(acceptingVerifier))
			})
			test.Run("owner demoted during verification", func(test *testing.T) {
				action, _ := newPendingGovernedAction(test, target)
				makeApproverOwner(test, action, "owner")
				refused(test, action, withVerifier(func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
					makeApproverOwner(test, action, "admin")
					return pgtype.UUID{}, nil
				}))
			})
		})
	}
}

// The owner-only flag also covers an agent-target action, whose target kind
// alone implies no owner rule.
func TestActionOwnerOnlyFollowsTheDefinitionFlag(test *testing.T) {
	normalize := func(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
		return raw, slots, nil
	}
	decisionaction.Register(&decisionaction.Definition{Type: "test.agent.plain", Version: 1, MinClass: "reversible", Normalize: normalize})
	decisionaction.Register(&decisionaction.Definition{Type: "test.agent.owner", Version: 1, MinClass: "reversible", OwnerOnly: true, Normalize: normalize})
	cases := map[string]bool{"test.agent.plain": false, "test.agent.owner": true, "agent.env.patch": false, squadNoopAction: false}
	for actionType, want := range cases {
		if actionType == squadNoopAction {
			registerGovernedTargetActions(test)
			want = true
		}
		if got := actionOwnerOnly(db.DecisionAction{ActionType: actionType, ActionVersion: 1}); got != want {
			test.Errorf("actionOwnerOnly(%s) = %v, want %v", actionType, got, want)
		}
	}
}

func TestApproveLocksTheGovernedTargetRow(test *testing.T) {
	for _, target := range governedTargets() {
		test.Run(target.name, func(test *testing.T) {
			action, id := newPendingGovernedAction(test, target)
			makeApproverOwner(test, action, "owner")
			table := map[string]string{"squad": "squad", "workspace": "workspace"}[target.name]
			transaction, err := testPool.Begin(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			defer transaction.Rollback(context.Background())
			if _, err := transaction.Exec(context.Background(), `SELECT 1 FROM `+table+` WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
				test.Fatal(err)
			}
			body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
			if !strings.Contains(body, "busy_retry") {
				test.Fatalf("body %q; want busy_retry while another writer holds the target row", body)
			}
			action.assertUntouched(test)
		})
	}
}

// A reversible-class action is not caught by the class rule, so the flag alone
// must keep a non-owner out, before the passkey and again under the member lock.
func TestApproveOwnerOnlyReversibleActionNeedsAnOwner(test *testing.T) {
	const actionType = "test.squad.reversible"
	decisionaction.Register(&decisionaction.Definition{
		Type: actionType, Version: 1, MinClass: "reversible", Target: decisionaction.TargetSquad, OwnerOnly: true,
		Normalize: func(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
			return raw, slots, nil
		},
	})
	decisionActionExecutors[actionType] = func(context.Context, decisionActionExecution) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}
	test.Cleanup(func() { delete(decisionActionExecutors, actionType) })
	target := governedTarget{name: "squad", actionType: actionType, paramKey: "squad_id", own: governedTargets()[0].own}
	refused := func(test *testing.T, action pendingAction, handler *Handler) {
		test.Helper()
		body := action.approve(test, handler, action.claim(test, map[string]string{}), http.StatusForbidden)
		if !strings.Contains(body, "class_needs_owner") {
			test.Fatalf("body %q; want class_needs_owner", body)
		}
		action.assertUntouched(test)
	}
	test.Run("before the passkey", func(test *testing.T) {
		action, _ := newPendingGovernedAction(test, target)
		makeApproverOwner(test, action, "admin")
		verified := false
		refused(test, action, withVerifier(func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
			verified = true
			return pgtype.UUID{}, nil
		}))
		if verified {
			test.Fatal("the passkey was verified for a non-owner approver")
		}
	})
	test.Run("under the member lock", func(test *testing.T) {
		action, _ := newPendingGovernedAction(test, target)
		makeApproverOwner(test, action, "owner")
		refused(test, action, withVerifier(func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
			makeApproverOwner(test, action, "admin")
			return pgtype.UUID{}, nil
		}))
	})
}

func TestApproveLocksTheWorkflowTargetRow(test *testing.T) {
	action, workflowID := newPendingWorkflowAction(test, workflowNoopAction)
	makeApproverOwner(test, action, "owner")
	transaction, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if _, err := transaction.Exec(context.Background(), `SELECT 1 FROM issue_workflow WHERE id = $1 FOR UPDATE`, workflowID); err != nil {
		test.Fatal(err)
	}
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
	if !strings.Contains(body, "busy_retry") {
		test.Fatalf("body %q; want busy_retry while another writer holds the workflow row", body)
	}
	action.assertUntouched(test)
}
