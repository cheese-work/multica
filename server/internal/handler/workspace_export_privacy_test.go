package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func exportPrivacyRequest(method, workspaceID, userID string, body any) *http.Request {
	var req *http.Request
	if body != nil {
		req = testutil.JSONRequest(method, "/api/workspaces/"+workspaceID+"/export-privacy", body)
	} else {
		req = httptest.NewRequest(method, "/api/workspaces/"+workspaceID+"/export-privacy", nil)
	}
	req = testutil.WithHeaders(req, "X-User-ID", userID, "X-Workspace-ID", workspaceID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", workspaceID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// resetExportPrivacy restores a workspace's export privacy columns to the
// migration defaults so tests do not leak state into each other via the
// shared testWorkspaceID fixture.
func resetExportPrivacy(t *testing.T, workspaceID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE workspace SET export_redaction_mode = 'small', export_manifest_retention_days = 90 WHERE id = $1`, workspaceID)
}

func TestWorkspaceExportPrivacy_RequiresOwnerOrAdmin(t *testing.T) {
	requireProvenanceDB(t)
	suffix := time.Now().UnixNano()
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	plainUser := dbfx.User(t, "Privacy Member", fmt.Sprintf("privacy-member-%d@example.test", suffix))
	dbfx.Member(t, testWorkspaceID, plainUser, "member")

	testutil.Call(t, testHandler.GetWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodGet, testWorkspaceID, plainUser, nil)).
		Want(http.StatusForbidden)
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, plainUser, map[string]any{"manifest_retention_days": 30})).
		Want(http.StatusForbidden)

	adminUser := dbfx.User(t, "Privacy Admin", fmt.Sprintf("privacy-admin-%d@example.test", suffix))
	dbfx.Member(t, testWorkspaceID, adminUser, "admin")
	testutil.Call(t, testHandler.GetWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodGet, testWorkspaceID, adminUser, nil)).
		Want(http.StatusOK)
}

func TestWorkspaceExportPrivacy_RejectsMachineActor(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	req := exportPrivacyRequest(http.MethodGet, testWorkspaceID, testUserID, nil)
	req.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.GetWorkspaceExportPrivacy, req).Want(http.StatusForbidden)

	patchReq := exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": 30})
	patchReq.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy, patchReq).Want(http.StatusForbidden)
}

func TestWorkspaceExportPrivacy_RejectsStrictMode(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"redaction_mode": "strict"})).
		Want(http.StatusBadRequest)

	var mode string
	dbfx.QueryRow(t, `SELECT export_redaction_mode FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&mode)
	if mode != "small" {
		t.Fatalf("redaction_mode changed to %q despite rejected request", mode)
	}
}

func TestWorkspaceExportPrivacy_RejectsInvalidRetentionDays(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	for _, days := range []int{0, -1, 3651} {
		testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
			exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": days})).
			Want(http.StatusBadRequest)
	}

	var days int
	dbfx.QueryRow(t, `SELECT export_manifest_retention_days FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&days)
	if days != 90 {
		t.Fatalf("manifest_retention_days changed to %d despite rejected requests", days)
	}
}

func TestWorkspaceExportPrivacy_UpdatesRetentionDays(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	resp := testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": 30})).
		Want(http.StatusOK)
	var out workspaceExportPrivacyResponse
	resp.JSON(&out)
	if out.ManifestRetentionDays != 30 || out.RedactionMode != "small" {
		t.Fatalf("unexpected response: %+v", out)
	}

	var days int
	var mode string
	dbfx.QueryRow(t, `SELECT export_redaction_mode, export_manifest_retention_days FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&mode, &days)
	if days != 30 || mode != "small" {
		t.Fatalf("stored settings not updated: mode=%s days=%d", mode, days)
	}
}

func TestWorkspaceExportPrivacy_CrossWorkspaceIsolation(t *testing.T) {
	requireProvenanceDB(t)
	suffix := time.Now().UnixNano()

	otherWS := dbfx.Workspace(t, "Privacy Other", fmt.Sprintf("privacy-other-%d", suffix))
	dbfx.Exec(t, `UPDATE workspace SET export_manifest_retention_days = 7 WHERE id = $1`, otherWS)

	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	// testUserID is not a member of otherWS, so this must 404, not leak
	// otherWS's settings and not let testUserID change them.
	testutil.Call(t, testHandler.GetWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodGet, otherWS, testUserID, nil)).
		Want(http.StatusNotFound)
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, otherWS, testUserID, map[string]any{"manifest_retention_days": 365})).
		Want(http.StatusNotFound)

	var days int
	dbfx.QueryRow(t, `SELECT export_manifest_retention_days FROM workspace WHERE id = $1`, otherWS).Scan(&days)
	if days != 7 {
		t.Fatalf("cross-workspace request mutated otherWS retention_days to %d", days)
	}
}

