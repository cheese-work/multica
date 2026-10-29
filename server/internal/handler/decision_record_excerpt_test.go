package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const multiDecisionMessage = "> D1: OK to add the excerpt scope\n\nyes\n\n> D2: Each week, what will you decide\n\nA\n\n---\n\nCheck the workspace for an existing mechanism."

// excerptRecord records the fixture's message as the answer after replacing
// its text, with an offered-options context message when contextText is set.
func excerptRecord(test *testing.T, source, contextText string, excerpts []string) (askedRecordFixture, decisionRecordResult, error) {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE chat_message SET content = $1 WHERE id = $2`, source, fixture.messageID)
	input := fixture.recordInput()
	input.Excerpts = excerpts
	if contextText != "" {
		contextID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "assistant", "content": contextText, "created_at": "2020-01-01T00:00:00Z"})
		input.ContextMessageID = parseUUID(contextID)
	}
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	return fixture, result, err
}

func TestDecisionRecordExcerptQuotesOnlyTheExcerpt(test *testing.T) {
	_, result, err := excerptRecord(test, multiDecisionMessage, "", []string{"yes"})
	if err != nil {
		test.Fatal(err)
	}
	content := result.Comment.Content
	if !strings.HasSuffix(content, "\n\n> yes") || strings.Contains(content, "D2") || strings.Contains(content, "weekly") || strings.Contains(content, "workspace") {
		test.Fatalf("record quotes more than the excerpt: %q", content)
	}
	if result.Answer.ContentSnapshot != multiDecisionMessage || !result.Answer.Excerpted || result.Answer.ReplyText.String != "yes" {
		test.Fatalf("answer keeps the whole message and flags the excerpt: excerpted=%v reply=%q", result.Answer.Excerpted, result.Answer.ReplyText.String)
	}
}

func TestDecisionRecordExcerptMatchesAfterWhitespaceNormalisationAndQuotesTheSourceSpan(test *testing.T) {
	_, result, err := excerptRecord(test, "intro\n\nShip   it\n  on Friday.\n\noutro", "", []string{"Ship it on   Friday."})
	if err != nil {
		test.Fatal(err)
	}
	if !strings.HasSuffix(result.Comment.Content, "\n\n> Ship   it\n>   on Friday.") || strings.Contains(result.Comment.Content, "intro") {
		test.Fatalf("record does not quote the source span verbatim: %q", result.Comment.Content)
	}
}

func TestDecisionRecordExcerptRefusesWordsTheSourceDoesNotHold(test *testing.T) {
	for name, excerpts := range map[string][]string{
		"reworded":   {"yes please"},
		"one bad":    {"yes", "B"},
		"empty":      {""},
		"whitespace": {" \n "},
	} {
		test.Run(name, func(test *testing.T) {
			fixture, result, err := excerptRecord(test, multiDecisionMessage, "", excerpts)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest || result.Answer.ID.Valid {
				test.Fatalf("err=%v answer=%v, want a 400 and no answer", err, result.Answer.ID.Valid)
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
}

func TestDecisionRecordTwoExcerptsAreTwoQuoteBlocksOfOneRecord(test *testing.T) {
	fixture, result, err := excerptRecord(test, multiDecisionMessage, "", []string{"yes", "A"})
	if err != nil {
		test.Fatal(err)
	}
	fixture.assertRecordCounts(test, 1)
	if !strings.HasSuffix(result.Comment.Content, "> yes\n>\n> A") || result.Answer.ReplyText.String != "yes\n\nA" {
		test.Fatalf("two excerpts: comment=%q reply=%q", result.Comment.Content, result.Answer.ReplyText.String)
	}
}

func TestDecisionRecordExcerptNamesTheOfferedOptionItAnswers(test *testing.T) {
	offered := "Decisions:\nD1: OK to add the excerpt scope\nD2: Each week, what will you decide"
	for name, wanted := range map[string]struct {
		excerpt []string
		label   string
	}{
		"first":  {[]string{"yes"}, "D1"},
		"second": {[]string{"A"}, "D2"},
	} {
		test.Run(name, func(test *testing.T) {
			fixture, result, err := excerptRecord(test, multiDecisionMessage, offered, wanted.excerpt)
			if err != nil {
				test.Fatal(err)
			}
			if strings.Contains(result.Comment.Content, "Decisions:") || strings.Contains(result.Comment.Content, "OK to add") {
				test.Fatalf("record pastes the offered block: %q", result.Comment.Content)
			}
			fields := testHandler.decisionCommentFields(test.Context(), fixture.issue.WorkspaceID, []db.Comment{result.Comment}, pgtype.UUID{})[uuidToString(result.Comment.ID)]
			if fields.DecisionAnsweredLabel != wanted.label || fields.DecisionFullMessage != multiDecisionMessage {
				test.Fatalf("fields label=%q full=%q, want %q and the whole message", fields.DecisionAnsweredLabel, fields.DecisionFullMessage, wanted.label)
			}
		})
	}
}

func TestDecisionRecordWithoutExcerptKeepsWholeMessageAndNoDisclosure(test *testing.T) {
	fixture, result, err := excerptRecord(test, multiDecisionMessage, "", nil)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(result.Comment.Content, "> D2: Each week") || result.Answer.Excerpted {
		test.Fatalf("no excerpt must keep today's whole-message record: %q", result.Comment.Content)
	}
	fields := testHandler.decisionCommentFields(test.Context(), fixture.issue.WorkspaceID, []db.Comment{result.Comment}, pgtype.UUID{})[uuidToString(result.Comment.ID)]
	if fields.DecisionFullMessage != "" || fields.DecisionAnsweredLabel != "" {
		test.Fatalf("whole-message record must not offer a disclosure: %+v", fields)
	}
}

func TestDecisionRecordFieldsCarryTimingForPendingAndEffective(test *testing.T) {
	fixture, result, err := excerptRecord(test, "ok", "", nil)
	if err != nil {
		test.Fatal(err)
	}
	read := func() DecisionCommentFields {
		return testHandler.decisionCommentFields(test.Context(), fixture.issue.WorkspaceID, []db.Comment{result.Comment}, pgtype.UUID{})[uuidToString(result.Comment.ID)]
	}
	pending := read()
	if pending.DecisionState != "pending" || pending.DecisionAnsweredAt == "" || pending.EffectiveAt == nil || pending.DecisionWokenAt != nil {
		test.Fatalf("pending timing: %+v", pending)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, uuidToString(result.Answer.ID))
	effective := read()
	if effective.DecisionState != "effective" || effective.DecisionWokenAt == nil {
		test.Fatalf("effective timing: %+v", effective)
	}
}

// twoMessageRecord records two source messages of one DM, with excerpts.
func twoMessageRecord(test *testing.T, first, second string, excerpts []string) (askedRecordFixture, decisionRecordResult, error) {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE chat_message SET content = $1 WHERE id = $2`, first, fixture.messageID)
	secondID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": second, "sent_via": "interactive_jwt"})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
	input := fixture.recordInput()
	input.MessageIDs = append(input.MessageIDs, parseUUID(secondID))
	input.Excerpts = excerpts
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	return fixture, result, err
}

