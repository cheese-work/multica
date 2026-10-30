package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// feedCaller is one credential shape calling R2 or its ack.
type feedCaller struct {
	source string
	scopes []string
	owner  bool
}

var feedSweep = feedCaller{source: "pat", scopes: []string{DecisionFeedScope}, owner: true}

func (caller feedCaller) request(test *testing.T, method, path, body string) *http.Request {
	test.Helper()
	userID := testUserID
	if !caller.owner {
		userID = dbfx.User(test, "Feed member", "feed-"+uuid.NewString()+"@test.invalid")
		dbfx.Exec(test, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, userID)
	}
	raw, err := auth.GeneratePATToken()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := testHandler.Queries.CreatePersonalAccessToken(context.Background(), db.CreatePersonalAccessTokenParams{UserID: parseUUID(userID), Name: "feed", TokenHash: auth.HashToken(raw), TokenPrefix: raw[:12], Scopes: caller.scopes}); err != nil {
		test.Fatal(err)
	}
	request := newRequest(method, path, nil)
	request.Body = ioBody(body)
	request.Header.Set("X-User-ID", userID)
	request.Header.Set("X-Actor-Source", caller.source)
	request.Header.Set("Authorization", "Bearer "+raw)
	member, err := testHandler.getWorkspaceMember(context.Background(), userID, testWorkspaceID)
	if err != nil {
		test.Fatal(err)
	}
	return request.WithContext(middleware.SetMemberContext(request.Context(), testWorkspaceID, member))
}

func feedItems(test *testing.T) map[string]DecisionFeedItem {
	test.Helper()
	response := httptest.NewRecorder()
	testHandler.ListDecisionFeed(response, feedSweep.request(test, http.MethodGet, "/api/decisions/feed", ""))
	if response.Code != http.StatusOK {
		test.Fatalf("feed status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Answers []DecisionFeedItem `json:"answers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		test.Fatal(err)
	}
	items := map[string]DecisionFeedItem{}
	for _, item := range body.Answers {
		items[item.AnswerID] = item
	}
	return items
}

func feedAck(test *testing.T, answerID string, version int64) (int, map[string]any) {
	test.Helper()
	response := httptest.NewRecorder()
	request := withURLParam(feedSweep.request(test, http.MethodPost, "/api/decisions/feed/"+answerID+"/ack", `{"version":`+strconv.FormatInt(version, 10)+`}`), "id", answerID)
	testHandler.AckDecisionFeed(response, request)
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	return response.Code, body
}

func answerVersion(test *testing.T, answerID pgtype.UUID) int64 {
	test.Helper()
	var version int64
	if err := testPool.QueryRow(context.Background(), `SELECT state_version FROM decision_answer WHERE id = $1`, answerID).Scan(&version); err != nil {
		test.Fatal(err)
	}
	return version
}

func TestDecisionAnswerStateVersionRisesOnEveryInputChange(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	id := record.Answer.ID
	steps := []struct{ name, sql string }{
		{"finalization", `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`},
		{"wake ack", `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`},
		{"disclaim", `UPDATE decision_answer SET disclaimed_at = clock_timestamp(), disclaim_reason = 'human' WHERE id = $1`},
		{"veto", `UPDATE decision_answer SET veto_reason = 'wrong' WHERE id = $1`},
		{"retraction", `UPDATE decision_answer SET retraction_outcome = 'run_enqueued' WHERE id = $1`},
	}
	for _, step := range steps {
		before := answerVersion(test, id)
		dbfx.Exec(test, step.sql, id)
		if after := answerVersion(test, id); after <= before {
			test.Fatalf("%s did not raise state_version (%d -> %d)", step.name, before, after)
		}
	}
	before := answerVersion(test, id)
	dbfx.Exec(test, `UPDATE decision_answer SET veto_note_posted_at = clock_timestamp() WHERE id = $1`, id)
	if after := answerVersion(test, id); after != before {
		test.Fatalf("a column outside the document input raised state_version (%d -> %d)", before, after)
	}
}

func TestDecisionClassChangeRaisesAnswerStateVersion(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	before := answerVersion(test, record.Answer.ID)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, record.Request.ID)
	if after := answerVersion(test, record.Answer.ID); after <= before {
		test.Fatalf("class change did not raise state_version (%d -> %d)", before, after)
	}
}

