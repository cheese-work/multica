package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// S0 red tests for the decision screen plan r4 (CHE-5480, CHE-5992): A1 auto-apply route,
// A4a member view, A6a backing lane. Each asserts behavior that does not exist yet.

// setWorkspaceDecisionSettings merges generic workspace settings for one test and restores them after.
func setWorkspaceDecisionSettings(test *testing.T, settings string) {
	test.Helper()
	var previous string
	dbfx.QueryRow(test, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(test, `UPDATE workspace SET settings = settings || $2::jsonb WHERE id = $1`, testWorkspaceID, settings)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, previous)
	})
}

// shadowAskFixture is a reversible ask with recommendation A and default_if_no_reply "Apply A" (plan r4 fixture).
func shadowAskFixture(test *testing.T, class string) (singleCallFixture, map[string]any) {
	test.Helper()
	fixture := newSingleCallFixture(test)
	ask := fixture.details
	ask.Options = []decisiontx.AskOption{testAskOption("A"), testAskOption("B")}
	ask.Recommendation, ask.RecommendedOption, ask.DefaultIfNoReply = "A", "A", "Apply A"
	fixture.details = ask
	body := fixture.body()
	body["escalation_class"] = class
	return fixture, body
}

func createAsk(test *testing.T, fixture singleCallFixture, body map[string]any) map[string]any {
	test.Helper()
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated).JSON(&created)
	return created
}

// A1: server-side route by class under decision_auto_apply.
func TestA1AutoApplyRouteByClass(test *testing.T) {
	test.Skip("red by design until slice A1 lands (CHE-5996, CHE-5480); remove this Skip in that PR")
	for class, route := range map[string]string{"reversible": "auto_apply", "spend": "human", "deploy": "human", "publish": "human", "irreversible": "human", "product_direction": "human"} {
		test.Run(class, func(test *testing.T) {
			setWorkspaceDecisionSettings(test, `{"decision_auto_apply":"on"}`)
			fixture, body := shadowAskFixture(test, class)
			created := createAsk(test, fixture, body)
			if created["route"] != route {
				test.Fatalf("class %s: route = %v, want %s", class, created["route"], route)
			}
			answers := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, created["id"])
			if (route == "auto_apply") != (answers == 1) {
				test.Fatalf("class %s: %d answers recorded for route %s", class, answers, route)
			}
		})
	}
}

func TestA1ShadowRecordsNoAnswer(test *testing.T) {
	test.Skip("red by design until slice A1 lands (CHE-5996, CHE-5480); remove this Skip in that PR")
	setWorkspaceDecisionSettings(test, `{"decision_auto_apply":"shadow"}`)
	fixture, body := shadowAskFixture(test, "reversible")
	created := createAsk(test, fixture, body)
	if created["route"] != "human" || created["would_auto_apply"] != true {
		test.Fatalf("shadow ask: route=%v would_auto_apply=%v, want human/true", created["route"], created["would_auto_apply"])
	}
	if answers := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, created["id"]); answers != 0 {
		test.Fatalf("shadow recorded %d answers, want 0", answers)
	}
}

// A4a: a member read of a blind request carries no recommendation and no default, as key or text.
func TestA4aMemberViewHidesRecommendationAndDefault(test *testing.T) {
	test.Skip("red by design until slice A4a lands (CHE-5997, CHE-5480); remove this Skip in that PR")
	setWorkspaceDecisionSettings(test, `{"decision_blind_pick":"on","decision_auto_apply":"shadow"}`)
	fixture, body := shadowAskFixture(test, "reversible")
	created := createAsk(test, fixture, body)
	id, _ := created["id"].(string)
	read := func(asMember bool) string {
		request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+id, nil), "id", id)
		if asMember {
			request.Header.Set("X-User-ID", fixture.humanID)
		} else {
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
		}
		return testutil.Call(test, testHandler.GetDecision, request).Want(http.StatusOK).Text()
	}
	hidden := []string{"Apply A", "**Recommendation**", "**Default if no reply**", "recommended_option_index", "recommended_option", "default_if_no_reply", "confidence"}
	member := read(true)
	for _, needle := range hidden {
		if strings.Contains(member, needle) {
			test.Errorf("member read of a blind request contains %q", needle)
		}
	}
	if agent := read(false); !strings.Contains(agent, "Apply A") || !strings.Contains(agent, "**Recommendation**") {
		test.Error("agent read must keep the recommendation and default")
	}
}

// A6a: with decision_screen on, an issue-agent ask for a member goes to a hidden backing lane.
func TestA6aBackingLaneIsHiddenFromMember(test *testing.T) {
	test.Skip("red by design until slice A6a lands (CHE-5998, CHE-5480); remove this Skip in that PR")
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	fixture, body := shadowAskFixture(test, "product_direction")
	body["for"] = testUserID // the test member, who has no decision DM yet
	created := createAsk(test, fixture, body)
	session, _ := created["chat_session_id"].(string)
	if session == "" {
		test.Fatalf("request has no chat_session_id: %+v", created)
	}
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, session)
		dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, session)
	})
	list := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/api/chat/sessions?status=all", nil))
	if listed := testutil.Call(test, testHandler.ListChatSessions, list).Want(http.StatusOK).Text(); strings.Contains(listed, session) {
		test.Errorf("member session list shows the backing lane %s", session)
	}
	send := withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+session+"/messages", map[string]any{"content": "hello"}), "sessionId", session)
	testutil.Call(test, testHandler.SendChatMessage, withChatTestWorkspaceCtx(test, send)).Want(http.StatusConflict)
}
