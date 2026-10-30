package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// askFor posts one structured ask through the HTTP handler. With singleCall the
// open intent is closed first and the person is named, the one-call path.
func (fixture requestCoreFixture) askFor(test *testing.T, ask decisiontx.Ask, singleCall bool) *http.Request {
	test.Helper()
	body := map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": ask}
	if singleCall {
		body["for"] = fixture.humanID
	}
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return request
}

func (fixture requestCoreFixture) prepare(test *testing.T, singleCall bool) {
	test.Helper()
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	if singleCall {
		dbfx.Exec(test, `UPDATE decision_intent SET closed_at = clock_timestamp(), close_reason = 'superseded', sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
	}
}

func (fixture requestCoreFixture) comment(test *testing.T, workspaceID, issueID, content string) string {
	test.Helper()
	return dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": workspaceID, "issue_id": issueID, "author_type": "member", "author_id": fixture.humanID, "content": content})
}

func commentLink(text, issueID, commentID string) string {
	return "[" + text + "](/" + handlerTestWorkspaceSlug + "/issues/" + issueID + "#comment-" + commentID + ")"
}

func askWithContext(fixture requestCoreFixture, context string) decisiontx.Ask {
	ask := fixture.structuredAsk()
	ask.ContextELI10 = context
	return ask
}

// Test 7: the stored ask names the asking agent inside Details, never its UUID.
func TestDecisionAskNamesTheAskingAgent(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, false)
	var created db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated).JSON(&created)
	askedBy := "**Asked by** " + fixture.agentName
	details, did := strings.Index(created.Ask, "<summary>Details</summary>"), strings.Index(created.Ask, "**Agent did**")
	line := strings.Index(created.Ask, askedBy+"\n\n")
	if details < 0 || line < details || line > did || strings.Contains(created.Ask, fixture.agentID) {
		test.Fatalf("ask does not carry %q first inside Details:\n%s", askedBy, created.Ask)
	}
	var comment string
	dbfx.QueryRow(test, `SELECT content FROM comment WHERE id = $1`, created.IssueCommentID).Scan(&comment)
	if !strings.Contains(comment, askedBy) {
		test.Fatalf("ask comment lacks the asked-by line:\n%s", comment)
	}
	var pingID pgtype.UUID
	dbfx.QueryRow(test, `SELECT ping_message_id FROM decision_request WHERE id = $1`, created.ID).Scan(&pingID)
	cards := testHandler.decisionRequestsForMessages(context.Background(), testWorkspaceID, []db.ChatMessage{{ID: pingID}})
	if card := cards[uuidToString(pingID)]; len(card) != 1 || card[0].AskingAgentName != fixture.agentName {
		test.Fatalf("card lacks the asking agent name: %+v", card)
	}
}

// Test 8: a same-workspace comment link gets a one-line excerpt; a deleted or
// foreign comment gets none.
func TestDecisionAskCommentExcerpt(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, false)
	workspace := fixture.issue.WorkspaceID
	live := fixture.comment(test, uuidToString(workspace), fixture.issueID, "\n  **Ranked list** of [@Cheese](mention://member/"+fixture.humanID+") picks\nsecond line")
	deleted := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "content": "Deleted text", "deleted_at": testutil.Raw("clock_timestamp()")})
	otherWorkspace := dbfx.Workspace(test, "Excerpt other workspace", "excerpt-other-"+fixture.issueID)
	foreign := fixture.comment(test, otherWorkspace, fixture.issueID, "Foreign text")
	long := fixture.comment(test, uuidToString(workspace), fixture.issueID, strings.Repeat("x", 150))
	context := strings.Join([]string{commentLink("the list", fixture.issueID, live), commentLink("gone", fixture.issueID, deleted), commentLink("other", fixture.issueID, foreign), commentLink("long", fixture.issueID, long)}, " ")
	var created db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, askWithContext(fixture, context), false)).Want(http.StatusCreated).JSON(&created)
	wants := []string{
		commentLink("the list", fixture.issueID, live) + " (“Ranked list of @Cheese picks”)",
		commentLink("gone", fixture.issueID, deleted) + " ",
		commentLink("other", fixture.issueID, foreign) + " ",
		commentLink("long", fixture.issueID, long) + " (“" + strings.Repeat("x", 100) + "…”)",
	}
	for _, want := range wants {
		if !strings.Contains(created.Ask, want) {
			test.Fatalf("ask lacks %q:\n%s", want, created.Ask)
		}
	}
	if strings.Contains(created.Ask, "Deleted text") || strings.Contains(created.Ask, "Foreign text") {
		test.Fatalf("ask leaked an excluded comment:\n%s", created.Ask)
	}
}

// Test 10 (regression guard for the replay fingerprint): a retry after the
// agent is renamed, the linked comment is edited or deleted, replays the stored
// request and creates nothing new.
func TestDecisionRetryReplaysDespiteLiveChanges(test *testing.T) {
	for _, singleCall := range []bool{false, true} {
		for _, change := range []string{"rename", "edit", "delete"} {
			test.Run(map[bool]string{false: "intent", true: "single_call"}[singleCall]+"/"+change, func(test *testing.T) {
				fixture := newRequestCoreFixture(test, false)
				fixture.prepare(test, singleCall)
				linked := fixture.comment(test, testWorkspaceID, fixture.issueID, "Original excerpt line")
				ask := askWithContext(fixture, "Read "+commentLink("the notes", fixture.issueID, linked)+" first.")
				var first, replay db.DecisionRequest
				testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, singleCall)).Want(http.StatusCreated).JSON(&first)
				switch change {
				case "rename":
					dbfx.Exec(test, `UPDATE agent SET name = name || ' renamed' WHERE id = $1`, fixture.agentID)
				case "edit":
					dbfx.Exec(test, `UPDATE comment SET content = 'Edited excerpt line' WHERE id = $1`, linked)
				case "delete":
					dbfx.Exec(test, `UPDATE comment SET deleted_at = clock_timestamp() WHERE id = $1`, linked)
				}
				testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, singleCall)).Want(http.StatusOK).JSON(&replay)
				if replay.ID != first.ID || replay.Ask != first.Ask || replay.SourceContentSnapshot != first.SourceContentSnapshot || replay.SourceCommentID != first.SourceCommentID {
					test.Fatalf("retry changed the stored request:\nfirst=%+v\nreplay=%+v", first, replay)
				}
				fixture.assertCounts(test, 1)
				if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != 1 {
					test.Fatalf("retry left %d inbox pings", count)
				}
			})
		}
	}
}

// Test 11: a new request after the rename shows the new name, so enrichment
// runs for a new request only. Single-call: the first request used the intent.
func TestDecisionNewRequestAfterRenameShowsNewName(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, true)
	var first, second db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, fixture.structuredAsk(), true)).Want(http.StatusCreated).JSON(&first)
	oldName := fixture.agentName
	dbfx.Exec(test, `UPDATE agent SET name = name || ' renamed' WHERE id = $1`, fixture.agentID)
	other := fixture.structuredAsk()
	other.Decision = "Ship the other change now?"
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, other, true)).Want(http.StatusCreated).JSON(&second)
	if !strings.Contains(first.Ask, "**Asked by** "+oldName+"\n") || !strings.Contains(second.Ask, "**Asked by** "+oldName+" renamed\n") {
		test.Fatalf("names: first=%q second=%q", first.Ask, second.Ask)
	}
}

// Test 12 (regression guard): concurrent identical retries with a linked
// comment create one request and one ping, on both paths.
func TestDecisionConcurrentRetryWithLinkedComment(test *testing.T) {
	for _, singleCall := range []bool{false, true} {
		test.Run(map[bool]string{false: "intent", true: "single_call"}[singleCall], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			fixture.prepare(test, singleCall)
			linked := fixture.comment(test, testWorkspaceID, fixture.issueID, "Linked excerpt")
			ask := askWithContext(fixture, "Read "+commentLink("the notes", fixture.issueID, linked)+" first.")
			statuses := make(chan int, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for range 2 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					recorder := httptest.NewRecorder()
					testHandler.RequestDecision(recorder, fixture.askFor(test, ask, singleCall))
					statuses <- recorder.Code
				}()
			}
			close(start)
			workers.Wait()
			close(statuses)
			created, replayed := 0, 0
			for status := range statuses {
				switch status {
				case http.StatusCreated:
					created++
				case http.StatusOK:
					replayed++
				default:
					test.Fatalf("unexpected status %d", status)
				}
			}
			if created != 1 || replayed != 1 {
				test.Fatalf("created=%d replayed=%d", created, replayed)
			}
			fixture.assertCounts(test, 1)
			if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != 1 {
				test.Fatalf("%d inbox pings", count)
			}
		})
	}
}

// legacyFingerprint is the pre-D1d formula, written out independently of the
// handler: SHA-256 of the JSON of the old input struct (the D1b head's, with
// Options), Fingerprint empty, Ask the un-enriched rendering.
func legacyFingerprint(input decisionRequestInput) string {
	encoded, err := json.Marshal(struct {
		SessionID       pgtype.UUID
		NewDM           bool
		HumanUserID     pgtype.UUID
		Ask             string
		Footer          string
		ParentID        pgtype.UUID
		EscalationClass string
		SourceCommentID pgtype.UUID
		Fingerprint     string
		Options         []decisiontx.AskOption
	}{input.SessionID, input.NewDM, input.HumanUserID, input.Ask, input.Footer, input.ParentID, input.EscalationClass, input.SourceCommentID, "", input.Options})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// Test 13 (regression guard): a row stored with the pre-D1d fingerprint is
// still replayed by a retry that crosses the deploy.
func TestDecisionRetryReplaysLegacyFingerprintRow(test *testing.T) {
	for _, singleCall := range []bool{false, true} {
		test.Run(map[bool]string{false: "intent", true: "single_call"}[singleCall], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			fixture.prepare(test, singleCall)
			ask := fixture.structuredAsk()
			var first, replay db.DecisionRequest
			testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, singleCall)).Want(http.StatusCreated).JSON(&first)
			bare := ask
			bare.AskedBy = "" // D1c rendered no asking agent
			legacy := decisionRequestInput{SessionID: parseUUID(fixture.sessionID), Ask: bare.Content(handlerTestWorkspaceSlug), Options: bare.Options, EscalationClass: "reversible", SourceCommentID: parseUUID(fixture.tagID)}
			if singleCall {
				legacy.HumanUserID = parseUUID(fixture.humanID)
			}
			dbfx.Exec(test, `UPDATE decision_request SET request_fingerprint = $1 WHERE id = $2`, legacyFingerprint(legacy), first.ID)
			testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, singleCall)).Want(http.StatusOK).JSON(&replay)
			if replay.ID != first.ID {
				test.Fatalf("legacy-fingerprint row was not replayed: %+v", replay)
			}
			fixture.assertCounts(test, 1)
		})
	}
}

// The legacy formula is pinned to the bytes the D1b head (with Options) produced, so a later
// change to decisionRequestInput cannot silently orphan stored rows.
func TestLegacyFingerprintIsPinned(test *testing.T) {
	input := decisionRequestInput{
		SessionID: parseUUID("11111111-1111-4111-8111-111111111111"), HumanUserID: parseUUID("22222222-2222-4222-8222-222222222222"),
		Ask: "ask", Footer: "footer", EscalationClass: "reversible", SourceCommentID: parseUUID("33333333-3333-4333-8333-333333333333"),
	}
	const pinned = "dcc7cb96ec041516e91e193f362abc8898033b4f817a2c1af644aff60074f24a"
	if got := legacyFingerprint(input); got != pinned {
		test.Fatalf("test formula drifted: %s", got)
	}
	if _, got, err := (decisionRequestInput{SessionID: input.SessionID, HumanUserID: input.HumanUserID, Ask: input.Ask, Footer: input.Footer, EscalationClass: input.EscalationClass, SourceCommentID: input.SourceCommentID, Parsed: &decisiontx.Ask{}}).fingerprints(); err != nil || got != pinned {
		test.Fatalf("handler legacy key = %s (%v), want the pinned bytes", got, err)
	}
}

// A caller's own curly-quoted text is not an excerpt: two different decisions
// that differ only inside it are two requests, not a re-ask.
func TestDecisionDistinctQuotedDecisionIsNotBlocked(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, true)
	ask := fixture.structuredAsk()
	ask.Decision = "Deploy (“canary”)?"
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusCreated)
	ask.Decision = "Deploy (“production”)?"
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusCreated)
}

// A two-step ask that matches the intent comment reuses that comment; the
// reused comment must carry the same enriched snapshot as the request.
func TestDecisionReusedIntentCommentIsEnriched(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, false)
	ask := fixture.structuredAsk()
	ask.AskedBy = ""
	dbfx.Exec(test, `UPDATE comment SET content = $1 WHERE id = $2`, ask.Content(handlerTestWorkspaceSlug), fixture.tagID)
	var created db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, false)).Want(http.StatusCreated).JSON(&created)
	var content string
	dbfx.QueryRow(test, `SELECT content FROM comment WHERE id = $1`, created.IssueCommentID).Scan(&content)
	if created.IssueCommentID != parseUUID(fixture.tagID) || content != created.Ask {
		test.Fatalf("reused=%v comment differs from the request ask:\n%s", created.IssueCommentID == parseUUID(fixture.tagID), content)
	}
}

// A later task repeating the same unanswered ask is refused even when the ask
// links a comment and carries the caller's own quote after the link.
func TestDecisionReaskWithLinkedCommentAndCallerQuote(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, true)
	linked := fixture.comment(test, testWorkspaceID, fixture.issueID, "Linked excerpt")
	ask := askWithContext(fixture, "Read "+commentLink("the notes", fixture.issueID, linked)+" (“mine”) first.")
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusCreated)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	fixture.taskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusConflict)
}

// A later task repeats the same ask. The earlier request keeps one link with an
// excerpt and one link to an empty comment, and the caller's own quote after
// the second link. The caller's identity decides, not the stored text.
func TestDecisionReaskMixedExcerptLinks(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, true)
	first := fixture.comment(test, testWorkspaceID, fixture.issueID, "Visible excerpt")
	second := fixture.comment(test, testWorkspaceID, fixture.issueID, "")
	ask := askWithContext(fixture, "Read "+commentLink("first", fixture.issueID, first)+" and "+commentLink("second", fixture.issueID, second)+" (“mine”) first.")
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusCreated)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	fixture.taskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusConflict)
}

// Dropping a caller quote after an empty-comment link makes a different ask.
func TestDecisionCallerQuoteOmissionIsADifferentAsk(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, true)
	linked := fixture.comment(test, testWorkspaceID, fixture.issueID, "")
	ask := askWithContext(fixture, "Read "+commentLink("notes", fixture.issueID, linked)+" (“canary”) first.")
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusCreated)
	ask.ContextELI10 = "Read " + commentLink("notes", fixture.issueID, linked) + " first."
	testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, true)).Want(http.StatusCreated)
}

// A caller suffix after a source-comment link on the lane path still counts as a re-ask.
func TestDecisionLaneReaskWithCallerSuffix(test *testing.T) {
	fixture := newSingleCallFixture(test)
	lane := fixture.newDMLane(test, "discussed here")
	fixture.discussIn(test, lane)
	body := fixture.body()
	body["session_id"] = lane
	ask := fixture.details
	ask.Decision = "Deploy " + commentLink("c", fixture.issueID, fixture.sourceID) + " (“canary”)?"
	body["ask"] = ask
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	fixture.taskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusConflict)
}

// The enriched ask can pass the length limit the caller's ask met: refuse and keep nothing.
func TestDecisionEnrichedLengthRefusalRollsBack(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	fixture.prepare(test, false)
	ask := fixture.structuredAsk()
	ask.AskedBy = ""
	ask.ContextELI10 += strings.Repeat("x", maxCommentContentBytes-len(ask.Content(handlerTestWorkspaceSlug)))
	response := testutil.Call(test, testHandler.RequestDecision, fixture.askFor(test, ask, false)).Want(http.StatusBadRequest)
	if !strings.Contains(response.Text(), "ask is too long") {
		test.Fatalf("unexpected refusal: %s", response.Text())
	}
	fixture.assertCounts(test, 0)
}
