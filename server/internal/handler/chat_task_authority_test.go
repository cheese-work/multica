package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
		// Continue archives or forks the DM; a compaction finishes in place only.
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

// authorityFixture is a DM on agent D with a compaction agent C configured.
type authorityFixture struct {
	*compactAgentFixture
	dmAgent, compactAgent, session string
}

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
	f := newCompactAgentFixture(t)
	a := &authorityFixture{compactAgentFixture: f, dmAgent: f.eligibleAgent("authority-dm-agent"), compactAgent: f.eligibleAgent("authority-compact-agent")}
	a.session = dbfx.ChatSession(t, a.dmAgent)
	f.seed(a.compactAgent)
	return a
}

func (a *authorityFixture) task(agentID, status, purpose string) string {
	a.t.Helper()
	cols := testutil.Cols{"runtime_id": handlerTestRuntimeID(a.t), "status": status, "chat_session_id": a.session}
	if purpose != "" {
		cols["purpose"] = purpose
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
	logs := captureLogs(t)
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
	logs := captureLogs(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	a.seed(a.dmAgent) // the setting moves to another agent

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
		{"clear", func(a *authorityFixture, _ string) {
			a.seed(a.dmAgent)
			a.send(http.MethodPatch, map[string]any{"compact_agent_id": nil}, humanActor)
		}},
		{"A to B", func(a *authorityFixture, other string) {
			a.send(http.MethodPatch, map[string]any{"compact_agent_id": other}, humanActor)
		}},
		{"A to B to A", func(a *authorityFixture, other string) {
			a.send(http.MethodPatch, map[string]any{"compact_agent_id": other}, humanActor)
			a.send(http.MethodPatch, map[string]any{"compact_agent_id": a.dmAgent}, humanActor)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthorityFixture(t)
			a.seed(a.dmAgent) // A = the DM agent is the compaction agent
			task := a.task(a.dmAgent, "running", taskPurposeCompaction)
			other := a.eligibleAgent("authority-other-agent")
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

func TestChatTaskAuthorityContinueRefusesCompactionEverywhere(t *testing.T) {
	a := newAuthorityFixture(t)
	task := a.task(a.compactAgent, "running", taskPurposeCompaction)
	requireStatus(t, a.continueDM(task), http.StatusForbidden, codeCompactionContinueRefused)

	forkReq := newRequest(http.MethodPost, "/api/chat/fork", map[string]any{})
	forkReq.Header.Set("X-Actor-Source", "task_token")
	forkReq.Header.Set("X-Task-ID", task)
	rec := httptest.NewRecorder()
	testHandler.ForkChatSession(rec, withChatTestWorkspaceCtx(t, forkReq))
	requireStatus(t, rec, http.StatusForbidden, codeCompactionContinueRefused)

	// Same refusal when the compaction agent is also the DM agent.
	a.seed(a.dmAgent)
	own := a.task(a.dmAgent, "running", taskPurposeCompaction)
	requireStatus(t, a.continueDM(own), http.StatusForbidden, codeCompactionContinueRefused)
}

func TestChatTaskAuthorityOrdinaryDMTaskKeepsAccess(t *testing.T) {
	a := newAuthorityFixture(t)
	task := a.task(a.dmAgent, "running", "")
	for _, setting := range []string{a.compactAgent, a.dmAgent, ""} {
		if setting == "" {
			dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'compact_agent_id' WHERE id = $1`, testWorkspaceID)
		} else {
			a.seed(setting)
		}
		requireStatus(t, a.read(task), http.StatusOK, "")
	}
	requireStatus(t, a.compact(task), http.StatusOK, "")

	// The ordinary branch never admits a compaction agent's non-compaction task.
	a.seed(a.compactAgent)
	foreign := a.task(a.compactAgent, "running", "")
	requireStatus(t, a.read(foreign), http.StatusForbidden, codeChatTaskAgentMismatch)
}
