//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-5094 (CHE-4678 S9 P5): one chain test for a script-mode autopilot run —
// real service dispatch, real claim handler, real daemon (client + handleTask +
// script executor), real settle path (CompleteTask/FailTask + SyncRunFromTask).
// Every assertion reads a database row or a captured log line.
//
// Seam that is not the production binary: cmd/server's registerAutopilotListeners
// is package main and cannot be imported, so chainEnv subscribes the same three
// task events to the same service call (SyncRunFromTask). Auth middleware is
// replaced by the daemon-context values DaemonAuth would set.

type chainEnv struct {
	*testutil.Fixture
	pool        *pgxpool.Pool
	q           *db.Queries
	svc         *service.AutopilotService
	d           *Daemon
	allowDir    string
	runtimeID   string
	autopilotID string
	triggerID   string
	usageCalls  *atomic.Int32
	logs        *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var chainSeq atomic.Int64

func newChainEnv(t *testing.T, scriptName string, args []string) *chainEnv {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		if ci := os.Getenv("CI"); ci == "true" || ci == "1" {
			t.Fatal("DATABASE_URL is required in CI for the script chain test")
		}
		t.Skip("DATABASE_URL not set; script chain test needs a migrated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("database unavailable: %v", err)
	}

	n := chainSeq.Add(1)
	fx := testutil.New(pool, "", "")
	ownerID := fx.User(t, "chain owner", fmt.Sprintf("chain-owner-%d-%d@multica.test", os.Getpid(), n))
	workspaceID := fx.Workspace(t, "chain ws", fmt.Sprintf("chain-ws-%d-%d", os.Getpid(), n))
	fx.Member(t, workspaceID, ownerID, "owner")
	fx.WorkspaceID, fx.UserID = workspaceID, ownerID
	daemonID := fmt.Sprintf("chain-daemon-%d-%d", os.Getpid(), n)
	runtimeID := fx.Runtime(t, "chain-rt", testutil.Cols{
		"daemon_id": daemonID, "runtime_mode": "local", "provider": "claude", "owner_id": ownerID,
	})
	agentID := fx.Agent(t, "chain-agent", runtimeID, testutil.Cols{"runtime_mode": "local", "owner_id": ownerID})
	cfgJSON, _ := json.Marshal(map[string]any{"name": scriptName, "args": args})
	autopilotID := fx.Insert(t, "autopilot", testutil.Cols{
		"workspace_id": workspaceID, "title": "chain-ap", "assignee_type": "agent", "assignee_id": agentID,
		"status": "active", "execution_mode": "script", "script_config": testutil.Raw("'" + string(cfgJSON) + "'::jsonb"),
		"created_by_type": "member", "created_by_id": ownerID,
	})
	triggerID := fx.Insert(t, "autopilot_trigger", testutil.Cols{
		"autopilot_id": autopilotID, "kind": "schedule", "enabled": true,
		"cron_expression": fmt.Sprintf("%d * * * *", n%60), "published_by_type": "member", "published_by_id": ownerID,
		"created_by_type": "member", "created_by_id": ownerID,
	})

	q := db.New(pool)
	hub := realtime.NewHub()
	go hub.Run()
	bus := events.New()
	h := handler.New(q, pool, hub, bus, service.NewEmailService(), nil, nil, analytics.NoopClient{}, handler.Config{})

	// The listener cmd/server/autopilot_listeners.go registers for task events.
	svc := h.AutopilotService
	for _, ev := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(ev, func(e events.Event) {
			payload, ok := e.Payload.(map[string]any)
			if !ok {
				return
			}
			id, _ := payload["task_id"].(string)
			task, err := q.GetAgentTask(context.Background(), util.MustParseUUID(id))
			if err == nil && task.AutopilotRunID.Valid {
				svc.SyncRunFromTask(context.Background(), task)
			}
		})
	}

	var usageCalls atomic.Int32
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasSuffix(req.URL.Path, "/usage") {
				usageCalls.Add(1)
			}
			ctx := middleware.WithDaemonContext(req.Context(), workspaceID, daemonID)
			ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/api/daemon", func(r chi.Router) {
		r.Post("/runtimes/{runtimeId}/tasks/claim", h.ClaimTaskByRuntime)
		r.Get("/tasks/{taskId}/status", h.GetTaskStatus)
		r.Post("/tasks/{taskId}/start", h.StartTask)
		r.Post("/tasks/{taskId}/progress", h.ReportTaskProgress)
		r.Post("/tasks/{taskId}/complete", h.CompleteTask)
		r.Post("/tasks/{taskId}/fail", h.FailTask)
		r.Post("/tasks/{taskId}/usage", h.ReportTaskUsage)
		r.Post("/tasks/{taskId}/messages", h.ReportTaskMessages)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	d := New(Config{
		ServerBaseURL:  srv.URL,
		DaemonID:       daemonID,
		WorkspacesRoot: t.TempDir(),
		ScriptTimeout:  10 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.runtimeIndex[runtimeID] = Runtime{ID: runtimeID, Provider: "claude"}
	d.cancelPollInterval = time.Hour

	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return &chainEnv{
		Fixture: fx, pool: pool, q: q, svc: svc, d: d,
		allowDir:  filepath.Join(home, ".multica", "scripts"),
		runtimeID: runtimeID, autopilotID: autopilotID, triggerID: triggerID,
		usageCalls: &usageCalls, logs: logs,
	}
}

// allow installs an allowlisted script (same layout as script_task_test.go).
func (e *chainEnv) allow(t *testing.T, name, body string, patterns []string) {
	t.Helper()
	if err := os.MkdirAll(e.allowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(e.allowDir, "bin-"+name)
	writeTestExecutable(t, exe, []byte("#!/bin/sh\n"+body+"\n"))
	entry, _ := json.Marshal(map[string]any{"executable": exe, "arg_patterns": patterns})
	if err := os.WriteFile(filepath.Join(e.allowDir, name+".json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tick dispatches the autopilot, has the daemon claim the task and runs it
// through handleTask. It returns the claimed task and its id.
func (e *chainEnv) tick(t *testing.T) (*Task, string) {
	t.Helper()
	ctx := context.Background()
	ap, err := e.q.GetAutopilot(ctx, util.MustParseUUID(e.autopilotID))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	run, err := e.svc.DispatchAutopilot(ctx, ap, util.MustParseUUID(e.triggerID), "schedule", nil)
	if err != nil || run == nil || run.Status != "running" {
		t.Fatalf("dispatch: run=%+v err=%v", run, err)
	}
	task, err := e.d.client.ClaimTask(ctx, e.runtimeID)
	if err != nil || task == nil {
		t.Fatalf("claim: task=%v err=%v", task, err)
	}
	if task.Script == nil || task.AuthToken == "" {
		t.Fatalf("claimed task has script=%v token-present=%v", task.Script, task.AuthToken != "")
	}
	if got := e.Count(t, `SELECT count(*) FROM task_token WHERE task_id = $1`, task.ID); got != 1 {
		t.Fatalf("task_token rows after claim = %d, want 1", got)
	}
	e.d.handleTask(ctx, *task, 0)
	return task, util.UUIDToString(run.ID)
}

type chainRun struct {
	Status        string
	FailureReason *string
	Result        struct {
		Script *struct {
			Name       string `json:"name"`
			ArgvSHA256 string `json:"argv_sha256"`
			ExitCode   *int   `json:"exit_code"`
			DurationMs *int64 `json:"duration_ms"`
		} `json:"script"`
		TokenRevokeFailed bool `json:"token_revoke_failed"`
	}
}

func (e *chainEnv) run(t *testing.T, runID string) chainRun {
	t.Helper()
	var out chainRun
	var raw []byte
	e.QueryRow(t, `SELECT status, failure_reason, result FROM autopilot_run WHERE id = $1`, runID).Scan(&out.Status, &out.FailureReason, &raw)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.Result); err != nil {
			t.Fatalf("run result %s: %v", raw, err)
		}
	}
	return out
}

func argvHash(args ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:])
}

// usageTokens reads the stored usage rows for the task.
func (e *chainEnv) usageTokens(t *testing.T, taskID string) (rows, tokens int) {
	t.Helper()
	e.QueryRow(t, `SELECT count(*), COALESCE(sum(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0)::int FROM task_usage WHERE task_id = $1`, taskID).Scan(&rows, &tokens)
	return rows, tokens
}

// Unknown script: fails closed with script_not_allowlisted, audit still
// written from the claimed snapshot, task token deleted, no usage.
func TestScriptChainUnknownScriptFailsClosedWithAudit(t *testing.T) {
	e := newChainEnv(t, "ghost", []string{"--x"})
	// No allowlist directory at all.
	task, runID := e.tick(t)

	var taskStatus, taskReason string
	e.QueryRow(t, `SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id = $1`, task.ID).Scan(&taskStatus, &taskReason)
	if taskStatus != "failed" || taskReason != "script_not_allowlisted" {
		t.Fatalf("task status=%q reason=%q, want failed/script_not_allowlisted", taskStatus, taskReason)
	}
	run := e.run(t, runID)
	if run.Status != "failed" || run.FailureReason == nil || !strings.HasPrefix(*run.FailureReason, "script_not_allowlisted") {
		got := "<nil>"
		if run.FailureReason != nil {
			got = *run.FailureReason
		}
		t.Fatalf("run status=%q reason=%q, want failed/script_not_allowlisted*", run.Status, got)
	}
	s := run.Result.Script
	if s == nil || s.Name != "ghost" || s.ArgvSHA256 != argvHash("--x") {
		t.Fatalf("run audit = %+v, want name=ghost argv_sha256=%s", s, argvHash("--x"))
	}
	if got := e.Count(t, `SELECT count(*) FROM task_token WHERE task_id = $1`, task.ID); got != 0 {
		t.Fatalf("task_token rows at task end = %d, want 0", got)
	}
	if run.Result.TokenRevokeFailed {
		t.Fatal("token_revoke_failed set although deletion succeeded")
	}
	if rows, tokens := e.usageTokens(t, task.ID); rows != 0 || tokens != 0 {
		t.Fatalf("usage rows=%d tokens=%d, want 0/0", rows, tokens)
	}
}

// Idle tick: completes, exit_code 0 audited, token deleted, zero model tokens
// in task_usage, and the daemon never called the usage endpoint.
func TestScriptChainIdleTickWritesZeroModelTokens(t *testing.T) {
	e := newChainEnv(t, "tick", []string{"--mode=a"})
	e.allow(t, "tick", `echo idle`, []string{`--mode=a`})
	task, runID := e.tick(t)

	run := e.run(t, runID)
	if run.Status != "completed" {
		t.Fatalf("run status = %q (%v), want completed", run.Status, run.FailureReason)
	}
	s := run.Result.Script
	if s == nil || s.Name != "tick" || s.ArgvSHA256 != argvHash("--mode=a") || s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("run audit = %+v, want name=tick exit_code=0", s)
	}
	if got := e.Count(t, `SELECT count(*) FROM task_token WHERE task_id = $1`, task.ID); got != 0 {
		t.Fatalf("task_token rows at task end = %d, want 0", got)
	}
	rows, tokens := e.usageTokens(t, task.ID)
	if rows != 0 || tokens != 0 {
		t.Fatalf("task_usage rows=%d tokens=%d, want 0/0", rows, tokens)
	}
	if n := e.usageCalls.Load(); n != 0 {
		t.Fatalf("daemon called /usage %d times for a script tick", n)
	}
	// Control: the same query sees tokens once a model-style report is stored.
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens) VALUES ($1, 'control', 'control', 7, 3)`, task.ID); err != nil {
		t.Fatalf("control insert: %v", err)
	}
	if rows, tokens := e.usageTokens(t, task.ID); rows != 1 || tokens != 10 {
		t.Fatalf("control: usage rows=%d tokens=%d, want 1/10", rows, tokens)
	}
}

// A forced task-token deletion failure is logged and recorded on the run, and
// does not erase the audit.
func TestScriptChainTokenRevokeFailureIsLoggedAndRecorded(t *testing.T) {
	e := newChainEnv(t, "tick", []string{"--mode=a"})
	e.allow(t, "tick", `echo idle`, []string{`--mode=a`})

	// Fail only deletes of this workspace's task tokens; scoped so a shared
	// test database stays usable for other rows.
	fn := fmt.Sprintf("che5094_block_token_delete_%d", chainSeq.Add(1))
	ddl := fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.workspace_id = '%[2]s'::uuid THEN RAISE EXCEPTION 'che5094 forced token delete failure'; END IF;
			RETURN OLD;
		END $$;
		CREATE TRIGGER %[1]s BEFORE DELETE ON task_token FOR EACH ROW EXECUTE FUNCTION %[1]s();`, fn, e.WorkspaceID)
	if _, err := e.pool.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		_, _ = e.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON task_token; DROP FUNCTION IF EXISTS %[1]s();`, fn))
	}
	t.Cleanup(drop)

	task, runID := e.tick(t)

	run := e.run(t, runID)
	if run.Status != "completed" {
		t.Fatalf("run status = %q (%v), want completed", run.Status, run.FailureReason)
	}
	if !run.Result.TokenRevokeFailed {
		t.Fatal("result.token_revoke_failed not set after a failed token deletion")
	}
	if s := run.Result.Script; s == nil || s.Name != "tick" || s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("audit lost after revoke failure: %+v", s)
	}
	if got := e.Count(t, `SELECT count(*) FROM task_token WHERE task_id = $1`, task.ID); got != 1 {
		t.Fatalf("task_token rows = %d, want 1 (deletion was blocked)", got)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "task end: failed to delete task tokens") || !strings.Contains(logs, "che5094 forced token delete failure") {
		t.Fatalf("deletion failure not logged with its error:\n%s", logs)
	}
	if strings.Contains(logs, task.AuthToken) {
		t.Fatal("task token leaked into the log")
	}
	drop()
}
