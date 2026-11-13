package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type dmFollowRow struct {
	Lineage, Owner, State, LastStatus string
	Reasons                           []string
	Muted                             bool
}

func followRows(t *testing.T, issueID string) []dmFollowRow {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `
		SELECT lineage_id::text, owner_user_id::text, reasons, state, last_rendered_status,
		       muted_at IS NOT NULL
		FROM dm_progress_follow WHERE issue_id = $1 ORDER BY created_at`, issueID)
	if err != nil {
		t.Fatalf("query follows: %v", err)
	}
	defer rows.Close()
	var out []dmFollowRow
	for rows.Next() {
		var r dmFollowRow
		if err := rows.Scan(&r.Lineage, &r.Owner, &r.Reasons, &r.State, &r.LastStatus, &r.Muted); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

type dmWorld struct {
	agentID, runtimeID string
}

func newDMWorld(t *testing.T) dmWorld {
	t.Helper()
	return dmWorld{agentID: createHandlerTestAgent(t, "dm-progress-agent", []byte("[]")), runtimeID: handlerTestRuntimeID(t)}
}

// dm inserts a native DM created by creator.
func (w dmWorld) dm(t *testing.T, creator string, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{"creator_id": creator}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	return dbfx.ChatSession(t, w.agentID, cols)
}

// task is a running agent task bound to session, started by initiator.
func (w dmWorld) task(t *testing.T, session, initiator string) string {
	t.Helper()
	cols := testutil.Cols{"status": "running", "runtime_id": w.runtimeID, "originator_user_id": initiator, "accountable_user_id": initiator}
	if session != "" {
		cols["chat_session_id"] = session
	}
	if initiator != "" {
		cols["initiator_user_id"] = initiator
	}
	return dbfx.Task(t, w.agentID, cols)
}

func (w dmWorld) agentReq(method, path, task string, body any) *http.Request {
	req := newRequest(method, path, body)
	req.Header.Set("X-Agent-ID", w.agentID)
	req.Header.Set("X-Task-ID", task)
	return req
}

// createIssue creates an issue through the HTTP handler and returns its id.
func createDMIssue(t *testing.T, req *http.Request) string {
	t.Helper()
	rec := httptest.NewRecorder()
	testHandler.CreateIssue(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateIssue = %d: %s", rec.Code, rec.Body.String())
	}
	var created IssueResponse
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue WHERE id = $1`, created.ID) })
	return created.ID
}

func (w dmWorld) agentIssue(t *testing.T, task string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"title": "dm progress " + t.Name()}
	for k, v := range extra {
		body[k] = v
	}
	return createDMIssue(t, w.agentReq("POST", "/api/issues?workspace_id="+testWorkspaceID, task, body))
}

func TestDMProgress_AgentCreateInDMFollowsOrigin(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	issue := w.agentIssue(t, w.task(t, dm, testUserID), nil)

	rows := followRows(t, issue)
	if len(rows) != 1 {
		t.Fatalf("follows = %+v, want exactly one origin follow", rows)
	}
	got := rows[0]
	if got.Lineage != dm || got.Owner != testUserID || !slices.Equal(got.Reasons, []string{"origin"}) || got.State != "active" || got.Muted || got.LastStatus != "todo" {
		t.Errorf("follow = %+v, want origin/active in lineage %s owned by %s with baseline status todo", got, dm, testUserID)
	}
}

func TestDMProgress_LineageIsRootOfContinuationChain(t *testing.T) {
	w := newDMWorld(t)
	root := w.dm(t, testUserID)
	successor := w.dm(t, testUserID, testutil.Cols{"continued_from_session_id": root})
	issue := w.agentIssue(t, w.task(t, successor, testUserID), nil)

	rows := followRows(t, issue)
	if len(rows) != 1 || rows[0].Lineage != root {
		t.Fatalf("follows = %+v, want one follow keyed by root %s", rows, root)
	}
}

func TestDMProgress_NoOriginFollowOutsideOwnNativeDM(t *testing.T) {
	w := newDMWorld(t)
	other := createSecondWorkspaceMember(t)
	cases := map[string]string{
		"channel-bridged DM":              w.task(t, w.dm(t, testUserID, testutil.Cols{"channel_origin_at": testutil.Raw("now()")}), testUserID),
		"DM creator is not the initiator": w.task(t, w.dm(t, other), testUserID),
		"task with no DM":                 w.task(t, "", testUserID),
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			issue := w.agentIssue(t, task, nil)
			if rows := followRows(t, issue); len(rows) != 0 {
				t.Fatalf("follows = %+v, want none", rows)
			}
		})
	}
}

func TestDMProgress_CallerCannotSupplyOrigin(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	rec := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "forged origin", "origin_type": "agent_create", "origin_id": w.task(t, dm, testUserID), "lineage_id": dm,
	})
	testHandler.CreateIssue(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged origin_type = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_follow WHERE lineage_id = $1`, dm); n != 0 {
		t.Fatalf("%d follows written for a forged origin", n)
	}

	// A plain member create with a lane field is just an ordinary issue.
	issue := createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "lane field", "lineage_id": dm}))
	if rows := followRows(t, issue); len(rows) != 0 {
		t.Fatalf("follows = %+v, want none for a member create", rows)
	}
}

