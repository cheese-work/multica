package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CHE-5651 (CHE-4807 E7): VCS integration mutations write one
// workspace_audit_event row in the mutation's own transaction. Each route has a
// success case (one row, no token or webhook secret in it) and a rollback case
// (audit insert fails: 500 + Retry-After and the mutation is not applied).

const vcsAuditToken = "AUDIT-SECRET-ACCESS-TOKEN"

func vcsAuditProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"audit-vcs-user"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func githubAuditRequest(method, installationID string) *http.Request {
	req := newRequest(method, "/api/workspaces/"+testWorkspaceID+"/github/installations/"+installationID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", testWorkspaceID)
	rctx.URLParams.Add("installationId", installationID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func seedGitHubInstallationForAudit(t *testing.T, githubInstallationID int64) string {
	t.Helper()
	id := uuid.NewString()
	if err := auditExec(t, `INSERT INTO github_installation (id, workspace_id, installation_id, account_login, account_type)
		VALUES ($1, $2, $3, 'audit-org', 'Organization')`, id, testWorkspaceID, githubInstallationID); err != nil {
		t.Fatalf("seed github installation: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_installation WHERE id = $1`, id)
	})
	return id
}

// vcsAuditCase is one route under test. setup seeds state and returns the
// target id the audit row must carry (empty for create: read from the
// response), the call, and whether the mutation took effect.
type vcsAuditCase struct {
	name       string
	kind       string
	targetType string
	wantAfter  map[string]any // subset of after fields; nil skips the check
	wantBefore map[string]any
	setup      func(t *testing.T) (targetID string, call func() *httptest.ResponseRecorder, applied func() bool)
}

func vcsAuditCases() []vcsAuditCase {
	return []vcsAuditCase{
		{
			name: "github installation delete", kind: "github.installation.delete", targetType: "github_installation",
			wantBefore: map[string]any{"provider": "github", "installation_id": float64(9500651), "account_login": "audit-org"},
			setup: func(t *testing.T) (string, func() *httptest.ResponseRecorder, func() bool) {
				id := seedGitHubInstallationForAudit(t, 9500651)
				return id,
					func() *httptest.ResponseRecorder {
						w := httptest.NewRecorder()
						testHandler.DeleteGitHubInstallation(w, githubAuditRequest(http.MethodDelete, id))
						return w
					},
					func() bool {
						return dbfx.Count(t, `SELECT count(*) FROM github_installation WHERE id = $1`, id) == 0
					}
			},
		},
		{
			name: "vcs connection create", kind: "vcs.connection.create", targetType: "vcs_connection",
			wantAfter: map[string]any{"provider": "forgejo", "account_login": "audit-vcs-user"},
			setup: func(t *testing.T) (string, func() *httptest.ResponseRecorder, func() bool) {
				withVCSBox(t)
				t.Cleanup(func() { cleanupVCS(context.Background(), "") })
				provider := vcsAuditProvider(t)
				return "", // target id is only known after the call; resolved below
					func() *httptest.ResponseRecorder {
						w := httptest.NewRecorder()
						testHandler.ConnectVCS(w, vcsHandlerRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/vcs/connections", map[string]any{
							"provider": "forgejo", "instance_url": provider.URL, "access_token": vcsAuditToken,
						}, ""))
						return w
					},
					func() bool {
						return dbfx.Count(t, `SELECT count(*) FROM vcs_connection WHERE workspace_id = $1 AND instance_url = $2`, testWorkspaceID, provider.URL) > 0
					}
			},
		},
		{
			name: "vcs connection delete", kind: "vcs.connection.delete", targetType: "vcs_connection",
			wantBefore: map[string]any{"provider": "forgejo", "instance_url": "https://forgejo-delete.test", "account_login": "acme"},
			setup: func(t *testing.T) (string, func() *httptest.ResponseRecorder, func() bool) {
				box := withVCSBox(t)
				t.Cleanup(func() { cleanupVCS(context.Background(), "") })
				connID := seedVCSConnection(t, context.Background(), box, "forgejo", "https://forgejo-delete.test")
				return connID,
					func() *httptest.ResponseRecorder {
						w := httptest.NewRecorder()
						testHandler.DeleteVCSConnection(w, vcsHandlerRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/vcs/connections/"+connID, nil, connID))
						return w
					},
					func() bool {
						return dbfx.Count(t, `SELECT count(*) FROM vcs_connection WHERE id = $1`, connID) == 0
					}
			},
		},
		{
			name: "vcs connection rotate webhook", kind: "vcs.webhook.rotate", targetType: "vcs_connection",
			wantAfter: map[string]any{"webhook_secret": "changed"},
			setup: func(t *testing.T) (string, func() *httptest.ResponseRecorder, func() bool) {
				box := withVCSBox(t)
				t.Cleanup(func() { cleanupVCS(context.Background(), "") })
				connID := seedVCSConnection(t, context.Background(), box, "forgejo", "https://forgejo-rotate.test")
				before := vcsWebhookSecretColumn(t, connID)
				return connID,
					func() *httptest.ResponseRecorder {
						w := httptest.NewRecorder()
						testHandler.RotateVCSConnectionWebhook(w, vcsHandlerRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/vcs/connections/"+connID+"/rotate-webhook", nil, connID))
						return w
					},
					func() bool { return vcsWebhookSecretColumn(t, connID) != before }
			},
		},
	}
}

func vcsWebhookSecretColumn(t *testing.T, connID string) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(context.Background(), `SELECT webhook_secret_encrypted FROM vcs_connection WHERE id = $1`, connID).Scan(&s); err != nil {
		t.Fatalf("read webhook secret column: %v", err)
	}
	return s
}

func TestVCSRoutes_RecordAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range vcsAuditCases() {
		t.Run(tc.name, func(t *testing.T) {
			targetID, call, _ := tc.setup(t)
			w := call()
			if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if targetID == "" { // create: read the id from the response
				var resp VCSConnectResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode create response: %v", err)
				}
				targetID = resp.ID
			}
			row := onlyAuditRow(t, tc.kind, targetID, tc.targetType)
			for k, want := range tc.wantBefore {
				if row.Before[k] != want {
					t.Errorf("before[%s] = %v; want %v (row %+v)", k, row.Before[k], want, row)
				}
			}
			for k, want := range tc.wantAfter {
				if row.After[k] != want {
					t.Errorf("after[%s] = %v; want %v (row %+v)", k, row.After[k], want, row)
				}
			}
			secrets := []string{vcsAuditToken, vcsTestSecret}
			var resp VCSConnectResponse
			if json.Unmarshal(w.Body.Bytes(), &resp) == nil && resp.WebhookSecret != "" {
				secrets = append(secrets, resp.WebhookSecret)
			}
			assertNoneStored(t, row, secrets...)
			stored, _ := json.Marshal(map[string]any{"before": row.Before, "after": row.After})
			if strings.Contains(string(stored), "_encrypted") {
				t.Errorf("audit row stores an encrypted secret column: %s", stored)
			}
		})
	}
}

func TestVCSRoutes_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range vcsAuditCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, call, applied := tc.setup(t)
			failSquadSkillAuditForKind(t, tc.kind, testWorkspaceID)
			w := call()
			assertAuditFailure(t, w)
			if applied() {
				t.Errorf("%s: mutation applied although the audit insert failed", tc.kind)
			}
		})
	}
}
