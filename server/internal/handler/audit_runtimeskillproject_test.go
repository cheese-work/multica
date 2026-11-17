package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5647 (CHE-4807 E2): runtime pause/resume/update/local-skill import, skill
// file and label writes, and project wakeup-definition and GitHub route-grant
// writes record one workspace_audit_event row in the mutation's transaction.
// Every case runs twice: a success run (one row, no secret in it) and a
// rollback run (the audit insert fails, so 500 + Retry-After and nothing applied).

var e2Seq atomic.Int64

func e2Name(prefix string) string { return fmt.Sprintf("%s-%d", prefix, e2Seq.Add(1)) }

// e2Run is one prepared mutation. applied reports whether the mutation took effect.
type e2Run struct {
	targetID string
	do       func() *httptest.ResponseRecorder
	wantCode int
	applied  func(t *testing.T) bool
	// secrets must never reach the stored before/after.
	secrets []string
	// wantAfter / wantBefore are exact stored values that must be present.
	wantBefore, wantAfter map[string]any
}

type e2Case struct {
	name, kind, targetType string
	setup                  func(t *testing.T) e2Run
}

func e2Req(method, path string, body any, kv ...string) *http.Request {
	return withURLParams(newRequest(method, path, body), kv...)
}

func e2Daemon(t *testing.T, name string) (runtimeID, daemonID string) {
	daemonID = e2Name(name)
	runtimeID = dbfx.Runtime(t, daemonID, testutil.Cols{"daemon_id": daemonID, "runtime_mode": "local"})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE daemon_id = $1`, daemonID)
	})
	return runtimeID, daemonID
}

func e2HasPause(t *testing.T, daemonID string) bool {
	return dbfx.Count(t, `SELECT count(*) FROM daemon_pause WHERE daemon_id = $1`, daemonID) == 1
}

func e2Skill(t *testing.T, prefix string) string {
	id := insertHandlerTestSkill(t, prefix, "body")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM skill_file WHERE skill_id = $1`, id)
		testPool.Exec(context.Background(), `DELETE FROM skill_to_label WHERE skill_id = $1`, id)
		testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, id)
	})
	return id
}

func e2SkillLabel(t *testing.T) string {
	return dbfx.Insert(t, "issue_label", testutil.Cols{
		"workspace_id": testWorkspaceID, "resource_type": "skill", "name": e2Name("e2-label"), "description": "", "color": "#fff",
	})
}

const e2SecretText = "E2-SECRET-TEXT"