func TestDMProgress_ChildInheritsFollow(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	parent := w.agentIssue(t, w.task(t, dm, testUserID), nil)

	child := createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "child", "parent_issue_id": parent}))
	rows := followRows(t, child)
	if len(rows) != 1 || rows[0].Lineage != dm || rows[0].Owner != testUserID || !slices.Equal(rows[0].Reasons, []string{"inherited:" + parent}) || rows[0].Muted {
		t.Fatalf("child follows = %+v, want one child follow in lineage %s inherited from %s", rows, dm, parent)
	}

	dbfx.Exec(t, `UPDATE dm_progress_follow SET muted_at = now() WHERE issue_id = $1`, parent)
	mutedChild := createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "child of muted", "parent_issue_id": parent}))
	if rows := followRows(t, mutedChild); len(rows) != 1 || !rows[0].Muted {
		t.Fatalf("child of a muted parent = %+v, want a muted follow", rows)
	}

	dbfx.Exec(t, `UPDATE dm_progress_follow SET state = 'unfollowed' WHERE issue_id = $1`, parent)
	lateChild := createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "child of unfollowed", "parent_issue_id": parent}))
	if rows := followRows(t, lateChild); len(rows) != 0 {
		t.Fatalf("child of an unfollowed parent = %+v, want none", rows)
	}
}

func progressReq(method, issueID, suffix, session string) *http.Request {
	path := "/api/issues/" + issueID + "/progress-follow" + suffix
	if session != "" {
		path += "?chat_session_id=" + session
	}
	return withURLParam(newRequest(method, path, nil), "id", issueID)
}

