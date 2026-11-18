package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/decisiontx/ownerscripttest"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-7138 S7a: the owner-script guard on decision asks, action asks and
// `multica decision ask` (AskDecision). Every ask-surface fixture runs through
// each entry point in each mode; the guard event is one activity_log row.

type ownerEvent struct {
	ID      string
	Details map[string]any
}

func (event ownerEvent) text(key string) string {
	value, _ := event.Details[key].(string)
	return value
}

func (event ownerEvent) hits() []string {
	var hits []string
	for _, hit := range event.Details["rule_hits"].([]any) {
		hits = append(hits, hit.(string))
	}
	return hits
}

func setOwnerScriptGuard(t *testing.T, mode string) {
	t.Helper()
	if mode != "" {
		dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{owner_script_guard}', to_jsonb($1::text)) WHERE id = $2`, mode, testWorkspaceID)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'owner_script_guard' WHERE id = $1`, testWorkspaceID)
	})
}

func ownerEvents(t *testing.T, issueID string) []ownerEvent {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text, details FROM activity_log WHERE issue_id = $1 AND action = 'owner_script_guard' ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []ownerEvent
	for rows.Next() {
		var event ownerEvent
		var raw []byte
		if err := rows.Scan(&event.ID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &event.Details); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id = $1 AND action = 'owner_script_guard'`, issueID)
	})
	return events
}

// ownerAskEntry sends one ask whose person-facing text is body, through one entry point.
type ownerAskEntry struct {
	name     string
	surface  string
	cards    func(*testing.T, string) int
	send     func(*testing.T, string) (status int, issueID, agentID, humanID string)
	idOfCard func(*testing.T, string) string
}

func ownerAskEntries() []ownerAskEntry {
	cards := func(t *testing.T, issueID string) int {
		return dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID)
	}
	return []ownerAskEntry{
		{name: "decision", surface: "decision_ask", cards: cards, send: func(t *testing.T, body string) (int, string, string, string) {
			fixture := newRequestCoreFixture(t, false)
			ask := fixture.structuredAsk()
			ask.AgentDid = body
			request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "spend"})
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			return testutil.Call(t, testHandler.RequestDecision, request).Code, fixture.issueID, fixture.agentID, fixture.humanID
		}},
		{name: "action", surface: "action_ask", cards: cards, send: func(t *testing.T, body string) (int, string, string, string) {
			enableActionRequests(t, true)
			fixture := newRequestCoreFixture(t, false)
			payload := fixture.actionBody()
			ask := payload["ask"].(map[string]any)
			ask["agent_did"] = body
			ask["steps"] = []string{"Open Settings, Secrets, New secret; add the token you created."}
			return testutil.Call(t, testHandler.RequestDecision, fixture.postAction(payload)).Code, fixture.issueID, fixture.agentID, fixture.humanID
		}},
		{name: "decision ask", surface: "decision_ask", cards: cards, send: func(t *testing.T, body string) (int, string, string, string) {
			fixture := newAskFixture(t)
			fixture.details.AgentDid = body
			payload := fixture.askBody()
			return testutil.Call(t, testHandler.AskDecision, fixture.askCall(payload)).Code, fixture.issueID, fixture.agentID, fixture.humanID
		}},
	}
}

// askCount keeps repeated asks on one fixture distinct, so none is a replay of another.
var askCount int

// sendAsk sends one single-call ask through AskDecision; the fixture's window is reusable.
func sendAsk(t *testing.T, fixture singleCallFixture, text string) int {
	t.Helper()
	askCount++
	fixture.details.AgentDid = text
	fixture.details.Decision = fmt.Sprintf("Approve the reversible change, number %d?", askCount)
	return testutil.Call(t, testHandler.AskDecision, fixture.askCall(fixture.askBody())).Code
}

func askSurfaceFixtures() []ownerscripttest.Fixture {
	var fixtures []ownerscripttest.Fixture
	for _, fixture := range ownerscripttest.All {
		if fixture.Surface == "ask" {
			fixtures = append(fixtures, fixture)
		}
	}
	return fixtures
}

func TestOwnerScriptGuardAskKindsAndFixtures(test *testing.T) {
	for _, entry := range ownerAskEntries() {
		for _, fixture := range askSurfaceFixtures() {
			if fixture.ResendOf != "" {
				continue // F-2 is a sequence; TestOwnerScriptGuardResendKeepsCommandHash
			}
			for _, mode := range []string{"off", "warn", "block", ""} {
				test.Run(entry.name+"/"+fixture.ID+"/mode="+mode, func(test *testing.T) {
					setOwnerScriptGuard(test, mode)
					status, issueID, agentID, humanID := entry.send(test, fixture.Text)
					events := ownerEvents(test, issueID)
					effective := mode
					if effective == "" {
						effective = "warn" // the default
					}
					hit := len(fixture.Hits) > 0
					switch {
					case effective == "off":
						if status != http.StatusCreated || len(events) != 0 {
							test.Fatalf("off: status %d events %d, want 201 and none", status, len(events))
						}
						return
					case effective == "block" && hit:
						if status != http.StatusUnprocessableEntity || entry.cards(test, issueID) != 0 {
							test.Fatalf("block: status %d cards %d, want 422 and none", status, entry.cards(test, issueID))
						}
					default:
						if status != http.StatusCreated || entry.cards(test, issueID) != 1 {
							test.Fatalf("%s: status %d cards %d, want 201 and one", effective, status, entry.cards(test, issueID))
						}
					}
					if len(events) != 1 {
						test.Fatalf("events = %d, want exactly one per check", len(events))
					}
					event := events[0]
					wantOutcome, delivered := "accepted", true
					switch {
					case effective == "block" && hit:
						wantOutcome, delivered = "refused", false
					case hit:
						wantOutcome = "warned"
					}
					if event.text("outcome") != wantOutcome || event.Details["delivered"] != delivered {
						test.Fatalf("outcome %v delivered %v, want %s %v", event.Details["outcome"], event.Details["delivered"], wantOutcome, delivered)
					}
					if got := event.hits(); strings.Join(got, ",") != strings.Join(fixture.Hits, ",") {
						test.Fatalf("rule_hits = %v, want %v", got, fixture.Hits)
					}
					if event.Details["rule_version"] != float64(decisiontx.OwnerScriptRuleVersion) || event.text("surface") != entry.surface || event.text("mode") != effective {
						test.Fatalf("event = %v", event.Details)
					}
					if event.text("author_agent_id") != agentID || event.text("recipient_member_id") != humanID {
						test.Fatalf("event ids = %v, want agent %s member %s", event.Details, agentID, humanID)
					}
					wantHashes := 0
					if strings.Contains(fixture.Text, ownerscripttest.Command) {
						wantHashes = 1
					}
					if hashes, _ := event.Details["cmd_hmacs"].([]any); len(hashes) != wantHashes {
						test.Fatalf("cmd_hmacs = %v, want %d", event.Details["cmd_hmacs"], wantHashes)
					}
					if delivered && event.text("request_id") == "" || !delivered && event.text("request_id") != "" {
						test.Fatalf("request_id = %q for delivered=%v", event.text("request_id"), delivered)
					}
					raw, _ := json.Marshal(event.Details)
					for _, secret := range []string{"systemctl", "connection refused", "Details are in", "retrying"} {
						if strings.Contains(string(raw), secret) {
							test.Fatalf("event holds message text %q: %s", secret, raw)
						}
					}
				})
			}
		}
	}
}

// F-2: the refused command re-sent in another fence is a retry of the refusal.
func TestOwnerScriptGuardResendKeepsCommandHash(test *testing.T) {
	first, second := ownerscripttest.ByID("F-1"), ownerscripttest.ByID("F-2")
	for _, mode := range []string{"block", "warn"} {
		test.Run(mode, func(test *testing.T) {
			setOwnerScriptGuard(test, "block")
			fixture := newAskFixture(test)
			send := func(text string) int { return sendAsk(test, fixture, text) }
			if status := send(first.Text); status != http.StatusUnprocessableEntity {
				test.Fatalf("F-1 status %d, want 422", status)
			}
			dbfx.Exec(test, `UPDATE workspace SET settings = jsonb_set(settings, '{owner_script_guard}', to_jsonb($1::text)) WHERE id = $2`, mode, testWorkspaceID)
			want := http.StatusUnprocessableEntity
			if mode == "warn" {
				want = http.StatusCreated
			}
			if status := send(second.Text); status != want {
				test.Fatalf("F-2 status %d, want %d", status, want)
			}
			events := ownerEvents(test, fixture.issueID)
			if len(events) != 2 {
				test.Fatalf("events = %d, want 2", len(events))
			}
			firstHashes, secondHashes := fmt.Sprint(events[0].Details["cmd_hmacs"]), fmt.Sprint(events[1].Details["cmd_hmacs"])
			if firstHashes != secondHashes || firstHashes == "[]" {
				test.Fatalf("hashes differ: %s vs %s", firstHashes, secondHashes)
			}
			if events[0].Details["retry_of"] != nil || events[1].text("retry_of") != events[0].ID {
				test.Fatalf("retry_of = %v / %v, want nil then %s", events[0].Details["retry_of"], events[1].Details["retry_of"], events[0].ID)
			}
		})
	}
}

// F-8: a run cue in a message that follows a flagged message from the same
// author within 30 minutes (or links to one) is R8, and an old one is not.
func TestOwnerScriptGuardRunCueAfterFlaggedMessage(test *testing.T) {
	setOwnerScriptGuard(test, "warn")
	fixture := newAskFixture(test)
	send := func(text string) int { return sendAsk(test, fixture, text) }
	cue := ownerscripttest.ByID("F-8").Text
	if status := send(cue); status != http.StatusCreated {
		test.Fatalf("cue alone: status %d", status)
	}
	if status := send(ownerscripttest.ByID("F-1").Text); status != http.StatusCreated {
		test.Fatalf("F-1: status %d", status)
	}
	if status := send(cue); status != http.StatusCreated {
		test.Fatalf("cue after F-1: status %d", status)
	}
	events := ownerEvents(test, fixture.issueID)
	if len(events) != 3 {
		test.Fatalf("events = %d, want 3", len(events))
	}
	if got := events[0].hits(); len(got) != 0 {
		test.Fatalf("a run cue alone hit %v", got)
	}
	if got := events[2].hits(); strings.Join(got, ",") != "R8" {
		test.Fatalf("cue after F-1 hit %v, want R8", got)
	}
	// Older than 30 minutes: not related, unless the message links to it.
	dbfx.Exec(test, `UPDATE activity_log SET created_at = now() - interval '2 hours' WHERE issue_id = $1 AND action = 'owner_script_guard'`, fixture.issueID)
	if status := send(cue); status != http.StatusCreated {
		test.Fatalf("old context: status %d", status)
	}
	events = ownerEvents(test, fixture.issueID)
	if got := events[len(events)-1].hits(); len(got) != 0 {
		test.Fatalf("a flagged message 2 h ago made the cue hit %v", got)
	}
	// The aged rows share one created_at, so find the flagged event by its hits.
	var flaggedComment string
	for _, event := range events {
		if strings.Join(event.hits(), ",") == "R6,R7" {
			flaggedComment = event.text("comment_id")
		}
	}
	if flaggedComment == "" {
		test.Fatalf("the flagged delivered event holds no comment_id: %v", events)
	}
	link := "please run the commands in [the ask](https://multica.example/issues/CHE-1#comment-" + flaggedComment + ")"
	if status := send(link); status != http.StatusCreated {
		test.Fatalf("linked: status %d", status)
	}
	events = ownerEvents(test, fixture.issueID)
	if got := events[len(events)-1].hits(); strings.Join(got, ",") != "R8" {
		test.Fatalf("cue linking to a flagged comment hit %v, want R8", got)
	}
}

// The event is written in the message's transaction: if the commit fails, the
// message, the request and the event all vanish together.
func TestOwnerScriptGuardEventCommitsWithMessage(test *testing.T) {
	setOwnerScriptGuard(test, "warn")
	fixture := newAskFixture(test)
	if status := sendAsk(test, fixture, ownerscripttest.ByID("F-3").Text); status != http.StatusCreated {
		test.Fatalf("status %d", status)
	}
	events := ownerEvents(test, fixture.issueID)
	if len(events) != 1 {
		test.Fatalf("events = %d", len(events))
	}
	if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE id = $1::uuid`, events[0].text("message_id")) != 1 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1::uuid AND ping_message_id = $2::uuid`, events[0].text("request_id"), events[0].text("message_id")) != 1 {
		test.Fatalf("event %v does not name the committed message and request", events[0].Details)
	}

	// Fail the commit of the next ask on its guard event alone: the message,
	// the request and the event must all vanish.
	other := newAskFixture(test)
	dbfx.Exec(test, `CREATE FUNCTION che7176_refuse_event() RETURNS trigger AS $$ BEGIN
		IF NEW.action = 'owner_script_guard' AND NEW.issue_id = '`+other.issueID+`' THEN RAISE EXCEPTION 'che7176 forced commit failure'; END IF; RETURN NULL; END $$ LANGUAGE plpgsql`)
	dbfx.Exec(test, `CREATE CONSTRAINT TRIGGER che7176_refuse_event AFTER INSERT ON activity_log DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION che7176_refuse_event()`)
	test.Cleanup(func() {
		testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS che7176_refuse_event ON activity_log`)
		testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS che7176_refuse_event()`)
	})
	if status := sendAsk(test, other, ownerscripttest.ByID("F-3").Text); status < 500 {
		test.Fatalf("status %d, want the forced commit failure", status)
	}
	if left := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, other.issueID); left != 0 {
		test.Fatalf("requests after a failed commit = %d, want 0", left)
	}
	if comments, messages, _ := other.counts(test); comments != 0 || messages != 0 {
		test.Fatalf("after a failed commit: comments %d messages %d, want 0", comments, messages)
	}
	if left := len(ownerEvents(test, other.issueID)); left != 0 {
		test.Fatalf("events after a failed commit = %d, want 0", left)
	}
}

// The guard's audit rows are not issue history: the timeline skips them and
// keeps every other activity row.
func TestOwnerScriptGuardEventIsNotInTheIssueTimeline(test *testing.T) {
	activities := []db.ActivityLog{{Action: ownerScriptAction, Details: []byte(`{"rule_hits":["R6"]}`)}, {Action: "status_changed", Details: []byte(`{}`)}}
	entries := testHandler.mergeTimeline(newRequest(http.MethodGet, "/api/issues/x/timeline", nil), nil, activities, true)
	if len(entries) != 1 || entries[0].Action == nil || *entries[0].Action != "status_changed" {
		test.Fatalf("timeline has %d entries, want only status_changed", len(entries))
	}
}
