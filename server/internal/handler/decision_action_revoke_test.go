package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// B v9 S6 (CHE-5416): revoke a decided action while it is revocable.

func (action pendingAction) revokeRequest(verb, userID, source string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-requests/"+action.requestID+"/"+verb, nil), "id", action.requestID)
	request.Header.Set("X-User-ID", userID)
	request.Header.Set("X-Actor-Source", source)
	request.Body = readerCloser{bytes.NewReader(raw)}
	return request
}

func (action pendingAction) revoke(test *testing.T, handler *Handler, body map[string]any, status int) string {
	test.Helper()
	return testutil.Call(test, handler.RevokeDecisionAction, action.revokeRequest("revoke", action.humanID, "interactive_jwt", body)).Want(status).Text()
}

// appliedPause is an approved agent.pause: the one action type with an exact
// inverse in this slice.
func appliedPause(test *testing.T) pendingAction {
	test.Helper()
	resetDecisionLease(test)
	action := newPendingAgentAction(test, "agent.pause")
	dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	if !action.schedulingPaused(test) {
		test.Fatal("the pause was not applied")
	}
	// The approval's own wake is not the revoke's: start the revoke from an empty queue.
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
	return action
}

type revokeRead struct {
	Revocable bool `json:"revocable"`
	Revoke    *struct {
		At     string `json:"at"`
		By     string `json:"by"`
		Reason string `json:"reason"`
	} `json:"revoke"`
}

func readRevoke(test *testing.T, requestID string) revokeRead {
	test.Helper()
	var read revokeRead
	request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+requestID, nil), "id", requestID)
	testutil.Call(test, testHandler.GetDecision, request).Want(http.StatusOK).JSON(&read)
	return read
}

// Only agent.pause registers an inverse in this slice, and every inverse has an
// executor, so a revoke can never name a type that cannot run.
func TestOnlyAgentPauseRegistersAnInverse(test *testing.T) {
	for _, definition := range decisionaction.Definitions() {
		want := ""
		if definition.Type == "agent.pause" {
			want = "agent.resume"
		}
		if definition.Inverse != want {
			test.Fatalf("%s inverse = %q, want %q", definition.Type, definition.Inverse, want)
		}
		if definition.Inverse != "" && decisionActionExecutors[definition.Inverse] == nil {
			test.Fatalf("%s names inverse %s without an executor", definition.Type, definition.Inverse)
		}
	}
}