func TestDMProgress_MemberFollowsOnlyOwnDM(t *testing.T) {
	w := newDMWorld(t)
	other := createSecondWorkspaceMember(t)
	own, foreign := w.dm(t, testUserID), w.dm(t, other)
	issue := dbfx.Issue(t, "followed manually")

	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", foreign)).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", "00000000-0000-0000-0000-000000000000")).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", "")).Want(http.StatusBadRequest)
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_lane WHERE lineage_id = $1`, foreign); n != 0 {
		t.Fatalf("a refused follow created a lane in someone else's DM")
	}
	if rows := followRows(t, issue); len(rows) != 0 {
		t.Fatalf("follows after refusals = %+v, want none", rows)
	}

	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", own)).Want(http.StatusOK)
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", own)).Want(http.StatusOK)
	rows := followRows(t, issue)
	if len(rows) != 1 || rows[0].Lineage != own || !slices.Equal(rows[0].Reasons, []string{"manual"}) || rows[0].Owner != testUserID {
		t.Fatalf("follows = %+v, want one manual follow in %s", rows, own)
	}
}

func TestDMProgress_MemberMuteUnmuteUnfollowOwnDMOnly(t *testing.T) {
	w := newDMWorld(t)
	other := createSecondWorkspaceMember(t)
	own, foreign := w.dm(t, testUserID), w.dm(t, other)
	issue := dbfx.Issue(t, "controls")
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", own)).Want(http.StatusOK)

	// Someone else's DM, and a follow that is not there: refused, nothing changes.
	for name, h := range map[string]http.HandlerFunc{"mute": testHandler.MuteIssueProgress, "unmute": testHandler.UnmuteIssueProgress, "unfollow": testHandler.UnfollowIssueProgress} {
		method, suffix := "POST", "/"+name
		if name == "unfollow" {
			method, suffix = "DELETE", ""
		}
		testutil.Call(t, h, progressReq(method, issue, suffix, foreign)).Want(http.StatusForbidden)
	}
	if r := followRows(t, issue)[0]; r.Muted || r.State != "active" {
		t.Fatalf("refused controls changed the follow: %+v", r)
	}

	testutil.Call(t, testHandler.MuteIssueProgress, progressReq("POST", issue, "/mute", own)).Want(http.StatusOK)
	if r := followRows(t, issue)[0]; !r.Muted {
		t.Fatalf("follow not muted: %+v", r)
	}
	testutil.Call(t, testHandler.UnmuteIssueProgress, progressReq("POST", issue, "/unmute", own)).Want(http.StatusOK)
	if r := followRows(t, issue)[0]; r.Muted {
		t.Fatalf("follow still muted: %+v", r)
	}
	testutil.Call(t, testHandler.UnfollowIssueProgress, progressReq("DELETE", issue, "", own)).Want(http.StatusOK)
	if r := followRows(t, issue)[0]; r.State != "unfollowed" {
		t.Fatalf("follow not unfollowed: %+v", r)
	}
	testutil.Call(t, testHandler.UnfollowIssueProgress, progressReq("DELETE", issue, "", own)).Want(http.StatusNotFound)

	// Following again reactivates the same row.
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", own)).Want(http.StatusOK)
	if rows := followRows(t, issue); len(rows) != 1 || rows[0].State != "active" {
		t.Fatalf("follows after refollow = %+v", rows)
	}
}

func TestDMProgress_AgentFollowsOnlyItsTaskDM(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	task := w.task(t, dm, testUserID)
	issue := dbfx.Issue(t, "agent follows")

	// A lane argument is never accepted from an agent.
	testutil.Call(t, testHandler.FollowIssueProgress, withURLParam(w.agentReq("POST", "/api/issues/"+issue+"/progress-follow?chat_session_id="+dm, task, nil), "id", issue)).Want(http.StatusForbidden)
	// A task with no DM binding has no lane.
	noDM := w.task(t, "", testUserID)
	testutil.Call(t, testHandler.FollowIssueProgress, withURLParam(w.agentReq("POST", "/api/issues/"+issue+"/progress-follow", noDM, nil), "id", issue)).Want(http.StatusForbidden)
	if rows := followRows(t, issue); len(rows) != 0 {
		t.Fatalf("follows after refusals = %+v", rows)
	}

	testutil.Call(t, testHandler.FollowIssueProgress, withURLParam(w.agentReq("POST", "/api/issues/"+issue+"/progress-follow", task, nil), "id", issue)).Want(http.StatusOK)
	if rows := followRows(t, issue); len(rows) != 1 || rows[0].Lineage != dm || !slices.Equal(rows[0].Reasons, []string{"manual"}) {
		t.Fatalf("follows = %+v, want one manual follow in %s", rows, dm)
	}

	// Controls are for the member only.
	for name, h := range map[string]http.HandlerFunc{"mute": testHandler.MuteIssueProgress, "unmute": testHandler.UnmuteIssueProgress} {
		testutil.Call(t, h, withURLParam(w.agentReq("POST", "/api/issues/"+issue+"/progress-follow/"+name, task, nil), "id", issue)).Want(http.StatusForbidden)
	}
	testutil.Call(t, testHandler.UnfollowIssueProgress, withURLParam(w.agentReq("DELETE", "/api/issues/"+issue+"/progress-follow", task, nil), "id", issue)).Want(http.StatusForbidden)
	if r := followRows(t, issue)[0]; r.Muted || r.State != "active" {
		t.Fatalf("agent controls changed the follow: %+v", r)
	}
}

func TestDMProgress_ListFollowsIsOwnerOnly(t *testing.T) {
	w := newDMWorld(t)
	other := createSecondWorkspaceMember(t)
	own, foreign := w.dm(t, testUserID), w.dm(t, other)
	issue := dbfx.Issue(t, "listed")
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", own)).Want(http.StatusOK)

	list := func(session string) *testutil.Response {
		return testutil.Call(t, testHandler.ListDMProgressFollows, newRequest("GET", "/api/dm-progress/follows?chat_session_id="+session, nil))
	}
	var got struct {
		Follows []struct {
			IssueID string `json:"issue_id"`
		} `json:"follows"`
	}
	list(own).Want(http.StatusOK).JSON(&got)
	if len(got.Follows) != 1 || got.Follows[0].IssueID != issue {
		t.Fatalf("follows = %+v, want [%s]", got.Follows, issue)
	}
	list(foreign).Want(http.StatusForbidden)
}

func TestDMProgress_OwnerWhoLeftTheWorkspaceGetsNoInheritedFollow(t *testing.T) {
	w := newDMWorld(t)
	other := createSecondWorkspaceMember(t)
	dm := w.dm(t, other)
	parent := dbfx.Issue(t, "parent of a departed owner")
	dbfx.Exec(t, `INSERT INTO dm_progress_lane (lineage_id, workspace_id, owner_user_id) VALUES ($1, $2, $3)`, dm, testWorkspaceID, other)
	dbfx.Exec(t, `INSERT INTO dm_progress_follow (workspace_id, issue_id, lineage_id, owner_user_id, reasons, last_rendered_status, reporting_root_id) VALUES ($1, $2, $3, $4, '{origin}', 'todo', $2)`, testWorkspaceID, parent, dm, other)
	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, other)

	child := createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "child", "parent_issue_id": parent}))
	if rows := followRows(t, child); len(rows) != 0 {
		t.Fatalf("child follows = %+v, want none for an owner who can no longer read the issue", rows)
	}
}

func laneCount(t *testing.T, lineage string) (n int) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM dm_progress_lane WHERE lineage_id = $1`, lineage).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDMProgress_DeletingIssueRemovesItsFollows(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	issue := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	if len(followRows(t, issue)) != 1 {
		t.Fatal("setup: expected an origin follow")
	}

	req := newRequest("DELETE", "/api/issues/"+issue+"?workspace_id="+testWorkspaceID, nil)
	testutil.Call(t, testHandler.DeleteIssue, withURLParam(req, "id", issue)).Want(http.StatusNoContent)

	if rows := followRows(t, issue); len(rows) != 0 {
		t.Errorf("follows after issue delete = %+v, want none", rows)
	}
}

