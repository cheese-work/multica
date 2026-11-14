package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// B v9 S5 (CHE-5415): discussion outcome, full readback and the passkey flag.

// The registry sets requiresPasskey; a proposer that tries to set it is refused
// and nothing is written.
func TestProposalThatSetsRequiresPasskeyIsRefused(test *testing.T) {
	for _, key := range []string{"requires_passkey", "requiresPasskey"} {
		test.Run(key, func(test *testing.T) {
			useProposalGuard(test)
			fixture := newProposalFixture(test)
			body := fixture.proposal(test)
			body["action_type"], body["params"] = "agent.pause", map[string]any{"agent_id": fixture.agentID}
			body[key] = false
			fixture.propose(test, body, http.StatusBadRequest)
			if rows := fixture.proposalRows(test); rows != 0 {
				test.Fatalf("a refused proposal wrote %d rows", rows)
			}
		})
	}
}

type decisionReadback struct {
	Effect       string  `json:"effect"`
	EffectReason *string `json:"effect_reason"`
	AppliedAt    *string `json:"applied_at"`
	DecidedBy    *string `json:"decided_by"`
	OnBehalfOf   *string `json:"on_behalf_of"`
	Revocable    *bool   `json:"revocable"`
}

func readDecision(test *testing.T, requestID string) decisionReadback {
	test.Helper()
	var read decisionReadback
	request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+requestID, nil), "id", requestID)
	testutil.Call(test, testHandler.GetDecision, request).Want(http.StatusOK).JSON(&read)
	return read
}

// `multica decision get` returns the effect, who decided, for whom, and that
// whether it is revocable (S6, CHE-5416).
func TestGetDecisionReadsEffectAndWho(test *testing.T) {
	for outcome, want := range map[string]struct{ effect, reason string }{
		"applied": {"applied", ""}, "not_applied": {"not_applied", "refused by the runtime"}, "rejected": {"not_applied", "rejected"},
	} {
		test.Run(outcome, func(test *testing.T) {
			action := settleProposal(test, outcome)
			read := readDecision(test, action.requestID)
			if read.Effect != want.effect || ptrOr(read.EffectReason) != want.reason {
				test.Fatalf("effect = %q (%q), want %q (%q)", read.Effect, ptrOr(read.EffectReason), want.effect, want.reason)
			}
			if (read.AppliedAt != nil) != (outcome == "applied") {
				test.Fatalf("applied_at = %v for %s", read.AppliedAt, outcome)
			}
			if ptrOr(read.DecidedBy) != action.humanID || ptrOr(read.OnBehalfOf) != action.humanID {
				test.Fatalf("decided_by = %q, on_behalf_of = %q, want %q", ptrOr(read.DecidedBy), ptrOr(read.OnBehalfOf), action.humanID)
			}
			if read.Revocable == nil || *read.Revocable {
				test.Fatalf("revocable = %v, want false", read.Revocable)
			}
		})
	}
	test.Run("expired", func(test *testing.T) {
		action := newPendingAction(test)
		dbfx.Exec(test, `UPDATE decision_action SET state = 'expired' WHERE id = $1`, action.actionID)
		if read := readDecision(test, action.requestID); read.Effect != "expired" || read.Revocable == nil || *read.Revocable {
			test.Fatalf("expired readback = %+v", read)
		}
	})
	test.Run("pending", func(test *testing.T) {
		action := newPendingAction(test)
		if read := readDecision(test, action.requestID); read.Effect != "" || read.DecidedBy != nil || read.Revocable == nil || *read.Revocable {
			test.Fatalf("pending readback = %+v", read)
		}
	})
}

