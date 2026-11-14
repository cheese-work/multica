package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5646 (E1): the agent label, MCP binding, preset, skill toggle, archive,
// restore, scheduling pause, cancel-tasks and Mika routes write one
// workspace_audit_event row in the mutation's own transaction. Each route has
// a success case (one row, right kind, member actor, target, no secret) and a
// rollback case (audit insert forced to fail: 500 + Retry-After and the
// mutation is not applied).

const (
	agentAuditSecretReason = "SECRET-PAUSE-REASON"
)

// agentAuditRun is one prepared route call.
type agentAuditRun struct {
	target     func(t *testing.T) string // id of the audit row's target
	call       func(t *testing.T) *httptest.ResponseRecorder
	wantStatus int
	// failFor installs the forced audit failure; default is by target id.
	failFor func(t *testing.T)
	// notApplied fails the test when the mutation leaked past the 500.
	notApplied func(t *testing.T)
	secrets    []string
}

type agentAuditCase struct {
	name, kind, targetType string
	setup                  func(t *testing.T) agentAuditRun
}

func agentRoute(t *testing.T, fn http.HandlerFunc, method, path string, body any, params ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	fn(w, withURLParams(newRequest(method, path, body), params...))
	return w
}

func newAgentLabel(t *testing.T) string {
	t.Helper()
	return dbfx.Insert(t, "issue_label", testutil.Cols{
		"workspace_id": testWorkspaceID, "resource_type": "agent",
		"name": "audit-agent-label-" + t.Name(), "color": "#3b82f6",
	})
}

