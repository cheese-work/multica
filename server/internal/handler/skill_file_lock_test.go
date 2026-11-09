package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// The approval of an agent.skills.set action holds its snapshot of a skill
// with `FOR SHARE NOWAIT` on the parent skill row. Every writer of the skill's
// files or of the skill row itself must conflict with that lock, and must hold
// its own conflicting lock to commit. skillSnapshotLock plays the approval.

type skillSnapshotLock struct {
	test *testing.T
	tx   pgx.Tx
}

const skillFilesDigestSQL = `SELECT COALESCE(string_agg(path || '=' || content, ';' ORDER BY path), '') FROM skill_file WHERE skill_id = $1`

func lockSkillSnapshot(test *testing.T, skillID string) (*skillSnapshotLock, error) {
	test.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = tx.Rollback(ctx) })
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM skill WHERE id = $1 FOR SHARE NOWAIT`, skillID).Scan(&id); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return &skillSnapshotLock{test: test, tx: tx}, nil
}

func (l *skillSnapshotLock) readback(skillID string) string {
	l.test.Helper()
	var digest string
	if err := l.tx.QueryRow(context.Background(), skillFilesDigestSQL, skillID).Scan(&digest); err != nil {
		l.test.Fatal(err)
	}
	return digest
}

func (l *skillSnapshotLock) commit() {
	l.test.Helper()
	if err := l.tx.Commit(context.Background()); err != nil {
		l.test.Fatal(err)
	}
}

func liveSkillFilesDigest(test *testing.T, skillID string) string {
	test.Helper()
	var digest string
	dbfx.QueryRow(test, skillFilesDigestSQL, skillID).Scan(&digest)
	return digest
}

// skillWriterFixture is a skill with one supporting file, optionally assigned
// to an agent. The unassigned skill is the proposed-only case.
type skillWriterFixture struct {
	skillID string
	fileID  string
}

func newSkillWriterFixture(test *testing.T, assigned bool) skillWriterFixture {
	test.Helper()
	skillID := dbfx.Insert(test, "skill", testutil.Cols{
		"workspace_id": testWorkspaceID, "created_by": testUserID,
		"name": "lock-" + time.Now().Format("150405.000000000"), "description": "d", "content": "# c",
	})
	fileID := dbfx.Insert(test, "skill_file", testutil.Cols{"skill_id": skillID, "path": "a.md", "content": "old"})
	if assigned {
		agentID := dbfx.Agent(test, "Skill lock agent", testRuntimeID, testutil.Cols{"workspace_id": testWorkspaceID, "owner_id": testUserID})
		dbfx.InsertNoID(test, "agent_skill", testutil.Cols{"agent_id": agentID, "skill_id": skillID}, "agent_id=$1 AND skill_id=$2", agentID, skillID)
	}
	return skillWriterFixture{skillID: skillID, fileID: fileID}
}

// skillWriter runs one writer against the handler and returns its HTTP status.
type skillWriter struct {
	name string
	// run calls the writer and returns its status code.
	run func(test *testing.T, f skillWriterFixture) int
	// wantStatus is the status of a writer that completes.
	wantStatus int
	// blockOn takes the lock a writer waits on after it holds its own lock;
	// nil when the writer cannot be parked that way.
	blockOn func(test *testing.T, f skillWriterFixture) (release func())
	// settled reports whether the writer's change is visible.
	settled func(test *testing.T, f skillWriterFixture) bool
}

func holdFileRow(test *testing.T, f skillWriterFixture) func() {
	return holdRowLock(test, `SELECT id FROM skill_file WHERE id = $1 FOR UPDATE`, f.fileID)
}

func skillFileWriters() []skillWriter {
	call := func(handler http.HandlerFunc, req *http.Request) int {
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	return []skillWriter{
		{
			name:       "file edit",
			wantStatus: http.StatusOK,
			run: func(test *testing.T, f skillWriterFixture) int {
				req := newRequest(http.MethodPut, "/api/skills/"+f.skillID+"/files", CreateSkillFileRequest{Path: "a.md", Content: "new"})
				return call(testHandler.UpsertSkillFile, withURLParam(req, "id", f.skillID))
			},
			blockOn: holdFileRow,
			settled: func(test *testing.T, f skillWriterFixture) bool {
				return liveSkillFilesDigest(test, f.skillID) == "a.md=new"
			},
		},
		{
			name:       "file add",
			wantStatus: http.StatusOK,
			run: func(test *testing.T, f skillWriterFixture) int {
				req := newRequest(http.MethodPut, "/api/skills/"+f.skillID+"/files", CreateSkillFileRequest{Path: "b.md", Content: "added"})
				return call(testHandler.UpsertSkillFile, withURLParam(req, "id", f.skillID))
			},
			// An uncommitted insert of the same path parks the writer on the unique index.
			blockOn: func(test *testing.T, f skillWriterFixture) func() {
				return holdRowLock(test, `INSERT INTO skill_file (skill_id, path, content) VALUES ($1, 'b.md', 'racer')`, f.skillID)
			},
			settled: func(test *testing.T, f skillWriterFixture) bool {
				return liveSkillFilesDigest(test, f.skillID) == "a.md=old;b.md=added"
			},
		},
		{
			name:       "file remove",
			wantStatus: http.StatusNoContent,
			run: func(test *testing.T, f skillWriterFixture) int {
				req := newRequest(http.MethodDelete, "/api/skills/"+f.skillID+"/files/"+f.fileID, nil)
				rctx := chi.NewRouteContext()
				rctx.URLParams.Add("id", f.skillID)
				rctx.URLParams.Add("fileId", f.fileID)
				return call(testHandler.DeleteSkillFile, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
			},
			blockOn: holdFileRow,
			settled: func(test *testing.T, f skillWriterFixture) bool { return liveSkillFilesDigest(test, f.skillID) == "" },
		},
	}
}

func skillParentRowWriters() []skillWriter {
	call := func(handler http.HandlerFunc, req *http.Request) int {
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	return []skillWriter{
		{
			name:       "UpdateSkill",
			wantStatus: http.StatusOK,
			run: func(test *testing.T, f skillWriterFixture) int {
				desc := "changed"
				files := []CreateSkillFileRequest{{Path: "a.md", Content: "replaced"}}
				req := newRequest(http.MethodPut, "/api/skills/"+f.skillID, UpdateSkillRequest{Description: &desc, Files: files})
				return call(testHandler.UpdateSkill, withURLParam(req, "id", f.skillID))
			},
			blockOn: holdFileRow,
			settled: func(test *testing.T, f skillWriterFixture) bool {
				return liveSkillFilesDigest(test, f.skillID) == "a.md=replaced"
			},
		},
		{
			name:       "import refresh",
			wantStatus: http.StatusOK,
			run: func(test *testing.T, f skillWriterFixture) int {
				_, err := testHandler.overwriteSkillWithFiles(context.Background(), newRequest(http.MethodPost, "/api/skills/"+f.skillID+"/refresh", nil), skillOverwriteInput{
					WorkspaceID:   parseUUID(testWorkspaceID),
					TargetSkillID: parseUUID(f.skillID),
					UserID:        testUserID,
					Description:   "refreshed",
					Content:       "# refreshed",
					Files:         []CreateSkillFileRequest{{Path: "a.md", Content: "refreshed"}},
				})
				if err != nil {
					test.Errorf("overwriteSkillWithFiles: %v", err)
					return http.StatusInternalServerError
				}
				return http.StatusOK
			},
			blockOn: holdFileRow,
			settled: func(test *testing.T, f skillWriterFixture) bool {
				return liveSkillFilesDigest(test, f.skillID) == "a.md=refreshed"
			},
		},
		{
			name:       "DeleteSkill",
			wantStatus: http.StatusNoContent,
			run: func(test *testing.T, f skillWriterFixture) int {
				req := newRequest(http.MethodDelete, "/api/skills/"+f.skillID, nil)
				return call(testHandler.DeleteSkill, withURLParam(req, "id", f.skillID))
			},
			// The cascade to the file rows waits on the held row after the skill row is deleted.
			blockOn: holdFileRow,
			settled: func(test *testing.T, f skillWriterFixture) bool {
				return dbfx.Count(test, `SELECT count(*) FROM skill WHERE id = $1`, f.skillID) == 0
			},
		},
	}
}

func waitForSkillFileLockWaiter(test *testing.T) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if dbfx.Count(test, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	test.Fatal("the writer never parked on a lock")
}

func runAsync(test *testing.T, writer skillWriter, f skillWriterFixture) <-chan int {
	done := make(chan int, 1)
	go func() { done <- writer.run(test, f) }()
	return done
}

func eachSkillWriter(test *testing.T, writers []skillWriter, body func(test *testing.T, writer skillWriter, f skillWriterFixture)) {
	for _, assigned := range []bool{true, false} {
		kind := "proposed-only skill"
		if assigned {
			kind = "assigned skill"
		}
		for _, writer := range writers {
			test.Run(kind+"/"+writer.name, func(test *testing.T) {
				body(test, writer, newSkillWriterFixture(test, assigned))
			})
		}
	}
}

// Approval first: the writer waits and commits after the approval commit,
// including a writer that starts after the approval's readback.
func testSkillWritersWaitForApproval(test *testing.T, writers []skillWriter) {
	eachSkillWriter(test, writers, func(test *testing.T, writer skillWriter, f skillWriterFixture) {
		approval, err := lockSkillSnapshot(test, f.skillID)
		if err != nil {
			test.Fatalf("approval lock: %v", err)
		}
		snapshot := approval.readback(f.skillID)
		done := runAsync(test, writer, f)
		select {
		case status := <-done:
			test.Fatalf("%s returned %d while the approval held its snapshot", writer.name, status)
		case <-time.After(300 * time.Millisecond):
		}
		if got := approval.readback(f.skillID); got != snapshot {
			test.Fatalf("snapshot changed before the approval commit: %q, want %q", got, snapshot)
		}
		approval.commit()
		select {
		case status := <-done:
			if status != writer.wantStatus {
				test.Fatalf("%s status = %d, want %d", writer.name, status, writer.wantStatus)
			}
		case <-time.After(5 * time.Second):
			test.Fatalf("%s still waiting after the approval committed", writer.name)
		}
		if !writer.settled(test, f) {
			test.Fatalf("%s change not visible after both committed", writer.name)
		}
	})
}

// Writer first: the approval's NOWAIT answers busy (55P03); after the writer
// commits the approval re-reads and sees a changed skill.
func testSkillApprovalBusyWhileWriterHolds(test *testing.T, writers []skillWriter) {
	eachSkillWriter(test, writers, func(test *testing.T, writer skillWriter, f skillWriterFixture) {
		before := liveSkillFilesDigest(test, f.skillID)
		release := writer.blockOn(test, f)
		done := runAsync(test, writer, f)
		waitForSkillFileLockWaiter(test)
		if _, err := lockSkillSnapshot(test, f.skillID); !isLockNotAvailable(err) {
			test.Fatalf("approval lock while %s is in flight: err = %v, want 55P03", writer.name, err)
		}
		release()
		select {
		case status := <-done:
			if status != writer.wantStatus {
				test.Fatalf("%s status = %d, want %d", writer.name, status, writer.wantStatus)
			}
		case <-time.After(5 * time.Second):
			test.Fatalf("%s did not finish after its blocker was released", writer.name)
		}
		if !writer.settled(test, f) {
			test.Fatalf("%s change not visible after commit", writer.name)
		}
		approval, err := lockSkillSnapshot(test, f.skillID)
		if errors.Is(err, pgx.ErrNoRows) {
			return // the skill itself is gone
		}
		if err != nil {
			test.Fatalf("approval lock after commit: %v", err)
		}
		if got := approval.readback(f.skillID); got == before {
			test.Fatalf("approval re-read %q, same as before the writer", got)
		}
	})
}

func TestSkillFileWritersWaitForApprovalSnapshot(test *testing.T) {
	testSkillWritersWaitForApproval(test, skillFileWriters())
}

func TestSkillApprovalBusyWhileFileWriterHoldsLock(test *testing.T) {
	testSkillApprovalBusyWhileWriterHolds(test, skillFileWriters())
}

func TestSkillParentRowWritersWaitForApprovalSnapshot(test *testing.T) {
	testSkillWritersWaitForApproval(test, skillParentRowWriters())
}

func TestSkillApprovalBusyWhileParentRowWriterHoldsLock(test *testing.T) {
	testSkillApprovalBusyWhileWriterHolds(test, skillParentRowWriters())
}
