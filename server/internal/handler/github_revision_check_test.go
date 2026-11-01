package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Revision check for the elevated read proxy (CHE-2216 plan 3.4, slice B2b;
// T26, T27). A read qualifies when it hashes to an admitted intake revision of
// this issue's PR (CHE-2214) or to an exact snapshot approval (CHE-2215).

const trustedAuthor, foreignEditor int64 = 42, 99

type revisionCheckFixture struct {
	elevatedFixture
	pullID   pgtype.UUID
	objectID int64
}

func newRevisionCheckFixture(t *testing.T) revisionCheckFixture {
	t.Helper()
	f := revisionCheckFixture{elevatedFixture: claimElevated(t, "multica_cross_issue"), objectID: time.Now().UnixNano()%1_000_000_000 + 1_000_000}
	f.pullID = f.linkPull(t, f.issueID)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM github_webhook_dispatch WHERE workspace_id=$1 AND object_id=$2`, testWorkspaceID, f.objectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM github_snapshot_approval WHERE workspace_id=$1 AND object_id=$2`, testWorkspaceID, f.objectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM github_webhook_revision WHERE workspace_id=$1 AND object_id=$2`, testWorkspaceID, f.objectID)
	})
	return f
}

// linkPull creates a same-repository PR row linked to issueID.
func (f revisionCheckFixture) linkPull(t *testing.T, issueID string) pgtype.UUID {
	t.Helper()
	now := time.Now().UTC()
	number := int32(time.Now().UnixNano()%1_000_000 + 1)
	pull, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: 2507001, RepoOwner: "acme", RepoName: "widget", PrNumber: number,
		Title: "revision check", State: "open", HtmlUrl: "https://github.com/acme/widget/pull/" + strconv.Itoa(int(number)),
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha: "head", ClearMergeableState: pgtype.Bool{Bool: true, Valid: true}, HeadRepoSame: true,
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	if _, err := testHandler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{IssueID: parseUUID(issueID), PullRequestID: pull.ID}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
	return pull.ID
}

func bodyHash(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

// revision records an intake revision of the fixture object on pull.
func (f revisionCheckFixture) revision(t *testing.T, pull pgtype.UUID, action, body, state string) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `INSERT INTO github_webhook_revision (workspace_id, installation_id, repository_id, pull_request_id, pr_number,
			object_kind, object_id, body_hash, event, action, sender_id, author_id, author_login, author_type, head_sha, review_state, state, reason)
		VALUES ($1, 2507001, 9001, $2, 1, 'issue_comment', $3, $4, 'issue_comment', $5, $6, $6, 'trusted', 'User', 'head', '', $7, 'test')
		RETURNING id`, testWorkspaceID, pull, f.objectID, bodyHash(body), action, trustedAuthor, state).Scan(&id)
	return id
}

// route records the intake's per-issue decision for a revision (the receipt the
// worker writes for each issue linked to the PR).
func (f revisionCheckFixture) route(t *testing.T, revisionID, issueID, state string) {
	t.Helper()
	comment, task := "NULL", "NULL"
	if state != "denied" {
		comment = "gen_random_uuid()"
	}
	if state == "dispatched" {
		task = "gen_random_uuid()"
	}
	dbfx.Exec(t, `INSERT INTO github_webhook_dispatch (workspace_id, object_kind, object_id, body_hash, issue_id, comment_id, task_id, revision_id, state, reason)
		SELECT workspace_id, object_kind, object_id, body_hash, $2, `+comment+`, `+task+`, id, $3, 'test'
		FROM github_webhook_revision WHERE id = $1`, revisionID, issueID, state)
}

type revisionRead struct {
	body         string
	editorID     int64
	lastEditedAt *time.Time
}

func (r revisionRead) request() map[string]any {
	editorType := ""
	if r.editorID != 0 {
		editorType = "User"
	}
	return map[string]any{"object_kind": "issue_comment", "object_id": 0, "author_id": trustedAuthor, "body_hash": bodyHash(r.body),
		"review_state": "", "editor_type": editorType, "editor_id": r.editorID, "last_edited_at": r.lastEditedAt}
}

func (f revisionCheckFixture) check(t *testing.T, read revisionRead) *testutil.Response {
	t.Helper()
	body := read.request()
	body["object_id"] = f.objectID
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/revision-check", body)
	return testutil.Call(t, testHandler.CheckElevatedRevision, withURLParam(req, "taskId", f.taskID))
}

func (f revisionCheckFixture) admitted(t *testing.T, read revisionRead) (bool, string) {
	t.Helper()
	var out struct {
		Admitted bool   `json:"admitted"`
		Via      string `json:"via"`
	}
	f.check(t, read).Want(http.StatusOK).JSON(&out)
	return out.Admitted, out.Via
}

func (f revisionCheckFixture) approve(t *testing.T, pull pgtype.UUID, revisionID string, read revisionRead) {
	t.Helper()
	snapshot := ghsnapshot.ApprovalSnapshot{ObjectKind: "issue_comment", ObjectID: f.objectID, AuthorID: trustedAuthor, BodyHash: bodyHash(read.body),
		EditorType: "User", EditorID: read.editorID, LastEditedAt: read.lastEditedAt}
	dbfx.Exec(t, `INSERT INTO github_snapshot_approval (workspace_id, revision_id, approver_user_id, snapshot_hash, object_kind, object_id, author_id,
			body_hash, review_state, editor_type, editor_id, last_edited_at, read_at)
		VALUES ($1, $2, gen_random_uuid(), $3, 'issue_comment', $4, $5, $6, '', 'User', $7, $8, now())`,
		testWorkspaceID, revisionID, snapshot.ContentHash(), f.objectID, trustedAuthor, snapshot.BodyHash, read.editorID, read.lastEditedAt)
}

func requireStub(t *testing.T, f revisionCheckFixture, read revisionRead, why string) {
	t.Helper()
	if ok, via := f.admitted(t, read); ok {
		t.Fatalf("%s: admitted via %q, want a stub", why, via)
	}
}

func TestRevisionCheckAdmitsAnAdmittedInitialRevisionOfThisIssue(t *testing.T) {
	f := newRevisionCheckFixture(t)
	f.route(t, f.revision(t, f.pullID, "created", "original", "dispatched"), f.issueID, "dispatched")
	if ok, via := f.admitted(t, revisionRead{body: "original"}); !ok || via != "revision" {
		t.Fatalf("admitted = %v via %q, want revision", ok, via)
	}
	requireStub(t, f, revisionRead{body: "unknown body"}, "body hash matches no revision")
}

// A projected revision is admitted too: gate G closed only stops the dispatch.
func TestRevisionCheckAdmitsAProjectedRevision(t *testing.T) {
	f := newRevisionCheckFixture(t)
	f.route(t, f.revision(t, f.pullID, "created", "original", "projected"), f.issueID, "projected")
	if ok, _ := f.admitted(t, revisionRead{body: "original"}); !ok {
		t.Fatal("projected revision not admitted")
	}
}

// T26: a trusted author's comment last edited by another user is a stub, even
// when that user restored an admitted body.
func TestRevisionCheckT26TrustedAuthorForeignEditorIsAStub(t *testing.T) {
	f := newRevisionCheckFixture(t)
	f.route(t, f.revision(t, f.pullID, "created", "original", "dispatched"), f.issueID, "dispatched")
	edited := time.Now().Add(-time.Minute).UTC()
	requireStub(t, f, revisionRead{body: "changed by someone else", editorID: foreignEditor, lastEditedAt: &edited}, "foreign edit")
	requireStub(t, f, revisionRead{body: "original", editorID: foreignEditor, lastEditedAt: &edited}, "foreign editor restored an admitted body")
}

// T27: a trusted author's body that matches no admitted or approved revision is
// a stub, including a self-edit whose signed edited event has not arrived.
func TestRevisionCheckT27UnadmittedBodyIsAStub(t *testing.T) {
	f := newRevisionCheckFixture(t)
	f.route(t, f.revision(t, f.pullID, "created", "original", "dispatched"), f.issueID, "dispatched")
	edited := time.Now().Add(-time.Minute).UTC()
	selfEdit := revisionRead{body: "self edit", editorID: trustedAuthor, lastEditedAt: &edited}
	requireStub(t, f, selfEdit, "self-edit before its signed edited event")
	for _, state := range []string{"unsigned", "quarantined", "denied", "deferred"} {
		id := f.revision(t, f.pullID, "edited", "self edit", state)
		f.route(t, id, f.issueID, "denied")
		requireStub(t, f, selfEdit, "self-edit revision in state "+state)
		dbfx.Exec(t, `DELETE FROM github_webhook_revision WHERE id=$1`, id)
	}
	f.route(t, f.revision(t, f.pullID, "edited", "self edit", "dispatched"), f.issueID, "dispatched")
	if ok, via := f.admitted(t, selfEdit); !ok || via != "revision" {
		t.Fatalf("self-edit after its admitted edited event: admitted = %v via %q", ok, via)
	}
}

func TestRevisionCheckAdmitsAnExactSnapshotApproval(t *testing.T) {
	f := newRevisionCheckFixture(t)
	edited := time.Now().Add(-time.Minute).UTC()
	foreign := revisionRead{body: "someone else's words", editorID: foreignEditor, lastEditedAt: &edited}
	id := f.revision(t, f.pullID, "created", "original", "quarantined")
	f.approve(t, f.pullID, id, foreign)
	f.route(t, id, f.issueID, "dispatched")
	if ok, via := f.admitted(t, foreign); !ok || via != "approval" {
		t.Fatalf("approved snapshot: admitted = %v via %q, want approval", ok, via)
	}
	later := edited.Add(time.Second)
	requireStub(t, f, revisionRead{body: foreign.body, editorID: foreignEditor, lastEditedAt: &later}, "same bytes, later edit")
	requireStub(t, f, revisionRead{body: foreign.body + "!", editorID: foreignEditor, lastEditedAt: &edited}, "changed bytes")
}

func TestRevisionCheckIgnoresAnotherIssuesRevisionsAndApprovals(t *testing.T) {
	f := newRevisionCheckFixture(t)
	otherIssue := dbfx.Issue(t, "revision check other issue")
	other := f.linkPull(t, otherIssue)
	f.route(t, f.revision(t, other, "created", "original", "dispatched"), otherIssue, "dispatched")
	edited := time.Now().Add(-time.Minute).UTC()
	foreign := revisionRead{body: "foreign", editorID: foreignEditor, lastEditedAt: &edited}
	approved := f.revision(t, other, "created", "foreign", "quarantined")
	f.approve(t, other, approved, foreign)
	f.route(t, approved, otherIssue, "dispatched")
	requireStub(t, f, revisionRead{body: "original"}, "admitted revision of another issue's PR")
	requireStub(t, f, foreign, "approval for another issue")
}

// linkAllowedIssue links a second issue to the fixture's PR, standing for the
// issue intake admitted while this fixture's issue was denied.
func (f revisionCheckFixture) linkAllowedIssue(t *testing.T) string {
	t.Helper()
	issueID := dbfx.Issue(t, "revision check allowed issue")
	if _, err := testHandler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{IssueID: parseUUID(issueID), PullRequestID: f.pullID}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
	return issueID
}

// One PR can link several issues. Intake decides per issue, so a revision denied
// for this issue stays a stub here even when the PR's other issue was admitted.
func TestRevisionCheckRevisionBindsToThisIssuesOwnIntakeDecision(t *testing.T) {
	f := newRevisionCheckFixture(t)
	allowedIssue := f.linkAllowedIssue(t)
	id := f.revision(t, f.pullID, "created", "original", "projected")
	f.route(t, id, allowedIssue, "projected")
	f.route(t, id, f.issueID, "denied")
	requireStub(t, f, revisionRead{body: "original"}, "revision denied for this issue, admitted for the PR's other issue")
}

// An approval opens the read only for the issues its approver could route.
func TestRevisionCheckApprovalBindsToThisIssuesOwnIntakeDecision(t *testing.T) {
	f := newRevisionCheckFixture(t)
	allowedIssue := f.linkAllowedIssue(t)
	edited := time.Now().Add(-time.Minute).UTC()
	foreign := revisionRead{body: "foreign", editorID: foreignEditor, lastEditedAt: &edited}
	id := f.revision(t, f.pullID, "created", "foreign", "dispatched")
	f.approve(t, f.pullID, id, foreign)
	f.route(t, id, allowedIssue, "dispatched")
	f.route(t, id, f.issueID, "denied")
	requireStub(t, f, foreign, "approval whose approver could not route this issue")
}

func TestRevisionCheckRefusesATaskThatIsNotElevated(t *testing.T) {
	f := newRevisionCheckFixture(t)
	f.route(t, f.revision(t, f.pullID, "created", "original", "dispatched"), f.issueID, "dispatched")
	read := revisionRead{body: "original"}

	dbfx.Exec(t, `UPDATE task_elevation SET state='fencing', epoch=epoch+1 WHERE task_id=$1`, f.taskID)
	f.check(t, read).Want(http.StatusForbidden)
	dbfx.Exec(t, `UPDATE task_elevation SET state='elevated' WHERE task_id=$1`, f.taskID)
	f.admitted(t, read)

	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids='{taint-1}' WHERE id=$1`, f.taskID)
	f.check(t, read).Want(http.StatusForbidden)
	dbfx.Exec(t, `DELETE FROM task_elevation WHERE task_id=$1`, f.taskID)
	f.check(t, read).Want(http.StatusForbidden)
}

func TestRevisionCheckRejectsAMalformedRead(t *testing.T) {
	f := newRevisionCheckFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"short body hash":    func(m map[string]any) { m["body_hash"] = "abc" },
		"unknown kind":       func(m map[string]any) { m["object_kind"] = "commit" },
		"non-positive id":    func(m map[string]any) { m["object_id"] = 0 },
		"unknown field":      func(m map[string]any) { m["extra"] = true },
		"editor without id":  func(m map[string]any) { m["editor_type"], m["editor_id"] = "User", 0 },
		"editor id no type":  func(m map[string]any) { m["editor_type"], m["editor_id"] = "", 5 },
		"non-user author id": func(m map[string]any) { m["author_id"] = 0 },
	} {
		body := revisionRead{body: "original"}.request()
		body["object_id"] = f.objectID
		mutate(body)
		req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/revision-check", body)
		if got := testutil.Call(t, testHandler.CheckElevatedRevision, withURLParam(req, "taskId", f.taskID)); got.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, got.Code)
		}
	}
}
