package handler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6432 (S6 of CHE-6385, plan r3 T5b): an agent that tags a member deep in a
// long agent thread asks in a new short thread instead.

type longThreadFixture struct {
	fromThreadFixture
}

// newLongThreadFixture builds a thread of `size` comments (root included) written
// by the agent, with the long-thread route set to mode and a minimum of 10.
func newLongThreadFixture(t *testing.T, mode string, size int) longThreadFixture {
	t.Helper()
	f := longThreadFixture{newFromThreadFixture(t)}
	// The base fixture already holds a root and one reply.
	for i := 2; i < size; i++ {
		f.insertComment(t, f.issueID, f.sourceRoot, fmt.Sprintf("agent update %d", i))
	}
	setLongThreadSettings(t, mode, 10)
	return f
}

func setLongThreadSettings(t *testing.T, mode string, min int) {
	t.Helper()
	dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object('long_thread_member_ask', $1::text, 'long_thread_member_ask_min', $2::int) WHERE id = $3`, mode, min, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'long_thread_member_ask' - 'long_thread_member_ask_min' WHERE id = $1`, testWorkspaceID)
	})
}

func (f longThreadFixture) memberReply(t *testing.T, parentID, content string) string {
	t.Helper()
	id := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO comment (id, issue_id, workspace_id, author_type, author_id, content, type, parent_id)
		VALUES ($1, $2, $3, 'member', $4, $5, 'comment', $6)`, id, f.issueID, testWorkspaceID, f.humanID, content, parentID)
	return id
}

type longThreadResponse struct {
	ID        string   `json:"id"`
	ParentID  *string  `json:"parent_id"`
	Hints     []string `json:"hints"`
	Placement *struct {
		Moved        bool   `json:"moved"`
		SourceRootID string `json:"source_root_id"`
		Reason       string `json:"reason"`
	} `json:"placement"`
}

func (f longThreadFixture) askIn(t *testing.T, extra map[string]any) (longThreadResponse, *testutil.Response) {
	t.Helper()
	body := map[string]any{"content": "Can you check this? " + f.mention(), "parent_id": f.sourceRoot}
	for k, v := range extra {
		body[k] = v
	}
	res := f.post(t, body).Want(http.StatusCreated)
	var out longThreadResponse
	res.JSON(&out)
	return out, res
}

func (f longThreadFixture) longThreadRows(t *testing.T, mode string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND operation_kind = 'long_thread_member_ask' AND mode = $2`, f.issueID, mode)
}

func (f longThreadFixture) allRows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1`, f.issueID)
}

func (f longThreadFixture) assertStaysInPlace(t *testing.T, got longThreadResponse) {
	t.Helper()
	if got.ParentID == nil || *got.ParentID != f.sourceRoot {
		t.Fatalf("comment parent = %v, want it posted in place under %s", got.ParentID, f.sourceRoot)
	}
	if got.Placement != nil {
		t.Errorf("placement = %+v, want none for a comment posted in place", got.Placement)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND ask_source_root_id IS NOT NULL`, got.ID); n != 0 {
		t.Errorf("in-place comment stored an ask source")
	}
}

