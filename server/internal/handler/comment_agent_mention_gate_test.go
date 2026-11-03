package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func postCommentForGate(t *testing.T, req *http.Request, issueID string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.CreateComment, withURLParam(req, "id", issueID))
}

func countIssueComments(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&n)
	return n
}

func countIssueTasks(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n)
	return n
}

func gateIssue(t *testing.T, title string) string {
	t.Helper()
	issueID := dbfx.Issue(t, title)
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	return issueID
}

// wantInvalidAgentMention asserts the 422 body for one bad mention and returns
// it with the offsets removed, so callers can compare bodies byte for byte.
func wantInvalidAgentMention(t *testing.T, res *testutil.Response, content, mention string) string {
	t.Helper()
	res.Want(http.StatusUnprocessableEntity)
	var body map[string]json.RawMessage
	res.JSON(&body)
	var code string
	_ = json.Unmarshal(body["code"], &code)
	if code != "invalid_agent_mentions" {
		t.Fatalf("code = %q, want invalid_agent_mentions; body %s", code, res.Text())
	}
	var ranges []struct{ Start, End int }
	if err := json.Unmarshal(body["invalid_mentions"], &ranges); err != nil {
		t.Fatalf("invalid_mentions: %v; body %s", err, res.Text())
	}
	start := strings.Index(content, mention)
	if len(ranges) != 1 || ranges[0].Start != start || ranges[0].End != start+len(mention) {
		t.Fatalf("invalid_mentions = %+v, want [{%d %d}]", ranges, start, start+len(mention))
	}
	delete(body, "invalid_mentions")
	rest, _ := json.Marshal(body)
	return string(rest)
}

func TestCreateCommentInvalidAgentMentionsRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	privateID, _, readerID := privateAgentTestFixture(t)
	archivedID := createHandlerTestAgent(t, "Archived gate target", nil)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, archivedID)
	foreignWorkspace := dbfx.Workspace(t, "Other gate workspace", "gate-"+uuid.NewString())
	foreignID := dbfx.Agent(t, "Other workspace gate secret", handlerTestRuntimeID(t), testutil.Cols{"workspace_id": foreignWorkspace})
	cases := []struct {
		name, agentID, callerID string
	}{
		{"unknown", uuid.NewString(), ""},
		{"malformed", "-", ""},
		{"other workspace", foreignID, ""},
		{"private not invocable", privateID, readerID},
		{"archived", archivedID, ""},
		{"inside a note", uuid.NewString(), ""},
	}
	var firstBody string
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issueID := gateIssue(t, "Gate "+tc.name)
			mention := fmt.Sprintf("[@Alias](mention://agent/%s)", tc.agentID)
			content := "please look " + mention + " now"
			if tc.name == "inside a note" {
				content = "/note reference " + mention
			}
			req := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
			if tc.callerID != "" {
				req = newRequestAs(tc.callerID, http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
			}
			res := postCommentForGate(t, req, issueID)
			got := wantInvalidAgentMention(t, res, content, mention)
			for _, leak := range []string{tc.agentID, "Other workspace gate secret", "Archived gate target", "archived", "private-access-test-agent"} {
				if strings.Contains(res.Text(), leak) {
					t.Errorf("body leaks %q: %s", leak, res.Text())
				}
			}
			if i == 0 {
				firstBody = got
			} else if got != firstBody {
				t.Errorf("body differs from the unknown-id body:\n got %s\nwant %s", got, firstBody)
			}
			if n := countIssueComments(t, issueID); n != 0 {
				t.Errorf("comments saved = %d, want 0", n)
			}
			if n := countIssueTasks(t, issueID); n != 0 {
				t.Errorf("tasks enqueued = %d, want 0", n)
			}
		})
		if t.Failed() && i == 0 {
			t.FailNow()
		}
	}
}

func TestCreateCommentInvalidAgentMentionsReportsEveryRange(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := gateIssue(t, "Gate two bad mentions")
	first := fmt.Sprintf("[@A](mention://agent/%s)", uuid.NewString())
	second := "[@B](mention://agent/-)"
	content := first + " and " + second
	res := postCommentForGate(t, newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), issueID).Want(http.StatusUnprocessableEntity)
	var body struct {
		InvalidMentions []struct{ Start, End int } `json:"invalid_mentions"`
	}
	res.JSON(&body)
	want := []struct{ Start, End int }{{0, len(first)}, {len(first) + 5, len(content)}}
	if len(body.InvalidMentions) != 2 || body.InvalidMentions[0] != want[0] || body.InvalidMentions[1] != want[1] {
		t.Fatalf("invalid_mentions = %+v, want %+v", body.InvalidMentions, want)
	}
}