// TestExportProvenance_FailsClosedOnUnimplementedMode is the direct coverage
// for CHE-766's core safety requirement: the export handler must refuse to
// run at all under a mode it does not fully enforce, rather than silently
// serving data under "small" while a workspace believes "strict" is active.
func TestExportProvenance_FailsClosedOnUnimplementedMode(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	// The API refuses to set "strict" (TestWorkspaceExportPrivacy_RejectsStrictMode),
	// so simulate the only other way the column could hold it: a stored value
	// from before this handler existed, or a manual DB edit.
	dbfx.Exec(t, `UPDATE workspace SET export_redaction_mode = 'strict' WHERE id = $1`, testWorkspaceID)

	before := provenanceLogCount(t)
	cutoff := time.Now().Add(-time.Hour)
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusConflict)

	if after := provenanceLogCount(t); after != before {
		t.Fatalf("export wrote an audit row despite fail-closed rejection: before=%d after=%d", before, after)
	}
}

// TestDeleteExpiredProvenanceExportLogsForWorkspace_ExpiryAndIsolation is the
// SQL-level counterpart to the scheduler's tests in internal/scheduler: it
// exercises the real DeleteExpiredProvenanceExportLogsForWorkspace query
// against Postgres to confirm the created_at cutoff math (read live from
// workspace.export_manifest_retention_days, not a caller-supplied value) and
// workspace_id scoping the scheduler job depends on actually hold — a row
// just inside the window survives, a row just past it is deleted, and a
// foreign workspace's row is never touched regardless of its own age.
func TestDeleteExpiredProvenanceExportLogsForWorkspace_ExpiryAndIsolation(t *testing.T) {
	requireProvenanceDB(t)
	suffix := time.Now().UnixNano()
	queries := db.New(testPool)
	ctx := context.Background()

	otherWS := dbfx.Workspace(t, "Retention Other", fmt.Sprintf("retention-other-%d", suffix))
	resetExportPrivacy(t, testWorkspaceID)
	dbfx.Exec(t, `UPDATE workspace SET export_manifest_retention_days = 90 WHERE id = $1`, testWorkspaceID)
	dbfx.Exec(t, `UPDATE workspace SET export_manifest_retention_days = 90 WHERE id = $1`, otherWS)

	insertLog := func(workspaceID string, age time.Duration) {
		dbfx.Insert(t, "provenance_export_log", testutil.Cols{
			"workspace_id":    workspaceID,
			"actor_type":      "member",
			"actor_id":        testUserID,
			"request_digest":  fmt.Sprintf("digest-%d-%d", suffix, age),
			"manifest_digest": fmt.Sprintf("manifest-%d-%d", suffix, age),
			"cutoff":          time.Now().Add(-age),
			"source_count":    1,
			"included_count":  0,
			"excluded_count":  0,
			"manifest":        `{"records":[],"exclusions":[]}`,
			"created_at":      time.Now().Add(-age),
		})
	}

	insertLog(testWorkspaceID, 100*24*time.Hour) // past a 90-day retention: must be deleted
	insertLog(testWorkspaceID, 10*24*time.Hour)  // inside a 90-day retention: must survive
	insertLog(otherWS, 200*24*time.Hour)         // far past retention, but a DIFFERENT workspace

	t.Cleanup(func() {
		dbfx.Cleanup(t, `DELETE FROM provenance_export_log WHERE workspace_id IN ($1, $2)`, testWorkspaceID, otherWS)
		resetExportPrivacy(t, testWorkspaceID)
	})

	deletedIDs, err := queries.DeleteExpiredProvenanceExportLogsForWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("delete expired logs: %v", err)
	}
	if len(deletedIDs) != 1 {
		t.Fatalf("deleted = %d, want 1 (only the 100-day-old row for testWorkspaceID)", len(deletedIDs))
	}

	remaining := dbfx.Count(t, `SELECT count(*) FROM provenance_export_log WHERE workspace_id = $1`, testWorkspaceID)
	if remaining != 1 {
		t.Fatalf("testWorkspaceID has %d rows left, want 1 (the 10-day-old row)", remaining)
	}
	otherRemaining := dbfx.Count(t, `SELECT count(*) FROM provenance_export_log WHERE workspace_id = $1`, otherWS)
	if otherRemaining != 1 {
		t.Fatalf("otherWS row was deleted by a call scoped to testWorkspaceID: remaining=%d, want 1", otherRemaining)
	}
}

// TestExportProvenance_KillSwitchDisabled covers review finding B1: turning
// off the export_privacy_controls flag must deny ExportProvenance outright,
// before authentication or any row is read, and must not write an audit row.
func TestExportProvenance_KillSwitchDisabled(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	withFeatureFlag(t, testHandler, featureflags.ExportPrivacyControls, false)

	before := provenanceLogCount(t)
	cutoff := time.Now().Add(-time.Hour)
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusServiceUnavailable)

	if after := provenanceLogCount(t); after != before {
		t.Fatalf("export wrote an audit row despite the kill switch being off: before=%d after=%d", before, after)
	}
}

