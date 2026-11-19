package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2913 (CHE-2894 step 10): /compact and auto-compact go to the agent named
// by background_agents.compact. Everything is asserted through the real send,
// complete and retry paths plus raw SQL, so the file builds on a tree without
// the feature.

type routedCompaction struct {
	AgentID, Status          string
	Purpose, Source, Session *string
	Context                  map[string]any
}

func routedCompactions(t *testing.T, sessionID string) []routedCompaction {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT agent_id::text, status, purpose, source_chat_session_id::text, chat_session_id::text, COALESCE(context, '{}'::jsonb)
		FROM agent_task_queue WHERE chat_session_id = $1 AND task_kind = 'compact' ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatalf("read compaction tasks: %v", err)
	}
	defer rows.Close()
	var out []routedCompaction
	for rows.Next() {
		var r routedCompaction
		var raw []byte
		if err := rows.Scan(&r.AgentID, &r.Status, &r.Purpose, &r.Source, &r.Session, &raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw, &r.Context)
		out = append(out, r)
	}
	return out
}

// compactionAgentOn is an eligible workspace agent on the given runtime, so a
// test can complete its task through the DM daemon.
func compactionAgentOn(t *testing.T, runtimeID, name string) string {
	t.Helper()
	agentID := dbfx.Agent(t, name, runtimeID, testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2)`, agentID, testWorkspaceID)
	return agentID
}

// compactingEvents captures the compacting flag events of one session as JSON.
func compactingEvents(t *testing.T, sessionID string) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var seen []map[string]any
	testHandler.Bus.Subscribe(protocol.EventChatSessionUpdated, func(e events.Event) {
		raw, err := json.Marshal(e.Payload)
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil || m["chat_session_id"] != sessionID || m["compacting"] != true {
			return
		}
		mu.Lock()
		seen = append(seen, m)
		mu.Unlock()
	})
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), seen...)
	}
}

type compactRouteFixture struct {
	dm           autoCompactDM
	compactAgent string
}

// newCompactRouteFixture is a DM plus an eligible compaction agent named by the
// workspace setting. Pass withSetting=false for the empty-setting regression.
func newCompactRouteFixture(t *testing.T, withSetting bool) compactRouteFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, legacyPct80, `{}`)
	f := compactRouteFixture{dm: dm, compactAgent: eligibleAgent(t, "route-compact-"+t.Name())}
	if withSetting {
		putBG(t, testWorkspaceID, map[string]any{"compact": map[string]any{"agent_id": f.compactAgent}}, nil).Want(200)
	}
	return f
}

func (f compactRouteFixture) sendCompact(t *testing.T) {
	t.Helper()
	sendDirectChat(t, context.Background(), f.dm.agentID, f.dm.sessionID, "/compact keep the decisions")
}

// triggerAuto completes one turn past the threshold, which queues auto-compact.
func (f compactRouteFixture) triggerAuto(t *testing.T) {
	t.Helper()
	f.dm.complete(t, f.dm.sendRunning(t, "hello"), 170000, 200000)
}

func (f compactRouteFixture) only(t *testing.T) routedCompaction {
	t.Helper()
	got := routedCompactions(t, f.dm.sessionID)
	if len(got) != 1 {
		t.Fatalf("compaction tasks = %d, want 1", len(got))
	}
	return got[0]
}

func ptrStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

var routeTriggers = map[string]func(*testing.T, compactRouteFixture){
	"manual": func(t *testing.T, f compactRouteFixture) { f.sendCompact(t) },
	"auto":   func(t *testing.T, f compactRouteFixture) { f.triggerAuto(t) },
}

func TestCompactRoute_GoesToCompactionAgent(t *testing.T) {
	for name, trigger := range routeTriggers {
		t.Run(name, func(t *testing.T) {
			f := newCompactRouteFixture(t, true)
			trigger(t, f)
			got := f.only(t)
			if got.AgentID != f.compactAgent {
				t.Fatalf("agent = %s, want compaction agent %s", got.AgentID, f.compactAgent)
			}
			if ptrStr(got.Purpose) != "compaction" {
				t.Fatalf("purpose = %s, want compaction", ptrStr(got.Purpose))
			}
			// Both session columns in the same insert (CHE-4095 condition 3).
			if ptrStr(got.Session) != f.dm.sessionID || ptrStr(got.Source) != f.dm.sessionID {
				t.Fatalf("chat_session_id=%s source_chat_session_id=%s, want both %s", ptrStr(got.Session), ptrStr(got.Source), f.dm.sessionID)
			}
			if got.Context["chain_source"] != "workspace" || got.Context["slot"] != "primary" || got.Context["member_id"] != testUserID {
				t.Fatalf("context = %v, want chain_source=workspace slot=primary member_id=%s", got.Context, testUserID)
			}
			if _, ok := got.Context["fallback_reason"]; ok {
				t.Fatalf("dedicated route carries fallback_reason: %v", got.Context)
			}
		})
	}
}

func TestCompactRoute_EmptySettingKeepsDMAgentPath(t *testing.T) {
	for name, trigger := range routeTriggers {
		t.Run(name, func(t *testing.T) {
			f := newCompactRouteFixture(t, false)
			trigger(t, f)
			got := f.only(t)
			if got.AgentID != f.dm.agentID || got.Purpose != nil || got.Source != nil {
				t.Fatalf("agent=%s purpose=%s source=%s, want DM agent %s with NULL purpose and source", got.AgentID, ptrStr(got.Purpose), ptrStr(got.Source), f.dm.agentID)
			}
			if _, ok := got.Context["fallback_reason"]; ok {
				t.Fatalf("empty setting is not a fallback, context = %v", got.Context)
			}
		})
	}
}

func TestCompactRoute_FallbackReasons(t *testing.T) {
	reasons := []struct {
		reason string
		break_ func(t *testing.T, f compactRouteFixture)
	}{
		{"agent_archived", func(t *testing.T, f compactRouteFixture) {
			dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.compactAgent)
		}},
		{"agent_ineligible", func(t *testing.T, f compactRouteFixture) {
			dbfx.Exec(t, `UPDATE agent SET visibility = 'private' WHERE id = $1`, f.compactAgent)
		}},
		{"no_invoke_right", func(t *testing.T, f compactRouteFixture) {
			other := dbfx.User(t, "Route Owner", fmt.Sprintf("route-owner-%d@example.com", time.Now().UnixNano()))
			dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, f.compactAgent)
			dbfx.Exec(t, `UPDATE agent SET owner_id = $1, permission_mode = 'private' WHERE id = $2`, other, f.compactAgent)
		}},
		{"runtime_offline", func(t *testing.T, f compactRouteFixture) {
			dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = (SELECT runtime_id FROM agent WHERE id = $1)`, f.compactAgent)
		}},
	}
	for _, rc := range reasons {
		for name, trigger := range routeTriggers {
			t.Run(rc.reason+"/"+name, func(t *testing.T) {
				f := newCompactRouteFixture(t, true)
				rc.break_(t, f)
				events := compactingEvents(t, f.dm.sessionID)
				trigger(t, f)
				got := f.only(t)
				if got.AgentID != f.dm.agentID || got.Purpose != nil {
					t.Fatalf("agent=%s purpose=%s, want DM agent %s with NULL purpose", got.AgentID, ptrStr(got.Purpose), f.dm.agentID)
				}
				if got.Context["fallback_reason"] != rc.reason {
					t.Fatalf("context.fallback_reason = %v, want %s", got.Context["fallback_reason"], rc.reason)
				}
				found := false
				for _, e := range events() {
					if e["compact_fallback_reason"] == rc.reason && e["compact_agent_name"] != nil && e["compact_agent_name"] != "" {
						found = true
					}
				}
				if !found {
					t.Fatalf("no compacting event with compact_fallback_reason=%s and an agent name: %v", rc.reason, events())
				}
			})
		}
	}
}