func TestDecisionRecordExcerptMustLieInsideOneSourceMessage(test *testing.T) {
	fixture, result, err := twoMessageRecord(test, "alpha beta", "gamma delta", []string{"beta gamma"})
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest || result.Answer.ID.Valid {
		test.Fatalf("err=%v, want a 400: an excerpt spanning two messages was never written", err)
	}
	fixture.assertRecordCounts(test, 0)
}

func TestDecisionRecordExcerptsFromEachSourceMessageAreAccepted(test *testing.T) {
	_, result, err := twoMessageRecord(test, "alpha beta", "gamma delta", []string{"beta", "gamma delta"})
	if err != nil || !strings.HasSuffix(result.Comment.Content, "> beta\n>\n> gamma delta") {
		test.Fatalf("one excerpt per message must pass: %v %q", err, result.Comment.Content)
	}
}

func TestDecisionRecordRetryMustNameTheSameExcerpts(test *testing.T) {
	fixture, first, err := excerptRecord(test, multiDecisionMessage, "", []string{"yes"})
	if err != nil {
		test.Fatal(err)
	}
	retry := func(excerpts []string) (decisionRecordResult, error) {
		input := fixture.recordInput()
		input.Excerpts = excerpts
		return testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	}
	if again, err := retry([]string{"yes"}); err != nil || !again.Replayed || again.Answer.ID != first.Answer.ID {
		test.Fatalf("identical retry must replay: %+v %v", again, err)
	}
	for name, excerpts := range map[string][]string{"reworded": {"yes please"}, "other part": {"A"}, "none": nil} {
		if again, err := retry(excerpts); err == nil {
			test.Fatalf("%s retry returned the old record: %+v", name, again)
		}
	}
	fixture.assertRecordCounts(test, 1)
}