func TestLongThreadMemberAskMovesAtTheMinimum(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)

	got, res := f.askIn(t, nil)

	if got.ParentID != nil {
		t.Fatalf("moved comment has parent %v, want a new root", *got.ParentID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND parent_id IS NULL AND ask_source_root_id = $2`, got.ID, f.sourceRoot); n != 1 {
		t.Fatalf("new root with ask_source_root_id = source root: %d rows, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE parent_id = $1 AND author_type = 'system' AND content = $2`, f.sourceRoot, "Asked Human in thread "+got.ID+"."); n != 1 {
		t.Errorf("stub in the source thread: %d rows, want 1", n)
	}
	if got.Placement == nil || !got.Placement.Moved || got.Placement.SourceRootID != f.sourceRoot || got.Placement.Reason != "long_thread_member_ask" {
		t.Errorf("placement = %+v, want moved from %s for long_thread_member_ask", got.Placement, f.sourceRoot)
	}
	want := fmt.Sprintf("Moved: this ask is new thread %s (thread %s has 10 comments). The source thread got a link.", got.ID, f.sourceRoot)
	if !containsString(got.Hints, want) {
		t.Errorf("hints = %q, want %q", got.Hints, want)
	}
	if n := f.longThreadRows(t, "move"); n != 1 {
		t.Errorf("move rows = %d, want 1", n)
	}
	if n := f.allRows(t); n != 1 {
		t.Errorf("ask_guard_event rows = %d, want only the move row; body %s", n, res.Text())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND mode = 'block'`, f.issueID); n != 0 {
		t.Errorf("block rows = %d, want 0", n)
	}
}

func TestLongThreadMemberAskOperationKeyIsTheRootAndContent(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)

	f.askIn(t, nil)

	var sum string
	dbfx.QueryRow(t, `SELECT payload_sha256 FROM ask_guard_event WHERE issue_id = $1`, f.issueID).Scan(&sum)
	if want := sha256Hex(f.sourceRoot + ":Can you check this? " + f.mention()); sum != want {
		t.Errorf("payload_sha256 = %s, want sha256(<thread root id>:<comment content>) = %s", sum, want)
	}
}

func TestLongThreadMemberAskStaysBelowTheMinimum(t *testing.T) {
	f := newLongThreadFixture(t, "move", 9)

	got, _ := f.askIn(t, nil)

	f.assertStaysInPlace(t, got)
	if n := f.allRows(t); n != 0 {
		t.Errorf("rows = %d, want 0 for a short thread", n)
	}
}

func TestLongThreadMemberAskHonoursTheMinimumSetting(t *testing.T) {
	f := newLongThreadFixture(t, "move", 5)
	setLongThreadSettings(t, "move", 5)

	got, _ := f.askIn(t, nil)

	if got.ParentID != nil {
		t.Errorf("a 5-comment thread under a minimum of 5 stayed in place")
	}
}

func TestLongThreadMemberAskStaysWhenTheMemberSpokeInTheLastThree(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)
	f.memberReply(t, f.sourceRoot, "I am here")

	got, _ := f.askIn(t, nil)

	f.assertStaysInPlace(t, got)
	if n := f.allRows(t); n != 0 {
		t.Errorf("rows = %d, want 0 while the member is in the conversation", n)
	}
}

func TestLongThreadMemberAskMovesWhenTheMemberSpokeEarlier(t *testing.T) {
	f := newLongThreadFixture(t, "move", 9)
	f.memberReply(t, f.sourceRoot, "early word")
	for i := 0; i < 3; i++ {
		f.insertComment(t, f.issueID, f.sourceRoot, fmt.Sprintf("agent tail %d", i))
	}

	got, _ := f.askIn(t, nil)

	if got.ParentID != nil {
		t.Errorf("a member four comments back still counts as active; want a move")
	}
}

func TestLongThreadMemberAskStaysForNotifyOnly(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)

	got, _ := f.askIn(t, map[string]any{"notify_only": true})

	f.assertStaysInPlace(t, got)
	if n := f.allRows(t); n != 0 {
		t.Errorf("rows = %d, want 0 for an FYI", n)
	}
}

func TestLongThreadMemberAskStaysWithoutAMemberMention(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)

	got, _ := f.askIn(t, map[string]any{"content": "no tag here"})

	f.assertStaysInPlace(t, got)
}

func TestLongThreadMemberAskResolveThreadPostsInPlaceWithAWarnRow(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)

	got, _ := f.askIn(t, map[string]any{"resolve_thread": true})

	f.assertStaysInPlace(t, got)
	want := "long_thread_member_ask: not moved because the comment resolves or supersedes; use --notify-only for an FYI or --from-thread for an ask"
	if !containsString(got.Hints, want) {
		t.Errorf("hints = %q, want %q", got.Hints, want)
	}
	if n := f.longThreadRows(t, "warn"); n != 1 {
		t.Errorf("warn rows = %d, want 1", n)
	}
	// The CLI resolves with its second call; the comment is in place, so it works.
	f.resolveAs(t, got.ID, "task_token").Want(http.StatusOK)
}

func TestLongThreadMemberAskSupersedesPostsInPlace(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)
	other := f.insertComment(t, f.issueID, "", "older thread")

	got, _ := f.askIn(t, map[string]any{"supersedes": []string{other}})

	f.assertStaysInPlace(t, got)
	if n := f.longThreadRows(t, "warn"); n != 1 {
		t.Errorf("warn rows = %d, want 1", n)
	}
}

// An older CLI sends no resolve_thread: the ask moves, then its resolve call hits
// ask_root_not_resolvable and the ask stays open and waiting.
func TestLongThreadMemberAskOlderCLIMovesThenResolveIsRefused(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)

	got, _ := f.askIn(t, nil)
	res := f.resolveAs(t, got.ID, "task_token").Want(http.StatusConflict)

	if !strings.Contains(res.Text(), "ask_root_not_resolvable") {
		t.Errorf("409 body lacks ask_root_not_resolvable: %s", res.Text())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, got.ID); n != 0 {
		t.Errorf("the ask root was resolved; it must stay open")
	}
}

func TestLongThreadMemberAskWarnPostsInPlaceWithTheHint(t *testing.T) {
	f := newLongThreadFixture(t, "warn", 10)

	got, _ := f.askIn(t, nil)

	f.assertStaysInPlace(t, got)
	want := fmt.Sprintf("long_thread_member_ask: thread %s has 10 comments. Next time: multica issue comment add %s --from-thread %s --content-file <file>", f.sourceRoot, f.issueID, f.sourceRoot)
	if !containsString(got.Hints, want) {
		t.Errorf("hints = %q, want %q", got.Hints, want)
	}
	if n := f.longThreadRows(t, "warn"); n != 1 {
		t.Errorf("warn rows = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, f.issueID); n != 0 {
		t.Errorf("warn wrote %d system comments, want 0", n)
	}
}

func TestLongThreadMemberAskOffKeepsTodaysBehaviour(t *testing.T) {
	f := newLongThreadFixture(t, "off", 12)

	got, _ := f.askIn(t, nil)

	f.assertStaysInPlace(t, got)
	if n := f.allRows(t); n != 0 {
		t.Errorf("rows = %d, want 0 under off", n)
	}
	for _, hint := range got.Hints {
		if strings.Contains(hint, "long_thread_member_ask") {
			t.Errorf("off added a hint: %q", hint)
		}
	}
}

func TestLongThreadMemberAskDefaultsLeaveTodaysBehaviour(t *testing.T) {
	f := longThreadFixture{newFromThreadFixture(t)}
	for i := 2; i < 12; i++ {
		f.insertComment(t, f.issueID, f.sourceRoot, fmt.Sprintf("agent update %d", i))
	}

	got, _ := f.askIn(t, nil)

	f.assertStaysInPlace(t, got)
	if n := f.allRows(t); n != 0 {
		t.Errorf("rows with the settings unset = %d, want 0", n)
	}
}

func TestLongThreadMemberAskNeverMovesAMemberComment(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": "hi " + f.mention(), "parent_id": f.sourceRoot}), "id", f.issueID)
	var got longThreadResponse
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&got)

	f.assertStaysInPlace(t, got)
}

func TestLongThreadMemberAskWithoutATaskWritesNoRowAndLogsInfo(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": "hi " + f.mention(), "parent_id": f.sourceRoot}), "id", f.issueID)
	// A task_token request names the agent; with no X-Task-ID it has no task.
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agentID)
	var got longThreadResponse
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&got)

	if got.ParentID != nil {
		t.Errorf("an agent comment without a task still moves; got parent %v", *got.ParentID)
	}
	if n := f.allRows(t); n != 0 {
		t.Errorf("rows = %d, want 0 without a task id", n)
	}
	if n := strings.Count(logs.String(), "long_thread_member_ask: no task id"); n != 1 {
		t.Errorf("INFO lines about the missing task id = %d, want 1; logs:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a WARN line was logged:\n%s", logs.String())
	}
}

func TestLongThreadMemberAskLogsNoWarnLine(t *testing.T) {
	for _, mode := range []string{"move", "warn"} {
		t.Run(mode, func(t *testing.T) {
			f := newLongThreadFixture(t, mode, 10)
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			f.askIn(t, nil)
			f.askIn(t, map[string]any{"resolve_thread": true})

			if strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("a WARN line was logged:\n%s", logs.String())
			}
		})
	}
}

func TestLongThreadMemberAskEditNeverMoves(t *testing.T) {
	for _, mode := range []string{"warn", "move"} {
		t.Run(mode, func(t *testing.T) {
			f := newLongThreadFixture(t, mode, 10)
			own := uuid.NewString()
			dbfx.Exec(t, `INSERT INTO comment (id, issue_id, workspace_id, author_type, author_id, content, type, parent_id, source_task_id)
				VALUES ($1, $2, $3, 'agent', $4, 'plain update', 'comment', $5, $6)`, own, f.issueID, testWorkspaceID, f.agentID, f.sourceRoot, f.taskID)

			req := withURLParam(newRequest(http.MethodPut, "/api/comments/"+own, map[string]any{"content": "plain update " + f.mention()}), "commentId", own)
			req.Header.Set("X-Actor-Source", "task_token")
			req.Header.Set("X-Agent-ID", f.agentID)
			req.Header.Set("X-Task-ID", f.taskID)
			var got longThreadResponse
			testutil.Call(t, testHandler.UpdateComment, req).Want(http.StatusOK).JSON(&got)

			if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND parent_id = $2 AND ask_source_root_id IS NULL`, own, f.sourceRoot); n != 1 {
				t.Fatalf("an edit moved the comment")
			}
			if n := f.longThreadRows(t, "warn"); n != 1 {
				t.Errorf("warn rows = %d, want 1", n)
			}
			if n := f.longThreadRows(t, "move"); n != 0 {
				t.Errorf("move rows = %d, want 0 for an edit", n)
			}
			var sum string
			dbfx.QueryRow(t, `SELECT payload_sha256 FROM ask_guard_event WHERE issue_id = $1`, f.issueID).Scan(&sum)
			if want := sha256Hex(own + ":plain update " + f.mention()); sum != want {
				t.Errorf("edit payload_sha256 = %s, want sha256(<comment id>:<new content>) = %s", sum, want)
			}
			if len(got.Hints) == 0 || !strings.Contains(strings.Join(got.Hints, "\n"), "--from-thread") {
				t.Errorf("edit hints = %q, want the from-thread hint", got.Hints)
			}
		})
	}
}

