package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// agentSkillWriters drives each ordinary writer of agent_skill rows on an
// existing agent. Every writer targets skill x.
var agentSkillWriters = []struct {
	name string
	call func(h *Handler, agentID, x string) *httptest.ResponseRecorder
	// wantX is the enabled state of x after the writer ran: nil = row gone.
	wantX *bool
	// wantN is the state of n (the skill the action adds) after the writer
	// ran: nil = row gone (the writer replaced the whole list).
	wantN *bool
}{
	{"AddAgentSkills", func(h *Handler, agentID, x string) *httptest.ResponseRecorder {
		return callAgentSkillWriter(h.AddAgentSkills, "POST", "/api/agents/"+agentID+"/skills/add", map[string]any{"skill_ids": []string{x}}, agentID, "")
	}, boolPtr(true), boolPtr(true)},
	{"SetAgentSkills", func(h *Handler, agentID, x string) *httptest.ResponseRecorder {
		return callAgentSkillWriter(h.SetAgentSkills, "PUT", "/api/agents/"+agentID+"/skills", map[string]any{"skill_ids": []string{}}, agentID, "")
	}, nil, nil},
	{"SetAgentSkillEnabled", func(h *Handler, agentID, x string) *httptest.ResponseRecorder {
		return callAgentSkillWriter(h.SetAgentSkillEnabled, "PUT", "/api/agents/"+agentID+"/skills/"+x+"/enabled", map[string]any{"enabled": false}, agentID, x)
	}, boolPtr(false), boolPtr(true)},
	{"RemoveAgentSkill", func(h *Handler, agentID, x string) *httptest.ResponseRecorder {
		return callAgentSkillWriter(h.RemoveAgentSkill, "DELETE", "/api/agents/"+agentID+"/skills/"+x, nil, agentID, x)
	}, nil, boolPtr(true)},
}

func boolPtr(b bool) *bool { return &b }

func callAgentSkillWriter(fn http.HandlerFunc, method, path string, body any, agentID, skillID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", agentID)
	if skillID != "" {
		rctx.URLParams.Add("skillId", skillID)
	}
	fn(w, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	return w
}

// waitForLockWaiter blocks until some backend waits on a lock, or fails when
// the writer finished first (it did not wait for the agent lock).
func waitForLockWaiter(t *testing.T, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case w := <-done:
			t.Fatalf("writer finished (%d) while the action transaction held the agent lock", w.Code)
		default:
		}
		var waiting int
		if err := testPool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no writer waited on the agent lock")
}

func agentSkillEnabled(t *testing.T, agentID, skillID string) *bool {
	t.Helper()
	var enabled bool
	err := testPool.QueryRow(context.Background(),
		`SELECT enabled FROM agent_skill WHERE agent_id = $1 AND skill_id = $2`, agentID, skillID).Scan(&enabled)
	if err != nil {
		return nil
	}
	return &enabled
}

