package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// CHE-5655 (E4 of CHE-4807): the last workspace-settings routes record an audit
// row in the mutation's transaction. One table drives every wired route
// through a success test and a rollback test.

// Built at run time so the file holds no secret-shaped literal for gitleaks.
var (
	wssQoderToken  = strings.Join([]string{"wss", "qoder", "pat", "0123456789"}, "-")
	wssGitHubToken = strings.Join([]string{"wss", "gh", "token", "0123456789"}, "-")
)

// wssRun is one prepared request. applied reports whether the mutation took
// effect, so the rollback test can prove nothing was applied. target resolves
// the audit target id after the call when it is not known beforehand.
type wssRun struct {
	workspaceID string // workspace the audit row lands in; "" when unknown
	targetID    string
	target      func() string
	secrets     []string
	failAudit   func(t *testing.T) // overrides the kind+workspace trigger
	call        func() *httptest.ResponseRecorder
	applied     func() bool
	check       func(t *testing.T, row auditRow)
}

type wssCase struct {
	name       string
	kind       string
	targetType string
	wantStatus int
	setup      func(t *testing.T) wssRun
}

func wssRecord(handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func wssCount(t *testing.T, query string, args ...any) func() bool {
	return func() bool {
		var n int
		dbfx.QueryRow(t, query, args...).Scan(&n)
		return n > 0
	}
}

func wssQoderCleanup(t *testing.T) {
	t.Helper()
	withVCSBox(t)
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1`, testWorkspaceID)
	})
}

func wssQoderInput(name string) map[string]any {
	return map[string]any{
		"name": name, "base_url": qoderAPIURL, "environment_id": "env_test", "qoder_token": wssQoderToken,
		"github_tokens": map[string]string{"https://github.com/acme/repo": wssGitHubToken},
	}
}

// wssFailCreateAudit fails the audit insert of workspace.create for one slug:
// the workspace id does not exist before the call.
func wssFailCreateAudit(slug string) func(t *testing.T) {
	return func(t *testing.T) {
		t.Helper()
		name := "audit_fail_slug_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if err := auditExec(t, fmt.Sprintf(`
			CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.kind = 'workspace.create' AND NEW."after"->>'slug' = '%[2]s' THEN RAISE EXCEPTION 'forced audit insert failure'; END IF;
				RETURN NEW;
			END $$`, name, slug)); err != nil {
			t.Fatalf("create failing function: %v", err)
		}
		if err := auditExec(t, fmt.Sprintf(
			`CREATE TRIGGER %[1]s BEFORE INSERT ON workspace_audit_event FOR EACH ROW EXECUTE FUNCTION %[1]s()`, name)); err != nil {
			t.Fatalf("create failing trigger: %v", err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON workspace_audit_event`, name))
			testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %[1]s()`, name))
		})
	}
}

func wssCases() []wssCase {
	return []wssCase{
		{
			name: "protected-path-policy-set", kind: "workspace.protected_path_policy.update", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) wssRun {
				name := "w" + dbfx.Issue(t, "wss policy")[:8]
				dbfx.Cleanup(t, `DELETE FROM protected_path_policy WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name)
				req := withURLParams(newRequest("PUT", "/api/workspaces/"+testWorkspaceID+"/protected-path-policies/"+name,
					map[string]any{"patterns": []string{".github/**", "**/AGENTS.md"}}), "id", testWorkspaceID, "name", name)
				return wssRun{
					workspaceID: testWorkspaceID, targetID: testWorkspaceID,
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.SetProtectedPathPolicy, req) },
					applied: wssCount(t, `SELECT count(*) FROM protected_path_policy WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name),
					check: func(t *testing.T, row auditRow) {
						if row.After["name"] != name || row.After["patterns"] != ".github/**,**/AGENTS.md" {
							t.Errorf("after = %v; want policy %s with its patterns", row.After, name)
						}
					},
				}
			},
		},
		{
			name: "share-link-revoke", kind: "share_link.revoke", targetType: "share_link", wantStatus: http.StatusNoContent,
			setup: func(t *testing.T) wssRun {
				clearShareLinksForTestWorkspace(t)
				link := createTestShareLink(t, testWorkspaceID, "member", 5, 24)
				linkID := uuidToString(link.ID)
				req := withURLParams(newRequest("DELETE", "/api/workspaces/"+testWorkspaceID+"/share-links/"+linkID, nil), "id", testWorkspaceID, "linkId", linkID)
				stillActive := wssCount(t, `SELECT count(*) FROM workspace_share_link WHERE id = $1 AND is_active`, linkID)
				return wssRun{
					workspaceID: testWorkspaceID, targetID: linkID, secrets: []string{link.Code},
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.RevokeShareLink, req) },
					applied: func() bool { return !stillActive() },
					check: func(t *testing.T, row auditRow) {
						if row.Before["is_active"] != true || row.After["is_active"] != false {
							t.Errorf("diff = %v -> %v; want is_active true -> false", row.Before, row.After)
						}
					},
				}
			},
		},
		{
			name: "workspace-create", kind: "workspace.create", targetType: "workspace", wantStatus: http.StatusCreated,
			setup: func(t *testing.T) wssRun {
				slug := "wss-create-" + uuid.NewString()[:8]
				dbfx.Cleanup(t, `DELETE FROM workspace WHERE slug = $1`, slug)
				req := newRequest("POST", "/api/workspaces", map[string]any{"name": "WSS Create", "slug": slug, "description": "audit probe"})
				created := wssCount(t, `SELECT count(*) FROM workspace WHERE slug = $1`, slug)
				return wssRun{
					failAudit: wssFailCreateAudit(slug),
					target: func() string {
						var id string
						dbfx.QueryRow(t, `SELECT id FROM workspace WHERE slug = $1`, slug).Scan(&id)
						return id
					},
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.CreateWorkspace, req) },
					applied: created,
					check: func(t *testing.T, row auditRow) {
						if row.After["slug"] != slug || row.After["name"] != "WSS Create" {
							t.Errorf("after = %v; want the new workspace name and slug %s", row.After, slug)
						}
					},
				}
			},
		},
		{
			name: "project-workflow-split-set", kind: "project.workflow_split.update", targetType: "project", wantStatus: http.StatusOK,
			setup: func(t *testing.T) wssRun {
				f := newSplitFixture(t)
				req := withURLParam(workflowRequest("PUT", "/api/projects/"+f.projectID+"/workflow-split", f.body(nil)), "id", f.projectID)
				req.Header.Set("X-User-ID", testUserID)
				return wssRun{
					workspaceID: testWorkspaceID, targetID: f.projectID,
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.SetProjectWorkflowSplit, req) },
					applied: wssCount(t, `SELECT count(*) FROM project WHERE id = $1 AND workflow_split IS NOT NULL`, f.projectID),
					check: func(t *testing.T, row auditRow) {
						if row.After["arm_b_workflow_id"] != f.armB {
							t.Errorf("after = %v; want arm B %s", row.After, f.armB)
						}
					},
				}
			},
		},
		{
			name: "project-workflow-split-clear", kind: "project.workflow_split.clear", targetType: "project", wantStatus: http.StatusOK,
			setup: func(t *testing.T) wssRun {
				f := newSplitFixture(t)
				f.put(t, testUserID, f.body(nil)).Want(http.StatusOK)
				req := withURLParam(workflowRequest("DELETE", "/api/projects/"+f.projectID+"/workflow-split", nil), "id", f.projectID)
				req.Header.Set("X-User-ID", testUserID)
				stillSplit := wssCount(t, `SELECT count(*) FROM project WHERE id = $1 AND workflow_split IS NOT NULL`, f.projectID)
				return wssRun{
					workspaceID: testWorkspaceID, targetID: f.projectID,
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.ClearProjectWorkflowSplit, req) },
					applied: func() bool { return !stillSplit() },
					check: func(t *testing.T, row auditRow) {
						if row.Before["arm_b_workflow_id"] != f.armB {
							t.Errorf("before = %v; want arm B %s", row.Before, f.armB)
						}
					},
				}
			},
		},
		{
			name: "qoder-save", kind: "qoder.save", targetType: "qoder_connection", wantStatus: http.StatusOK,
			setup: func(t *testing.T) wssRun {
				wssQoderCleanup(t)
				req := vcsHandlerRequest("PUT", "/qoder", wssQoderInput("QCA save"), "")
				return wssRun{
					workspaceID: testWorkspaceID, targetID: testWorkspaceID, secrets: []string{wssQoderToken, wssGitHubToken},
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.SaveQoderConnection, req) },
					applied: wssCount(t, `SELECT count(*) FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id = $1`, testWorkspaceID),
					check: func(t *testing.T, row auditRow) {
						if row.After["name"] != "QCA save" || row.After["environment_id"] != "env_test" {
							t.Errorf("after = %v; want the saved name and environment", row.After)
						}
					},
				}
			},
		},
		{
			name: "qoder-resume", kind: "qoder.resume", targetType: "qoder_connection", wantStatus: http.StatusOK,
			setup: func(t *testing.T) wssRun {
				wssQoderCleanup(t)
				testutilCallOK(t, testHandler.SaveQoderConnection, vcsHandlerRequest("PUT", "/qoder", wssQoderInput("QCA resume"), ""))
				// A new connection starts enabled: stop it so resume has something to change.
				dbfx.Exec(t, `UPDATE cloud_agent_connection SET enabled = false, status = 'stopped' WHERE provider='qoder_cloud' AND workspace_id = $1`, testWorkspaceID)
				req := vcsHandlerRequest("POST", "/qoder/resume", wssQoderInput("QCA resume"), "")
				return wssRun{
					workspaceID: testWorkspaceID, targetID: testWorkspaceID, secrets: []string{wssQoderToken, wssGitHubToken},
					call:    func() *httptest.ResponseRecorder { return wssRecord(testHandler.ResumeQoderConnection, req) },
					applied: wssCount(t, `SELECT count(*) FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id = $1 AND enabled`, testWorkspaceID),
					check: func(t *testing.T, row auditRow) {
						if row.Before["enabled"] != false || row.After["enabled"] != true {
							t.Errorf("diff = %v -> %v; want enabled false -> true", row.Before, row.After)
						}
					},
				}
			},
		},
	}
}

func testutilCallOK(t *testing.T, handler http.HandlerFunc, req *http.Request) {
	t.Helper()
	if w := wssRecord(handler, req); w.Code != http.StatusOK {
		t.Fatalf("setup call status = %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceSettingsRoutes_WriteOneAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range wssCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.setup(t)
			// Rows from earlier runs may already sit on a shared target.
			base := 0
			if run.targetID != "" {
				base = len(auditRowsFor(t, tc.kind, run.targetID))
			}
			w := run.call()
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			targetID := run.targetID
			if run.target != nil {
				targetID = run.target()
			}
			rows := auditRowsFor(t, tc.kind, targetID)
			if len(rows)-base != 1 {
				t.Fatalf("%s audit rows for %s = %d; want 1", tc.kind, targetID, len(rows)-base)
			}
			row := rows[base]
			if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != tc.targetType {
				t.Errorf("row = %+v; want %s by member %s on %s", row, tc.kind, testUserID, tc.targetType)
			}
			apjAssertNoLeak(t, row, run.secrets...)
			if run.check != nil {
				run.check(t, row)
			}
			if !run.applied() {
				t.Errorf("%s: audit row written but the mutation is not applied", tc.name)
			}
		})
	}
}

func TestWorkspaceSettingsRoutes_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range wssCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.setup(t)
			if run.failAudit != nil {
				run.failAudit(t)
			} else {
				failAuditInsertForKind(t, tc.kind, run.workspaceID)
			}
			base := 0
			if run.targetID != "" {
				base = len(auditRowsFor(t, tc.kind, run.targetID))
			}
			w := run.call()
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 with a failing audit insert: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("500 from a failed audit insert must carry Retry-After")
			}
			if run.applied() {
				t.Errorf("%s: mutation applied despite the failed audit insert", tc.name)
			}
			if run.targetID != "" {
				if n := len(auditRowsFor(t, tc.kind, run.targetID)) - base; n != 0 {
					t.Errorf("%s new audit rows = %d after a failed insert; want 0", tc.kind, n)
				}
			}
		})
	}
}

// The Qoder check and environment-list routes call the Qoder API with a PAT and
// change no workspace state, so they are exempt from the audit trail (see
// auditExemptRoutes).
func TestQoderReadOnlyRoutes_WriteNoAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wssQoderCleanup(t)
	var before, after int
	dbfx.QueryRow(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1`, testWorkspaceID).Scan(&before)
	// An unreachable base URL is refused before any outbound call; the point is
	// that no handler path writes an audit row.
	wssRecord(testHandler.CheckQoderConnection, vcsHandlerRequest("POST", "/qoder/check", wssQoderInput("QCA check"), ""))
	wssRecord(testHandler.ListQoderEnvironments, vcsHandlerRequest("POST", "/qoder/environments", wssQoderInput("QCA envs"), ""))
	dbfx.QueryRow(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1`, testWorkspaceID).Scan(&after)
	if after != before {
		t.Fatalf("read-only Qoder routes wrote %d audit rows; want 0", after-before)
	}
}
