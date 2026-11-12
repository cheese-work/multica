package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// watchCardUpdates records every decision_answer:updated payload that names
// the issue. The bus has no unsubscribe, so the filter keeps tests apart.
func watchCardUpdates(test *testing.T, issueID string) func() []map[string]any {
	var mutex sync.Mutex
	var seen []map[string]any
	testHandler.Bus.Subscribe(protocol.EventDecisionAnswerUpdated, func(event events.Event) {
		raw, _ := json.Marshal(event.Payload)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if payload["issue_id"] != issueID {
			return
		}
		mutex.Lock()
		seen = append(seen, payload)
		mutex.Unlock()
	})
	return func() []map[string]any {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]map[string]any(nil), seen...)
	}
}

func wantCardUpdate(test *testing.T, updates []map[string]any, requestID string) {
	test.Helper()
	if len(updates) != 1 {
		test.Fatalf("decision_answer:updated events = %d, want 1: %+v", len(updates), updates)
	}
	var want string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM decision_answer WHERE decision_request_id = $1 AND answer_source = 'card'`, requestID).Scan(&want); err != nil {
		test.Fatal(err)
	}
	if updates[0]["answer_id"] != want {
		test.Fatalf("event answer_id = %v, want the card answer %s", updates[0]["answer_id"], want)
	}
}

// The DM card re-reads its action state on this event; without it an open
// client keeps showing the pending buttons after the click.
func TestApproveDecisionActionPublishesTheCardUpdate(test *testing.T) {
	action := newPendingSecretAction(test)
	seen := watchCardUpdates(test, action.issueID)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
	wantCardUpdate(test, seen(), action.requestID)
}

func TestApproveDecisionActionFailurePublishesTheCardUpdate(test *testing.T) {
	action := newPendingSecretAction(test)
	restore := decisionActionExecutors["agent.env.patch"]
	decisionActionExecutors["agent.env.patch"] = func(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
		return nil, actionFailure{Code: "boom", Message: "refused"}
	}
	test.Cleanup(func() { decisionActionExecutors["agent.env.patch"] = restore })
	seen := watchCardUpdates(test, action.issueID)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
	wantCardUpdate(test, seen(), action.requestID)
}

func TestRejectDecisionActionPublishesTheCardUpdate(test *testing.T) {
	action := newPendingAction(test)
	seen := watchCardUpdates(test, action.issueID)
	action.reject(test, testHandler, http.StatusOK)
	wantCardUpdate(test, seen(), action.requestID)
}