// TestExportProvenance_KillSwitchErroringFlagDeniesRegardlessOfDefault
// exercises the "unavailable" half of the fail-closed contract: a provider
// that errors evaluating the flag returns Enabled=false unconditionally
// (pkg/featureflag's ReasonError contract), so export must be denied even
// though ExportPrivacyControlsEnabled's own default is true.
func TestExportProvenance_KillSwitchErroringFlagDeniesRegardlessOfDefault(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)

	errFlags := featureflag.NewService(&errorFlagProvider{key: featureflags.ExportPrivacyControls})
	origFlags := testHandler.FeatureFlags
	testHandler.FeatureFlags = errFlags
	t.Cleanup(func() { testHandler.FeatureFlags = origFlags })

	before := provenanceLogCount(t)
	cutoff := time.Now().Add(-time.Hour)
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusServiceUnavailable)

	if after := provenanceLogCount(t); after != before {
		t.Fatalf("export wrote an audit row despite an erroring flag provider: before=%d after=%d", before, after)
	}
}

// errorFlagProvider always returns a ReasonError decision with Enabled=false
// for the named key, modeling a provider that is "unavailable" (e.g. its
// backing config source failed to parse or load) — see
// pkg/featureflag/env_provider.go for the same contract in the real env
// provider.
type errorFlagProvider struct{ key string }

func (p *errorFlagProvider) Name() string { return "error-provider-test" }
func (p *errorFlagProvider) Lookup(ctx context.Context, key string) (featureflag.Decision, bool) {
	if key != p.key {
		return featureflag.Decision{}, false
	}
	return featureflag.Decision{
		Key:     key,
		Enabled: false,
		Variant: "off",
		Reason:  featureflag.ReasonError,
		Source:  "error-provider-test",
	}, true
}

// TestWorkspaceExportPrivacy_KillSwitchDisabled covers the same B1 finding
// for both config endpoints: disabling the flag hides and blocks the config
// surface along with the capability it configures.
func TestWorkspaceExportPrivacy_KillSwitchDisabled(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })
	withFeatureFlag(t, testHandler, featureflags.ExportPrivacyControls, false)

	testutil.Call(t, testHandler.GetWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodGet, testWorkspaceID, testUserID, nil)).
		Want(http.StatusServiceUnavailable)
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": 30})).
		Want(http.StatusServiceUnavailable)

	var days int
	dbfx.QueryRow(t, `SELECT export_manifest_retention_days FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&days)
	if days != 90 {
		t.Fatalf("manifest_retention_days changed to %d despite the kill switch being off", days)
	}
}

// countExportPrivacyAuditRows counts durable policy-change receipts for a
// workspace, scoped to CHE-766's own action key so unrelated activity_log
// rows from other tests/features never inflate the count.
func countExportPrivacyAuditRows(t *testing.T, workspaceID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1 AND action = $2`,
		workspaceID, exportPrivacyPolicyChangedActivity)
}

// TestWorkspaceExportPrivacy_WritesDurableAuditRecord covers review finding
// B1 directly: a successful policy update must leave a durable,
// actor-attributed activity_log row recording the old and new values, not
// only a slog line.
func TestWorkspaceExportPrivacy_WritesDurableAuditRecord(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	before := countExportPrivacyAuditRows(t, testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": 30})).
		Want(http.StatusOK)

	after := countExportPrivacyAuditRows(t, testWorkspaceID)
	if after != before+1 {
		t.Fatalf("audit rows = %d, want %d (before+1)", after, before+1)
	}

	var actorID string
	var detailsRaw []byte
	dbfx.QueryRow(t,
		`SELECT actor_id, details FROM activity_log
		 WHERE workspace_id = $1 AND action = $2
		 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID, exportPrivacyPolicyChangedActivity).Scan(&actorID, &detailsRaw)
	if actorID != testUserID {
		t.Fatalf("audit actor_id = %q, want %q", actorID, testUserID)
	}
	var details exportPrivacyAuditDetails
	if err := json.Unmarshal(detailsRaw, &details); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if details.FromManifestRetentionDays != 90 || details.ToManifestRetentionDays != 30 {
		t.Fatalf("audit details = %+v, want from=90 to=30", details)
	}
}

// TestWorkspaceExportPrivacy_NoOpUpdateWritesNoAuditRow guards against a
// resend of the current values manufacturing a misleading "policy changed"
// entry: PATCHing with the already-stored values must not add a row.
func TestWorkspaceExportPrivacy_NoOpUpdateWritesNoAuditRow(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })

	before := countExportPrivacyAuditRows(t, testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{
			"redaction_mode": "small", "manifest_retention_days": 90,
		})).
		Want(http.StatusOK)

	after := countExportPrivacyAuditRows(t, testWorkspaceID)
	if after != before {
		t.Fatalf("audit rows = %d, want %d (no-op PATCH must not write a row)", after, before)
	}
}