func requestStatus(test *testing.T, requestID string) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM decision_request WHERE id = $1`, requestID).Scan(&status)
	return status
}

func wakeText(test *testing.T, requestID string) string {
	test.Helper()
	var snapshot string
	dbfx.QueryRow(test, `SELECT content_snapshot FROM decision_answer WHERE decision_request_id = $1`, requestID).Scan(&snapshot)
	return snapshot
}

// Approve wakes with the optional note; reject wakes with the optional reason.
func TestApproveNoteAndRejectReasonReachTheWake(test *testing.T) {
	test.Run("approve note", func(test *testing.T) {
		resetDecisionLease(test)
		action, _ := newPendingTopUp(test, true)
		dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
		claim := action.claim(test, nil)
		claim["note"] = "go ahead, budget is fine"
		action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusOK)
		if got := wakeText(test, action.requestID); !strings.Contains(got, "Applied issue.run_budget.top_up.") || !strings.Contains(got, "Note: go ahead, budget is fine") {
			test.Fatalf("wake text = %q", got)
		}
	})
	test.Run("reject reason", func(test *testing.T) {
		resetDecisionLease(test)
		action := newPendingAction(test)
		dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
		raw, _ := json.Marshal(map[string]any{"reason": "too risky today"})
		request := action.decide("reject", action.humanID, "interactive_jwt")
		request.Body = readerCloser{bytes.NewReader(raw)}
		testutil.Call(test, testHandler.RejectDecisionAction, request).Want(http.StatusOK)
		if got := wakeText(test, action.requestID); !strings.Contains(got, "Rejected agent.env.patch.") || !strings.Contains(got, "Reason: too risky today") {
			test.Fatalf("wake text = %q", got)
		}
	})
	test.Run("note too long", func(test *testing.T) {
		action := newPendingAction(test)
		claim := action.claim(test, nil)
		claim["note"] = strings.Repeat("x", 1025)
		action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusBadRequest)
		if state := action.state(test); state != "pending" {
			test.Fatalf("state after a refused note = %q", state)
		}
	})
}

func (action pendingAction) discuss(test *testing.T, message string, status int) {
	test.Helper()
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-requests/"+action.requestID+"/discuss", map[string]any{"message": message}), "id", action.requestID)
	request.Header.Set("X-User-ID", action.humanID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(test, testHandler.DiscussDecisionRequest, request).Want(status)
}

func (action pendingAction) runs(test *testing.T) int {
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
}

// Discuss more keeps the ask open and wakes the proposer once per message with
// outcome `discussion` and the message.
func TestDiscussionWakesTheProposerOncePerMessageAndKeepsTheAskOpen(test *testing.T) {
	resetDecisionLease(test)
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)

	action.discuss(test, "why this agent and not the other one?", http.StatusOK)
	if n := action.runs(test); n != 1 {
		test.Fatalf("runs after the first message = %d, want 1", n)
	}
	var task string
	dbfx.QueryRow(test, `SELECT row_to_json(t)::text FROM agent_task_queue t WHERE issue_id = $1`, action.issueID).Scan(&task)
	if !strings.Contains(task, "discussion") || !strings.Contains(task, "why this agent and not the other one?") {
		test.Fatalf("task lacks the discussion outcome or the message: %s", task)
	}
	if status := requestStatus(test, action.requestID); status != "open" {
		test.Fatalf("request status after discuss = %q, want open", status)
	}
	if state := action.state(test); state != "pending" {
		test.Fatalf("action state after discuss = %q, want pending", state)
	}

	// A finished run frees the next message to start its own run.
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE issue_id = $1`, action.issueID)
	action.discuss(test, "and what if I say no?", http.StatusOK)
	if n := action.runs(test); n != 2 {
		test.Fatalf("runs after the second message = %d, want 2", n)
	}
	// The ask is still decidable afterwards.
	action.reject(test, testHandler, http.StatusOK)
}

// Typing "đồng ý" or "approve" is a message, never an approval.
func TestTypedApprovalIsOnlyAMessage(test *testing.T) {
	for _, typed := range []string{"đồng ý", "approve"} {
		test.Run(typed, func(test *testing.T) {
			resetDecisionLease(test)
			action := newPendingAction(test)
			version, env := action.agentEnvVersion(test)
			action.discuss(test, typed, http.StatusOK)
			if status := requestStatus(test, action.requestID); status != "open" {
				test.Fatalf("request status = %q, want open", status)
			}
			if state := action.state(test); state != "pending" {
				test.Fatalf("action state = %q, want pending", state)
			}
			if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, action.requestID); n != 0 {
				test.Fatalf("a typed approval recorded %d answers", n)
			}
			if gotVersion, gotEnv := action.agentEnvVersion(test); gotVersion != version || gotEnv != env {
				test.Fatalf("a typed approval changed the agent env (%d → %d)", version, gotVersion)
			}
		})
	}
}

// Only the person the ask is for can discuss it; an empty message is refused.
func TestDiscussionRefusesAStrangerAndAnEmptyMessage(test *testing.T) {
	action := newPendingAction(test)
	action.discuss(test, "   ", http.StatusBadRequest)
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-requests/"+action.requestID+"/discuss", map[string]any{"message": "hi"}), "id", action.requestID)
	request.Header.Set("X-User-ID", "00000000-0000-0000-0000-0000000000ff")
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(test, testHandler.DiscussDecisionRequest, request).WantOneOf(http.StatusForbidden, http.StatusNotFound)
}
