package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2909 (CHE-2894 step 6): one purpose-first authority function for chat
// task access (SEC-1 rules 1-3, ENG-2, ENG-5, DX-4).

func TestResolveChatTaskAuthority(t *testing.T) {
	var (
		dmAgent      = util.MustParseUUID("00000000-0000-0000-0000-0000000000a1")
		compactAgent = util.MustParseUUID("00000000-0000-0000-0000-0000000000a2")
		sessionID    = util.MustParseUUID("00000000-0000-0000-0000-0000000000b1")
		otherSession = util.MustParseUUID("00000000-0000-0000-0000-0000000000b2")
		workspace    = "00000000-0000-0000-0000-0000000000c1"
		compactID    = "00000000-0000-0000-0000-0000000000a2"
	)
	session := db.ChatSession{ID: sessionID, WorkspaceID: util.MustParseUUID(workspace), AgentID: dmAgent}
	ordinary := func(agent pgtype.UUID, status string) db.AgentTaskQueue {
		return db.AgentTaskQueue{AgentID: agent, ChatSessionID: sessionID, Status: status}
	}
	compaction := func(agent pgtype.UUID, status string) db.AgentTaskQueue {
		task := ordinary(agent, status)
		task.Purpose = pgtype.Text{String: taskPurposeCompaction, Valid: true}
		return task
	}
	elsewhere := func(task db.AgentTaskQueue) db.AgentTaskQueue { task.ChatSessionID = otherSession; return task }

	tests := []struct {
		name     string
		op       chatTaskOp
		task     db.AgentTaskQueue
		setting  string
		usable   bool
		workspID string
		want     string // refusal code, "" = allowed
	}{
		{"ordinary read running", chatOpRead, ordinary(dmAgent, "running"), "", false, workspace, ""},
		{"ordinary read dispatched stays refused", chatOpRead, ordinary(dmAgent, "dispatched"), "", false, workspace, codeChatTaskNotActive},
		{"ordinary compact dispatched", chatOpCompact, ordinary(dmAgent, "dispatched"), "", false, workspace, ""},
		{"ordinary continue running", chatOpContinue, ordinary(dmAgent, "running"), "", false, workspace, ""},
		{"ordinary other agent", chatOpRead, ordinary(compactAgent, "running"), compactID, true, workspace, codeChatTaskAgentMismatch},
		{"ordinary completed", chatOpCompact, ordinary(dmAgent, "completed"), "", false, workspace, codeChatTaskNotActive},
		{"ordinary other session", chatOpRead, elsewhere(ordinary(dmAgent, "running")), "", false, workspace, codeChatTaskSessionMismatch},
		{"ordinary other workspace", chatOpRead, ordinary(dmAgent, "running"), "", false, "00000000-0000-0000-0000-0000000000c2", codeChatTaskSessionMismatch},
		{"no chat session", chatOpRead, db.AgentTaskQueue{AgentID: dmAgent, Status: "running"}, "", false, workspace, codeChatTaskNotActive},

		{"compaction read running", chatOpRead, compaction(compactAgent, "running"), compactID, true, workspace, ""},
		{"compaction read dispatched", chatOpRead, compaction(compactAgent, "dispatched"), compactID, true, workspace, ""},
		{"compaction compact", chatOpCompact, compaction(compactAgent, "running"), compactID, true, workspace, ""},
		{"compaction queued", chatOpRead, compaction(compactAgent, "queued"), compactID, true, workspace, codeChatTaskNotActive},
		{"compaction cancelled", chatOpCompact, compaction(compactAgent, "cancelled"), compactID, true, workspace, codeChatTaskNotActive},
		{"compaction other session", chatOpRead, elsewhere(compaction(compactAgent, "running")), compactID, true, workspace, codeChatTaskSessionMismatch},
		{"compaction other workspace", chatOpCompact, compaction(compactAgent, "running"), compactID, true, "00000000-0000-0000-0000-0000000000c2", codeChatTaskSessionMismatch},
		{"compaction setting cleared", chatOpCompact, compaction(compactAgent, "running"), "", true, workspace, codeCompactionAgentNotCurrent},
		{"compaction setting moved", chatOpRead, compaction(compactAgent, "running"), "00000000-0000-0000-0000-0000000000a3", true, workspace, codeCompactionAgentNotCurrent},
		{"compaction agent unusable", chatOpRead, compaction(compactAgent, "running"), compactID, false, workspace, codeCompactionAgentNotCurrent},
		// SEC-1 rule 2: equality with the DM agent gives a compaction task nothing.
		{"compaction by DM agent, setting cleared", chatOpCompact, compaction(dmAgent, "running"), "", true, workspace, codeCompactionAgentNotCurrent},
		{"compaction by DM agent, setting moved", chatOpRead, compaction(dmAgent, "running"), compactID, true, workspace, codeCompactionAgentNotCurrent},
		{"compaction by DM agent, setting is DM agent", chatOpCompact, compaction(dmAgent, "running"), "00000000-0000-0000-0000-0000000000a1", true, workspace, ""},
		// Fork and the archive branch refuse compaction; it finishes in place only.
		{"compaction continue", chatOpContinue, compaction(compactAgent, "running"), compactID, true, workspace, codeCompactionContinueRefused},
		{"compaction continue by DM agent", chatOpContinue, compaction(dmAgent, "running"), "", true, workspace, codeCompactionContinueRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refusal := resolveChatTaskAuthority(chatTaskAuthorityInput{
				Op: tt.op, Task: tt.task, Session: session, RequestWorkspaceID: tt.workspID,
				CompactAgentID: tt.setting, CompactAgentUsable: tt.usable,
			})
			got := ""
			if refusal != nil {
				got = refusal.Code
				if refusal.Message == "" {
					t.Fatalf("refusal %q has no user message", got)
				}
			}
			if got != tt.want {
				t.Fatalf("refusal = %q, want %q", got, tt.want)
			}
		})
	}
}

