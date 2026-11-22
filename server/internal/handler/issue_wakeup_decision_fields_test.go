package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6447: the list response (which the CLI `wakeup list|get` prints) must
// carry decision_request_id and purpose, so a reader can apply the ask-guard
// rule "member-eligible and no decision binding" without guessing.
func TestIssueWakeupListExposesDecisionRequestIDAndPurpose(t *testing.T) {
	f := newAskGuardWakeupFixture(t)
	bound := f.request(t, f.issueID, f.humanID, "open")
	var boundWakeup, observeWakeup, plainWakeup map[string]any
	f.create(t, f.replyWait("wait for the answer", map[string]any{"decision_request_id": bound, "filter_actor_type": "member", "filter_actor_id": f.humanID})).Want(http.StatusCreated).JSON(&boundWakeup)
	f.create(t, map[string]any{"kind": "event", "event_types": []string{"comment.created"}, "instruction": "observe", "purpose": "observe"}).Want(http.StatusCreated).JSON(&observeWakeup)
	f.create(t, map[string]any{"kind": "at", "after_seconds": 600, "instruction": "check"}).Want(http.StatusCreated).JSON(&plainWakeup)

	var list []map[string]any
	testutil.Call(t, testHandler.ListIssueWakeups, f.wakeupReq(http.MethodGet, "", nil)).Want(http.StatusOK).JSON(&list)
	byID := map[any]map[string]any{}
	for _, w := range list {
		byID[w["id"]] = w
	}
	for _, w := range list {
		for _, key := range []string{"decision_request_id", "purpose"} {
			if _, present := w[key]; !present {
				t.Fatalf("list row %v lacks %q: %v", w["id"], key, w)
			}
		}
	}
	if got := byID[boundWakeup["id"]]["decision_request_id"]; got != bound {
		t.Errorf("bound decision_request_id = %v, want %v", got, bound)
	}
	if got := byID[observeWakeup["id"]]["purpose"]; got != "observe" {
		t.Errorf("observe purpose = %v, want observe", got)
	}
	if got := byID[plainWakeup["id"]]["decision_request_id"]; got != nil {
		t.Errorf("unbound decision_request_id = %v, want null", got)
	}
	if got := byID[plainWakeup["id"]]["purpose"]; got != nil {
		t.Errorf("no-purpose purpose = %v, want null", got)
	}
}
