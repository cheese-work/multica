package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8961 (CHE-8945 plan r6, step 1): the claim-time fence for one-shot
// `at --watch issue` wakeups. Tests C1-C8 of the plan. The fence is inert in
// this PR: it only writes a `watch` v2 key into the claim snapshot.

const watchFenceSnapshotKey = "watch"

// claimIssueTask queues one assignment task for agentID on issueID, claims it
// and returns the stored snapshot as raw JSON keys. The runtime is private to
// the caller, so the claim cannot pick up another test's task.
func claimIssueTask(t *testing.T, runtimeID, agentID, issueID string) map[string]json.RawMessage {
	t.Helper()
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
	})
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
	req = withURLParam(req, "runtimeId", runtimeID)
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)

	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT issue_snapshot FROM agent_task_queue WHERE id = $1`, taskID).Scan(&raw); err != nil {
		t.Fatalf("read back issue_snapshot: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("stored snapshot is not a JSON object: %q: %v", string(raw), err)
	}
	return keys
}

func newFenceFixture(t *testing.T, label string) (runtimeID, agentID, issueID string) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID = createClaimReclaimRuntime(t, ctx, "watch fence runtime "+label)
	agentID, issueID = createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "watch fence agent "+label)
	return runtimeID, agentID, issueID
}

func fenceUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("bad uuid %q: %v", s, err)
	}
	return u
}

// readFence runs the projection with no run credit (the claim-time shape).
func readFence(t *testing.T, q *db.Queries, issueID string) db.IssueWatchFenceRow {
	t.Helper()
	row, err := q.IssueWatchFence(context.Background(), db.IssueWatchFenceParams{IssueID: fenceUUID(t, issueID)})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	return row
}

// C1: an issue-bound claim on an issue with nothing attached stores watch v2.
func TestIssueWatchFence_C1_ClaimStoresWatchV2(t *testing.T) {
	runtimeID, agentID, issueID := newFenceFixture(t, "c1")
	for i := 0; i < 3; i++ {
		dbfx.Comment(t, issueID, fmt.Sprintf("context %d", i))
	}
	want := readFence(t, testHandler.Queries, issueID)

	keys := claimIssueTask(t, runtimeID, agentID, issueID)
	rawWatch, ok := keys[watchFenceSnapshotKey]
	if !ok {
		t.Fatalf("claim snapshot has no %q key: %v", watchFenceSnapshotKey, keys)
	}
	var watch struct {
		V        int    `json:"v"`
		Row      string `json:"row"`
		Revision int64  `json:"revision"`
		Comments string `json:"comments"`
		At       string `json:"at"`
	}
	if err := json.Unmarshal(rawWatch, &watch); err != nil {
		t.Fatalf("decode watch: %v", err)
	}
	if watch.V != 2 {
		t.Errorf("watch.v = %d, want 2", watch.V)
	}
	if watch.Row != want.RowHash {
		t.Errorf("watch.row = %q, want the row hash %q", watch.Row, want.RowHash)
	}
	if watch.Revision != want.Revision {
		t.Errorf("watch.revision = %d, want %d", watch.Revision, want.Revision)
	}
	if watch.Comments != want.CommentHash {
		t.Errorf("watch.comments = %q, want %q", watch.Comments, want.CommentHash)
	}
	if watch.At == "" {
		t.Errorf("watch.at must carry the projection's read time")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(watch.Row) {
		t.Errorf("watch.row must be an md5 digest, got %q", watch.Row)
	}
	// The existing snapshot fields stay.
	if _, ok := keys["title_sha256"]; !ok {
		t.Errorf("title_sha256 missing: the MUL-7344 snapshot must be unchanged")
	}
}

// C2: a snapshot that carries `watch` still decodes, and the MUL-7344 delta
// compares only title and description.
func TestIssueWatchFence_C2_ReaderIgnoresWatchKey(t *testing.T) {
	runtimeID, agentID, issueID := newFenceFixture(t, "c2")
	name := "watch fence agent c2"

	withWatch := func(description string) string {
		return fmt.Sprintf(
			`{"v":%d,"title_sha256":%q,"description_sha256":%q,"watch":{"v":2,"row":"%032d","revision":9,"comments":"%032d","at":"2026-11-21T00:00:00Z"}}`,
			issueSnapshotVersion, sha256Hex(name+" issue"), sha256Hex(description), 0, 0)
	}
	if _, ok := decodeIssueStateSnapshot([]byte(withWatch(""))); !ok {
		t.Fatalf("a snapshot that carries a watch key must still decode")
	}

	seedPriorRunWithSnapshot(t, agentID, runtimeID, issueID, withWatch(""))
	createCommentTriggeredClaimTask(t, context.Background(), agentID, runtimeID, issueID, nil)
	resp := claimCommentTask(t, runtimeID, "watch-c2-unchanged")
	if !resp.Task.IssueStateDeltaKnown || len(resp.Task.IssueChangedFields) != 0 {
		t.Errorf("delta known=%v changed=%v, want known and empty", resp.Task.IssueStateDeltaKnown, resp.Task.IssueChangedFields)
	}
}

func TestIssueWatchFence_C2_DeltaStillNamesOnlyTitleAndDescription(t *testing.T) {
	runtimeID, agentID, issueID := newFenceFixture(t, "c2b")
	name := "watch fence agent c2b"
	snap := fmt.Sprintf(
		`{"v":%d,"title_sha256":%q,"description_sha256":%q,"watch":{"v":2,"row":"x","revision":1,"comments":"y","at":"2026-11-21T00:00:00Z"}}`,
		issueSnapshotVersion, sha256Hex(name+" issue"), sha256Hex("older description"))
	seedPriorRunWithSnapshot(t, agentID, runtimeID, issueID, snap)
	createCommentTriggeredClaimTask(t, context.Background(), agentID, runtimeID, issueID, nil)
	resp := claimCommentTask(t, runtimeID, "watch-c2-changed")
	if got := strings.Join(resp.Task.IssueChangedFields, ","); got != "description" {
		t.Errorf("issue_changed_fields = %q, want \"description\"", got)
	}
}

// C3: a failing or timed-out fence read leaves `watch` out and the claim
// still succeeds (fail-open).
func TestIssueWatchFence_C3_FailedReadLeavesWatchOut(t *testing.T) {
	cases := map[string]func(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (db.IssueWatchFenceRow, error){
		"error": func(context.Context, *db.Queries, pgtype.UUID) (db.IssueWatchFenceRow, error) {
			return db.IssueWatchFenceRow{}, errors.New("boom")
		},
		// pg_sleep(1) outlives the 300 ms deadline the claim sets.
		"timeout": func(ctx context.Context, q *db.Queries, _ pgtype.UUID) (db.IssueWatchFenceRow, error) {
			_, err := testPool.Exec(ctx, `SELECT pg_sleep(1)`)
			return db.IssueWatchFenceRow{}, err
		},
	}
	for label, fake := range cases {
		t.Run(label, func(t *testing.T) {
			runtimeID, agentID, issueID := newFenceFixture(t, "c3 "+label)
			prev := issueWatchFenceRead
			issueWatchFenceRead = fake
			t.Cleanup(func() { issueWatchFenceRead = prev })

			keys := claimIssueTask(t, runtimeID, agentID, issueID)
			if _, ok := keys[watchFenceSnapshotKey]; ok {
				t.Errorf("a failed fence read must leave the watch key out, got %s", keys[watchFenceSnapshotKey])
			}
			if _, ok := keys["title_sha256"]; !ok {
				t.Errorf("the claim must still record the MUL-7344 snapshot")
			}
		})
	}
}

// C4: every INSERT INTO comment sits in a statement that also bumps
// issue.revision, so the credit rule "one bump per own comment" holds.
func TestIssueWatchFence_C4_CommentInsertsBumpIssueRevision(t *testing.T) {
	bump := regexp.MustCompile(`(?is)UPDATE\s+issue\b.*?\brevision\s*=\s*(issue\.)?revision\s*\+\s*1`)
	found := 0
	for name, stmt := range queryStatements(t) {
		if !regexp.MustCompile(`(?i)INSERT\s+INTO\s+comment\b`).MatchString(stmt) {
			continue
		}
		found++
		if !bump.MatchString(stmt) {
			t.Errorf("%s inserts a comment without bumping issue.revision in the same statement", name)
		}
	}
	if found < 3 {
		t.Errorf("found %d comment-insert statements, expected at least 3 (comment, supplement, github intake): the scan is broken", found)
	}
}

// C8: every UPDATE comment either bumps the comment's revision or sets a
// column of the pristine list; the rest sit on an allowlist with a reason.
var pristineCommentColumns = []string{
	"decision_intent_id", "decision_request_id", "decision_answer_id",
	"decision_comment_kind", "recovery_settled_at", "deleted_at", "resolved_at",
}

var updateCommentAllowlist = map[string]string{
	// Detaches parent_id while the whole workspace is being deleted.
	"workspace_delete.sql": "parent_id",
}

func TestIssueWatchFence_C8_NonBumpingCommentUpdatesAreListed(t *testing.T) {
	setClause := regexp.MustCompile(`(?is)UPDATE\s+comment\b(?:\s+\w+)?\s+SET\b(.*?)(?:\bWHERE\b|;|\z)`)
	revisionBump := regexp.MustCompile(`(?i)\brevision\s*=\s*(comment\.)?revision\s*\+`)
	pristine := regexp.MustCompile(`(?i)\b(` + strings.Join(pristineCommentColumns, "|") + `)\s*=`)
	updates := 0
	allowed := map[string]bool{}
	for name, stmt := range queryStatements(t) {
		for _, m := range setClause.FindAllStringSubmatch(stmt, -1) {
			updates++
			set := m[1]
			if revisionBump.MatchString(set) || pristine.MatchString(set) {
				continue
			}
			file := strings.SplitN(name, ":", 2)[0]
			col, ok := updateCommentAllowlist[file]
			if !ok || !regexp.MustCompile(`(?i)\b`+col+`\s*=`).MatchString(set) {
				t.Errorf("%s: UPDATE comment neither bumps revision nor sets a pristine-list column; "+
					"add the column to the fence's pristine filter or allowlist it with a reason: %s",
					name, strings.TrimSpace(set))
				continue
			}
			allowed[file] = true
		}
	}
	if updates < 10 {
		t.Errorf("found %d UPDATE comment statements, expected at least 10: the scan is broken", updates)
	}
	for file := range updateCommentAllowlist {
		if !allowed[file] {
			t.Errorf("allowlist entry %s no longer matches a statement: remove it", file)
		}
	}
}

// C8 (second half): no Go source outside tests writes the comment table.
func TestIssueWatchFence_C8_NoCommentUpdateInGoSource(t *testing.T) {
	re := regexp.MustCompile(`(?i)\bupdate\s+comment(\s+\w+)?\s+set\b`)
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// generated holds the sqlc copy of the .sql files scanned above.
			if d.Name() == "generated" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if re.Match(src) {
			t.Errorf("%s writes the comment table in Go source; move it to server/pkg/db/queries so tripwire C8 sees it", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// queryStatements returns every named sqlc statement keyed "file:name".
func queryStatements(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "pkg", "db", "queries", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no query files found: %v", err)
	}
	header := regexp.MustCompile(`(?m)^-- name: (\w+)`)
	out := map[string]string{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		locs := header.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			out[filepath.Base(f)+":"+text[loc[2]:loc[3]]] = text[loc[0]:end]
		}
	}
	return out
}

// C5: the row hash ignores R's own comment insert and MarkIssueFirstExecuted,
// and moves when any other issue column moves.
func TestIssueWatchFence_C5_RowHashIgnoresOwnWrites(t *testing.T) {
	runtimeID, agentID, issueID := newFenceFixture(t, "c5")
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
	q := testHandler.Queries

	fence := readFence(t, q, issueID)

	if _, err := q.CreateComment(context.Background(), db.CreateCommentParams{
		IssueID:      fenceUUID(t, issueID),
		WorkspaceID:  fenceUUID(t, testWorkspaceID),
		AuthorType:   "agent",
		AuthorID:     fenceUUID(t, agentID),
		Content:      "R's own comment",
		Type:         "comment",
		SourceTaskID: fenceUUID(t, taskID),
	}); err != nil {
		t.Fatalf("create R's comment: %v", err)
	}
	if _, err := q.MarkIssueFirstExecuted(context.Background(), fenceUUID(t, issueID)); err != nil {
		t.Fatalf("mark first executed: %v", err)
	}

	got, err := q.IssueWatchFence(context.Background(), db.IssueWatchFenceParams{
		IssueID: fenceUUID(t, issueID),
		RunID:   fenceUUID(t, taskID),
		AgentID: fenceUUID(t, agentID),
		FenceAt: fence.ReadAt,
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if got.RowHash != fence.RowHash {
		t.Errorf("row hash moved on R's comment insert / MarkIssueFirstExecuted: %s -> %s", fence.RowHash, got.RowHash)
	}
	if got.Credit != 1 {
		t.Errorf("credit = %d, want 1 (R's one pristine fresh comment)", got.Credit)
	}
	if got.CommentHash != fence.CommentHash {
		t.Errorf("comment hash moved on R's pristine fresh comment: %s -> %s", fence.CommentHash, got.CommentHash)
	}
	if got.Revision-fence.Revision != got.Credit {
		t.Errorf("revision moved by %d, credit %d: the insert must bump the issue exactly once", got.Revision-fence.Revision, got.Credit)
	}

	// A member's comment is not credited and changes the comment hash.
	dbfx.Comment(t, issueID, "someone else")
	other, err := q.IssueWatchFence(context.Background(), db.IssueWatchFenceParams{
		IssueID: fenceUUID(t, issueID), RunID: fenceUUID(t, taskID),
		AgentID: fenceUUID(t, agentID), FenceAt: fence.ReadAt,
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if other.CommentHash == fence.CommentHash {
		t.Errorf("a comment by another author must change the comment hash")
	}
	if other.Credit != 1 {
		t.Errorf("credit = %d after a member comment, want 1: only R's pristine comments count", other.Credit)
	}
}

// issueColumnMutations maps each mutable `issue` column to an UPDATE
// expression that yields a different value and satisfies its CHECKs. Foreign
// keys are skipped by running the transaction with session_replication_role =
// replica, and the transaction is rolled back.
var issueColumnMutations = map[string]string{
	"workspace_id":              "gen_random_uuid()",
	"title":                     "title || 'x'",
	"description":               "COALESCE(description, '') || 'x'",
	"status":                    "CASE WHEN status = 'zz_x' THEN 'zz_y' ELSE 'zz_x' END",
	"priority":                  "CASE WHEN priority = 'urgent' THEN 'low' ELSE 'urgent' END",
	"assignee_type":             "CASE WHEN assignee_type = 'member' THEN 'agent' ELSE 'member' END",
	"assignee_id":               "gen_random_uuid()",
	"creator_type":              "CASE WHEN creator_type = 'member' THEN 'agent' ELSE 'member' END",
	"creator_id":                "gen_random_uuid()",
	"parent_issue_id":           "gen_random_uuid()",
	"acceptance_criteria":       "'[\"x\"]'::jsonb",
	"context_refs":              "'[\"x\"]'::jsonb",
	"position":                  "position + 1",
	"due_date":                  "COALESCE(due_date, current_date) + 1",
	"created_at":                "created_at + interval '1 second'",
	"number":                    "number + 1000000",
	"project_id":                "gen_random_uuid()",
	"origin_type":               "CASE WHEN origin_type = 'autopilot' THEN 'quick_create' ELSE 'autopilot' END",
	"origin_id":                 "gen_random_uuid()",
	"start_date":                "COALESCE(start_date, current_date) + 1",
	"metadata":                  "'{\"k\":\"v\"}'::jsonb",
	"stage":                     "COALESCE(stage, 0) + 1",
	"properties":                "'{\"k\":\"v\"}'::jsonb",
	"triage_state":              "CASE WHEN triage_state IS NULL THEN 'pending' ELSE NULL END",
	"duplicate_of_issue_id":     "gen_random_uuid()",
	"workflow_id":               "gen_random_uuid()",
	"requested_by_id":           "gen_random_uuid()",
	"max_runs_override":         "COALESCE(max_runs_override, 0) + 1",
	"description_external_read": "NOT description_external_read",
	"created_via":               "COALESCE(created_via, '') || 'x'",
}

// Columns the row hash leaves out on purpose, and the lookup key.
var issueColumnsOutsideRowHash = map[string]bool{
	"revision": true, "updated_at": true, "last_activity_at": true, "first_executed_at": true,
}

func TestIssueWatchFence_C5_RowHashCoversEveryOtherColumn(t *testing.T) {
	_, _, issueID := newFenceFixture(t, "c5 columns")
	ctx := context.Background()

	rows, err := testPool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'issue' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
	}
	rows.Close()

	for _, col := range columns {
		if col == "id" || issueColumnsOutsideRowHash[col] {
			continue
		}
		if _, ok := issueColumnMutations[col]; !ok {
			t.Errorf("issue column %q has no mutation in issueColumnMutations: add one so the row hash is proven to cover it", col)
		}
	}

	for _, col := range columns {
		mut, mutable := issueColumnMutations[col]
		excluded := issueColumnsOutsideRowHash[col]
		if !mutable && !excluded {
			continue
		}
		if excluded {
			mut = map[string]string{
				"revision":          "revision + 5",
				"updated_at":        "updated_at + interval '1 minute'",
				"last_activity_at":  "COALESCE(last_activity_at, now()) + interval '1 minute'",
				"first_executed_at": "COALESCE(first_executed_at, now()) + interval '1 minute'",
			}[col]
		}
		t.Run(col, func(t *testing.T) {
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				t.Skipf("cannot disable FK triggers (needs a superuser role): %v", err)
			}
			q := db.New(tx)
			before := readFence(t, q, issueID)
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE issue SET %s = %s WHERE id = $1`, col, mut), issueID); err != nil {
				t.Fatalf("mutate %s: %v", col, err)
			}
			after := readFence(t, q, issueID)
			if excluded && after.RowHash != before.RowHash {
				t.Errorf("row hash moved on excluded column %s", col)
			}
			if !excluded && after.RowHash == before.RowHash {
				t.Errorf("row hash did not move when %s changed", col)
			}
		})
	}
}