func TestRevokeUndoesTheActionAndRecordsWhoWhenWhy(test *testing.T) {
	action := appliedPause(test)
	if read := readRevoke(test, action.requestID); !read.Revocable || read.Revoke != nil {
		test.Fatalf("before revoke: %+v, want revocable with no record", read)
	}
	action.revoke(test, withVerifier(acceptingVerifier), map[string]any{"reason": "paused the wrong agent"}, http.StatusOK)
	if action.schedulingPaused(test) {
		test.Fatal("the inverse did not resume the agent")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'executed' AND revoked_at IS NOT NULL AND revoked_by_user_id = $2 AND revoke_reason = 'paused the wrong agent'`, action.actionID, action.humanID); n != 1 {
		test.Fatal("the revoke record (who, when, why) was not stored")
	}
	read := readRevoke(test, action.requestID)
	if read.Revocable || read.Revoke == nil || read.Revoke.By != action.humanID || read.Revoke.Reason != "paused the wrong agent" || read.Revoke.At == "" {
		test.Fatalf("after revoke: %+v", read)
	}
	var task string
	dbfx.QueryRow(test, `SELECT row_to_json(t)::text FROM agent_task_queue t WHERE issue_id = $1`, action.issueID).Scan(&task)
	if !strings.Contains(task, "Decision outcome: revoked") || !strings.Contains(task, "paused the wrong agent") {
		test.Fatalf("the proposing agent was not woken with outcome revoked and the reason: %s", task)
	}
	if n := action.runs(test); n != 1 {
		test.Fatalf("runs after a revoke = %d, want 1", n)
	}
}

func TestRevokeNeedsAReason(test *testing.T) {
	action := appliedPause(test)
	for _, reason := range []string{"", "   ", strings.Repeat("x", 1025)} {
		action.revoke(test, withVerifier(acceptingVerifier), map[string]any{"reason": reason}, http.StatusBadRequest)
	}
	if !action.schedulingPaused(test) {
		test.Fatal("a refused revoke resumed the agent")
	}
}

// Every state that is not "applied and not yet revoked" refuses, writes nothing
// and leaves the target as it was.
func TestRevokeRefusals(test *testing.T) {
	handler := withVerifier(acceptingVerifier)
	refuse := func(test *testing.T, action pendingAction, code string) {
		test.Helper()
		revokedBefore := 0
		if action.actionID != "" {
			revokedBefore = dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND revoked_at IS NOT NULL`, action.actionID)
		}
		body := action.revoke(test, handler, map[string]any{"reason": "changed my mind"}, http.StatusConflict)
		if !strings.Contains(body, code) {
			test.Fatalf("refusal %q lacks code %q", body, code)
		}
		if action.actionID != "" {
			if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND revoked_at IS NOT NULL`, action.actionID); n != revokedBefore {
				test.Fatal("a refused revoke wrote a revoke record")
			}
		}
		if read := readRevoke(test, action.requestID); read.Revocable {
			test.Fatal("a refused state reads as revocable")
		}
	}
	test.Run("rejected", func(test *testing.T) {
		action := newPendingAgentAction(test, "agent.pause")
		action.reject(test, testHandler, http.StatusOK)
		refuse(test, action, "rejected")
	})
	test.Run("expired", func(test *testing.T) {
		action := newPendingAgentAction(test, "agent.pause")
		dbfx.Exec(test, `UPDATE decision_action SET state = 'expired' WHERE id = $1`, action.actionID)
		refuse(test, action, "expired")
	})
	test.Run("not applied: still pending", func(test *testing.T) {
		refuse(test, newPendingAgentAction(test, "agent.pause"), "not_applied")
	})
	test.Run("not applied: failed", func(test *testing.T) {
		action := newPendingAgentAction(test, "agent.pause")
		dbfx.Exec(test, `UPDATE decision_action SET state = 'failed', executed_at = now(), error_code = 'boom' WHERE id = $1`, action.actionID)
		refuse(test, action, "not_applied")
	})
	test.Run("answered with discussion only", func(test *testing.T) {
		action := newPendingAgentAction(test, "agent.pause")
		action.discuss(test, "why this agent?", http.StatusOK)
		refuse(test, action, "not_applied")
	})
	test.Run("answered with no action", func(test *testing.T) {
		fixture := newActionFixture(test)
		var created actionResponse
		fixture.respond(test, fixture.body(), http.StatusCreated, &created)
		plain := pendingAction{fixture, created.ID, ""}
		refuse(test, plain, "no_action")
	})
	test.Run("already revoked", func(test *testing.T) {
		action := appliedPause(test)
		action.revoke(test, handler, map[string]any{"reason": "first"}, http.StatusOK)
		refuse(test, action, "already_revoked")
	})
	test.Run("action type without an inverse", func(test *testing.T) {
		action, _ := newPendingTopUp(test, true)
		resetDecisionLease(test)
		action.approve(test, handler, action.claim(test, nil), http.StatusOK)
		refuse(test, action, "no_inverse")
	})
	test.Run("resume has no inverse", func(test *testing.T) {
		resetDecisionLease(test)
		action := newPendingAgentAction(test, "agent.resume")
		action.approve(test, handler, action.claim(test, nil), http.StatusOK)
		refuse(test, action, "no_inverse")
	})
	test.Run("pause that changed nothing", func(test *testing.T) {
		// The agent was already paused: approving kept the earlier pause, so
		// resuming it would undo something this decision never did.
		resetDecisionLease(test)
		action := newPendingAgentAction(test, "agent.pause")
		dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = now(), scheduling_paused_by = $2 WHERE id = $1`, action.agentID, action.humanID)
		action.approve(test, handler, action.claim(test, nil), http.StatusOK)
		refuse(test, action, "target_changed")
		if !action.schedulingPaused(test) {
			test.Fatal("the refused revoke resumed an agent this decision never paused")
		}
	})
	test.Run("target changed since", func(test *testing.T) {
		action := appliedPause(test)
		dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, action.agentID)
		dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = now() + interval '1 second', scheduling_paused_by = $2 WHERE id = $1`, action.agentID, action.humanID)
		refuse(test, action, "target_changed")
		if !action.schedulingPaused(test) {
			test.Fatal("the refused revoke resumed an agent paused by someone else")
		}
	})
}

// Revoke is authorized exactly like Approve: same human, same verifier.
func TestRevokeAuthorizationMatchesApprove(test *testing.T) {
	body := map[string]any{"reason": "no"}
	test.Run("fails closed without a verifier", func(test *testing.T) {
		action := appliedPause(test)
		action.revoke(test, withVerifier(nil), body, http.StatusForbidden)
		if !action.schedulingPaused(test) {
			test.Fatal("a revoke with no verifier resumed the agent")
		}
	})
	test.Run("a verifier that refuses writes nothing", func(test *testing.T) {
		action := appliedPause(test)
		refusing := withVerifier(func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
			return pgtype.UUID{}, decisionIntentRefusal{http.StatusConflict, "payload_mismatch: refused"}
		})
		action.revoke(test, refusing, body, http.StatusConflict)
		if !action.schedulingPaused(test) || dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND revoked_at IS NOT NULL`, action.actionID) != 0 {
			test.Fatal("a refused proof still revoked")
		}
	})
	test.Run("only the request's human", func(test *testing.T) {
		action := appliedPause(test)
		stranger := action.revokeRequest("revoke", "00000000-0000-0000-0000-0000000000ff", "interactive_jwt", body)
		testutil.Call(test, withVerifier(acceptingVerifier).RevokeDecisionAction, stranger).WantOneOf(http.StatusForbidden, http.StatusNotFound)
		agent := action.revokeRequest("revoke", action.humanID, "task_token", body)
		testutil.Call(test, withVerifier(acceptingVerifier).RevokeDecisionAction, agent).Want(http.StatusForbidden)
		if !action.schedulingPaused(test) {
			test.Fatal("a refused caller resumed the agent")
		}
	})
	test.Run("a plain member may not resume agents", func(test *testing.T) {
		action := appliedPause(test)
		dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE user_id = $1 AND workspace_id = $2`, action.humanID, testWorkspaceID)
		test.Cleanup(func() {
			dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE user_id = $1 AND workspace_id = $2`, action.humanID, testWorkspaceID)
		})
		action.revoke(test, withVerifier(acceptingVerifier), body, http.StatusForbidden)
		if !action.schedulingPaused(test) {
			test.Fatal("a member revoked what only an admin may undo")
		}
	})
}