var e2Cases = []e2Case{
	{"runtime pause", "runtime.pause", "runtime", func(t *testing.T) e2Run {
		rt, daemon := e2Daemon(t, "e2-pause")
		return e2Run{
			targetID: rt, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.PauseDaemon(w, e2Req(http.MethodPut, "/api/runtimes/"+rt+"/pause", map[string]any{"reason": "incident"}, "runtimeId", rt))
				return w
			},
			applied:   func(t *testing.T) bool { return e2HasPause(t, daemon) },
			wantAfter: map[string]any{"paused": true, "reason": "incident"},
		}
	}},
	{"runtime resume", "runtime.resume", "runtime", func(t *testing.T) e2Run {
		rt, daemon := e2Daemon(t, "e2-resume")
		pauseDaemonForTest(t, daemon)
		return e2Run{
			targetID: rt, wantCode: http.StatusNoContent,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.ResumeDaemon(w, e2Req(http.MethodDelete, "/api/runtimes/"+rt+"/pause", nil, "runtimeId", rt))
				return w
			},
			applied:    func(t *testing.T) bool { return !e2HasPause(t, daemon) },
			wantBefore: map[string]any{"paused": true, "reason": "maintenance"},
			wantAfter:  map[string]any{"paused": false},
		}
	}},
	{"runtime update", "runtime.update", "runtime", func(t *testing.T) e2Run {
		rt := dbfx.Runtime(t, e2Name("e2-update"))
		return e2Run{
			targetID: rt, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.InitiateUpdate(w, e2Req(http.MethodPost, "/api/runtimes/"+rt+"/update", map[string]any{"target_version": "v9.9.9"}, "runtimeId", rt))
				return w
			},
			applied: func(t *testing.T) bool {
				pending, err := testHandler.UpdateStore.HasPending(context.Background(), rt)
				return err == nil && pending
			},
			wantAfter: map[string]any{"target_version": "v9.9.9"},
		}
	}},
	{"runtime local-skill import", "runtime.local_skill.import", "runtime", func(t *testing.T) e2Run {
		rt := dbfx.Runtime(t, e2Name("e2-import"))
		return e2Run{
			targetID: rt, wantCode: http.StatusOK, secrets: []string{e2SecretText},
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.InitiateImportLocalSkill(w, e2Req(http.MethodPost, "/api/runtimes/"+rt+"/local-skills/import", map[string]any{
					"skill_key": "my-skill", "name": e2SecretText, "description": e2SecretText,
				}, "runtimeId", rt))
				return w
			},
			applied: func(t *testing.T) bool {
				pending, err := testHandler.LocalSkillImportStore.HasPending(context.Background(), rt)
				return err == nil && pending
			},
			wantAfter: map[string]any{"skill_key": "my-skill"},
		}
	}},
	{"skill file upsert", "skill.update", "skill", func(t *testing.T) e2Run {
		id := e2Skill(t, "e2-file-put")
		return e2Run{
			targetID: id, wantCode: http.StatusOK, secrets: []string{e2SecretText},
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.UpsertSkillFile(w, e2Req(http.MethodPut, "/api/skills/"+id+"/files", map[string]any{"path": "notes.md", "content": e2SecretText}, "id", id))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM skill_file WHERE skill_id = $1`, id) == 1
			},
			wantAfter: map[string]any{"file_count": float64(1)},
		}
	}},
	{"skill file delete", "skill.update", "skill", func(t *testing.T) e2Run {
		id := e2Skill(t, "e2-file-del")
		fileID := dbfx.Insert(t, "skill_file", testutil.Cols{"skill_id": id, "path": "notes.md", "content": e2SecretText})
		return e2Run{
			targetID: id, wantCode: http.StatusNoContent, secrets: []string{e2SecretText},
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.DeleteSkillFile(w, e2Req(http.MethodDelete, "/api/skills/"+id+"/files/"+fileID, nil, "id", id, "fileId", fileID))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM skill_file WHERE skill_id = $1`, id) == 0
			},
			wantBefore: map[string]any{"file_count": float64(1)},
			wantAfter:  map[string]any{"file_count": float64(0)},
		}
	}},
	{"skill version restore", "skill.update", "skill", func(t *testing.T) e2Run {
		id := e2Skill(t, "e2-restore")
		dbfx.Insert(t, "skill_file", testutil.Cols{"skill_id": id, "path": "notes.md", "content": "later notes"})
		dbfx.Exec(t, `INSERT INTO skill_version (workspace_id, skill_id, version, name, content, content_hash, source)
			SELECT workspace_id, id, 1, name, content, 'e2-restore', 'update' FROM skill WHERE id = $1`, id)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM skill_version WHERE skill_id = $1`, id)
		})
		return e2Run{
			targetID: id, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.RestoreSkillVersion(w, e2Req(http.MethodPost, "/api/skills/"+id+"/versions/1/restore", nil, "id", id, "version", "1"))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM skill_file WHERE skill_id = $1`, id) == 0
			},
			wantBefore: map[string]any{"file_count": float64(1)},
			wantAfter:  map[string]any{"file_count": float64(0)},
		}
	}},
	{"skill label add", "skill.label.attach", "skill", func(t *testing.T) e2Run {
		id, label := e2Skill(t, "e2-label-add"), e2SkillLabel(t)
		return e2Run{
			targetID: id, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.AttachLabelToSkill(w, e2Req(http.MethodPost, "/api/skills/"+id+"/labels", map[string]any{"label_id": label}, "id", id))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM skill_to_label WHERE skill_id = $1`, id) == 1
			},
			wantAfter: map[string]any{"label_ids": label},
		}
	}},
	{"skill label remove", "skill.label.detach", "skill", func(t *testing.T) e2Run {
		id, label := e2Skill(t, "e2-label-del"), e2SkillLabel(t)
		dbfx.Exec(t, `INSERT INTO skill_to_label (skill_id, label_id) VALUES ($1, $2)`, id, label)
		return e2Run{
			targetID: id, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.DetachLabelFromSkill(w, e2Req(http.MethodDelete, "/api/skills/"+id+"/labels/"+label, nil, "id", id, "labelId", label))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM skill_to_label WHERE skill_id = $1`, id) == 0
			},
			wantBefore: map[string]any{"label_ids": label},
		}
	}},
	{"project wakeup definition create", "project.wakeup_definition.create", "project", func(t *testing.T) e2Run {
		k := newWakeupDefinitionKit(t)
		return e2Run{
			targetID: k.projectID, wantCode: http.StatusCreated, secrets: []string{e2SecretText},
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				k.project().api().Create(w, e2Req(http.MethodPost, "/wakeup-definitions", wdBody(0, cfg(map[string]any{
					"trigger": map[string]any{"kind": "pr_merged"}, "instruction": e2SecretText,
				})), "id", k.projectID))
				return w
			},
			applied: func(t *testing.T) bool { return wdCount(t) == 1 },
		}
	}},
	{"project wakeup definition put", "project.wakeup_definition.update", "project", func(t *testing.T) e2Run {
		k := newWakeupDefinitionKit(t)
		return e2Run{
			targetID: k.projectID, wantCode: http.StatusOK, secrets: []string{e2SecretText},
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				k.project().api().Put(w, e2Req(http.MethodPut, "/wakeup-definitions/child_done", wdBody(0, cfg(map[string]any{"name": e2SecretText})), "id", k.projectID, "rule", "child_done"))
				return w
			},
			applied:   func(t *testing.T) bool { return wdCount(t) == 1 },
			wantAfter: map[string]any{"rule_key": "child_done"},
		}
	}},
	{"project wakeup definition delete", "project.wakeup_definition.delete", "project", func(t *testing.T) e2Run {
		k := newWakeupDefinitionKit(t)
		k.project().put("child_done", 0, cfg(map[string]any{"name": e2SecretText})).Want(http.StatusOK)
		return e2Run{
			targetID: k.projectID, wantCode: http.StatusNoContent, secrets: []string{e2SecretText},
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				k.project().api().Delete(w, e2Req(http.MethodDelete, "/wakeup-definitions/child_done?revision=1", nil, "id", k.projectID, "rule", "child_done"))
				return w
			},
			applied:    func(t *testing.T) bool { return wdCount(t) == 0 },
			wantBefore: map[string]any{"rule_key": "child_done"},
		}
	}},
	{"project route grant set", "project.route_grant.set", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-grant-set"))
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM github_route_grant WHERE project_id = $1`, project)
		})
		return e2Run{
			targetID: project, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.SetGitHubRouteGrant(w, e2Req(http.MethodPut, "/api/projects/"+project+"/github-route-grants/"+testUserID, nil, "id", project, "userId", testUserID))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM github_route_grant WHERE project_id = $1`, project) == 1
			},
			wantAfter: map[string]any{"route_grant_user_id": testUserID},
		}
	}},
	{"project route grant remove", "project.route_grant.remove", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-grant-del"))
		dbfx.Exec(t, `INSERT INTO github_route_grant (project_id, user_id, granted_by) VALUES ($1, $2, $2)`, project, testUserID)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM github_route_grant WHERE project_id = $1`, project)
		})
		return e2Run{
			targetID: project, wantCode: http.StatusNoContent,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.DeleteGitHubRouteGrant(w, e2Req(http.MethodDelete, "/api/projects/"+project+"/github-route-grants/"+testUserID, nil, "id", project, "userId", testUserID))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM github_route_grant WHERE project_id = $1`, project) == 0
			},
			wantBefore: map[string]any{"route_grant_user_id": testUserID},
		}
	}},
	// CHE-7023: v0.10.0 routes that change who sees a project, whether it runs
	// automation, or which connected apps an agent may call.
	{"project archive", "project.archive", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-archive"))
		return e2Run{
			targetID: project, wantCode: http.StatusOK,
			do:         func() *httptest.ResponseRecorder { return archiveProjectForTest(t, testUserID, project) },
			applied:    func(t *testing.T) bool { return e2ProjectArchived(t, project) },
			wantBefore: map[string]any{"archived": false}, wantAfter: map[string]any{"archived": true},
		}
	}},
	{"project restore", "project.restore", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-restore"))
		dbfx.Exec(t, `UPDATE project SET archived_at = now() WHERE id = $1`, project)
		return e2Run{
			targetID: project, wantCode: http.StatusOK,
			do:         func() *httptest.ResponseRecorder { return restoreProjectForTest(t, testUserID, project) },
			applied:    func(t *testing.T) bool { return !e2ProjectArchived(t, project) },
			wantBefore: map[string]any{"archived": true}, wantAfter: map[string]any{"archived": false},
		}
	}},
	{"project member add", "project.member.add", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-member-add"))
		user := createTestUserAndMember(t, "member")
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM project_member WHERE project_id = $1`, project)
		})
		return e2Run{
			targetID: project, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.AddProjectMembers(w, e2Req(http.MethodPost, "/api/projects/"+project+"/members", map[string]any{"user_ids": []string{user}}, "id", project))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM project_member WHERE project_id = $1 AND user_id = $2`, project, user) == 1
			},
			wantAfter: map[string]any{"member_user_ids": user},
		}
	}},
	{"project member remove", "project.member.remove", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-member-del"))
		user := createTestUserAndMember(t, "member")
		dbfx.Exec(t, `INSERT INTO project_member (workspace_id, project_id, user_id, added_by) VALUES ($1, $2, $3, $4)`, testWorkspaceID, project, user, testUserID)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM project_member WHERE project_id = $1`, project)
		})
		return e2Run{
			targetID: project, wantCode: http.StatusNoContent,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.RemoveProjectMember(w, e2Req(http.MethodDelete, "/api/projects/"+project+"/members/"+user, nil, "id", project, "userId", user))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM project_member WHERE project_id = $1 AND user_id = $2`, project, user) == 0
			},
			wantBefore: map[string]any{"member_user_ids": user},
		}
	}},
	{"project owner join", "project.member.add", "project", func(t *testing.T) e2Run {
		project := dbfx.Project(t, e2Name("e2-join"))
		dbfx.Exec(t, `UPDATE project SET visibility = 'private' WHERE id = $1`, project)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM project_member WHERE project_id = $1`, project)
		})
		return e2Run{
			targetID: project, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				testHandler.JoinPrivateProject(w, e2Req(http.MethodPost, "/api/projects/"+project+"/join", nil, "id", project))
				return w
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM project_member WHERE project_id = $1 AND user_id = $2`, project, testUserID) == 1
			},
			wantAfter: map[string]any{"member_user_ids": testUserID},
		}
	}},
	{"agent apps update", "agent.apps.update", "agent", func(t *testing.T) e2Run {
		f := newAppsFixture(t)
		return e2Run{
			targetID: f.agentID, wantCode: http.StatusOK,
			do: func() *httptest.ResponseRecorder {
				return f.putAgentApps(t, testUserID, []map[string]string{{"toolkit_slug": "gmail", "account_scope": "personal", "level": "read"}})
			},
			applied: func(t *testing.T) bool {
				return dbfx.Count(t, `SELECT count(*) FROM agent_app WHERE agent_id = $1`, f.agentID) == 1
			},
			wantAfter: map[string]any{"apps": "gmail/personal/read"},
		}
	}},
}