func TestCompactRoute_EventNamesCompactionAgent(t *testing.T) {
	f := newCompactRouteFixture(t, true)
	events := compactingEvents(t, f.dm.sessionID)
	f.sendCompact(t)
	var name string
	dbfx.QueryRow(t, `SELECT name FROM agent WHERE id = $1`, f.compactAgent).Scan(&name)
	for _, e := range events() {
		if e["compact_agent_name"] == name && e["compact_fallback_reason"] == nil {
			return
		}
	}
	t.Fatalf("no compacting event naming %q without a fallback reason: %v", name, events())
}

// The compaction agent writes the divider itself; its completion output must
// not become an assistant reply. The DM-agent path keeps today's reply.
func TestCompactRoute_CompletionWritesNoAssistantMessage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withSetting bool
		wantReplies int
	}{{"dedicated", true, 0}, {"empty setting", false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompactRouteFixture(t, false)
			if tc.withSetting {
				f.compactAgent = compactionAgentOn(t, f.dm.runtimeID, "route-same-runtime-"+t.Name())
				putBG(t, testWorkspaceID, map[string]any{"compact": map[string]any{"agent_id": f.compactAgent}}, nil).Want(200)
			}
			f.sendCompact(t)
			task := f.only(t)
			var taskID string
			dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE chat_session_id = $1 AND task_kind = 'compact'`, f.dm.sessionID).Scan(&taskID)
			markTaskRunning(t, context.Background(), taskID)
			f.dm.complete(t, taskID, 0, 0)
			replies := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'assistant' AND COALESCE(message_kind, 'message') IN ('message', 'no_response')`, f.dm.sessionID)
			if replies != tc.wantReplies {
				t.Fatalf("assistant replies after completing %s (agent %s) = %d, want %d", tc.name, task.AgentID, replies, tc.wantReplies)
			}
		})
	}
}

