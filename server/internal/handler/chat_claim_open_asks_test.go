package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// CHE-4150 (L6 of CHE-2769): the claim of a DM turn lists the agent's open ask ids so the prompt can
// show them to `multica chat reply-state`. Resolved asks, other agents' asks and non-asks stay out.
func TestClaimListsTheAgentsOpenAsks(t *testing.T) {
	dm := newAttentionDM(t)
	first := dm.item(t, "ask")
	second := dm.item(t, "ask")
	dbfx.Exec(t, `UPDATE chat_attention_item SET opened_at = now() - interval '2 hours' WHERE id = $1`, first)
	dbfx.Exec(t, `UPDATE chat_attention_item SET opened_at = now() - interval '1 hour' WHERE id = $1`, second)
	done := dm.item(t, "ask")
	dbfx.Exec(t, `UPDATE chat_attention_item SET resolved_at = now(), resolved_by = 'agent_done' WHERE id = $1`, done)
	dm.item(t, "later")
	dm.mustSend(t, "hello", nil)

	w := httptest.NewRecorder()
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+dm.runtimeID+"/tasks/claim", nil)
	testHandler.ClaimTaskByRuntime(w, withURLParam(req, "runtimeId", dm.runtimeID))
	if w.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *struct {
			OpenAskIDs []string `json:"chat_open_ask_ids"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Task == nil {
		t.Fatalf("decode claim: %v %s", err, w.Body.String())
	}
	if want := []string{first, second}; !slices.Equal(resp.Task.OpenAskIDs, want) {
		t.Fatalf("chat_open_ask_ids = %v, want %v (open asks, oldest first)", resp.Task.OpenAskIDs, want)
	}
}
