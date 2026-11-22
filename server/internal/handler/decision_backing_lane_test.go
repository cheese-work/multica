package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A6a (CHE-5480 plan r4, section 3.7): with decision_screen on, an issue-agent ask goes to a
// hidden decision backing lane, a chat_session registered in decision_backing_lane.

// memberSessionList is the member's chat session list as the web client reads it.
func memberSessionList(test *testing.T) string {
	test.Helper()
	list := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/api/chat/sessions?status=all", nil))
	return testutil.Call(test, testHandler.ListChatSessions, list).Want(http.StatusOK).Text()
}

// memberAsk is a reversible-class ask to the test member (who has no DM for the fixture's topic).
func memberAsk(test *testing.T, fixture singleCallFixture, extra map[string]any) map[string]any {
	test.Helper()
	body := fixture.body()
	body["for"] = testUserID
	for key, value := range extra {
		body[key] = value
	}
	created := createAsk(test, fixture, body)
	if session, _ := created["chat_session_id"].(string); session != "" {
		test.Cleanup(func() {
			dbfx.Exec(test, `DELETE FROM decision_backing_lane WHERE session_id = $1`, session)
			dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, session)
			dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, session)
		})
	}
	return created
}

func registeredLanes(test *testing.T, session any) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_backing_lane WHERE session_id = $1`, session)
}

// Core red test: a member with no DM gets an ask.
func TestA6aNoDMMemberAskGoesToNewBackingLane(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	fixture := newSingleCallFixture(test)
	listBefore := memberSessionList(test)
	created := memberAsk(test, fixture, nil)
	session, _ := created["chat_session_id"].(string)
	if session == "" || registeredLanes(test, session) != 1 {
		test.Fatalf("chat_session_id %q is not a registered backing lane", session)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND human_user_id = $2 AND chat_session_id = $3 AND authority = 'human'`, created["id"], testUserID, session); got != 1 {
		test.Fatalf("decision_human_identity: request does not hold the member and the lane (%d rows)", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, session); got != 1 {
		test.Fatalf("lane holds %d ping messages, want 1", got)
	}
	if listAfter := memberSessionList(test); listAfter != listBefore {
		test.Fatalf("member session list changed:\nbefore %s\nafter  %s", listBefore, listAfter)
	}
	var unread int
	dbfx.QueryRow(test, `SELECT count(*) FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.creator_id = $1 AND m.role = 'assistant' AND m.created_at > s.last_read_at AND s.id = $2`, testUserID, session).Scan(&unread)
	listAfter := memberSessionList(test)
	if strings.Contains(listAfter, session) {
		test.Fatalf("member list shows lane %s with %d unread", session, unread)
	}
	if before, after := strings.Count(listBefore, `"has_unread":true`), strings.Count(listAfter, `"has_unread":true`); before != after {
		test.Fatalf("member unread count changed: %d before, %d after", before, after)
	}
}

func TestA6aSecondAskOnTopicReusesLane(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	fixture := newSingleCallFixture(test)
	first := memberAsk(test, fixture, nil)
	second := memberAsk(test, fixture, map[string]any{"ask": structuredDecisionAsk(fixture.issueID, fixture.sourceID, "A second question on the same topic?")})
	if first["chat_session_id"] != second["chat_session_id"] {
		test.Fatalf("second ask used lane %v, want %v", second["chat_session_id"], first["chat_session_id"])
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_backing_lane WHERE creator_id = $1 AND session_id = $2`, testUserID, first["chat_session_id"]); got != 1 {
		test.Fatalf("lane registered %d times, want 1", got)
	}
}

func TestA6aMemberWithVisibleDMGetsNoMessageThere(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	fixture := newSingleCallFixture(test)
	visible := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": testUserID, "title": "plain DM"})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": visible, "role": "user", "content": "see [issue](mention://issue/" + fixture.issueID + ")"})
	created := memberAsk(test, fixture, nil)
	if created["chat_session_id"] == visible || registeredLanes(test, created["chat_session_id"]) != 1 {
		test.Fatalf("ask landed in %v, want a backing lane other than the visible DM %s", created["chat_session_id"], visible)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, visible); got != 1 {
		test.Fatalf("visible DM holds %d messages, want only its own 1", got)
	}
}

func TestA6aIssueAgentAskIgnoresSessionAndNewDM(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	for name, extra := range map[string]func(fixture singleCallFixture) map[string]any{
		"session": func(fixture singleCallFixture) map[string]any {
			dm := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": testUserID, "title": "named DM"})
			dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": dm, "role": "user", "content": "see [issue](mention://issue/" + fixture.issueID + ")"})
			return map[string]any{"session_id": dm}
		},
		"new_dm": func(singleCallFixture) map[string]any { return map[string]any{"new_dm": true} },
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			body := fixture.body()
			body["for"] = testUserID
			for key, value := range extra(fixture) {
				body[key] = value
			}
			var created map[string]any
			response := testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
			response.JSON(&created)
			session, _ := created["chat_session_id"].(string)
			test.Cleanup(func() {
				dbfx.Exec(test, `DELETE FROM decision_backing_lane WHERE session_id = $1`, session)
				dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, session)
				dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, session)
			})
			if registeredLanes(test, session) != 1 || session == body["session_id"] {
				test.Fatalf("ask landed in %s, want a backing lane", session)
			}
			if !strings.Contains(response.Text(), "backing lane") {
				test.Fatalf("response carries no warning line about the ignored flag: %s", response.Text())
			}
		})
	}
}

func TestA6aDMAgentAskStillLandsInItsDM(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	fixture := newSingleCallFixture(test)
	dm := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": testUserID, "title": "agent DM"})
	dbfx.Exec(test, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, dm, fixture.taskID)
	body := fixture.body()
	body["for"] = testUserID
	created := createAsk(test, fixture, body)
	if created["chat_session_id"] != dm || registeredLanes(test, dm) != 0 {
		test.Fatalf("DM-agent ask landed in %v, want its DM %s", created["chat_session_id"], dm)
	}
}

func TestA6aFlagOffKeepsToday(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"off"}`)
	fixture := newSingleCallFixture(test)
	created := memberAsk(test, fixture, nil)
	session, _ := created["chat_session_id"].(string)
	if registeredLanes(test, session) != 0 {
		test.Fatalf("flag off registered a backing lane for %s", session)
	}
	if !strings.Contains(memberSessionList(test), session) {
		test.Fatalf("flag off: member list lacks the topic lane %s", session)
	}
}

