package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// CHE-5649 (E5): the plugin routes write one workspace_audit_event row inside
// the transaction the PluginService opens, so the row commits or rolls back
// with the mutation, secrets included. Each case has a success run (one row,
// no secret in it) and a rollback run (audit insert forced to fail: 500 with
// Retry-After and the mutation not applied).

const (
	auditPluginSecretValue = "SECRET-TOKEN-VALUE-5649"
	auditPluginRepoValue   = "SECRET-REPO-VALUE-5649"
)

var auditPluginScopes = []string{"issues:read", "comments:write", "storage:user"}

// pluginAuditRun is one prepared route call. setup has already run; call
// performs the mutation.
type pluginAuditRun struct {
	call       func() *httptest.ResponseRecorder
	wantStatus int
	// target returns the audit row's target id from the response.
	target func(*httptest.ResponseRecorder) string
	// applied reports whether the mutation took effect in the database.
	applied func() bool
	// leaks lists values that must not appear in the stored before/after.
	leaks      func(*httptest.ResponseRecorder) []string
	wantBefore []string
	wantAfter  []string
}

type pluginAuditCase struct {
	kind       string
	targetType string
	setup      func(t *testing.T) pluginAuditRun
}

func auditPluginInstall(t *testing.T) string {
	t.Helper()
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	versionID := withLocalPluginSource(t, handlerTestManifest)
	return auditPluginInstallVersion(t, versionID)
}

func auditPluginInstallVersion(t *testing.T, versionID string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"version_id": versionID, "granted_scopes": auditPluginScopes})
	w := httptest.NewRecorder()
	testHandler.InstallPlugin(w, pluginHandlerRequest(http.MethodPost, "/plugins", body, map[string]string{"id": testWorkspaceID}))
	if w.Code != http.StatusCreated {
		t.Fatalf("install: status=%d body=%s", w.Code, w.Body.String())
	}
	return responseID(t, w)
}

func responseID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("decode id from %q: %v", w.Body.String(), err)
	}
	return out.ID
}

func auditPluginParams(installationID string) map[string]string {
	return map[string]string{"id": testWorkspaceID, "installationId": installationID}
}

func auditPluginCount(t *testing.T, query string, args ...any) int {
	t.Helper()
	return dbfx.Count(t, query, args...)
}

func auditPluginUsePublishHost(t *testing.T) {
	t.Helper()
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	previousHost, previousDir := testHandler.PluginService.Host, testHandler.PluginService.LocalDir
	testHandler.PluginService.Host = plugincontract.HostCapabilities()
	t.Cleanup(func() {
		testHandler.PluginService.Host = previousHost
		testHandler.PluginService.LocalDir = previousDir
	})
}