// authorityFixture is a DM on agent D with a compaction agent C configured
// through background_agents.compact (CHE-4097).
type authorityFixture struct {
	t                              *testing.T
	dmAgent, compactAgent, session string
}

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	var settings []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'background_agents' - 'compact_agent_id' WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, settings, testWorkspaceID)
	})
	a := &authorityFixture{t: t, dmAgent: eligibleAgent(t, "authority-dm-agent"), compactAgent: eligibleAgent(t, "authority-compact-agent")}
	a.session = dbfx.ChatSession(t, a.dmAgent)
	a.setCompact(a.compactAgent)
	return a
}

// eligibleAgent is workspace-visible, not archived, on a public workspace runtime.
func eligibleAgent(t *testing.T, name string) string {
	t.Helper()
	runtimeID := dbfx.Runtime(t, name+"-runtime", testutil.Cols{"visibility": "public"})
	agentID := dbfx.Agent(t, name, runtimeID, testutil.Cols{
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2)`, agentID, testWorkspaceID)
	return agentID
}

// setCompact writes background_agents.compact.agent_id as a human admin (the
// public write path, so the stored shape is the real one).
func (a *authorityFixture) setCompact(agentID string) {
	a.t.Helper()
	putBG(a.t, testWorkspaceID, map[string]any{"compact": map[string]any{"agent_id": agentID}}, nil).Want(http.StatusOK)
}

func (a *authorityFixture) clearCompact() {
	a.t.Helper()
	bgCall(a.t, testHandler.ResetBackgroundAgents, "DELETE", testWorkspaceID, nil, nil).Want(http.StatusOK)
}

// seedOldKey writes only the retired settings.compact_agent_id.
func (a *authorityFixture) seedOldKey(agentID string) {
	a.t.Helper()
	dbfx.Exec(a.t, `UPDATE workspace SET settings = jsonb_set(settings - 'background_agents', '{compact_agent_id}', to_jsonb($1::text)) WHERE id = $2`, agentID, testWorkspaceID)
}

func (a *authorityFixture) task(agentID, status, purpose string) string {
	a.t.Helper()
	cols := testutil.Cols{"runtime_id": handlerTestRuntimeID(a.t), "status": status, "chat_session_id": a.session}
	if purpose != "" {
		cols["purpose"] = purpose
		if purpose == taskPurposeCompaction {
			cols["task_kind"] = "compact" // what MarkChatTaskCompact writes in production
		}
	}
	if status == "running" {
		cols["started_at"] = testutil.Raw("now()")
	}
	return dbfx.Task(a.t, agentID, cols)
}

func (a *authorityFixture) read(taskID string) *httptest.ResponseRecorder {
	req := withChatTestWorkspaceCtx(a.t, taskActorReq("/api/chat/sessions", taskID))
	rec := httptest.NewRecorder()
	testHandler.ListChatSessions(rec, req)
	return rec
}

func (a *authorityFixture) compact(taskID string) *httptest.ResponseRecorder {
	req := newRequest(http.MethodPost, "/api/chat/compact", map[string]any{"summary": "the summary"})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	req = withChatTestWorkspaceCtx(a.t, req)
	rec := httptest.NewRecorder()
	testHandler.CompactChatSession(rec, req)
	return rec
}

func (a *authorityFixture) continueDM(taskID string) *httptest.ResponseRecorder {
	return continueChatRequest(a.t, a.session, "the summary", taskID)
}

// logSink captures slog records so a test can assert which events were emitted.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records returns the captured records whose message is msg.
func (s *logSink) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(s.buf.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func captureJSONLogs(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response %q is not JSON: %v", rec.Body.String(), err)
	}
	return body.Code
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int, code string) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
	if code != "" {
		if got := errorCode(t, rec); got != code {
			t.Fatalf("code = %q, want %q: %s", got, code, rec.Body.String())
		}
	}
}

func TestChatTaskAuthorityCompactionAgentReadsAndCompactsItsOwnDM(t *testing.T) {
	a := newAuthorityFixture(t)
	logs := captureJSONLogs(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)

	requireStatus(t, a.read(task), http.StatusOK, "")
	requireStatus(t, a.compact(task), http.StatusOK, "")

	grants := logs.records(t, chatTaskAuthorityEvent)
	if len(grants) == 0 {
		t.Fatalf("no %s log record for the grant", chatTaskAuthorityEvent)
	}
	rec := grants[0]
	if rec["decision"] != "granted" || rec["task_id"] != task || rec["session_id"] != a.session || rec["compact_agent_id"] != a.compactAgent {
		t.Fatalf("grant log = %v, want granted task %s session %s setting %s", rec, task, a.session, a.compactAgent)
	}
}

func TestChatTaskAuthorityDispatchedCompactionAgentMayCompact(t *testing.T) {
	a := newAuthorityFixture(t)
	requireStatus(t, a.compact(a.task(a.compactAgent, "dispatched", taskPurposeCompaction)), http.StatusOK, "")
}

func TestChatTaskAuthorityRefusalsAreLoggedAndCoded(t *testing.T) {
	a := newAuthorityFixture(t)
	logs := captureJSONLogs(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	a.setCompact(a.dmAgent) // the setting moves to another agent
	// The compact-chain change cancels compaction tasks; revive this one so the
	// authority check, not the cancel, is what refuses it.
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}

	rec := a.read(task)
	requireStatus(t, rec, http.StatusForbidden, codeCompactionAgentNotCurrent)
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("refusal body %q has no user message", rec.Body.String())
	}
	refusals := logs.records(t, chatTaskAuthorityEvent)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal log, got %d", len(refusals))
	}
	r := refusals[0]
	if r["decision"] != "refused" || r["code"] != codeCompactionAgentNotCurrent || r["task_id"] != task || r["session_id"] != a.session || r["compact_agent_id"] != a.dmAgent {
		t.Fatalf("refusal log = %v", r)
	}
}

// Compaction agent equal to the DM agent: refused after clear, A→B and A→B→A.
func TestChatTaskAuthorityCompactionByDMAgentRefusedAfterSettingChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(a *authorityFixture, other string)
	}{
		{"clear", func(a *authorityFixture, _ string) { a.clearCompact() }},
		{"A to B", func(a *authorityFixture, other string) { a.setCompact(other) }},
		{"A to B to A", func(a *authorityFixture, other string) {
			a.setCompact(other)
			a.setCompact(a.dmAgent)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthorityFixture(t)
			a.setCompact(a.dmAgent) // A = the DM agent is the compaction agent
			task := a.task(a.dmAgent, "running", taskPurposeCompaction)
			other := eligibleAgent(t, "authority-other-agent")
			tc.edit(a, other)
			requireStatus(t, a.read(task), http.StatusForbidden, "")
			requireStatus(t, a.compact(task), http.StatusForbidden, "")
			requireStatus(t, a.continueDM(task), http.StatusForbidden, "")
		})
	}
}

func TestChatTaskAuthorityArchivedCompactionAgentRefused(t *testing.T) {
	a := newAuthorityFixture(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, a.compactAgent)
	requireStatus(t, a.read(task), http.StatusForbidden, codeCompactionAgentNotCurrent)
	requireStatus(t, a.compact(task), http.StatusForbidden, codeCompactionAgentNotCurrent)
}

func TestChatTaskAuthorityCompactionContinueFinishesInPlace(t *testing.T) {
	a := newAuthorityFixture(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	requireStatus(t, a.continueDM(task), http.StatusOK, "")

	var status string
	var summaries, continued int
	dbfx.QueryRow(t, `SELECT status FROM chat_session WHERE id = $1`, a.session).Scan(&status)
	dbfx.QueryRow(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND task_id = $2 AND message_kind = 'compaction_summary'`, a.session, task).Scan(&summaries)
	dbfx.QueryRow(t, `SELECT count(*) FROM chat_session WHERE continued_from_session_id = $1`, a.session).Scan(&continued)
	if status != "active" || summaries != 1 || continued != 0 {
		t.Fatalf("status = %q, summaries = %d, continued DMs = %d; want active DM, one in-place summary, no new DM", status, summaries, continued)
	}
}