// C6: anything `issue get` returns from another table that the fence does not
// cover makes the projection report attached, and the claim stores no watch.
func TestIssueWatchFence_C6_AttachedMeansNoWatch(t *testing.T) {
	seed := map[string]func(t *testing.T, issueID, commentID string){
		"label": func(t *testing.T, issueID, _ string) {
			var labelID string
			dbfx.QueryRow(t, `INSERT INTO issue_label (workspace_id, name, color)
				VALUES ($1, 'c6-'||gen_random_uuid()::text, '#fff') RETURNING id`, testWorkspaceID).Scan(&labelID)
			dbfx.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, issueID, labelID)
		},
		"issue attachment": func(t *testing.T, issueID, _ string) {
			dbfx.Exec(t, `INSERT INTO attachment (workspace_id, issue_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
				VALUES ($1, $2, 'member', $3, 'f.txt', 'u', 'text/plain', 1)`, testWorkspaceID, issueID, testUserID)
		},
		"comment attachment": func(t *testing.T, _, commentID string) {
			dbfx.Exec(t, `INSERT INTO attachment (workspace_id, comment_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
				VALUES ($1, $2, 'member', $3, 'f.txt', 'u', 'text/plain', 1)`, testWorkspaceID, commentID, testUserID)
		},
		"issue reaction": func(t *testing.T, issueID, _ string) {
			dbfx.Exec(t, `INSERT INTO issue_reaction (issue_id, workspace_id, actor_type, actor_id, emoji)
				VALUES ($1, $2, 'member', $3, '👍')`, issueID, testWorkspaceID, testUserID)
		},
		"comment reaction": func(t *testing.T, _, commentID string) {
			dbfx.Exec(t, `INSERT INTO comment_reaction (comment_id, workspace_id, actor_type, actor_id, emoji)
				VALUES ($1, $2, 'member', $3, '👍')`, commentID, testWorkspaceID, testUserID)
		},
		"attached source context": func(t *testing.T, issueID, commentID string) {
			dbfx.Exec(t, `INSERT INTO issue_source_context
				(id, workspace_id, issue_id, source_issue_id, anchor_comment_id, captured_by_user_id,
				 snapshot_version, snapshot, capture_digest, state, attached_at)
				VALUES (gen_random_uuid(), $1, $2, $2, $3, $4, 1, '{}'::jsonb, 'd', 'attached', now())`,
				testWorkspaceID, issueID, commentID, testUserID)
		},
	}
	for label, fn := range seed {
		t.Run(label, func(t *testing.T) {
			runtimeID, agentID, issueID := newFenceFixture(t, "c6 "+label)
			commentID := dbfx.Comment(t, issueID, "anchor")
			fn(t, issueID, commentID)

			if !readFence(t, testHandler.Queries, issueID).Attached {
				t.Errorf("projection must report attached for %s", label)
			}
			keys := claimIssueTask(t, runtimeID, agentID, issueID)
			if _, ok := keys[watchFenceSnapshotKey]; ok {
				t.Errorf("%s: an issue with attached rows must get no watch key", label)
			}
		})
	}
}