func pluginAuditCases() []pluginAuditCase {
	return []pluginAuditCase{
		{kind: "plugin.install", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			withPluginsV1Flag(t, testHandler, true)
			cleanupPluginInstallations(t)
			versionID := withLocalPluginSource(t, handlerTestManifest)
			body, _ := json.Marshal(map[string]any{"version_id": versionID, "granted_scopes": auditPluginScopes})
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.InstallPlugin(w, pluginHandlerRequest(http.MethodPost, "/plugins", body, map[string]string{"id": testWorkspaceID}))
					return w
				},
				wantStatus: http.StatusCreated,
				target:     func(w *httptest.ResponseRecorder) string { return responseID(t, w) },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE workspace_id = $1`, testWorkspaceID) > 0
				},
				wantAfter: []string{"version", "granted_scopes"},
			}
		}},
		{kind: "plugin.uninstall", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			id := auditPluginInstall(t)
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.UninstallPlugin(w, pluginHandlerRequest(http.MethodDelete, "/plugins/"+id, nil, auditPluginParams(id)))
					return w
				},
				wantStatus: http.StatusNoContent,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1`, id) == 0
				},
				wantBefore: []string{"enabled", "version"},
			}
		}},
		{kind: "plugin.enable", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			id := auditPluginInstall(t)
			if err := auditExec(t, `UPDATE plugin_installation SET enabled = false WHERE id = $1`, id); err != nil {
				t.Fatalf("disable fixture: %v", err)
			}
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.EnablePlugin(w, pluginHandlerRequest(http.MethodPost, "/enable", nil, auditPluginParams(id)))
					return w
				},
				wantStatus: http.StatusOK,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND enabled`, id) == 1
				},
				wantBefore: []string{"enabled"}, wantAfter: []string{"enabled"},
			}
		}},
		{kind: "plugin.disable", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			id := auditPluginInstall(t)
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.DisablePlugin(w, pluginHandlerRequest(http.MethodPost, "/disable", nil, auditPluginParams(id)))
					return w
				},
				wantStatus: http.StatusOK,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND NOT enabled`, id) == 1
				},
				wantBefore: []string{"enabled"}, wantAfter: []string{"enabled"},
			}
		}},
		{kind: "plugin.config.update", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			id := auditPluginInstall(t)
			body, _ := json.Marshal(map[string]any{"values": map[string]any{"repo": auditPluginRepoValue, "token": auditPluginSecretValue}})
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.ConfigurePlugin(w, pluginHandlerRequest(http.MethodPut, "/config", body, auditPluginParams(id)))
					return w
				},
				wantStatus: http.StatusOK,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_secret WHERE installation_id = $1`, id) > 0 ||
						auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND config::text LIKE '%repo%'`, id) > 0
				},
				leaks: func(*httptest.ResponseRecorder) []string {
					return []string{auditPluginSecretValue, auditPluginRepoValue}
				},
				wantAfter: []string{"repo", "token"},
			}
		}},
		{kind: "plugin.token.rotate", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			id := auditPluginInstall(t)
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.RotatePluginToken(w, pluginHandlerRequest(http.MethodPost, "/token", nil, auditPluginParams(id)))
					return w
				},
				wantStatus: http.StatusOK,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND token_hash IS NOT NULL`, id) == 1
				},
				leaks: func(w *httptest.ResponseRecorder) []string {
					var out struct {
						Token         string `json:"token"`
						SigningSecret string `json:"signing_secret"`
					}
					_ = json.Unmarshal(w.Body.Bytes(), &out)
					leaks := []string{out.Token}
					if out.SigningSecret != "" {
						leaks = append(leaks, out.SigningSecret)
					}
					return leaks
				},
				wantAfter: []string{"token"},
			}
		}},
		{kind: "plugin.token.revoke", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			id := auditPluginInstall(t)
			token, err := testHandler.PluginService.IssueInstallToken(context.Background(), parseUUID(id))
			if err != nil {
				t.Fatalf("issue token fixture: %v", err)
			}
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.RevokePluginToken(w, pluginHandlerRequest(http.MethodDelete, "/token", nil, auditPluginParams(id)))
					return w
				},
				wantStatus: http.StatusNoContent,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND token_hash IS NULL`, id) == 1
				},
				leaks:      func(*httptest.ResponseRecorder) []string { return []string{token} },
				wantBefore: []string{"token"},
			}
		}},
		{kind: "plugin.mcp_tools.approve", targetType: "plugin_installation", setup: func(t *testing.T) pluginAuditRun {
			cleanupPluginInstallations(t)
			servers := exampleServers{
				sentinel: quietServer(t, func(http.ResponseWriter, *http.Request) {}),
				metrics:  metricsServer(t),
			}
			id := installExamplePlugin(t, servers)
			body, _ := json.Marshal(map[string]any{"tools": []string{"query_timeseries", "list_alerts"}})
			params := auditPluginParams(id)
			params["hookKey"] = "metrics"
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.ApprovePluginMCPTools(w, pluginHandlerRequest(http.MethodPut, "/mcp/tools", body, params))
					return w
				},
				wantStatus: http.StatusOK,
				target:     func(*httptest.ResponseRecorder) string { return id },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND mcp_approvals ? 'metrics'`, id) == 1
				},
				wantAfter: []string{"hook", "tools"},
			}
		}},
		{kind: "plugin.package.publish", targetType: "plugin_package", setup: func(t *testing.T) pluginAuditRun {
			auditPluginUsePublishHost(t)
			archive := pluginBundleZip(t, packageManifest("1.0.0"), "// audit publish")
			return pluginAuditRun{
				call:       func() *httptest.ResponseRecorder { return uploadPluginBundle(t, archive) },
				wantStatus: http.StatusCreated,
				target:     func(w *httptest.ResponseRecorder) string { return responseID(t, w) },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_package WHERE workspace_id = $1`, testWorkspaceID) > 0
				},
				wantAfter: []string{"version"},
			}
		}},
		{kind: "plugin.package.publish", targetType: "plugin_package", setup: func(t *testing.T) pluginAuditRun {
			auditPluginUsePublishHost(t)
			root := t.TempDir()
			writeLocalPluginManifest(t, root, handlerTestManifest)
			testHandler.PluginService.LocalDir = root
			body, _ := json.Marshal(map[string]string{"name": "hello"})
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.PublishLocalPluginPackage(w, pluginHandlerRequest(http.MethodPost, "/plugins/packages/local", body, map[string]string{"id": testWorkspaceID}))
					return w
				},
				wantStatus: http.StatusCreated,
				target:     func(w *httptest.ResponseRecorder) string { return responseID(t, w) },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_package WHERE workspace_id = $1`, testWorkspaceID) > 0
				},
				wantAfter: []string{"version"},
			}
		}},
		{kind: "plugin.package.delete", targetType: "plugin_package", setup: func(t *testing.T) pluginAuditRun {
			auditPluginUsePublishHost(t)
			pkg := publishUploadedBundle(t, packageManifest("1.0.0"), "// audit delete")
			return pluginAuditRun{
				call: func() *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					testHandler.DeletePluginPackage(w, pluginHandlerRequest(http.MethodDelete, "/plugins/packages/"+pkg.ID, nil,
						map[string]string{"id": testWorkspaceID, "packageId": pkg.ID}))
					return w
				},
				wantStatus: http.StatusNoContent,
				target:     func(*httptest.ResponseRecorder) string { return pkg.ID },
				applied: func() bool {
					return auditPluginCount(t, `SELECT count(*) FROM plugin_package WHERE id = $1`, pkg.ID) == 0
				},
				wantBefore: []string{"plugin_key"},
			}
		}},
	}
}

func hasKeys(t *testing.T, side string, got map[string]any, want []string) {
	t.Helper()
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s = %v; want key %q", side, got, key)
		}
	}
}

func TestPluginRoutes_RecordAuditRow(t *testing.T) {
	for _, tc := range pluginAuditCases() {
		t.Run(tc.kind+"/"+t.Name(), func(t *testing.T) {
			run := tc.setup(t)
			w := run.call()
			if w.Code != run.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, run.wantStatus, w.Body.String())
			}
			row := onlyAuditRow(t, tc.kind, run.target(w), tc.targetType)
			hasKeys(t, "before", row.Before, run.wantBefore)
			hasKeys(t, "after", row.After, run.wantAfter)
			leaks := []string{}
			if run.leaks != nil {
				leaks = run.leaks(w)
			}
			assertNoneStored(t, row, leaks...)
		})
	}
}

func TestPluginRoutes_AuditInsertFailureRollsBack(t *testing.T) {
	for _, tc := range pluginAuditCases() {
		t.Run(tc.kind+"/"+t.Name(), func(t *testing.T) {
			run := tc.setup(t)
			failSquadSkillAuditForKind(t, tc.kind, testWorkspaceID)
			// An applied() that is already true before the call (a fixture left
			// state behind) would make the rollback assertion vacuous.
			wasApplied := run.applied()
			w := run.call()
			assertAuditFailure(t, w)
			if run.applied() != wasApplied {
				t.Errorf("%s was applied although its audit insert failed", tc.kind)
			}
		})
	}
}

// A config save with a secret that fails its audit insert leaves no secret
// row and the config row untouched.
func TestConfigurePlugin_AuditFailureWritesNoSecretOrConfig(t *testing.T) {
	id := auditPluginInstall(t)
	failSquadSkillAuditForKind(t, "plugin.config.update", testWorkspaceID)
	body, _ := json.Marshal(map[string]any{"values": map[string]any{"repo": auditPluginRepoValue, "token": auditPluginSecretValue}})
	w := httptest.NewRecorder()
	testHandler.ConfigurePlugin(w, pluginHandlerRequest(http.MethodPut, "/config", body, auditPluginParams(id)))
	assertAuditFailure(t, w)
	if n := auditPluginCount(t, `SELECT count(*) FROM plugin_secret WHERE installation_id = $1`, id); n != 0 {
		t.Errorf("secret rows = %d; want 0", n)
	}
	if n := auditPluginCount(t, `SELECT count(*) FROM plugin_installation WHERE id = $1 AND config::text LIKE '%repo%'`, id); n != 0 {
		t.Errorf("config row changed")
	}
}

// Preview only reads; a plain member never reaches any mutation and writes no
// row. Handlers run behind the admin gate, so this walks the 11 routes through it.
func TestPluginRoutes_MemberGets403AndWritesNoAuditRow(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	memberID := createTestUserAndMember(t, "member")
	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Use(middleware.RequireWorkspaceRoleFromURL(testHandler.Queries, "id", "owner", "admin"))
		r.Post("/plugins/packages", testHandler.PublishPluginPackage)
		r.Post("/plugins/packages/local", testHandler.PublishLocalPluginPackage)
		r.Delete("/plugins/packages/{packageId}", testHandler.DeletePluginPackage)
		r.Post("/plugins", testHandler.InstallPlugin)
		r.Post("/plugins/{installationId}/token", testHandler.RotatePluginToken)
		r.Delete("/plugins/{installationId}/token", testHandler.RevokePluginToken)
		r.Put("/plugins/{installationId}/mcp/{hookKey}/tools", testHandler.ApprovePluginMCPTools)
		r.Put("/plugins/{installationId}/config", testHandler.ConfigurePlugin)
		r.Post("/plugins/{installationId}/enable", testHandler.EnablePlugin)
		r.Post("/plugins/{installationId}/disable", testHandler.DisablePlugin)
		r.Delete("/plugins/{installationId}", testHandler.UninstallPlugin)
	})
	base := "/api/workspaces/" + testWorkspaceID
	const other = "11111111-1111-1111-1111-111111111111"
	routes := []struct{ method, path string }{
		{http.MethodPost, "/plugins/packages"},
		{http.MethodPost, "/plugins/packages/local"},
		{http.MethodDelete, "/plugins/packages/" + other},
		{http.MethodPost, "/plugins"},
		{http.MethodPost, "/plugins/" + other + "/token"},
		{http.MethodDelete, "/plugins/" + other + "/token"},
		{http.MethodPut, "/plugins/" + other + "/mcp/metrics/tools"},
		{http.MethodPut, "/plugins/" + other + "/config"},
		{http.MethodPost, "/plugins/" + other + "/enable"},
		{http.MethodPost, "/plugins/" + other + "/disable"},
		{http.MethodDelete, "/plugins/" + other},
	}
	before := auditPluginCount(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1 AND kind LIKE 'plugin.%'`, testWorkspaceID)
	for _, route := range routes {
		req := newRequest(route.method, base+route.path, map[string]any{})
		req.Header.Set("X-User-ID", memberID)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", route.method, route.path, w.Code)
		}
	}
	after := auditPluginCount(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1 AND kind LIKE 'plugin.%'`, testWorkspaceID)
	if after != before {
		t.Errorf("plugin audit rows %d -> %d; a refused request wrote a row", before, after)
	}
}
