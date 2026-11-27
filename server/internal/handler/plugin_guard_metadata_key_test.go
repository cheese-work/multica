package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

const guardMetadataTestManifest = `{
  "manifest_version": 1,
  "key": "com.example.guarded",
  "name": "Guarded",
  "version": "1.0.0",
  "author": { "name": "example" },
  "scopes": ["issues:read", "net:example.com"],
  "contributes": {
    "hooks": [{
      "key": "qa-proof",
      "name": "QA proof",
      "description": "Allow or deny the move.",
      "triggers": ["guard"],
      "guard": { "timeout_ms": 2000, "on_error": "closed", "overridable": true, "metadata_writers": ["post_close_work", "qa_note"] },
      "transport": { "type": "http", "url": "https://example.com/hooks/qa-proof" }
    }]
  }
}`

type guardKeyRow struct {
	key     string
	enabled bool
}

func guardMetadataKeys(t *testing.T, installationID string) []guardKeyRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT metadata_key, enabled FROM plugin_guard_metadata_key WHERE installation_id = $1 ORDER BY metadata_key`, installationID)
	if err != nil {
		t.Fatalf("list declared keys: %v", err)
	}
	defer rows.Close()
	var out []guardKeyRow
	for rows.Next() {
		var row guardKeyRow
		if err := rows.Scan(&row.key, &row.enabled); err != nil {
			t.Fatalf("scan declared key: %v", err)
		}
		out = append(out, row)
	}
	return out
}

func isMetadataKeyDeclared(t *testing.T, key string) bool {
	t.Helper()
	var declared bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM plugin_guard_metadata_key WHERE workspace_id = $1 AND metadata_key = $2 AND enabled)`,
		testWorkspaceID, key).Scan(&declared); err != nil {
		t.Fatalf("read declaration of %s: %v", key, err)
	}
	return declared
}

func installGuardPlugin(t *testing.T, versionID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"version_id": versionID, "granted_scopes": []string{"issues:read", "net:example.com"}})
	recorder := httptest.NewRecorder()
	testHandler.InstallPlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins", body, map[string]string{"id": testWorkspaceID}))
	return recorder
}

func TestPluginGuardMetadataKeysFollowTheInstallationLifecycle(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	root := t.TempDir()
	versionID := withLocalPluginSourceIn(t, root, guardMetadataTestManifest)
	// The host does not ship the guard trigger yet; the projection must not
	// depend on that, so the test enables it on its own copy of the host.
	testHandler.PluginService.Host.HookTriggers[plugincontract.TriggerGuard] = true

	recorder := installGuardPlugin(t, versionID)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var installed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode installation: %v", err)
	}
	want := func(label string, keys ...guardKeyRow) {
		t.Helper()
		if got := guardMetadataKeys(t, installed.ID); !slices.Equal(got, keys) {
			t.Fatalf("%s: declared keys = %+v, want %+v", label, got, keys)
		}
	}
	want("install", guardKeyRow{"post_close_work", true}, guardKeyRow{"qa_note", true})
	if !isMetadataKeyDeclared(t, "post_close_work") {
		t.Fatal("post_close_work is not declared after install")
	}

	params := map[string]string{"id": testWorkspaceID, "installationId": installed.ID}
	recorder = httptest.NewRecorder()
	testHandler.DisablePlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/disable", nil, params))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want("disable", guardKeyRow{"post_close_work", false}, guardKeyRow{"qa_note", false})
	if isMetadataKeyDeclared(t, "post_close_work") {
		t.Fatal("a disabled installation still declares post_close_work")
	}

	recorder = httptest.NewRecorder()
	testHandler.EnablePlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/enable", nil, params))
	if recorder.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want("enable", guardKeyRow{"post_close_work", true}, guardKeyRow{"qa_note", true})

	// An upgrade that drops a key and adds another reconciles both.
	upgraded := strings.Replace(guardMetadataTestManifest, `"version": "1.0.0"`, `"version": "2.0.0"`, 1)
	upgraded = strings.Replace(upgraded, `["post_close_work", "qa_note"]`, `["post_close_work", "release_note"]`, 1)
	writeLocalPluginManifest(t, root, upgraded)
	recorder = installGuardPlugin(t, publishLocalPlugin(t, "hello"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upgrade status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want("upgrade", guardKeyRow{"post_close_work", true}, guardKeyRow{"release_note", true})

	// An upgrade of a disabled installation keeps the new keys undeclared.
	recorder = httptest.NewRecorder()
	testHandler.DisablePlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/disable", nil, params))
	if recorder.Code != http.StatusOK {
		t.Fatalf("second disable status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	again := strings.Replace(upgraded, `"version": "2.0.0"`, `"version": "3.0.0"`, 1)
	again = strings.Replace(again, `["post_close_work", "release_note"]`, `["post_close_work", "audit_note"]`, 1)
	writeLocalPluginManifest(t, root, again)
	recorder = installGuardPlugin(t, publishLocalPlugin(t, "hello"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("disabled upgrade status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want("disabled upgrade", guardKeyRow{"audit_note", false}, guardKeyRow{"post_close_work", false})

	recorder = httptest.NewRecorder()
	testHandler.UninstallPlugin(recorder, pluginHandlerRequest(http.MethodDelete, "/plugins", nil, params))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("uninstall status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want("uninstall")
}
