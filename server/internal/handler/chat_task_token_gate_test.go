package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chatGateRouter mounts the gate in front of the chat routes the tests drive,
// registered with the production patterns (the class map is keyed by pattern).
// history, when set, replaces the history handler so a test controls the body.
func chatGateRouter(history http.HandlerFunc) http.Handler {
	reached := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("reached")) }
	if history == nil {
		history = testHandler.GetChatChannelHistory
	}
	r := chi.NewRouter()
	r.Use(testHandler.ChatTaskTokenGate)
	r.Get("/api/chat/history", history)
	r.Get("/api/chat/thread", testHandler.GetChatThread)
	r.Get("/api/chat/unlisted", reached)
	r.Get("/api/other", reached)
	r.Route("/api/chat/sessions", func(r chi.Router) {
		r.Route("/{sessionId}", func(r chi.Router) {
			r.Get("/messages", reached)
			r.Get("/messages/page", reached)
			r.Get("/pending-task", reached)
			r.Get("/draft-restores", reached)
			r.Get("/usage", reached)
		})
	})
	return r
}

func serveChat(t *testing.T, router http.Handler, target, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	req := taskActorReq(target, taskID)
	if taskID == "" {
		req = newRequest(http.MethodGet, target, nil)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, withChatTestWorkspaceCtx(t, req))
	return w
}

// CHE-3702 round 20: a task token cannot read a chat transcript through the
// session routes, and a /api/chat* route the class map does not name is closed.
func TestChatTaskTokenGateDeniesTranscriptRoutes(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "gate"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": chatID})
	router := chatGateRouter(nil)
	base := "/api/chat/sessions/" + chatID
	for _, tc := range []struct {
		target string
		want   int
	}{
		{base + "/messages", http.StatusForbidden},
		{base + "/messages/page", http.StatusForbidden},
		{base + "/pending-task", http.StatusForbidden},
		{base + "/draft-restores", http.StatusForbidden},
		{"/api/chat/unlisted", http.StatusForbidden},
		{base + "/usage", http.StatusOK},
		{"/api/other", http.StatusOK},
	} {
		w := serveChat(t, router, tc.target, taskID)
		if w.Code != tc.want || (tc.want == http.StatusForbidden && strings.Contains(w.Body.String(), "reached")) {
			t.Errorf("task token %s = %d %q, want %d", tc.target, w.Code, w.Body.String(), tc.want)
		}
		if tc.want == http.StatusForbidden {
			if w := serveChat(t, router, tc.target, ""); w.Code != http.StatusOK || w.Body.String() != "reached" {
				t.Errorf("interactive %s = %d %q, want 200 reached", tc.target, w.Code, w.Body.String())
			}
		}
	}
}

// failInheritChatTaint fails the chat-taint query only.
type failInheritChatTaint struct{ db.DBTX }

func (d failInheritChatTaint) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if strings.Contains(sql, "name: InheritChatTaint :one") {
		return inheritErrRow{}
	}
	return d.DBTX.QueryRow(ctx, sql, args...)
}

type inheritErrRow struct{}

func (inheritErrRow) Scan(...interface{}) error { return &pgconn.PgError{Message: "boom"} }

// The gate reads first, then records the chat's taint on the reading task, then
// flushes the handler's status, headers and body; a taint failure drops the body.
func TestChatTaskTokenGateTaintsContentReadsAfterTheHandlerRan(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "gate content", "taint_ids": []string{"t1"}, "taint_verified": true})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": chatID})
	taintOf := func() (stored []string) {
		dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE id = $1`, taskID).Scan(&stored)
		return stored
	}
	var taintWhenHandlerRan []string
	router := chatGateRouter(func(w http.ResponseWriter, _ *http.Request) {
		taintWhenHandlerRan = taintOf()
		w.Header().Set("X-Gate-Test", "kept")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("transcript body"))
	})

	w := serveChat(t, router, "/api/chat/history", taskID)
	if w.Code != http.StatusCreated || w.Body.String() != "transcript body" || w.Header().Get("X-Gate-Test") != "kept" {
		t.Fatalf("flushed = %d %q header %q, want the handler's response", w.Code, w.Body.String(), w.Header().Get("X-Gate-Test"))
	}
	if len(taintWhenHandlerRan) != 0 {
		t.Fatalf("taint when the handler read = %v, want none yet (read first, then record)", taintWhenHandlerRan)
	}
	if got := taintOf(); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("reading task taint = %v, want [t1]", got)
	}
	delegateClaimsRestricted(t, taskID, "gate content", []string{"t1"})

	orig := testHandler.Queries
	testHandler.Queries = db.New(failInheritChatTaint{DBTX: testPool})
	t.Cleanup(func() { testHandler.Queries = orig })
	w = serveChat(t, router, "/api/chat/history", taskID)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "transcript body") {
		t.Fatalf("taint failure = %d %q, want 500 without the body", w.Code, w.Body.String())
	}
}

// CHE-3702 round 20 cost guard: recording the taint of a chat that has none
// writes neither the task row nor the chat row.
func TestRecordChatTaintDoesNotRewriteAnUnchangedRow(t *testing.T) {
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "cost"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": chatID})
	version := func() (task, chat string) {
		dbfx.QueryRow(t, `SELECT xmin::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&task)
		dbfx.QueryRow(t, `SELECT xmin::text FROM chat_session WHERE id = $1`, chatID).Scan(&chat)
		return task, chat
	}
	wantTask, wantChat := version()
	row, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	for range 2 {
		if err := testHandler.recordChatTaint(ctx, &row, ""); err != nil {
			t.Fatalf("record chat taint: %v", err)
		}
	}
	if gotTask, gotChat := version(); gotTask != wantTask || gotChat != wantChat {
		t.Fatalf("rows rewritten: task xmin %s -> %s, chat xmin %s -> %s", wantTask, gotTask, wantChat, gotChat)
	}
}

// CHE-3702 round 20: the supplement claim hands a chat message to a running
// task, so that task takes the chat's taint and a delegate of it is restricted.
func TestChatSupplementClaimTaintsTheTaskAndReachesDelegate(t *testing.T) {
	f := steeredFixture(t)
	dbfx.Exec(t, `UPDATE chat_session SET taint_ids = '{t1}', taint_verified = true WHERE id = $1`, f.sessionID)
	claimSteer(t, newChatSupplementDaemonClient(t), f)
	var stored []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE id = $1`, f.runningID).Scan(&stored)
	if !reflect.DeepEqual(stored, []string{"t1"}) {
		t.Fatalf("claiming task taint = %v, want [t1]", stored)
	}
	delegateClaimsRestricted(t, f.runningID, "supplement", []string{"t1"})
}
