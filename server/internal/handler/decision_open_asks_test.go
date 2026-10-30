package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestOpenAsksHeadlineIsTheStructuredQuestion(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated)
	request := newRequest(http.MethodGet, "/api/decisions/open-asks?for="+fixture.humanID, nil)
	request.Header.Set("X-Actor-Source", "task_token")
	var asks []openAskResponse
	testutil.Call(test, testHandler.ListOpenDecisionAsks, request).Want(http.StatusOK).JSON(&asks)
	if len(asks) != 1 || asks[0].Ask != fixture.input().Ask || asks[0].DefaultIfNoReply != "Do not proceed." {
		test.Fatalf("a real request must read as its question, got %+v", asks)
	}
}

func TestOpenAsksCrossMemberReadIsTaskTokenOnly(test *testing.T) {
	other := uuid.NewString()
	for source, want := range map[string]int{
		"task_token": http.StatusOK, "cloud_pat": http.StatusForbidden, "pat": http.StatusForbidden,
		"cli_jwt": http.StatusForbidden, "interactive_jwt": http.StatusForbidden, "": http.StatusForbidden,
	} {
		test.Run(source, func(test *testing.T) {
			request := newRequest(http.MethodGet, "/api/decisions/open-asks?for="+other, nil)
			request.Header.Set("X-Actor-Source", source)
			testutil.Call(test, testHandler.ListOpenDecisionAsks, request).Want(want)
		})
	}
}

func TestOpenAsksKeepsOnlyTheFirstLineOfAMultilineQuestion(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	request := fixture.apiRequest()
	body := map[string]any{
		"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": fixture.input().EscalationClass,
		"ask": structuredDecisionAsk(fixture.issueID, fixture.tagID, "Approve the change?\nSECOND-LINE-HASH sha256:abc\nTHIRD-LINE"),
	}
	multiline := newRequest(http.MethodPost, "/api/decisions", body)
	multiline.Header = request.Header
	testutil.Call(test, testHandler.RequestDecision, multiline).Want(http.StatusCreated)
	read := newRequest(http.MethodGet, "/api/decisions/open-asks?for="+fixture.humanID, nil)
	read.Header.Set("X-Actor-Source", "task_token")
	var raw json.RawMessage
	testutil.Call(test, testHandler.ListOpenDecisionAsks, read).Want(http.StatusOK).JSON(&raw)
	var asks []openAskResponse
	if err := json.Unmarshal(raw, &asks); err != nil {
		test.Fatal(err)
	}
	if len(asks) != 1 || asks[0].Ask != "Approve the change?" {
		test.Fatalf("want the first line only, got %+v", asks)
	}
	if strings.Contains(string(raw), "SECOND-LINE-HASH") || strings.Contains(string(raw), "THIRD-LINE") {
		test.Fatalf("later lines leaked: %s", raw)
	}
}