func e2ProjectArchived(t *testing.T, project string) bool {
	return dbfx.Count(t, `SELECT count(*) FROM project WHERE id = $1 AND archived_at IS NOT NULL`, project) == 1
}

func e2AssertContains(t *testing.T, label string, got, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s[%q] = %v; want %v (full: %v)", label, k, got[k], v, got)
		}
	}
}

func TestE2Routes_RecordOneAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, c := range e2Cases {
		t.Run(c.name, func(t *testing.T) {
			run := c.setup(t)
			w := run.do()
			if w.Code != run.wantCode {
				t.Fatalf("status = %d, want %d: %s", w.Code, run.wantCode, w.Body.String())
			}
			if !run.applied(t) {
				t.Fatalf("mutation not applied")
			}
			row := onlyAuditRow(t, c.kind, run.targetID, c.targetType)
			e2AssertContains(t, "before", row.Before, run.wantBefore)
			e2AssertContains(t, "after", row.After, run.wantAfter)
			assertNoneStored(t, row, run.secrets...)
		})
	}
}

func TestE2Routes_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, c := range e2Cases {
		t.Run(c.name, func(t *testing.T) {
			run := c.setup(t)
			failSquadSkillAuditForKind(t, c.kind, testWorkspaceID)
			assertAuditFailure(t, run.do())
			if run.applied(t) {
				t.Errorf("mutation applied though the audit insert failed")
			}
			if rows := auditRowsFor(t, c.kind, run.targetID); len(rows) != 0 {
				t.Errorf("%s audit rows for %s = %d; want 0", c.kind, run.targetID, len(rows))
			}
		})
	}
}
