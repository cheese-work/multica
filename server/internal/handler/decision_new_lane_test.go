package handler

import (
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestDecisionRequestExplicitNewLane(test *testing.T) {
	for _, scenario := range []string{"allowed", "expired", "bad_tag"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
			createdEvents := 0
			testHandler.Bus.Subscribe(protocol.EventChatSessionCreated, func(event events.Event) {
				if event.ActorID == fixture.agentID {
					createdEvents++
				}
			})
			if scenario == "expired" {
				dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, fixture.intentID)
			}
			if scenario == "bad_tag" {
				dbfx.Exec(test, `UPDATE comment SET author_id = $1 WHERE id = $2`, testUserID, fixture.tagID)
			}
			request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "new_dm": true, "for": fixture.humanID, "ask": fixture.structuredAsk(), "escalation_class": "reversible"})
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			want := http.StatusCreated
			if scenario != "allowed" {
				want = http.StatusForbidden
			}
			var response map[string]any
			testutil.Call(test, testHandler.RequestDecision, request).Want(want).JSON(&response)
			count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1 AND title LIKE 'Decisions:%' AND explicitly_created_at IS NOT NULL`, fixture.agentID)
			if scenario != "allowed" && count != 0 || scenario == "allowed" && count != 1 {
				test.Fatalf("new-DM count=%d scenario=%s", count, scenario)
			}
			if int(count) != createdEvents {
				test.Fatalf("new-DM events=%d committed sessions=%d", createdEvents, count)
			}
			if scenario == "allowed" && response["chat_session_id"] == fixture.sessionID {
				test.Fatal("explicit new-DM request reused the most recently active DM")
			}
		})
	}
}

func TestDecisionRequestConcurrentNewDM(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	input := decisionRequestInput{NewDM: true, HumanUserID: parseUUID(fixture.humanID), Ask: "Create explicitly", EscalationClass: "reversible"}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			requireRequestRefusal(test, err)
		}
	}
	lanes := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1 AND title LIKE 'Decisions:%'`, fixture.agentID)
	if winners != 1 || lanes != 1 {
		test.Fatalf("concurrent new-DM winners=%d lanes=%d", winners, lanes)
	}
	fixture.assertCounts(test, 1)
}

func TestDecisionRequestLaneChoiceRefusals(test *testing.T) {
	for _, scenario := range []string{"implicit", "both", "new_without_person", "archived", "unnamed_person"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			body := map[string]any{"issue_id": fixture.issueID, "ask": fixture.structuredAsk(), "escalation_class": "reversible"}
			want := http.StatusBadRequest
			switch scenario {
			case "both":
				body["session_id"], body["new_dm"], body["for"] = fixture.sessionID, true, fixture.humanID
			case "new_without_person":
				body["new_dm"] = true
			case "unnamed_person":
				body["new_dm"], body["for"] = true, testUserID
				want = http.StatusForbidden
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
				body["session_id"] = fixture.sessionID
				want = http.StatusForbidden
			}
			request := newRequest(http.MethodPost, "/api/decisions", body)
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			testutil.Call(test, testHandler.RequestDecision, request).Want(want)
			fixture.assertCounts(test, 0)
		})
	}
}