// With a real passkey service, revoke needs its own assertion: the assertion
// that approved the action cannot be replayed to revoke it.
func TestRevokeNeedsItsOwnPasskeyAssertion(test *testing.T) {
	handler := passkeyHandler(test)
	resetDecisionLease(test)
	action := newPendingAgentAction(test, "agent.pause")
	dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
	device, _ := action.enrollPasskey(test)
	approval := action.assertion(test, device, action.issue(test, handler), passkeyStart+1)
	action.approveWith(test, handler, approval, http.StatusOK)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)

	revoke := func(assertion any, status int) {
		test.Helper()
		testutil.Call(test, handler.RevokeDecisionAction, action.revokeRequest("revoke", action.humanID, "interactive_jwt", map[string]any{"reason": "undo", "assertion": assertion})).Want(status)
	}
	revoke(nil, http.StatusForbidden)
	revoke(approval, http.StatusForbidden)
	if !action.schedulingPaused(test) {
		test.Fatal("a missing or replayed assertion revoked the action")
	}
	var issued issuedChallenge
	decodeJSON(test, testutil.Call(test, handler.ChallengeDecisionRevoke, action.revokeRequest("revoke/challenge", action.humanID, "interactive_jwt", nil)).Want(http.StatusOK).Text(), &issued)
	revoke(action.assertion(test, device, issued, passkeyStart+2), http.StatusOK)
	if action.schedulingPaused(test) {
		test.Fatal("the right assertion did not revoke")
	}
}

// The revoke challenge is refused for the same states the revoke is.
func TestRevokeChallengeRefusesWhatRevokeRefuses(test *testing.T) {
	handler := passkeyHandler(test)
	action := newPendingAgentAction(test, "agent.pause")
	action.enrollPasskey(test)
	testutil.Call(test, handler.ChallengeDecisionRevoke, action.revokeRequest("revoke/challenge", action.humanID, "interactive_jwt", nil)).Want(http.StatusConflict)
}
