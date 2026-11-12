package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5334: an owner or admin may say why an agent is paused. The reason is
// stored with the pause, shown to readers, and cleared by resume.

type pauseReasonBody struct {
	Reason *string `json:"scheduling_paused_reason"`
}

func callPauseWithReason(t *testing.T, id string, body any) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("POST", "/api/agents/"+id+"/pause", body), "id", id)
	return testutil.Call(t, testHandler.PauseAgentScheduling, req)
}

func TestPauseAgentStoresTrimmedReasonAndResumeClearsIt(t *testing.T) {
	agentID := createHandlerTestAgent(t, "ReasonTarget", []byte("[]"))

	var got pauseReasonBody
	callPauseWithReason(t, agentID, map[string]string{"reason": "  budget freeze  "}).Want(http.StatusOK).JSON(&got)
	if got.Reason == nil || *got.Reason != "budget freeze" {
		t.Fatalf("reason = %v, want %q", got.Reason, "budget freeze")
	}

	resume := withURLParam(newRequest("POST", "/api/agents/"+agentID+"/resume", nil), "id", agentID)
	got = pauseReasonBody{}
	testutil.Call(t, testHandler.ResumeAgentScheduling, resume).Want(http.StatusOK).JSON(&got)
	if got.Reason != nil {
		t.Fatalf("reason after resume = %q, want null", *got.Reason)
	}
}

func TestPauseAgentWithoutBodyHasNoReason(t *testing.T) {
	agentID := createHandlerTestAgent(t, "ReasonNone", []byte("[]"))
	var got pauseReasonBody
	callPauseWithReason(t, agentID, nil).Want(http.StatusOK).JSON(&got)
	if got.Reason != nil {
		t.Fatalf("reason = %q, want null", *got.Reason)
	}
}

func TestPauseAgentRejectsOverlongReason(t *testing.T) {
	agentID := createHandlerTestAgent(t, "ReasonLong", []byte("[]"))
	callPauseWithReason(t, agentID, map[string]string{"reason": strings.Repeat("x", 501)}).Want(http.StatusBadRequest)
}