func TestLongThreadMemberAskEditOffWritesNothing(t *testing.T) {
	f := newLongThreadFixture(t, "off", 10)
	own := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO comment (id, issue_id, workspace_id, author_type, author_id, content, type, parent_id, source_task_id)
		VALUES ($1, $2, $3, 'agent', $4, 'plain update', 'comment', $5, $6)`, own, f.issueID, testWorkspaceID, f.agentID, f.sourceRoot, f.taskID)

	req := withURLParam(newRequest(http.MethodPut, "/api/comments/"+own, map[string]any{"content": "plain update " + f.mention()}), "commentId", own)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	testutil.Call(t, testHandler.UpdateComment, req).Want(http.StatusOK)

	if n := f.allRows(t); n != 0 {
		t.Errorf("rows = %d, want 0 under off", n)
	}
}

// The three recovery and repair queries read every open block row of an ended
// task with no operation_kind filter (F7): a T5b row must never be one.
func TestLongThreadMemberAskRowsAreInvisibleToTheWorkerQueries(t *testing.T) {
	f := newLongThreadFixture(t, "move", 10)
	f.askIn(t, nil)
	f.askIn(t, map[string]any{"resolve_thread": true})
	if n := f.allRows(t); n != 2 {
		t.Fatalf("rows = %d, want a move row and a warn row", n)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, f.taskID)
	ctx := context.Background()
	recovery, err := testHandler.Queries.ListAskRecoveryEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	recoverable, err := testHandler.Queries.ListAskGuardRecoverableEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := testHandler.Queries.ListAskRepairEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for name, events := range map[string][]string{
		"ListAskRecoveryEvents":         eventTaskIDs(recovery),
		"ListAskGuardRecoverableEvents": eventTaskIDs(recoverable),
		"ListAskRepairEvents":           eventTaskIDs(repair),
	} {
		if containsString(events, f.taskID) {
			t.Errorf("%s returned a row of the ended task", name)
		}
	}
}

func eventTaskIDs(events []db.AskGuardEvent) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, uuidToString(event.TaskID))
	}
	return ids
}

// The two settings are validated on the server: a bad value is a 400 that names
// the key and leaves the stored settings alone.
func TestLongThreadMemberAskSettingsAreValidated(t *testing.T) {
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'long_thread_member_ask' - 'long_thread_member_ask_min' WHERE id = $1`, testWorkspaceID)
	})
	patch := func(settings map[string]any) *testutil.Response {
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": settings}), "id", testWorkspaceID)
		return testutil.Call(t, testHandler.UpdateWorkspace, request)
	}

	for _, ok := range []map[string]any{
		{"long_thread_member_ask": "off"},
		{"long_thread_member_ask": "warn"},
		{"long_thread_member_ask": "move", "long_thread_member_ask_min": 3},
		{"long_thread_member_ask_min": 100},
	} {
		patch(ok).Want(http.StatusOK)
	}
	for key, bad := range map[string][]any{
		"long_thread_member_ask":     {"block", "ON", "", 1, nil, true},
		"long_thread_member_ask_min": {2, 101, 10.5, "10", nil, -1},
	} {
		for _, value := range bad {
			res := patch(map[string]any{key: value}).Want(http.StatusBadRequest)
			if !strings.Contains(res.Text(), key) {
				t.Errorf("%s = %v: error %q does not name the key", key, value, res.Text())
			}
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM workspace WHERE id = $1 AND settings->>'long_thread_member_ask_min' IN ('2','101')`, testWorkspaceID); n != 0 {
		t.Errorf("a refused value was stored")
	}
}