func sameState(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// An action transaction holds the agent lock: each ordinary writer waits and
// commits after it, so the action's change is not lost.
func TestAgentSkillWritersWaitForAgentLock(t *testing.T) {
	for _, writer := range agentSkillWriters {
		t.Run(writer.name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "Skill Lock "+writer.name, nil)
			x := insertHandlerTestSkill(t, "skill-lock-x-"+writer.name, "# x")
			n := insertHandlerTestSkill(t, "skill-lock-n-"+writer.name, "# n")
			if _, err := testPool.Exec(context.Background(), `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, x); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			qtx := testHandler.Queries.WithTx(tx)
			if _, err := qtx.LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{
				WorkspaceID: parseUUID(testWorkspaceID), Ids: []pgtype.UUID{parseUUID(agentID)},
			}); err != nil {
				t.Fatal(err)
			}
			// The action's change: assign n.
			if err := qtx.AddAgentSkill(ctx, db.AddAgentSkillParams{AgentID: parseUUID(agentID), SkillID: parseUUID(n)}); err != nil {
				t.Fatal(err)
			}

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- writer.call(testHandler, agentID, x) }()
			waitForLockWaiter(t, done)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case resp := <-done:
				if resp.Code != http.StatusOK && resp.Code != http.StatusNoContent {
					t.Fatalf("writer status=%d body=%s", resp.Code, resp.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not finish after the action committed")
			}
			if got := agentSkillEnabled(t, agentID, x); !sameState(got, writer.wantX) {
				t.Fatalf("skill x state = %v, want %v", got, writer.wantX)
			}
			if got := agentSkillEnabled(t, agentID, n); !sameState(got, writer.wantN) {
				t.Fatalf("skill n state = %v, want %v", got, writer.wantN)
			}
		})
	}
}

// An ordinary writer holds the agent lock: the action's NOWAIT lock fails with
// lock_not_available, which the approval path answers as busy_retry.
func TestAgentSkillWriterLockMakesActionLockBusy(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Skill Lock Reverse", nil)
	ctx := context.Background()
	writerTx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writerTx.Rollback(ctx)
	if _, err := db.New(writerTx).LockAgentForSkillWrite(ctx, db.LockAgentForSkillWriteParams{ID: parseUUID(agentID), WorkspaceID: parseUUID(testWorkspaceID)}); err != nil {
		t.Fatal(err)
	}

	actionTx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer actionTx.Rollback(ctx)
	_, err = testHandler.Queries.WithTx(actionTx).LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{
		WorkspaceID: parseUUID(testWorkspaceID), Ids: []pgtype.UUID{parseUUID(agentID)},
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("action lock err = %v, want 55P03 lock_not_available", err)
	}
}

// stalledBody blocks its first Read until release is closed, then yields data.
type stalledBody struct {
	started chan struct{}
	release chan struct{}
	data    *strings.Reader
	once    sync.Once
}

func (b *stalledBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.data.Read(p)
}

// A client that stalls its request body must not pin the agent row lock or a
// transaction: the body is read before the lock is taken (CWE-400).
func TestAgentSkillWritersReadBodyBeforeLocking(t *testing.T) {
	writers := []struct {
		name, method, path, body string
		fn                       func(h *Handler) http.HandlerFunc
		withSkill                bool
	}{
		{"SetAgentSkills", "PUT", "/skills", `{"skill_ids":[]}`, func(h *Handler) http.HandlerFunc { return h.SetAgentSkills }, false},
		{"AddAgentSkills", "POST", "/skills/add", `{"skill_ids":[]}`, func(h *Handler) http.HandlerFunc { return h.AddAgentSkills }, false},
		{"SetAgentSkillEnabled", "PUT", "/skills/{x}/enabled", `{"enabled":false}`, func(h *Handler) http.HandlerFunc { return h.SetAgentSkillEnabled }, true},
	}
	for _, writer := range writers {
		t.Run(writer.name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "Skill Body "+writer.name, nil)
			x := insertHandlerTestSkill(t, "skill-body-x-"+writer.name, "# x")
			if _, err := testPool.Exec(context.Background(), `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, x); err != nil {
				t.Fatal(err)
			}
			body := &stalledBody{started: make(chan struct{}), release: make(chan struct{}), data: strings.NewReader(writer.body)}
			req := httptest.NewRequest(writer.method, "/api/agents/"+agentID+strings.ReplaceAll(writer.path, "{x}", x), body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-ID", testUserID)
			req.Header.Set("X-Workspace-ID", testWorkspaceID)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", agentID)
			if writer.withSkill {
				rctx.URLParams.Add("skillId", x)
			}
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				writer.fn(testHandler)(w, req)
				done <- w
			}()
			select {
			case <-body.started:
			case <-time.After(5 * time.Second):
				t.Fatal("handler never read the body")
			}

			// While the body is stalled, an action must get the agent lock.
			ctx := context.Background()
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, lockErr := testHandler.Queries.WithTx(tx).LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{
				WorkspaceID: parseUUID(testWorkspaceID), Ids: []pgtype.UUID{parseUUID(agentID)},
			})
			tx.Rollback(ctx)
			close(body.release)
			resp := <-done
			if lockErr != nil {
				t.Fatalf("stalled body pinned the agent row lock: %v", lockErr)
			}
			if resp.Code != http.StatusOK {
				t.Fatalf("writer status=%d body=%s", resp.Code, resp.Body.String())
			}
		})
	}
}

// The writer transaction holds one pool connection. Every read inside it must
// go through that transaction: a read through the pool waits for a connection
// the transaction never frees, and enough concurrent writers exhaust the real
// pool the same way. A pool of one connection makes the wait deterministic.
func TestAgentSkillWritersReadOnlyThroughTheirTransaction(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cfg := testPool.Config()
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	h := New(db.New(small), small, realtime.NewHub(), events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, Config{})

	// Non-empty lists reach the qoder provider check inside the transaction.
	writers := []struct {
		name string
		call func(ctx context.Context, agentID, x string) *httptest.ResponseRecorder
	}{
		{"SetAgentSkills", func(ctx context.Context, agentID, x string) *httptest.ResponseRecorder {
			return callAgentSkillWriterCtx(ctx, h.SetAgentSkills, "PUT", map[string]any{"skill_ids": []string{x}}, agentID, "")
		}},
		{"AddAgentSkills", func(ctx context.Context, agentID, x string) *httptest.ResponseRecorder {
			return callAgentSkillWriterCtx(ctx, h.AddAgentSkills, "POST", map[string]any{"skill_ids": []string{x}}, agentID, "")
		}},
		{"SetAgentSkillEnabled", func(ctx context.Context, agentID, x string) *httptest.ResponseRecorder {
			return callAgentSkillWriterCtx(ctx, h.SetAgentSkillEnabled, "PUT", map[string]any{"enabled": false}, agentID, x)
		}},
		{"RemoveAgentSkill", func(ctx context.Context, agentID, x string) *httptest.ResponseRecorder {
			return callAgentSkillWriterCtx(ctx, h.RemoveAgentSkill, "DELETE", nil, agentID, x)
		}},
	}
	for _, writer := range writers {
		t.Run(writer.name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "Skill Pool "+writer.name, nil)
			x := insertHandlerTestSkill(t, "skill-pool-x-"+writer.name, "# x")
			if _, err := testPool.Exec(context.Background(), `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, x); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp := writer.call(ctx, agentID, x)
			if resp.Code != http.StatusOK {
				t.Fatalf("writer on a pool of one connection: status=%d body=%s", resp.Code, resp.Body.String())
			}
		})
	}
}

func callAgentSkillWriterCtx(ctx context.Context, fn http.HandlerFunc, method string, body any, agentID, skillID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequest(method, "/api/agents/"+agentID+"/skills", body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", agentID)
	if skillID != "" {
		rctx.URLParams.Add("skillId", skillID)
	}
	fn(w, req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx)))
	return w
}