func TestDMProgress_DeletingChatSessionRemovesLaneAndFollows(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	issue := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	if len(followRows(t, issue)) != 1 || laneCount(t, dm) != 1 {
		t.Fatal("setup: expected an origin follow and its lane")
	}

	req := newRequest("DELETE", "/api/chat/sessions/"+dm, nil)
	req = withURLParam(req, "sessionId", dm)
	req = withChatTestWorkspaceCtx(t, req)
	testutil.Call(t, testHandler.DeleteChatSession, req).Want(http.StatusNoContent)

	if rows := followRows(t, issue); len(rows) != 0 || laneCount(t, dm) != 0 {
		t.Errorf("after chat delete: follows = %+v, lanes = %d, want none", rows, laneCount(t, dm))
	}
}

// childCreatedWhileParentFollowChanges creates a child of parent while another
// transaction holds the lane lock, then commits change (an uncommitted write to
// the parent's follow) and returns the child's id. Create reads the parent
// follow before the lane lock is free; the follow it inherits must be the one
// committed after the wait.
func childCreatedWhileParentFollowChanges(t *testing.T, lineage, parent, change string) string {
	t.Helper()
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM dm_progress_lane WHERE lineage_id = $1 FOR UPDATE`, lineage); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), change, parent); err != nil {
		t.Fatal(err)
	}

	type created struct {
		id   string
		code int
	}
	done := make(chan created, 1)
	go func() {
		rec := httptest.NewRecorder()
		testHandler.CreateIssue(rec, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "racing child", "parent_issue_id": parent}))
		var issue IssueResponse
		_ = json.NewDecoder(rec.Body).Decode(&issue)
		done <- created{issue.ID, rec.Code}
	}()
	time.Sleep(500 * time.Millisecond)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.code != http.StatusCreated {
		t.Fatalf("CreateIssue = %d", got.code)
	}
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue WHERE id = $1`, got.id) })
	return got.id
}

func TestDMProgress_ChildSeesParentMuteCommittedWhileItWaitsForTheLane(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	parent := w.agentIssue(t, w.task(t, dm, testUserID), nil)

	child := childCreatedWhileParentFollowChanges(t, dm, parent, `UPDATE dm_progress_follow SET muted_at = now() WHERE issue_id = $1`)
	if rows := followRows(t, child); len(rows) != 1 || !rows[0].Muted {
		t.Fatalf("child follows = %+v, want one muted follow", rows)
	}
}

func TestDMProgress_ChildSeesParentUnfollowCommittedWhileItWaitsForTheLane(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	parent := w.agentIssue(t, w.task(t, dm, testUserID), nil)

	child := childCreatedWhileParentFollowChanges(t, dm, parent, `UPDATE dm_progress_follow SET state = 'unfollowed' WHERE issue_id = $1`)
	if rows := followRows(t, child); len(rows) != 0 {
		t.Fatalf("child follows = %+v, want none after the parent was unfollowed", rows)
	}
}

func TestDMProgress_BrokenContinuationChainHasNoLane(t *testing.T) {
	w := newDMWorld(t)
	cyclicA := w.dm(t, testUserID)
	cyclicB := w.dm(t, testUserID, testutil.Cols{"continued_from_session_id": cyclicA})
	dbfx.Exec(t, `UPDATE chat_session SET continued_from_session_id = $2 WHERE id = $1`, cyclicA, cyclicB)

	tooDeep := w.dm(t, testUserID)
	for i := 0; i < 33; i++ {
		tooDeep = w.dm(t, testUserID, testutil.Cols{"continued_from_session_id": tooDeep})
	}

	for name, session := range map[string]string{"cyclic chain": cyclicB, "chain deeper than the bound": tooDeep} {
		issue := w.agentIssue(t, w.task(t, session, testUserID), map[string]any{"title": name})
		if rows := followRows(t, issue); len(rows) != 0 {
			t.Errorf("%s: follows = %+v, want none rather than a partial root", name, rows)
		}
		testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", issue, "", session)).Want(http.StatusForbidden)
	}
}