// A retry clone of a real compaction task keeps both session columns.
func TestCompactRoute_RetryCloneKeepsSessionColumns(t *testing.T) {
	f := newCompactRouteFixture(t, true)
	f.sendCompact(t)
	var parentID string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE chat_session_id = $1 AND task_kind = 'compact'`, f.dm.sessionID).Scan(&parentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', attempt = 1, max_attempts = 3 WHERE id = $1`, parentID)
	child, err := testHandler.Queries.CreateRetryTask(context.Background(), db.CreateRetryTaskParams{ID: util.MustParseUUID(parentID)})
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if uuidToString(child.ChatSessionID) != f.dm.sessionID || uuidToString(child.SourceChatSessionID) != f.dm.sessionID || child.Purpose.String != "compaction" {
		t.Fatalf("retry clone chat_session_id=%s source=%s purpose=%q, want both %s and compaction",
			uuidToString(child.ChatSessionID), uuidToString(child.SourceChatSessionID), child.Purpose.String, f.dm.sessionID)
	}
}

// Member B sees a routed compaction row (presence) but never its content on
// the dedicated route, because both session columns tie the row to the DM
// creator. DM-agent rows are covered by chat_task_privacy_test.go.
func TestCompactRoute_OtherMemberSeesRowMetadataOnly(t *testing.T) {
	f := newCompactRouteFixture(t, true)
	f.sendCompact(t)
	var taskID string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE chat_session_id = $1 AND task_kind = 'compact'`, f.dm.sessionID).Scan(&taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now(), result = '{"output":"private summary"}'::jsonb, error = 'private error' WHERE id = $1`, taskID)

	otherID := dbfx.User(t, "Compact viewer", fmt.Sprintf("compact-viewer-%d@example.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, otherID, "member")
	list := func(userID string) AgentTaskResponse {
		req := newRequestAs(userID, "GET", "/api/agent-task-snapshot", nil)
		var tasks []AgentTaskResponse
		testutil.Call(t, testHandler.ListWorkspaceAgentTaskSnapshot, req).Want(200).JSON(&tasks)
		for _, task := range tasks {
			if task.ID == taskID {
				return task
			}
		}
		t.Fatalf("task %s missing from %s's list; rows stay listed for presence", taskID, userID)
		return AgentTaskResponse{}
	}
	if own := list(testUserID); own.Result == nil || own.Error == nil {
		t.Fatalf("creator sees result=%v error=%v, want full content", own.Result, own.Error)
	}
	if other := list(otherID); other.Result != nil || other.Error != nil || other.ChatSessionID != f.dm.sessionID {
		t.Fatalf("member B sees result=%v error=%v chat_session_id=%q, want content redacted and metadata kept", other.Result, other.Error, other.ChatSessionID)
	}
}