func TestCreateCommentInvalidAgentMentionsStillSavesOtherTargets(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	privateID, _, readerID := privateAgentTestFixture(t)
	validID := createHandlerTestAgent(t, "Valid gate target", nil)
	unboundID := dbfx.Agent(t, "Unbound gate target", "", testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2) ON CONFLICT DO NOTHING`, unboundID, testWorkspaceID)
	squadID := createCommentTriggerPreviewSquad(t, "Gate squad with private leader", privateID)

	t.Run("valid agent queues", func(t *testing.T) {
		issueID := gateIssue(t, "Gate valid")
		content := fmt.Sprintf("[@Valid](mention://agent/%s)", validID)
		var posted CommentResponse
		postCommentForGate(t, newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), issueID).Want(http.StatusCreated).JSON(&posted)
		if findCommentOutcome(t, posted.TriggerOutcomes, validID).Status != DispatchQueued {
			t.Fatalf("outcomes = %+v", posted.TriggerOutcomes)
		}
	})
	t.Run("unusable runtime stays a blocked outcome", func(t *testing.T) {
		issueID := gateIssue(t, "Gate unbound")
		content := fmt.Sprintf("[@Unbound](mention://agent/%s)", unboundID)
		var posted CommentResponse
		postCommentForGate(t, newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), issueID).Want(http.StatusCreated).JSON(&posted)
		if findCommentOutcome(t, posted.TriggerOutcomes, unboundID).Status != DispatchBlocked {
			t.Fatalf("outcomes = %+v", posted.TriggerOutcomes)
		}
	})
	t.Run("squad with private leader and member mentions are unchanged", func(t *testing.T) {
		issueID := gateIssue(t, "Gate squad member")
		content := fmt.Sprintf("[@Team](mention://squad/%s) [@Reader](mention://member/%s)", squadID, readerID)
		var posted CommentResponse
		postCommentForGate(t, newRequestAs(readerID, http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), issueID).Want(http.StatusCreated).JSON(&posted)
		if findCommentOutcome(t, posted.TriggerOutcomes, squadID).Status != DispatchBlocked {
			t.Fatalf("outcomes = %+v", posted.TriggerOutcomes)
		}
	})
}

func TestUpdateCommentInvalidAgentMentionsOnlyForNewMentions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	oldID := createHandlerTestAgent(t, "Later archived gate target", nil)
	issueID := gateIssue(t, "Gate edit")
	oldMention := fmt.Sprintf("[@Old](mention://agent/%s)", oldID)
	original := "first " + oldMention
	var posted CommentResponse
	postCommentForGate(t, newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": original}), issueID).Want(http.StatusCreated).JSON(&posted)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, oldID)
	edit := func(content string) *testutil.Response {
		req := newRequest(http.MethodPut, "/api/comments/"+posted.ID, map[string]any{"content": content})
		return testutil.Call(t, testHandler.UpdateComment, withURLParam(req, "commentId", posted.ID))
	}
	tasksBefore := countIssueTasks(t, issueID)

	newMention := fmt.Sprintf("[@New](mention://agent/%s)", uuid.NewString())
	bad := "second " + oldMention + " " + newMention
	wantInvalidAgentMention(t, edit(bad), bad, newMention)
	var stored string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, posted.ID).Scan(&stored)
	if stored != original {
		t.Errorf("rejected edit changed the row: %q", stored)
	}
	if n := countIssueTasks(t, issueID); n != tasksBefore {
		t.Errorf("rejected edit changed tasks: %d -> %d", tasksBefore, n)
	}

	edit("typo fixed " + oldMention).Want(http.StatusOK)
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, posted.ID).Scan(&stored)
	if !bytes.Contains([]byte(stored), []byte("typo fixed")) {
		t.Errorf("edit keeping an archived mention was not saved: %q", stored)
	}
}

// countingDBTX counts queries so a test can prove a repeated mention does not
// repeat its authorization lookups.
type countingDBTX struct {
	db.DBTX
	n int
}

func (c *countingDBTX) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	c.n++
	return c.DBTX.QueryRow(ctx, sql, args...)
}

func (c *countingDBTX) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	c.n++
	return c.DBTX.Query(ctx, sql, args...)
}

func TestInvalidAgentMentionsChecksEachTargetOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := gateIssue(t, "Gate memoize")
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	unknown := uuid.NewString()
	var sb strings.Builder
	const repeats = 50
	for i := 0; i < repeats; i++ {
		fmt.Fprintf(&sb, "[@A](mention://agent/%s) ", unknown)
		if i%2 == 0 { // same agent, upper-case spelling
			fmt.Fprintf(&sb, "[@A](mention://agent/%s) ", strings.ToUpper(unknown))
		}
	}
	counter := &countingDBTX{DBTX: testPool}
	h := *testHandler
	h.Queries = db.New(counter)

	got := h.invalidAgentMentionRanges(context.Background(), issue, sb.String(), "member", testUserID, "", nil)

	if want := repeats + repeats/2; len(got) != want {
		t.Fatalf("invalid ranges = %d, want every mention reported (%d)", len(got), want)
	}
	// One agent lookup, plus whatever canInvokeAgent needs for that one target.
	counter.n = 0
	h.invalidAgentMentionRanges(context.Background(), issue, "[@A]("+"mention://agent/"+unknown+")", "member", testUserID, "", nil)
	perTarget := counter.n
	counter.n = 0
	h.invalidAgentMentionRanges(context.Background(), issue, sb.String(), "member", testUserID, "", nil)
	if counter.n != perTarget {
		t.Fatalf("queries for %d repeated mentions = %d, want %d (one target)", repeats+repeats/2, counter.n, perTarget)
	}
}