func TestChatTaskAuthorityForkRefusesCompaction(t *testing.T) {
	a := newAuthorityFixture(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	forkReq := newRequest(http.MethodPost, "/api/chat/fork", map[string]any{})
	forkReq.Header.Set("X-Actor-Source", "task_token")
	forkReq.Header.Set("X-Task-ID", task)
	rec := httptest.NewRecorder()
	testHandler.ForkChatSession(rec, withChatTestWorkspaceCtx(t, forkReq))
	requireStatus(t, rec, http.StatusForbidden, codeCompactionContinueRefused)
}

func TestChatTaskAuthorityCompactionByDMAgentCannotContinue(t *testing.T) {
	a := newAuthorityFixture(t)
	own := a.task(a.dmAgent, "running", taskPurposeCompaction)
	requireStatus(t, a.continueDM(own), http.StatusForbidden, codeCompactionAgentNotCurrent)
}

// A read aimed at another session is refused without a grant record: the grant
// log means the task was allowed to act.
func TestChatTaskAuthorityOtherSessionTargetLogsNoGrant(t *testing.T) {
	a := newAuthorityFixture(t)
	logs := captureJSONLogs(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	otherSession := dbfx.ChatSession(t, a.dmAgent)
	req := withURLParam(withChatTestWorkspaceCtx(t, taskActorReq("/api/chat/sessions/"+otherSession+"/messages", task)), "sessionId", otherSession)
	rec := httptest.NewRecorder()
	testHandler.ListChatMessages(rec, req)
	requireStatus(t, rec, http.StatusForbidden, "")
	if got := logs.records(t, chatTaskAuthorityEvent); len(got) != 0 {
		t.Fatalf("authority log records = %v, want none for a refused target", got)
	}
}

func TestChatTaskAuthorityOrdinaryDMTaskKeepsAccess(t *testing.T) {
	t.Skip("CHE-4873 quarantine (CHE-4909): seeds two active tasks in one chat session; idx_agent_task_queue_one_active_per_chat rejects it")
	a := newAuthorityFixture(t)
	task := a.task(a.dmAgent, "running", "")
	for _, setting := range []string{a.compactAgent, a.dmAgent, ""} {
		if setting == "" {
			a.clearCompact()
		} else {
			a.setCompact(setting)
		}
		requireStatus(t, a.read(task), http.StatusOK, "")
	}
	requireStatus(t, a.compact(task), http.StatusOK, "")

	// The ordinary branch never admits a compaction agent's non-compaction task.
	a.setCompact(a.compactAgent)
	foreign := a.task(a.compactAgent, "running", "")
	requireStatus(t, a.read(foreign), http.StatusForbidden, codeChatTaskAgentMismatch)
}

// The retired settings.compact_agent_id grants nothing: only
// background_agents.compact does (CHE-2894 step 6 re-scope).
func TestChatTaskAuthorityOldCompactAgentKeyGrantsNothing(t *testing.T) {
	a := newAuthorityFixture(t)
	a.clearCompact()
	a.seedOldKey(a.compactAgent)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	requireStatus(t, a.read(task), http.StatusForbidden, codeCompactionAgentNotCurrent)
	requireStatus(t, a.compact(task), http.StatusForbidden, codeCompactionAgentNotCurrent)
}

// Only the primary slot of background_agents.compact is the compaction agent;
// a fallback agent is not (the fallback chain is a runner concern, SEC-1 rule 2).
func TestChatTaskAuthorityFallbackSlotGrantsNothing(t *testing.T) {
	a := newAuthorityFixture(t)
	fallback := eligibleAgent(t, "authority-fallback-agent")
	putBG(t, testWorkspaceID, map[string]any{"compact": map[string]any{"agent_id": a.compactAgent, "fallback_agent_id": fallback}}, nil).Want(http.StatusOK)
	task := a.task(fallback, "running", taskPurposeCompaction)
	requireStatus(t, a.read(task), http.StatusForbidden, codeCompactionAgentNotCurrent)
}

// Manual /compact and auto-compaction both run as the DM agent today and are
// flagged by MarkChatTaskCompact (no purpose). They keep the ordinary
// DM-agent path whatever the compact setting is; only a task stamped
// purpose='compaction' needs the dedicated compact agent.
func TestChatTaskAuthorityManualAndAutoCompactionKeepDMAgentPath(t *testing.T) {
	for _, flow := range []struct {
		name   string
		reason pgtype.Text
		tokens pgtype.Int8
	}{
		{"manual", pgtype.Text{}, pgtype.Int8{}},
		{"auto", pgtype.Text{String: "prompt_size", Valid: true}, pgtype.Int8{Int64: 180000, Valid: true}},
	} {
		for _, setting := range []string{"unset", "other agent", "DM agent"} {
			t.Run(flow.name+"/"+setting, func(t *testing.T) {
				a := newAuthorityFixture(t)
				switch setting {
				case "unset":
					a.clearCompact()
				case "DM agent":
					a.setCompact(a.dmAgent)
				}
				task := a.task(a.dmAgent, "running", "")
				marked, err := testHandler.Queries.MarkChatTaskCompact(context.Background(), db.MarkChatTaskCompactParams{
					ID: parseUUID(task), CompactReason: flow.reason, CompactTriggerTokens: flow.tokens,
				})
				if err != nil {
					t.Fatal(err)
				}
				if marked.TaskKind.String != "compact" || marked.Purpose.Valid {
					t.Fatalf("task_kind=%+v purpose=%+v, want compact with no purpose", marked.TaskKind, marked.Purpose)
				}
				requireStatus(t, a.read(task), http.StatusOK, "")
				requireStatus(t, a.compact(task), http.StatusOK, "")
			})
		}
	}
}

// A retry child copies purpose but not task_kind. It must still be cancelled by
// a compact-chain change, or it would keep DM access with its task token after
// A -> B -> A.
func TestChatTaskAuthorityRetryChildOfCompactionTaskIsRevoked(t *testing.T) {
	a := newAuthorityFixture(t)
	parent := a.task(a.compactAgent, "failed", taskPurposeCompaction)
	child, err := testHandler.Queries.CreateRetryTask(context.Background(), db.CreateRetryTaskParams{ID: parseUUID(parent)})
	if err != nil {
		t.Fatal(err)
	}
	childID := uuidToString(child.ID)
	if !child.Purpose.Valid || child.Purpose.String != taskPurposeCompaction {
		t.Fatalf("retry purpose = %+v, want %s", child.Purpose, taskPurposeCompaction)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, childID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, a.read(childID), http.StatusOK, "")

	a.setCompact(a.dmAgent)
	a.setCompact(a.compactAgent) // A -> B -> A

	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM agent_task_queue WHERE id = $1`, childID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("retry child status = %q after A->B->A, want cancelled", status)
	}
	requireStatus(t, a.read(childID), http.StatusForbidden, codeChatTaskNotActive)
}

// Orphan recovery commits a failed task first and creates its retry later. A
// compact chain change in between (A -> B -> A) found no live task to cancel,
// so the retry must be refused at admission or it would come back active.
func TestChatTaskAuthorityRetryAfterChainChangeIsRefused(t *testing.T) {
	failed := func(a *authorityFixture) db.AgentTaskQueue {
		id := dbfx.Task(a.t, a.compactAgent, testutil.Cols{
			"runtime_id": handlerTestRuntimeID(a.t), "status": "failed", "chat_session_id": a.session,
			"purpose": taskPurposeCompaction, "task_kind": "compact",
			"failure_reason": "timeout", "attempt": 1, "max_attempts": 2,
			"completed_at": testutil.Raw("now()"),
		})
		parent, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		return parent
	}

	t.Run("no chain change: retry is created", func(t *testing.T) {
		a := newAuthorityFixture(t)
		child, err := testHandler.TaskService.MaybeRetryFailedTask(context.Background(), failed(a))
		if err != nil || child == nil {
			t.Fatalf("child=%v err=%v, want a retry", child, err)
		}
	})

	t.Run("A->B->A between failure and retry: refused", func(t *testing.T) {
		a := newAuthorityFixture(t)
		parent := failed(a)
		a.setCompact(a.dmAgent)
		a.setCompact(a.compactAgent)
		child, err := testHandler.TaskService.MaybeRetryFailedTask(context.Background(), parent)
		if err != nil || child != nil {
			t.Fatalf("child=%v err=%v, want no retry after a chain change", child, err)
		}
	})
}

// afterBegin runs hook right after the transaction starts, so a test can order
// other work between the settings transaction's start (now()) and its locks.
type afterBegin struct {
	txStarter
	hook func()
}

func (s afterBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.txStarter.Begin(ctx)
	if err == nil {
		s.hook()
	}
	return tx, err
}

// A settings transaction starts, then a compaction task is created and fails,
// then the transaction takes its locks and commits a fallback-only change. The
// change's ordering mark must come from when it holds the locks, not from
// the transaction start, or the failed task retries into the new chain.
func TestChatTaskAuthorityRetryAfterSlowSettingsTransactionIsRefused(t *testing.T) {
	a := newAuthorityFixture(t)
	var parent db.AgentTaskQueue
	orig := testHandler.TxStarter
	testHandler.TxStarter = afterBegin{orig, func() {
		time.Sleep(20 * time.Millisecond)
		id := dbfx.Task(t, a.compactAgent, testutil.Cols{
			"runtime_id": handlerTestRuntimeID(t), "status": "failed", "chat_session_id": a.session,
			"purpose": taskPurposeCompaction, "task_kind": "compact",
			"failure_reason": "timeout", "attempt": 1, "max_attempts": 2,
			"completed_at": testutil.Raw("now()"),
		})
		var err error
		if parent, err = testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}}
	t.Cleanup(func() { testHandler.TxStarter = orig })

	putBG(t, testWorkspaceID, map[string]any{"compact": map[string]any{
		"agent_id": a.compactAgent, "fallback_agent_id": a.dmAgent,
	}}, nil).Want(http.StatusOK)
	testHandler.TxStarter = orig

	child, err := testHandler.TaskService.MaybeRetryFailedTask(context.Background(), parent)
	if err != nil || child != nil {
		t.Fatalf("child=%v err=%v, want no retry after a change committed behind the failed task", child, err)
	}
}

// A retry admission and a settings write must take the workspace lock before
// the chat_session lock. The retry holding the session lock while it waits for
// the workspace (CreateRetryTask) against a settings write holding the workspace
// and waiting for the session is a deadlock. Here the settings write holds the
// workspace first, a retry starts, then the write asks for the session locks:
// both must finish, and the retry must see the committed change.
func TestChatTaskAuthorityRetryAndSettingsWriteTakeLocksInOneOrder(t *testing.T) {
	a := newAuthorityFixture(t)
	ctx := context.Background()
	id := dbfx.Task(t, a.compactAgent, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t), "status": "failed", "chat_session_id": a.session,
		"purpose": taskPurposeCompaction, "task_kind": "compact",
		"failure_reason": "timeout", "attempt": 1, "max_attempts": 2,
		"completed_at": testutil.Raw("now()"),
	})
	parent, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := testHandler.Queries.WithTx(tx)
	if _, err := q.LockWorkspaceSettings(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}

	type result struct {
		child *db.AgentTaskQueue
		err   error
	}
	done := make(chan result, 1)
	go func() {
		child, err := testHandler.TaskService.MaybeRetryFailedTask(ctx, parent)
		done <- result{child, err}
	}()
	time.Sleep(300 * time.Millisecond) // let the retry reach its first lock

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := q.LockChatSessionsByWorkspace(lockCtx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("settings write could not lock the sessions behind a retry: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("retry error: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retry did not finish after the settings write committed")
	}
}

// FailTask retries in its own transaction, which already holds the session when
// it reaches the retry guard. It must take the workspace first all the same, or
// a settings write holding the workspace and waiting for the sessions deadlocks
// against it and one side rolls back.
func TestChatTaskAuthorityFailTaskAndSettingsWriteTakeLocksInOneOrder(t *testing.T) {
	a := newAuthorityFixture(t)
	ctx := context.Background()
	id := a.task(a.compactAgent, "running", taskPurposeCompaction)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := testHandler.Queries.WithTx(tx)
	if _, err := q.LockWorkspaceSettings(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := testHandler.TaskService.FailTask(ctx, parseUUID(id), "timed out", "", "", "", "timeout", false, "", "")
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the failure reach its first lock

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := q.LockChatSessionsByWorkspace(lockCtx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("settings write could not lock the sessions behind a failing task: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FailTask error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FailTask did not finish after the settings write committed")
	}
}

// The history and thread routes take the same authority check as the messages
// route: a compaction task whose agent is no longer the current compact agent
// reads nothing, even though its token still binds it to the DM session.
func TestChatTaskAuthorityHistoryRoutesUseSharedCheck(t *testing.T) {
	t.Skip("CHE-4873 quarantine (CHE-4909): seeds two active tasks in one chat session; idx_agent_task_queue_one_active_per_chat rejects it")
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"history": testHandler.GetChatChannelHistory,
		"thread":  testHandler.GetChatThread,
	}
	get := func(a *authorityFixture, route, taskID string) int {
		req := withChatTestWorkspaceCtx(a.t, taskActorReq("/api/chat/"+route, taskID))
		rec := httptest.NewRecorder()
		routes[route](rec, req)
		return rec.Code
	}
	for route := range routes {
		t.Run(route, func(t *testing.T) {
			a := newAuthorityFixture(t)
			a.setCompact(a.dmAgent)
			stale := a.task(a.dmAgent, "running", taskPurposeCompaction)
			a.setCompact(a.compactAgent)
			current := a.task(a.compactAgent, "running", taskPurposeCompaction)
			ordinary := a.task(a.dmAgent, "running", "")

			if code := get(a, route, stale); code != http.StatusForbidden {
				t.Fatalf("stale compaction task: status %d, want 403", code)
			}
			for name, id := range map[string]string{"current compact agent": current, "ordinary DM task": ordinary} {
				if code := get(a, route, id); code != http.StatusOK {
					t.Fatalf("%s: status %d, want 200", name, code)
				}
			}
		})
	}
}