// explainRecorder captures the projection's SQL and arguments so the test can
// EXPLAIN exactly what sqlc generated.
type explainRecorder struct {
	db.DBTX
	sql  string
	args []any
}

func (r *explainRecorder) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.sql, r.args = sql, args
	return r.DBTX.QueryRow(ctx, sql, args...)
}
func (r *explainRecorder) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return r.DBTX.Exec(ctx, sql, args...)
}
func (r *explainRecorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return r.DBTX.Query(ctx, sql, args...)
}

type planNode struct {
	NodeType    string     `json:"Node Type"`
	Relation    string     `json:"Relation Name"`
	ActualRows  float64    `json:"Actual Rows"`
	ActualLoops float64    `json:"Actual Loops"`
	RowsRemoved float64    `json:"Rows Removed by Filter"`
	RowsRecheck float64    `json:"Rows Removed by Index Recheck"`
	Plans       []planNode `json:"Plans"`
}

func (n planNode) walk(f func(planNode)) {
	f(n)
	for _, c := range n.Plans {
		c.walk(f)
	}
}

// C7: work bound, checked in the database.
func TestIssueWatchFence_C7_WorkBound(t *testing.T) {
	runtimeID, agentID, issueID := newFenceFixture(t, "c7")
	ctx := context.Background()
	insert := func(n int) {
		dbfx.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content)
			SELECT $1, $2, 'member', $3, 'c' || g FROM generate_series(1, $4::int) g`,
			issueID, testWorkspaceID, testUserID, n)
	}
	insert(250)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rec := &explainRecorder{DBTX: tx}
	got, err := db.New(rec).IssueWatchFence(ctx, db.IssueWatchFenceParams{IssueID: fenceUUID(t, issueID)})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if got.CommentRows != 201 {
		t.Errorf("comment rows = %d on a 250-comment issue, want 201 (the cap proves itself)", got.CommentRows)
	}

	var planJSON []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+rec.sql, rec.args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) == 0 {
		t.Fatalf("decode plan: %v: %s", err, planJSON)
	}
	commentNodes := 0
	plans[0].Plan.walk(func(n planNode) {
		// A Sort whose subtree scans comment sorts comment rows, so it would read
		// every row before the Limit. The ordered string_agg sorts only the
		// <=201 kept rows from the CTE, which has no comment relation of its own.
		if n.NodeType == "Sort" {
			hasComment := false
			n.walk(func(c planNode) {
				if c.Relation == "comment" {
					hasComment = true
				}
			})
			if hasComment {
				t.Errorf("the projection must not sort comment rows below the Limit: %s", planJSON)
			}
		}
		if n.Relation == "comment" {
			commentNodes++
			if n.ActualRows*n.ActualLoops > 201 {
				t.Errorf("%s on comment returned %v rows (loops %v), want at most 201", n.NodeType, n.ActualRows, n.ActualLoops)
			}
		}
	})
	if commentNodes == 0 {
		t.Fatalf("plan has no comment scan: %s", planJSON)
	}

	// Over the cap: no watch key. Exactly at the cap: stored.
	keys := claimIssueTask(t, runtimeID, agentID, issueID)
	if _, ok := keys[watchFenceSnapshotKey]; ok {
		t.Errorf("250 comments must leave the watch key out")
	}

	runtime2, agent2, issue2 := newFenceFixture(t, "c7 at cap")
	dbfx.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content)
		SELECT $1, $2, 'member', $3, 'c' || g FROM generate_series(1, 200) g`, issue2, testWorkspaceID, testUserID)
	keys = claimIssueTask(t, runtime2, agent2, issue2)
	if _, ok := keys[watchFenceSnapshotKey]; !ok {
		t.Errorf("200 comments must still store the watch key")
	}
}