func TestDecisionFeedLateFinalizationAndWithdrawal(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	id := uuidToString(record.Answer.ID)
	item, found := feedItems(test)[id]
	if !found || item.State != "pending" || item.Quote == "" || item.Issue == "" {
		test.Fatalf("pending answer missing or wrong in feed: %+v", item)
	}
	seen := item.StateVersion
	if status, body := feedAck(test, id, seen); status != http.StatusOK || body["applied"] != true {
		test.Fatalf("ack pending: %d %v", status, body)
	}
	if _, found := feedItems(test)[id]; found {
		test.Fatal("acked version still in feed")
	}
	// late finalization and wake arrive after the ack: the answer must resurface
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, record.Answer.ID)
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, record.Answer.ID)
	item, found = feedItems(test)[id]
	if !found || item.State != "effective" || item.StateVersion <= seen {
		test.Fatalf("late finalization did not resurface as effective: %+v", item)
	}
	// crash between retain and ack: reading again returns the same version, ack once, repeat is a no-op
	if again := feedItems(test)[id]; again.StateVersion != item.StateVersion {
		test.Fatalf("repeat read changed version: %+v", again)
	}
	if status, body := feedAck(test, id, item.StateVersion); status != http.StatusOK || body["applied"] != true {
		test.Fatalf("ack effective: %d %v", status, body)
	}
	if status, body := feedAck(test, id, item.StateVersion); status != http.StatusOK || body["applied"] != false {
		test.Fatalf("repeat ack must not apply: %d %v", status, body)
	}
	if status, body := feedAck(test, id, seen); status != http.StatusOK || body["applied"] != false {
		test.Fatalf("stale ack must not apply: %d %v", status, body)
	}
	if status, body := feedAck(test, id, item.StateVersion+50); status != http.StatusOK || body["applied"] != false {
		test.Fatalf("ack above state_version must not apply: %d %v", status, body)
	}
	var acked int64
	if err := testPool.QueryRow(context.Background(), `SELECT hindsight_acked_version FROM decision_answer WHERE id = $1`, record.Answer.ID).Scan(&acked); err != nil || acked != item.StateVersion {
		test.Fatalf("acked version moved: %d %v", acked, err)
	}
	// disclaimed after retain: the answer returns as withdrawn with its reason
	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp(), disclaim_reason = 'human' WHERE id = $1`, record.Answer.ID)
	item, found = feedItems(test)[id]
	if !found || item.State != "withdrawn" || item.DisclaimReason == nil || *item.DisclaimReason != "human" {
		test.Fatalf("disclaim after retain not reported as withdrawn: %+v", item)
	}
}

func TestDecisionFeedWithdrawnBeforeDeliveryIsNotDelivered(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	input := fixture.recordInput()
	input.ParentID, input.EscalationClass = pgtype.UUID{}, "reversible"
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	// reopen-then-cancel: an unprompted record disclaimed before its wake cancels its request
	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp(), disclaim_reason = 'human' WHERE id = $1`, record.Answer.ID)
	dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'record_disclaimed' WHERE id = $1`, record.Request.ID)
	if item, found := feedItems(test)[uuidToString(record.Answer.ID)]; !found || item.State != "not_delivered" {
		test.Fatalf("cancelled request's undelivered answer must still reach the feed as not_delivered: %+v", item)
	}
}

func TestDecisionFeedRefusesEveryOtherCredential(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	id := uuidToString(record.Answer.ID)
	refused := map[string]feedCaller{
		"task token":         {source: "task_token", scopes: []string{DecisionFeedScope}, owner: true},
		"cloud pat":          {source: "cloud_pat", scopes: []string{DecisionFeedScope}, owner: true},
		"jwt":                {source: "interactive_jwt", scopes: []string{DecisionFeedScope}, owner: true},
		"unscoped owner pat": {source: "pat", owner: true},
		"scoped member pat":  {source: "pat", scopes: []string{DecisionFeedScope}, owner: false},
	}
	for name, caller := range refused {
		feed := httptest.NewRecorder()
		testHandler.ListDecisionFeed(feed, caller.request(test, http.MethodGet, "/api/decisions/feed", ""))
		ack := httptest.NewRecorder()
		testHandler.AckDecisionFeed(ack, withURLParam(caller.request(test, http.MethodPost, "/api/decisions/feed/"+id+"/ack", `{"version":1}`), "id", id))
		if feed.Code != http.StatusForbidden || ack.Code != http.StatusForbidden {
			test.Fatalf("%s: feed=%d ack=%d, want 403 both", name, feed.Code, ack.Code)
		}
	}
	var acked int64
	if err := testPool.QueryRow(context.Background(), `SELECT hindsight_acked_version FROM decision_answer WHERE id = $1`, record.Answer.ID).Scan(&acked); err != nil || acked != 0 {
		test.Fatalf("a refused ack moved the stored version: %d %v", acked, err)
	}
}

func TestCreatePersonalAccessTokenScopes(test *testing.T) {
	create := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := newRequest(http.MethodPost, "/api/tokens", nil)
		request.Body = ioBody(body)
		request.Header.Set("X-Actor-Source", "interactive_jwt")
		testHandler.CreatePersonalAccessToken(response, request)
		return response
	}
	if response := create(`{"name":"sweep","scopes":["decisions:feed"]}`); response.Code != http.StatusCreated || !bytes.Contains(response.Body.Bytes(), []byte(`"scopes":["decisions:feed"]`)) {
		test.Fatalf("scoped create: %d %s", response.Code, response.Body.String())
	}
	if response := create(`{"name":"bad","scopes":["admin"]}`); response.Code != http.StatusBadRequest {
		test.Fatalf("unknown scope accepted: %d %s", response.Code, response.Body.String())
	}
	testPool.Exec(context.Background(), `DELETE FROM personal_access_token WHERE name = 'sweep'`)
}