func agentAuditCases() []agentAuditCase {
	byAgent := func(id string) func(*testing.T) string { return func(*testing.T) string { return id } }
	return []agentAuditCase{
		{"label attach", "agent.label.attach", "agent", func(t *testing.T) agentAuditRun {
			agentID, labelID := createHandlerTestAgent(t, "audit-label-attach", nil), newAgentLabel(t)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.AttachLabelToAgent, "POST", "/api/agents/"+agentID+"/labels",
						map[string]any{"label_id": labelID}, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_to_label WHERE agent_id = $1`, agentID); n != 0 {
						t.Errorf("agent_to_label rows = %d; want 0 (rolled back)", n)
					}
				},
			}
		}},
		{"label detach", "agent.label.detach", "agent", func(t *testing.T) agentAuditRun {
			agentID, labelID := createHandlerTestAgent(t, "audit-label-detach", nil), newAgentLabel(t)
			dbfx.Exec(t, `INSERT INTO agent_to_label (agent_id, label_id) VALUES ($1, $2)`, agentID, labelID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.DetachLabelFromAgent, "DELETE", "/api/agents/"+agentID+"/labels/"+labelID,
						nil, "id", agentID, "labelId", labelID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_to_label WHERE agent_id = $1`, agentID); n != 1 {
						t.Errorf("agent_to_label rows = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"mcp-server remove", "agent.mcp_server.remove", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-mcp-remove", nil)
			serverID := createWorkspaceMcpServerForTest(t, "audit-mcp-remove-"+t.Name(), workspaceMcpTestEntry)
			dbfx.Exec(t, `INSERT INTO agent_mcp_server (agent_id, server_id) VALUES ($1, $2)`, agentID, serverID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK, secrets: []string{workspaceMcpTestSecret, "linear.example"},
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.RemoveAgentMcpServer, "DELETE", "/api/agents/"+agentID+"/mcp-servers/"+serverID,
						nil, "id", agentID, "serverId", serverID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_mcp_server WHERE agent_id = $1`, agentID); n != 1 {
						t.Errorf("agent_mcp_server rows = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"mcp-server enable", "agent.mcp_server.enable", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-mcp-enable", nil)
			serverID := createWorkspaceMcpServerForTest(t, "audit-mcp-enable-"+t.Name(), workspaceMcpTestEntry)
			dbfx.Exec(t, `INSERT INTO agent_mcp_server (agent_id, server_id, enabled) VALUES ($1, $2, true)`, agentID, serverID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK, secrets: []string{workspaceMcpTestSecret, "linear.example"},
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.SetAgentMcpServerEnabled, "PUT", "/api/agents/"+agentID+"/mcp-servers/"+serverID+"/enabled",
						map[string]any{"enabled": false}, "id", agentID, "serverId", serverID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_mcp_server WHERE agent_id = $1 AND enabled`, agentID); n != 1 {
						t.Errorf("enabled bindings = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"skill remove", "agent.skills.update", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-skill-remove", nil)
			skillID := insertHandlerTestSkill(t, "audit-skill-remove", "body")
			dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, skillID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.RemoveAgentSkill, "DELETE", "/api/agents/"+agentID+"/skills/"+skillID,
						nil, "id", agentID, "skillId", skillID)
				},
				notApplied: func(t *testing.T) {
					if got := agentSkillIDs(t, agentID); got != skillID {
						t.Errorf("agent skills = %q; want %q (rolled back)", got, skillID)
					}
				},
			}
		}},
		{"skill enable", "agent.skill.enable", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-skill-enable", nil)
			skillID := insertHandlerTestSkill(t, "audit-skill-enable", "body")
			dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id, enabled) VALUES ($1, $2, true)`, agentID, skillID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.SetAgentSkillEnabled, "PUT", "/api/agents/"+agentID+"/skills/"+skillID+"/enabled",
						map[string]any{"enabled": false}, "id", agentID, "skillId", skillID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_skill WHERE agent_id = $1 AND enabled`, agentID); n != 1 {
						t.Errorf("enabled skills = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"preset create", "agent.preset.create", "agent", func(t *testing.T) agentAuditRun {
			agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
			cleanupAgentPresets(t, agentID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusCreated, secrets: []string{"200k"},
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.CreateAgentPreset, "POST", "/api/agents/"+agentID+"/presets",
						map[string]any{"name": "fast", "model": "claude-sonnet-5-5", "thinking_level": "low",
							"context_args": []string{"--autocompact", "200k"}}, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_preset WHERE agent_id = $1`, agentID); n != 0 {
						t.Errorf("agent_preset rows = %d; want 0 (rolled back)", n)
					}
				},
			}
		}},
		{"preset update", "agent.preset.update", "agent", func(t *testing.T) agentAuditRun {
			agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
			cleanupAgentPresets(t, agentID)
			addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5"), thinking: sp("low")})
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.UpdateAgentPreset, "PATCH", "/api/agents/"+agentID+"/presets/fast",
						map[string]any{"thinking_level": "medium"}, "id", agentID, "name", "fast")
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_preset WHERE agent_id = $1 AND thinking_level = 'low'`, agentID); n != 1 {
						t.Errorf("presets still at thinking_level=low = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"preset delete", "agent.preset.delete", "agent", func(t *testing.T) agentAuditRun {
			agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
			cleanupAgentPresets(t, agentID)
			addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5")})
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusNoContent,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.DeleteAgentPreset, "DELETE", "/api/agents/"+agentID+"/presets/fast",
						nil, "id", agentID, "name", "fast")
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_preset WHERE agent_id = $1`, agentID); n != 1 {
						t.Errorf("agent_preset rows = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"preset default", "agent.preset.default", "agent", func(t *testing.T) agentAuditRun {
			agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
			cleanupAgentPresets(t, agentID)
			addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5")})
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.SetAgentDefaultPreset, "PUT", "/api/agents/"+agentID+"/presets/default",
						map[string]any{"name": "fast"}, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_preset WHERE agent_id = $1 AND is_default`, agentID); n != 0 {
						t.Errorf("default presets = %d; want 0 (rolled back)", n)
					}
				},
			}
		}},
		{"archive", "agent.archive", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-archive", nil)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.ArchiveAgent, "POST", "/api/agents/"+agentID+"/archive", nil, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE id = $1 AND archived_at IS NULL`, agentID); n != 1 {
						t.Errorf("agent still active = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"restore", "agent.restore", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-restore", nil)
			dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, agentID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.RestoreAgent, "POST", "/api/agents/"+agentID+"/restore", nil, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE id = $1 AND archived_at IS NOT NULL`, agentID); n != 1 {
						t.Errorf("agent still archived = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"pause", "agent.pause", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-pause", nil)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK, secrets: []string{agentAuditSecretReason},
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.PauseAgentScheduling, "POST", "/api/agents/"+agentID+"/pause",
						map[string]any{"reason": agentAuditSecretReason}, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE id = $1 AND scheduling_paused_at IS NULL`, agentID); n != 1 {
						t.Errorf("agent still running = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"resume", "agent.resume", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-resume", nil)
			dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, agentID)
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.ResumeAgentScheduling, "POST", "/api/agents/"+agentID+"/resume", nil, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE id = $1 AND scheduling_paused_at IS NOT NULL`, agentID); n != 1 {
						t.Errorf("agent still paused = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"cancel-tasks", "agent.tasks.cancel", "agent", func(t *testing.T) agentAuditRun {
			agentID := createHandlerTestAgent(t, "audit-cancel-tasks", nil)
			dbfx.Task(t, agentID, testutil.Cols{"runtime_id": handlerTestRuntimeID(t)})
			return agentAuditRun{
				target: byAgent(agentID), wantStatus: http.StatusOK,
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return agentRoute(t, testHandler.CancelAgentTasks, "POST", "/api/agents/"+agentID+"/cancel-tasks", nil, "id", agentID)
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status = 'queued'`, agentID); n != 1 {
						t.Errorf("queued tasks = %d; want 1 (rolled back)", n)
					}
				},
			}
		}},
		{"mika", "agent.create", "agent", func(t *testing.T) agentAuditRun {
			cleanupMika(t)
			return agentAuditRun{
				target: func(t *testing.T) string {
					var id string
					dbfx.QueryRow(t, `SELECT id FROM agent WHERE workspace_id = $1 AND system_key = $2`,
						testWorkspaceID, service.MikaSystemKey).Scan(&id)
					return id
				},
				wantStatus: http.StatusCreated,
				failFor:    func(t *testing.T) { failSquadSkillAuditForKind(t, "agent.create", testWorkspaceID) },
				call: func(t *testing.T) *httptest.ResponseRecorder {
					return createMika(t, map[string]any{"runtime_id": handlerTestRuntimeID(t), "language": "en"})
				},
				notApplied: func(t *testing.T) {
					if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE workspace_id = $1 AND system_key = $2`,
						testWorkspaceID, service.MikaSystemKey); n != 0 {
						t.Errorf("mika agents = %d; want 0 (rolled back)", n)
					}
				},
			}
		}},
	}
}

func TestAgentRoutes_RecordAuditRow(t *testing.T) {
	requireDB(t)
	for _, tc := range agentAuditCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.setup(t)
			if w := run.call(t); w.Code != run.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, run.wantStatus, w.Body.String())
			}
			row := onlyAuditRow(t, tc.kind, run.target(t), tc.targetType)
			if row.Kind != tc.kind {
				t.Errorf("kind = %q; want %q", row.Kind, tc.kind)
			}
			assertNoneStored(t, row, run.secrets...)
		})
	}
}

func TestAgentRoutes_AuditInsertFailureRollsBack(t *testing.T) {
	requireDB(t)
	for _, tc := range agentAuditCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.setup(t)
			if run.failFor != nil {
				run.failFor(t)
			} else {
				failAuditInsertFor(t, run.target(t))
			}
			assertAuditFailure(t, run.call(t))
			run.notApplied(t)
		})
	}
}
