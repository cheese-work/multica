package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2774: workspace_audit_event table, trigger, purge and the admin audit
// helper. DB tests act on random workspace ids (the table has no foreign
// keys); handler tests act on the shared test workspace and look rows up by
// kind + target id.

func auditExec(t *testing.T, sql string, args ...any) error {
	t.Helper()
	_, err := testPool.Exec(context.Background(), sql, args...)
	return err
}

func auditInsertRow(t *testing.T, workspaceID, kind string, ageDays int) {
	t.Helper()
	if err := auditExec(t, `
		INSERT INTO workspace_audit_event (id, workspace_id, at, actor_type, kind, target_type, target_id)
		VALUES ($1, $2, now() - make_interval(days => $3), 'member', $4, 'workspace', $2)
	`, uuid.NewString(), workspaceID, ageDays, kind); err != nil {
		t.Fatalf("insert audit row: %v", err)
	}
}

func auditCountFor(t *testing.T, workspaceID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

type auditRow struct {
	ActorType, ActorID, TaskID, ActorSource, Kind, TargetType string
	Before, After                                             map[string]any
	External                                                  []string
}

// auditRowsFor returns the rows for one kind + target, oldest first.
func auditRowsFor(t *testing.T, kind, targetID string) []auditRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT actor_type, COALESCE(actor_id::text, ''), COALESCE(task_id::text, ''), actor_source,
		       kind, target_type, "before", "after", external_requested
		FROM workspace_audit_event
		WHERE kind = $1 AND target_id = $2
		ORDER BY at, id
	`, kind, targetID)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var before, after []byte
		if err := rows.Scan(&r.ActorType, &r.ActorID, &r.TaskID, &r.ActorSource, &r.Kind, &r.TargetType, &before, &after, &r.External); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		if err := json.Unmarshal(before, &r.Before); err != nil {
			t.Fatalf("decode before: %v", err)
		}
		if err := json.Unmarshal(after, &r.After); err != nil {
			t.Fatalf("decode after: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// failAuditInsertFor installs a BEFORE INSERT trigger that raises for audit
// rows about targetID. It is a real database failure, not a mock, so the
// handler sees exactly what a full disk or lost connection would produce.
func failAuditInsertFor(t *testing.T, targetID string) {
	t.Helper()
	name := "audit_fail_" + strings.ReplaceAll(targetID, "-", "")
	if err := auditExec(t, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.target_id = '%[2]s' THEN RAISE EXCEPTION 'forced audit insert failure'; END IF;
			RETURN NEW;
		END $$`, name, targetID)); err != nil {
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

// failCommitFor installs a deferred constraint trigger on table that raises at
// COMMIT for the row with id. The mutation and its audit row are both written,
// then the transaction fails to commit: both must vanish.
func failCommitFor(t *testing.T, table, id string) {
	t.Helper()
	name := "audit_commit_fail_" + strings.ReplaceAll(id, "-", "")
	if err := auditExec(t, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%[2]s' THEN RAISE EXCEPTION 'forced commit failure'; END IF;
			RETURN NEW;
		END $$`, name, id)); err != nil {
		t.Fatalf("create commit-fail function: %v", err)
	}
	if err := auditExec(t, fmt.Sprintf(`
		CREATE CONSTRAINT TRIGGER %[1]s AFTER UPDATE ON %[3]s
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %[1]s()`, name, id, table)); err != nil {
		t.Fatalf("create commit-fail trigger: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON %[2]s`, name, table))
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %[1]s()`, name))
	})
}

// ── append-only trigger ──────────────────────────────────────────────────

func TestAuditTable_UpdateRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := uuid.NewString()
	auditInsertRow(t, ws, "workspace.update", 0)
	if err := auditExec(t, `UPDATE workspace_audit_event SET kind = 'x' WHERE workspace_id = $1`, ws); err == nil {
		t.Fatal("UPDATE on workspace_audit_event succeeded; want trigger error")
	}
	// The flag must not open UPDATE: only purge and workspace deletion set it, and only for DELETE.
	if err := auditExec(t, `
		DO $$ BEGIN
			PERFORM set_config('multica.audit_purge', 'on', true);
			UPDATE workspace_audit_event SET kind = 'x' WHERE workspace_id = '`+ws+`';
		END $$`); err == nil {
		t.Fatal("UPDATE with the purge flag on succeeded; want trigger error")
	}
}

func TestAuditTable_DeleteRejectedWithoutFlag(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := uuid.NewString()
	auditInsertRow(t, ws, "workspace.update", 0)
	if err := auditExec(t, `DELETE FROM workspace_audit_event WHERE workspace_id = $1`, ws); err == nil {
		t.Fatal("DELETE without the flag succeeded; want trigger error")
	}
	if err := auditExec(t, `
		DO $$ BEGIN
			PERFORM set_config('multica.audit_purge', 'off', true);
			DELETE FROM workspace_audit_event WHERE workspace_id = '`+ws+`';
		END $$`); err == nil {
		t.Fatal("DELETE with the flag off succeeded; want trigger error")
	}
	if err := auditExec(t, `TRUNCATE workspace_audit_event`); err == nil {
		t.Fatal("TRUNCATE succeeded; want trigger error")
	}
	if got := auditCountFor(t, ws); got != 1 {
		t.Fatalf("rows after rejected deletes = %d; want 1", got)
	}
}

// ── purge ────────────────────────────────────────────────────────────────

func TestAuditPurge_DeletesOnlyPastFloorAndWritesSystemRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := uuid.NewString()
	auditInsertRow(t, ws, "workspace.update", 800)
	auditInsertRow(t, ws, "workspace.update", 500)
	auditInsertRow(t, ws, "workspace.update", 100)

	var deleted int64
	if err := testPool.QueryRow(context.Background(),
		`SELECT audit_retention_purge($1, now() - interval '400 days')`, ws).Scan(&deleted); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("purge deleted %d rows; want 2 (the 800 d and 500 d rows)", deleted)
	}

	var kept, purgeRows int
	testPool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE kind = 'workspace.update'), count(*) FILTER (WHERE kind = 'audit.retention_purge')
		 FROM workspace_audit_event WHERE workspace_id = $1`, ws).Scan(&kept, &purgeRows)
	if kept != 1 || purgeRows != 1 {
		t.Fatalf("after purge: kept=%d purge rows=%d; want 1 and 1", kept, purgeRows)
	}

	var actorType string
	var count float64
	var after map[string]any
	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT actor_type, "after" FROM workspace_audit_event WHERE workspace_id = $1 AND kind = 'audit.retention_purge'`, ws).
		Scan(&actorType, &raw); err != nil {
		t.Fatalf("read purge row: %v", err)
	}
	json.Unmarshal(raw, &after)
	count, _ = after["deleted"].(float64)
	if actorType != "system" || count != 2 {
		t.Fatalf("purge row actor=%q deleted=%v; want system and 2", actorType, after["deleted"])
	}
}

func TestAuditPurge_RejectsCutoffInsideFloor(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := uuid.NewString()
	auditInsertRow(t, ws, "workspace.update", 800)
	var deleted int64
	err := testPool.QueryRow(context.Background(),
		`SELECT audit_retention_purge($1, now() - interval '30 days')`, ws).Scan(&deleted)
	if err == nil {
		t.Fatal("purge with a cutoff inside the 365 d floor succeeded; want error")
	}
	if got := auditCountFor(t, ws); got != 1 {
		t.Fatalf("rows after rejected purge = %d; want 1 and no system row", got)
	}
}

func TestAuditPurge_DeletesAtMostOneBatch(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := uuid.NewString()
	if err := auditExec(t, `
		INSERT INTO workspace_audit_event (id, workspace_id, at, actor_type, kind, target_type, target_id)
		SELECT gen_random_uuid(), $1::uuid, now() - interval '900 days', 'member', 'workspace.update', 'workspace', $1::uuid
		FROM generate_series(1, 10005)`, ws); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	var first, second int64
	testPool.QueryRow(context.Background(), `SELECT audit_retention_purge($1, now() - interval '400 days')`, ws).Scan(&first)
	testPool.QueryRow(context.Background(), `SELECT audit_retention_purge($1, now() - interval '400 days')`, ws).Scan(&second)
	if first != 10000 || second != 5 {
		t.Fatalf("batches = %d then %d; want 10000 then 5", first, second)
	}
}

func TestAuditWorkspaceDelete_RemovesOnlyThatWorkspace(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws, other := uuid.NewString(), uuid.NewString()
	auditInsertRow(t, ws, "workspace.update", 0)
	auditInsertRow(t, ws, "agent.update", 0)
	auditInsertRow(t, other, "workspace.update", 0)

	var deleted int64
	if err := testPool.QueryRow(context.Background(), `SELECT audit_workspace_delete($1)`, ws).Scan(&deleted); err != nil {
		t.Fatalf("audit_workspace_delete: %v", err)
	}
	if deleted != 2 || auditCountFor(t, ws) != 0 || auditCountFor(t, other) != 1 {
		t.Fatalf("deleted=%d ws rows=%d other rows=%d; want 2, 0, 1", deleted, auditCountFor(t, ws), auditCountFor(t, other))
	}
	// The flag is tx-local: a later direct DELETE on the same pooled session must still fail.
	if err := auditExec(t, `DELETE FROM workspace_audit_event WHERE workspace_id = $1`, other); err == nil {
		t.Fatal("DELETE after audit_workspace_delete succeeded; the flag leaked")
	}
}

// ── redaction ────────────────────────────────────────────────────────────

func TestRedactAuditChange(t *testing.T) {
	spec := auditKindSpec{
		TargetType: "agent",
		Allow:      []string{"model", "max_concurrent_tasks"},
		Text:       []string{"instructions"},
	}
	before := map[string]any{
		"model": "a", "max_concurrent_tasks": 2, "same": "unchanged",
		"api_key": "old-secret", "instructions": "line one\nline two",
	}
	after := map[string]any{
		"model": "b", "max_concurrent_tasks": 2, "same": "unchanged",
		"api_key": "new-secret", "instructions": "line one\nline three\nline four",
	}
	gotBefore, gotAfter := redactAuditChange(spec, before, after)

	if gotBefore["model"] != "a" || gotAfter["model"] != "b" {
		t.Errorf("allowlisted field: before=%v after=%v; want a and b", gotBefore["model"], gotAfter["model"])
	}
	for _, m := range []map[string]any{gotBefore, gotAfter} {
		if _, ok := m["max_concurrent_tasks"]; ok {
			t.Errorf("unchanged field max_concurrent_tasks must be omitted, got %v", m)
		}
		if _, ok := m["same"]; ok {
			t.Errorf("unchanged field must be omitted, got %v", m)
		}
	}
	if gotBefore["api_key"] != "changed" || gotAfter["api_key"] != "changed" {
		t.Errorf("non-allowlisted field must be the marker, got before=%v after=%v", gotBefore["api_key"], gotAfter["api_key"])
	}
	text, _ := gotAfter["instructions"].(map[string]any)
	if text["len"] != len("line one\nline three\nline four") || text["lines"] != 3 ||
		text["lines_added"] != 2 || text["lines_removed"] != 1 {
		t.Errorf("text digest = %v; want len 29, lines 3, +2 -1", text)
	}
	if text["sha256"] == "" || text["sha256"] == nil {
		t.Errorf("text digest must carry a hash, got %v", text)
	}
	encoded, _ := json.Marshal([]any{gotBefore, gotAfter})
	for _, leaked := range []string{"old-secret", "new-secret", "line three"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("redacted summary leaks %q: %s", leaked, encoded)
		}
	}
}

func TestRedactAuditChange_AllowlistedNonScalarDegradesToMarker(t *testing.T) {
	spec := auditKindSpec{Allow: []string{"model"}}
	_, after := redactAuditChange(spec, map[string]any{}, map[string]any{"model": map[string]any{"token": "s3cret"}})
	if after["model"] != "changed" {
		t.Fatalf("non-scalar value on an allowlisted key = %v; want the marker", after["model"])
	}
}

// ── handler: actor, redaction, atomicity ─────────────────────────────────

func TestUpdateWorkspace_WritesMemberAuditRowWithRedaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var prevName string
	var prevDescription *string
	dbfx.QueryRow(t, `SELECT name, description FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prevName, &prevDescription)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET name = $1, description = $2 WHERE id = $3`, prevName, prevDescription, testWorkspaceID)
	})

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"name":        "audit-renamed-" + uuid.NewString()[:8],
		"description": "secret-looking description text",
	}), "id", testWorkspaceID)
	req.Header.Set("X-Actor-Source", "web")
	before := len(auditRowsFor(t, "workspace.update", testWorkspaceID))
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateWorkspace: %d %s", w.Code, w.Body.String())
	}

	rows := auditRowsFor(t, "workspace.update", testWorkspaceID)
	if len(rows) != before+1 {
		t.Fatalf("workspace.update audit rows = %d; want %d", len(rows), before+1)
	}
	row := rows[len(rows)-1]
	if row.ActorType != "member" || row.ActorID != testUserID || row.ActorSource != "web" || row.TaskID != "" {
		t.Errorf("actor = %+v; want member %s via web with no task", row, testUserID)
	}
	if row.After["name"] == nil || row.Before["name"] != prevName {
		t.Errorf("allowlisted name not recorded: before=%v after=%v", row.Before, row.After)
	}
	if desc, _ := row.After["description"].(map[string]any); desc == nil || desc["sha256"] == nil {
		t.Errorf("description must be a digest, got %v", row.After["description"])
	}
	if encoded, _ := json.Marshal(row); strings.Contains(string(encoded), "secret-looking") {
		t.Errorf("description text leaked into the audit row: %s", encoded)
	}
}

func TestUpdateWorkspace_AgentActorRowCarriesTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che2774-ws-agent-actor", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)
	var prev *string
	dbfx.QueryRow(t, `SELECT description FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prev)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET description = $1 WHERE id = $2`, prev, testWorkspaceID)
	})

	req := asAgentActor(withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"description": "agent wrote " + uuid.NewString(),
	}), "id", testWorkspaceID), hostAgentID, hostTaskID)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateWorkspace as agent: %d %s", w.Code, w.Body.String())
	}

	rows := auditRowsFor(t, "workspace.update", testWorkspaceID)
	var found bool
	for _, row := range rows {
		if row.ActorType == "agent" && row.ActorID == hostAgentID && row.TaskID == hostTaskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no agent audit row for agent %s task %s in %+v", hostAgentID, hostTaskID, rows)
	}
}

func TestUpdateWorkspace_AuditInsertFailureRollsBackMutation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var prevName string
	dbfx.QueryRow(t, `SELECT name FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prevName)
	failAuditInsertFor(t, testWorkspaceID)

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"name": "must-not-be-applied",
	}), "id", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateWorkspace with failing audit insert: %d; want 500", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("audit failure response must carry Retry-After so the caller knows to retry")
	}
	var name string
	dbfx.QueryRow(t, `SELECT name FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&name)
	if name != prevName {
		t.Fatalf("workspace name = %q after failed audit; want unchanged %q", name, prevName)
	}
}

func TestUpdateWorkspace_RolledBackMutationLeavesNoAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var prevName string
	dbfx.QueryRow(t, `SELECT name FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prevName)
	before := len(auditRowsFor(t, "workspace.update", testWorkspaceID))
	failCommitFor(t, "workspace", testWorkspaceID)

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"name": "commit-fails",
	}), "id", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("UpdateWorkspace succeeded though commit was forced to fail: %s", w.Body.String())
	}
	if after := len(auditRowsFor(t, "workspace.update", testWorkspaceID)); after != before {
		t.Fatalf("audit rows = %d after a rolled-back mutation; want %d", after, before)
	}
	var name string
	dbfx.QueryRow(t, `SELECT name FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&name)
	if name != prevName {
		t.Fatalf("workspace name = %q; want unchanged %q", name, prevName)
	}
}

func TestUpdateAgent_WritesAuditRowWithRedaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "che2774-update-agent", nil)

	req := withURLParam(newRequest(http.MethodPut, "/api/agents/"+agentID, map[string]any{
		"model":        "audit-model-2",
		"instructions": "do the secret thing\nand another line",
	}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent: %d %s", w.Code, w.Body.String())
	}

	rows := auditRowsFor(t, "agent.update", agentID)
	if len(rows) != 1 {
		t.Fatalf("agent.update audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != "agent" {
		t.Errorf("row = %+v; want member %s on target agent", row, testUserID)
	}
	if row.After["model"] != "audit-model-2" {
		t.Errorf("model not recorded: %v", row.After)
	}
	if ins, _ := row.After["instructions"].(map[string]any); ins == nil || ins["lines"] != float64(2) {
		t.Errorf("instructions must be a 2-line digest, got %v", row.After["instructions"])
	}
	if encoded, _ := json.Marshal(row); strings.Contains(string(encoded), "secret thing") {
		t.Errorf("instructions text leaked: %s", encoded)
	}
}

func TestUpdateAgent_AgentActorRowCarriesTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, hostAgentID, hostTaskID := governedInstructionActorFixture(t, "che2774-agent-actor@multica.test", "che2774-agent-actor-host")
	targetAgentID := createHandlerTestAgent(t, "che2774-agent-actor-target", nil)
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, ownerUserID, targetAgentID)

	req := asAgentActor(withURLParam(newRequestAs(ownerUserID, http.MethodPut, "/api/agents/"+targetAgentID, map[string]any{
		"max_concurrent_tasks": 4,
	}), "id", targetAgentID), hostAgentID, hostTaskID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent as agent actor: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "agent.update", targetAgentID)
	if len(rows) != 1 || rows[0].ActorType != "agent" || rows[0].ActorID != hostAgentID || rows[0].TaskID != hostTaskID {
		t.Fatalf("rows = %+v; want one agent row for %s task %s", rows, hostAgentID, hostTaskID)
	}
}

func TestUpdateAgent_AuditInsertFailureRollsBackMutation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "che2774-update-agent-fail", nil)
	failAuditInsertFor(t, agentID)

	req := withURLParam(newRequest(http.MethodPut, "/api/agents/"+agentID, map[string]any{
		"model": "must-not-stick",
	}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateAgent with failing audit insert: %d; want 500", w.Code)
	}
	var model string
	dbfx.QueryRow(t, `SELECT COALESCE(model, '') FROM agent WHERE id = $1`, agentID).Scan(&model)
	if model == "must-not-stick" {
		t.Fatal("agent model changed though the audit insert failed")
	}
}

func TestUpdateAgent_RolledBackMutationLeavesNoAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "che2774-update-agent-commit-fail", nil)
	failCommitFor(t, "agent", agentID)

	req := withURLParam(newRequest(http.MethodPut, "/api/agents/"+agentID, map[string]any{
		"model": "rolled-back",
	}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("UpdateAgent succeeded though commit was forced to fail")
	}
	if rows := auditRowsFor(t, "agent.update", agentID); len(rows) != 0 {
		t.Fatalf("audit rows after rollback = %+v; want none", rows)
	}
}

func TestUpdateAgentEnv_AuditRowNeverCarriesValues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerUserID := agentEnvOwnerFixture(t, "che2774-env-agent", "che2774-env-owner@multica.test")

	req := withURLParam(newRequestAs(ownerUserID, http.MethodPut, "/api/agents/"+agentID+"/env", map[string]any{
		"custom_env": map[string]string{"API_KEY": "rotated-value-xyz", "NEW_TOKEN": "brand-new-token-abc"},
	}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgentEnv(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgentEnv: %d %s", w.Code, w.Body.String())
	}

	rows := auditRowsFor(t, "agent.env.update", agentID)
	if len(rows) != 1 {
		t.Fatalf("agent.env.update audit rows = %d; want 1", len(rows))
	}
	if rows[0].ActorType != "member" || rows[0].ActorID != ownerUserID {
		t.Errorf("actor = %s %s; want member %s", rows[0].ActorType, rows[0].ActorID, ownerUserID)
	}
	encoded, _ := json.Marshal(rows[0])
	for _, leaked := range []string{"secret-value", "rotated-value-xyz", "brand-new-token-abc"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("env value %q reached the audit row: %s", leaked, encoded)
		}
	}
	if rows[0].After["API_KEY"] != "changed" || rows[0].After["NEW_TOKEN"] != "changed" {
		t.Errorf("env keys must appear as the changed marker, got %v", rows[0].After)
	}
}

func TestUpdateAgentEnv_AuditInsertFailureRollsBackMutation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerUserID := agentEnvOwnerFixture(t, "che2774-env-fail-agent", "che2774-env-fail-owner@multica.test")
	failAuditInsertFor(t, agentID)

	req := withURLParam(newRequestAs(ownerUserID, http.MethodPut, "/api/agents/"+agentID+"/env", map[string]any{
		"custom_env": map[string]string{"API_KEY": "should-not-persist"},
	}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgentEnv(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateAgentEnv with failing audit insert: %d; want 500", w.Code)
	}
	var stored string
	dbfx.QueryRow(t, `SELECT custom_env::text FROM agent WHERE id = $1`, agentID).Scan(&stored)
	if strings.Contains(stored, "should-not-persist") {
		t.Fatalf("env changed though the audit insert failed: %s", stored)
	}
}

// ── outside steps and workspace deletion ─────────────────────────────────

func TestDeleteAgentRuntime_RecordsOutsideStepAndStillRunsIt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "che2774-runtime-delete")
	notifier := &recordingRuntimeGoneNotifier{}
	h := *testHandler
	h.DaemonRuntimeGone = notifier

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/runtimes/"+runtimeID, nil), "runtimeId", runtimeID)
	h.DeleteAgentRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteAgentRuntime: %d %s", w.Code, w.Body.String())
	}

	rows := auditRowsFor(t, "runtime.delete", runtimeID)
	if len(rows) != 1 {
		t.Fatalf("runtime.delete audit rows = %d; want 1", len(rows))
	}
	if len(rows[0].External) != 1 || rows[0].External[0] != "runtime_gone_notice" {
		t.Errorf("external_requested = %v; want [runtime_gone_notice]", rows[0].External)
	}
	if len(notifier.runtimeIDs) != 1 || notifier.runtimeIDs[0] != runtimeID {
		t.Errorf("runtime-gone notifications = %v; the outside step must still run inline", notifier.runtimeIDs)
	}
}

func TestDeleteAgentRuntime_AuditInsertFailureKeepsRuntimeAndSkipsOutsideStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "che2774-runtime-delete-fail")
	failAuditInsertFor(t, runtimeID)
	notifier := &recordingRuntimeGoneNotifier{}
	h := *testHandler
	h.DaemonRuntimeGone = notifier

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/runtimes/"+runtimeID, nil), "runtimeId", runtimeID)
	h.DeleteAgentRuntime(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("DeleteAgentRuntime with failing audit insert: %d; want 500", w.Code)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_runtime WHERE id = $1`, runtimeID); got != 1 {
		t.Fatalf("runtime rows = %d after failed audit; the delete must roll back", got)
	}
	if len(notifier.runtimeIDs) != 0 {
		t.Errorf("outside step ran though nothing committed: %v", notifier.runtimeIDs)
	}
}

func TestDeleteWorkspace_RemovesItsAuditRows(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wsID := dbfx.Workspace(t, "che2774-audit-delete", "che2774-audit-delete-"+uuid.NewString()[:8], testutil.Cols{"issue_prefix": "CAD"})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, wsID, testUserID)
	auditInsertRow(t, wsID, "workspace.update", 0)
	auditInsertRow(t, wsID, "agent.update", 800)
	other := uuid.NewString()
	auditInsertRow(t, other, "workspace.update", 0)

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+wsID, nil), "id", wsID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace: %d %s", w.Code, w.Body.String())
	}
	if got := auditCountFor(t, wsID); got != 0 {
		t.Fatalf("audit rows left for the deleted workspace = %d; want 0", got)
	}
	if got := auditCountFor(t, other); got != 1 {
		t.Fatalf("another workspace's audit rows = %d; want 1", got)
	}
}