func TestA6aLaneIsInvisibleToLaneQueries(test *testing.T) {
	setWorkspaceDecisionSettings(test, `{"decision_screen":"on"}`)
	fixture := newSingleCallFixture(test)
	created := memberAsk(test, fixture, nil)
	session, _ := created["chat_session_id"].(string)
	var topicID string
	dbfx.QueryRow(test, `SELECT topic_id::text FROM decision_backing_lane WHERE session_id = $1`, session).Scan(&topicID)
	ctx := test.Context()
	if _, err := testHandler.Queries.GetDecisionTopicLane(ctx, db.GetDecisionTopicLaneParams{WorkspaceID: parseUUID(testWorkspaceID), CreatorID: parseUUID(testUserID), TopicID: parseUUID(topicID)}); !errors.Is(err, pgx.ErrNoRows) {
		test.Fatalf("GetDecisionTopicLane returned the backing lane (err %v)", err)
	}
	lanes, err := testHandler.Queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: parseUUID(testWorkspaceID), AgentID: parseUUID(fixture.agentID), TopicID: parseUUID(topicID), CreatorID: parseUUID(testUserID), SessionID: parseUUID(session)})
	if err != nil || len(lanes) != 0 {
		test.Fatalf("ListDecisionLanes lists the backing lane: %d rows, err %v", len(lanes), err)
	}
	decisions, err := testHandler.Queries.ListChatAttentionDecisions(ctx, db.ListChatAttentionDecisionsParams{HumanUserID: parseUUID(testUserID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	for _, row := range decisions {
		if uuidToString(row.ID) == created["id"] {
			test.Fatalf("Needs-you decision rows include the backing-lane request %v", created["id"])
		}
	}
}

// A6a review round 1: an open-intent (two-step) ask from an issue task takes the same lane.
func TestA6aOpenIntentAskIgnoresSessionAndNewDM(test *testing.T) {
	for _, flag := range []string{"on", "off"} {
		for _, choice := range []string{"session_id", "new_dm"} {
			test.Run(flag+"/"+choice, func(test *testing.T) {
				setWorkspaceDecisionSettings(test, `{"decision_screen":"`+flag+`"}`)
				fixture := newRequestCoreFixture(test, false)
				body := map[string]any{"issue_id": fixture.issueID, "for": fixture.humanID, "ask": fixture.structuredAsk(), "escalation_class": "reversible"}
				if choice == "session_id" {
					body["session_id"] = fixture.sessionID
				} else {
					body["new_dm"] = true
				}
				request := newRequest(http.MethodPost, "/api/decisions", body)
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Agent-ID", fixture.agentID)
				request.Header.Set("X-Task-ID", fixture.taskID)
				var created map[string]any
				response := testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusCreated)
				response.JSON(&created)
				session, _ := created["chat_session_id"].(string)
				test.Cleanup(func() {
					dbfx.Exec(test, `DELETE FROM decision_backing_lane WHERE session_id = $1`, session)
					dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, session)
					dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1 AND id <> $2`, session, fixture.sessionID)
				})
				visibleDMPings := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID)
				if flag == "off" {
					if registeredLanes(test, session) != 0 || strings.Contains(response.Text(), "backing lane") {
						test.Fatalf("flag off: ask must keep today's path, got lane %s: %s", session, response.Text())
					}
					return
				}
				if registeredLanes(test, session) != 1 || session == fixture.sessionID || visibleDMPings != 0 {
					test.Fatalf("flag on: ask landed in %s (visible DM pings %d), want a new backing lane", session, visibleDMPings)
				}
				if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND human_user_id = $2 AND chat_session_id = $3`, created["id"], fixture.humanID, session) != 1 {
					test.Fatal("request does not hold the named person and the lane")
				}
				if !strings.Contains(response.Text(), "backing lane") {
					test.Fatalf("response carries no hint about the ignored flag: %s", response.Text())
				}
			})
		}
	}
}
