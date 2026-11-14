package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2774 L5a: squads, skills, agent skill bindings, runtime-skill toggles and
// agent MCP bindings write one workspace_audit_event row in the mutation's own
// transaction. Each group has a success test (one row, no secret or long text
// in it) and an audit-failure test (500 and the mutation is not applied).

// failSquadSkillAuditForKind makes the audit insert fail for one kind in one
// workspace. Creates use it because the target id does not exist yet.
func failSquadSkillAuditForKind(t *testing.T, kind, workspaceID string) {
	t.Helper()
	name := fmt.Sprintf("audit_fail_kind_%d", time.Now().UnixNano())
	if err := auditExec(t, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.kind = '%[2]s' AND NEW.workspace_id = '%[3]s' THEN RAISE EXCEPTION 'forced audit insert failure'; END IF;
			RETURN NEW;
		END $$`, name, kind, workspaceID)); err != nil {
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

// onlyAuditRow fails unless exactly one row exists for kind + target.
func onlyAuditRow(t *testing.T, kind, targetID, targetType string) auditRow {
	t.Helper()
	rows := auditRowsFor(t, kind, targetID)
	if len(rows) != 1 {
		t.Fatalf("%s audit rows for %s = %d; want 1", kind, targetID, len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != targetType {
		t.Fatalf("row = %+v; want member %s on %s", row, testUserID, targetType)
	}
	return row
}

// assertNoneStored fails when any secret appears in the stored before/after.
func assertNoneStored(t *testing.T, row auditRow, secrets ...string) {
	t.Helper()
	stored, _ := json.Marshal(map[string]any{"before": row.Before, "after": row.After})
	for _, s := range secrets {
		if strings.Contains(string(stored), s) {
			t.Errorf("audit row stores %q: %s", s, stored)
		}
	}
}

func assertAuditFailure(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After on audit failure")
	}
}

// ── squads ──────────────────────────────────────────────────────────────────

func squadAuditReq(method, path string, body any, squadID string) *http.Request {
	return squadScopeReq("", method, path, body, map[string]string{"id": squadID})
}

func newAuditSquad(t *testing.T, name string) (squadID, leaderID string) {
	t.Helper()
	leaderID = createHandlerTestAgent(t, name+"-leader", nil)
	return createSquadInWorkspace(t, testUserID, testWorkspaceID, name, leaderID), leaderID
}

func TestCreateSquad_RecordsAuditRow(t *testing.T) {
	leaderID := createHandlerTestAgent(t, "audit-squad-create-leader", nil)
	name := "audit-squad-create-" + t.Name()
	w := httptest.NewRecorder()
	testHandler.CreateSquad(w, squadScopeReq("", http.MethodPost, "/api/squads", map[string]any{
		"name": name, "leader_id": leaderID, "description": "SECRET-SQUAD-DESCRIPTION",
	}, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp SquadResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad_member WHERE squad_id = $1`, resp.ID)
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, resp.ID)
	})

	row := onlyAuditRow(t, "squad.create", resp.ID, "squad")
	if row.After["name"] != name || row.After["leader_id"] != leaderID {
		t.Errorf("after = %v; want name and leader_id", row.After)
	}
	assertNoneStored(t, row, "SECRET-SQUAD-DESCRIPTION")
}

func TestCreateSquad_AuditInsertFailureCreatesNothing(t *testing.T) {
	leaderID := createHandlerTestAgent(t, "audit-squad-create-fail-leader", nil)
	name := "audit-squad-create-fail-" + t.Name()
	failSquadSkillAuditForKind(t, "squad.create", testWorkspaceID)

	w := httptest.NewRecorder()
	testHandler.CreateSquad(w, squadScopeReq("", http.MethodPost, "/api/squads", map[string]any{
		"name": name, "leader_id": leaderID,
	}, nil))
	assertAuditFailure(t, w)
	if n := dbfx.Count(t, `SELECT count(*) FROM squad WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name); n != 0 {
		t.Errorf("squad rows named %q = %d; want 0", name, n)
	}
}

func TestUpdateSquad_RecordsAuditRow(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-squad-update-"+t.Name())
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, squadAuditReq(http.MethodPut, "/api/squads/"+squadID, map[string]any{
		"name": "renamed-" + t.Name(), "description": "SECRET-SQUAD-DESCRIPTION",
	}, squadID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "squad.update", squadID, "squad")
	if row.After["name"] != "renamed-"+t.Name() {
		t.Errorf("after = %v; want the new name", row.After)
	}
	if _, ok := row.After["description"]; !ok {
		t.Errorf("after = %v; want a description digest", row.After)
	}
	assertNoneStored(t, row, "SECRET-SQUAD-DESCRIPTION")
}

func TestUpdateSquad_AuditInsertFailureKeepsSquad(t *testing.T) {
	name := "audit-squad-update-fail-" + t.Name()
	squadID, _ := newAuditSquad(t, name)
	failAuditInsertFor(t, squadID)

	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, squadAuditReq(http.MethodPut, "/api/squads/"+squadID, map[string]any{"name": "renamed"}, squadID))
	assertAuditFailure(t, w)

	var got string
	dbfx.QueryRow(t, `SELECT name FROM squad WHERE id = $1`, squadID).Scan(&got)
	if got != name {
		t.Errorf("name = %q; want %q (rolled back)", got, name)
	}
}

func TestDeleteSquad_RecordsAuditRow(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-squad-delete-"+t.Name())
	w := httptest.NewRecorder()
	testHandler.DeleteSquad(w, squadAuditReq(http.MethodDelete, "/api/squads/"+squadID, nil, squadID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "squad.delete", squadID, "squad")
	if row.Before["name"] != "audit-squad-delete-"+t.Name() || len(row.After) != 0 {
		t.Errorf("before = %v after = %v; want the snapshot and nothing after", row.Before, row.After)
	}
}

func TestDeleteSquad_AuditInsertFailureKeepsSquad(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-squad-delete-fail-"+t.Name())
	failAuditInsertFor(t, squadID)

	w := httptest.NewRecorder()
	testHandler.DeleteSquad(w, squadAuditReq(http.MethodDelete, "/api/squads/"+squadID, nil, squadID))
	assertAuditFailure(t, w)
	if n := dbfx.Count(t, `SELECT count(*) FROM squad WHERE id = $1 AND archived_at IS NULL`, squadID); n != 1 {
		t.Errorf("active squad rows = %d; want 1 (rolled back)", n)
	}
}

func TestAddSquadMember_RecordsAuditRow(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-member-add-"+t.Name())
	agentID := createHandlerTestAgent(t, "audit-member-add-agent", nil)
	w := httptest.NewRecorder()
	testHandler.AddSquadMember(w, squadAuditReq(http.MethodPost, "/api/squads/"+squadID+"/members", map[string]any{
		"member_type": "agent", "member_id": agentID, "role": "reviewer",
	}, squadID))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "squad.member.add", squadID, "squad")
	if row.After["member"] != "agent:"+agentID+":reviewer" {
		t.Errorf("after = %v; want the added member", row.After)
	}
}

func TestAddSquadMember_AuditInsertFailureAddsNothing(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-member-add-fail-"+t.Name())
	agentID := createHandlerTestAgent(t, "audit-member-add-fail-agent", nil)
	failAuditInsertFor(t, squadID)

	w := httptest.NewRecorder()
	testHandler.AddSquadMember(w, squadAuditReq(http.MethodPost, "/api/squads/"+squadID+"/members", map[string]any{
		"member_type": "agent", "member_id": agentID, "role": "reviewer",
	}, squadID))
	assertAuditFailure(t, w)
	if n := dbfx.Count(t, `SELECT count(*) FROM squad_member WHERE squad_id = $1 AND member_id = $2`, squadID, agentID); n != 0 {
		t.Errorf("squad_member rows = %d; want 0 (rolled back)", n)
	}
}

func TestRemoveSquadMember_RecordsAuditRow(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-member-remove-"+t.Name())
	agentID := createHandlerTestAgent(t, "audit-member-remove-agent", nil)
	dbfx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "reviewer"})

	w := httptest.NewRecorder()
	testHandler.RemoveSquadMember(w, squadAuditReq(http.MethodDelete, "/api/squads/"+squadID+"/members", map[string]any{
		"member_type": "agent", "member_id": agentID,
	}, squadID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "squad.member.remove", squadID, "squad")
	if row.Before["member"] != "agent:"+agentID+":reviewer" {
		t.Errorf("before = %v; want the removed member", row.Before)
	}
}

func TestRemoveSquadMember_AuditInsertFailureKeepsMember(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-member-remove-fail-"+t.Name())
	agentID := createHandlerTestAgent(t, "audit-member-remove-fail-agent", nil)
	dbfx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "reviewer"})
	failAuditInsertFor(t, squadID)

	w := httptest.NewRecorder()
	testHandler.RemoveSquadMember(w, squadAuditReq(http.MethodDelete, "/api/squads/"+squadID+"/members", map[string]any{
		"member_type": "agent", "member_id": agentID,
	}, squadID))
	assertAuditFailure(t, w)
	if n := dbfx.Count(t, `SELECT count(*) FROM squad_member WHERE squad_id = $1 AND member_id = $2`, squadID, agentID); n != 1 {
		t.Errorf("squad_member rows = %d; want 1 (rolled back)", n)
	}
}

func TestUpdateSquadMemberRole_RecordsAuditRow(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-member-role-"+t.Name())
	agentID := createHandlerTestAgent(t, "audit-member-role-agent", nil)
	dbfx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "reviewer"})

	w := httptest.NewRecorder()
	testHandler.UpdateSquadMemberRole(w, squadAuditReq(http.MethodPatch, "/api/squads/"+squadID+"/members/role", map[string]any{
		"member_type": "agent", "member_id": agentID, "role": "tester",
	}, squadID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "squad.member.role.update", squadID, "squad")
	if row.Before["member"] != "agent:"+agentID+":reviewer" || row.After["member"] != "agent:"+agentID+":tester" {
		t.Errorf("before = %v after = %v; want the role change", row.Before, row.After)
	}
}

func TestUpdateSquadMemberRole_AuditInsertFailureKeepsRole(t *testing.T) {
	squadID, _ := newAuditSquad(t, "audit-member-role-fail-"+t.Name())
	agentID := createHandlerTestAgent(t, "audit-member-role-fail-agent", nil)
	dbfx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "reviewer"})
	failAuditInsertFor(t, squadID)

	w := httptest.NewRecorder()
	testHandler.UpdateSquadMemberRole(w, squadAuditReq(http.MethodPatch, "/api/squads/"+squadID+"/members/role", map[string]any{
		"member_type": "agent", "member_id": agentID, "role": "tester",
	}, squadID))
	assertAuditFailure(t, w)

	var role string
	dbfx.QueryRow(t, `SELECT role FROM squad_member WHERE squad_id = $1 AND member_id = $2`, squadID, agentID).Scan(&role)
	if role != "reviewer" {
		t.Errorf("role = %q; want reviewer (rolled back)", role)
	}
}

// ── skills ──────────────────────────────────────────────────────────────────

func skillAuditReq(method, path string, body any, id string) *http.Request {
	return withURLParam(newRequest(method, path, body), "id", id)
}

const (
	skillSecretContent = "SECRET-SKILL-CONTENT"
	skillSecretFile    = "SECRET-SKILL-FILE"
	skillSecretConfig  = "user:SECRET-CONFIG-PASS@example.test"
	skillSecretDesc    = "SECRET-SKILL-DESCRIPTION"
)

var skillSecrets = []string{skillSecretContent, skillSecretFile, skillSecretConfig, skillSecretDesc}

func TestCreateSkill_RecordsAuditRow(t *testing.T) {
	name := "audit-skill-create-" + t.Name()
	w := httptest.NewRecorder()
	testHandler.CreateSkill(w, newRequest(http.MethodPost, "/api/skills", map[string]any{
		"name": name, "description": skillSecretDesc, "content": skillSecretContent,
		"config": map[string]any{"origin": "https://" + skillSecretConfig},
		"files":  []map[string]string{{"path": "notes.md", "content": skillSecretFile}},
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp SkillWithFilesResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, resp.ID) })

	row := onlyAuditRow(t, "skill.create", resp.ID, "skill")
	if row.After["name"] != name || row.After["file_count"] != float64(1) {
		t.Errorf("after = %v; want name and file_count 1", row.After)
	}
	assertNoneStored(t, row, skillSecrets...)
}

func TestCreateSkill_AuditInsertFailureCreatesNothing(t *testing.T) {
	name := "audit-skill-create-fail-" + t.Name()
	failSquadSkillAuditForKind(t, "skill.create", testWorkspaceID)

	w := httptest.NewRecorder()
	testHandler.CreateSkill(w, newRequest(http.MethodPost, "/api/skills", map[string]any{
		"name": name, "content": "body",
		"files": []map[string]string{{"path": "notes.md", "content": "x"}},
	}))
	assertAuditFailure(t, w)
	if n := countSkillsByName(t, name); n != 0 {
		t.Errorf("skills named %q = %d; want 0", name, n)
	}
}

func TestUpdateSkill_RecordsAuditRow(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-skill-update", "old body")
	newName := "audit-skill-renamed-" + t.Name()
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, id) })

	w := httptest.NewRecorder()
	testHandler.UpdateSkill(w, skillAuditReq(http.MethodPut, "/api/skills/"+id, map[string]any{
		"name": newName, "description": skillSecretDesc, "content": skillSecretContent,
		"config": map[string]any{"origin": "https://" + skillSecretConfig},
		"files":  []map[string]string{{"path": "notes.md", "content": skillSecretFile}},
	}, id))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "skill.update", id, "skill")
	if row.After["name"] != newName || row.After["file_count"] != float64(1) {
		t.Errorf("after = %v; want the new name and file_count 1", row.After)
	}
	assertNoneStored(t, row, skillSecrets...)
}

func TestUpdateSkill_AuditInsertFailureKeepsSkill(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-skill-update-fail", "old body")
	failAuditInsertFor(t, id)

	w := httptest.NewRecorder()
	testHandler.UpdateSkill(w, skillAuditReq(http.MethodPut, "/api/skills/"+id, map[string]any{
		"content": "new body", "files": []map[string]string{{"path": "notes.md", "content": "x"}},
	}, id))
	assertAuditFailure(t, w)

	var content string
	dbfx.QueryRow(t, `SELECT content FROM skill WHERE id = $1`, id).Scan(&content)
	if content != "old body" {
		t.Errorf("content = %q; want the old body (rolled back)", content)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM skill_file WHERE skill_id = $1`, id); n != 0 {
		t.Errorf("skill_file rows = %d; want 0 (rolled back)", n)
	}
}

func TestDeleteSkill_RecordsAuditRow(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-skill-delete", SecretSkillBody())
	w := httptest.NewRecorder()
	testHandler.DeleteSkill(w, skillAuditReq(http.MethodDelete, "/api/skills/"+id, nil, id))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "skill.delete", id, "skill")
	if row.Before["name"] != "audit-skill-delete-"+t.Name() || len(row.After) != 0 {
		t.Errorf("before = %v after = %v; want the snapshot and nothing after", row.Before, row.After)
	}
	assertNoneStored(t, row, skillSecretContent)
}

// SecretSkillBody is the content a delete snapshot must never store.
func SecretSkillBody() string { return skillSecretContent }

func TestDeleteSkill_AuditInsertFailureKeepsSkill(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-skill-delete-fail", "body")
	failAuditInsertFor(t, id)

	w := httptest.NewRecorder()
	testHandler.DeleteSkill(w, skillAuditReq(http.MethodDelete, "/api/skills/"+id, nil, id))
	assertAuditFailure(t, w)
	if n := dbfx.Count(t, `SELECT count(*) FROM skill WHERE id = $1`, id); n != 1 {
		t.Errorf("skill rows = %d; want 1 (rolled back)", n)
	}
}

// ── skill import and refresh ────────────────────────────────────────────────

func archiveImport(t *testing.T, name, onConflict string) *httptest.ResponseRecorder {
	t.Helper()
	archive := buildTestZip(t, map[string]string{
		name + "/SKILL.md": skillMdWithName(name, skillSecretDesc),
		name + "/notes.md": skillSecretFile,
	})
	w := httptest.NewRecorder()
	testHandler.ImportSkill(w, newSkillArchiveImportRequest(testUserID, archive, name+".skill", onConflict))
	return w
}

func TestImportSkill_ArchiveCreateRecordsAuditRow(t *testing.T) {
	name := "audit-import-create-" + t.Name()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM skill WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name)
	})
	w := archiveImport(t, name, "fail")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body SkillImportResult
	json.Unmarshal(w.Body.Bytes(), &body)

	row := onlyAuditRow(t, "skill.create", body.Skill.ID, "skill")
	if row.After["name"] != name {
		t.Errorf("after = %v; want name %q", row.After, name)
	}
	assertNoneStored(t, row, skillSecretDesc, skillSecretFile)
}

func TestImportSkill_ArchiveCreateAuditInsertFailureCreatesNothing(t *testing.T) {
	name := "audit-import-create-fail-" + t.Name()
	failSquadSkillAuditForKind(t, "skill.create", testWorkspaceID)

	assertAuditFailure(t, archiveImport(t, name, "fail"))
	if n := countSkillsByName(t, name); n != 0 {
		t.Errorf("skills named %q = %d; want 0", name, n)
	}
}

func TestImportSkill_ArchiveOverwriteRecordsAuditRow(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-import-overwrite", "old body")
	name := "audit-import-overwrite-" + t.Name()
	w := archiveImport(t, name, "overwrite")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "skill.update", id, "skill")
	assertNoneStored(t, row, skillSecretDesc, skillSecretFile)
}

func TestImportSkill_ArchiveOverwriteAuditInsertFailureKeepsSkill(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-import-overwrite-fail", "old body")
	name := "audit-import-overwrite-fail-" + t.Name()
	failAuditInsertFor(t, id)

	assertAuditFailure(t, archiveImport(t, name, "overwrite"))
	var content string
	dbfx.QueryRow(t, `SELECT content FROM skill WHERE id = $1`, id).Scan(&content)
	if content != "old body" {
		t.Errorf("content = %q; want the old body (rolled back)", content)
	}
}

func TestImportSkill_ArchiveRenameRecordsAuditRow(t *testing.T) {
	insertHandlerTestSkill(t, "audit-import-rename", "old body")
	name := "audit-import-rename-" + t.Name()
	renamed := name + "-2"
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM skill WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, renamed)
	})
	w := archiveImport(t, name, "rename")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body SkillImportResult
	json.Unmarshal(w.Body.Bytes(), &body)

	row := onlyAuditRow(t, "skill.create", body.Skill.ID, "skill")
	if row.After["name"] != renamed {
		t.Errorf("after = %v; want name %q", row.After, renamed)
	}
}

func TestImportSkill_ArchiveRenameAuditInsertFailureCreatesNothing(t *testing.T) {
	insertHandlerTestSkill(t, "audit-import-rename-fail", "old body")
	name := "audit-import-rename-fail-" + t.Name()
	failSquadSkillAuditForKind(t, "skill.create", testWorkspaceID)

	assertAuditFailure(t, archiveImport(t, name, "rename"))
	if n := countSkillsByName(t, name+"-2"); n != 0 {
		t.Errorf("renamed skills = %d; want 0", n)
	}
}

func TestRefreshSkill_RecordsAuditRow(t *testing.T) {
	slug := fmt.Sprintf("audit-refresh-%d", time.Now().UnixNano())
	skillID := createImportTargetSkillWithConfig(t, slug, testUserID, clawhubOriginConfig(slug), nil)
	installClawHubFixture(t, slug, slug, skillSecretDesc, map[string]string{"SKILL.md": skillSecretContent})

	w := doRefreshSkill(t, testUserID, skillID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	row := onlyAuditRow(t, "skill.update", skillID, "skill")
	assertNoneStored(t, row, skillSecretDesc, skillSecretContent)
}

func TestRefreshSkill_AuditInsertFailureKeepsSkill(t *testing.T) {
	slug := fmt.Sprintf("audit-refresh-fail-%d", time.Now().UnixNano())
	skillID := createImportTargetSkillWithConfig(t, slug, testUserID, clawhubOriginConfig(slug), nil)
	installClawHubFixture(t, slug, slug, "fresh", map[string]string{"SKILL.md": "# fresh"})
	failAuditInsertFor(t, skillID)

	assertAuditFailure(t, doRefreshSkill(t, testUserID, skillID))
	var content string
	dbfx.QueryRow(t, `SELECT content FROM skill WHERE id = $1`, skillID).Scan(&content)
	if content != "# original" {
		t.Errorf("content = %q; want the original (rolled back)", content)
	}
}

// ── agent skill bindings ────────────────────────────────────────────────────

func agentSkillIDs(t *testing.T, agentID string) string {
	t.Helper()
	var ids string
	dbfx.QueryRow(t, `SELECT COALESCE(string_agg(skill_id::text, ',' ORDER BY skill_id::text), '') FROM agent_skill WHERE agent_id = $1`, agentID).Scan(&ids)
	return ids
}

func TestSetAgentSkills_RecordsAuditRow(t *testing.T) {
	agentID := createHandlerTestAgent(t, "audit-set-skills-agent", nil)
	keep := insertHandlerTestSkill(t, "audit-set-keep", "body")
	drop := insertHandlerTestSkill(t, "audit-set-drop", "body")
	dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, drop)

	w := httptest.NewRecorder()
	testHandler.SetAgentSkills(w, skillAuditReq(http.MethodPut, "/api/agents/"+agentID+"/skills", map[string]any{
		"skill_ids": []string{keep},
	}, agentID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "agent.skills.update", agentID, "agent")
	if row.Before["skill_ids"] != drop || row.After["skill_ids"] != keep {
		t.Errorf("before = %v after = %v; want %s -> %s", row.Before, row.After, drop, keep)
	}
}

func TestSetAgentSkills_AuditInsertFailureKeepsBindings(t *testing.T) {
	agentID := createHandlerTestAgent(t, "audit-set-skills-fail-agent", nil)
	keep := insertHandlerTestSkill(t, "audit-set-fail-keep", "body")
	drop := insertHandlerTestSkill(t, "audit-set-fail-drop", "body")
	dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, drop)
	failAuditInsertFor(t, agentID)

	w := httptest.NewRecorder()
	testHandler.SetAgentSkills(w, skillAuditReq(http.MethodPut, "/api/agents/"+agentID+"/skills", map[string]any{
		"skill_ids": []string{keep},
	}, agentID))
	assertAuditFailure(t, w)
	if got := agentSkillIDs(t, agentID); got != drop {
		t.Errorf("agent skills = %q; want %q (rolled back)", got, drop)
	}
}

func TestAddAgentSkills_RecordsAuditRow(t *testing.T) {
	agentID := createHandlerTestAgent(t, "audit-add-skills-agent", nil)
	existing := insertHandlerTestSkill(t, "audit-add-existing", "body")
	added := insertHandlerTestSkill(t, "audit-add-new", "body")
	dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, existing)

	w := httptest.NewRecorder()
	testHandler.AddAgentSkills(w, skillAuditReq(http.MethodPost, "/api/agents/"+agentID+"/skills/add", map[string]any{
		"skill_ids": []string{added},
	}, agentID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "agent.skills.update", agentID, "agent")
	if row.Before["skill_ids"] != existing || !strings.Contains(fmt.Sprint(row.After["skill_ids"]), added) {
		t.Errorf("before = %v after = %v; want %s added", row.Before, row.After, added)
	}
}

func TestAddAgentSkills_AuditInsertFailureAddsNothing(t *testing.T) {
	agentID := createHandlerTestAgent(t, "audit-add-skills-fail-agent", nil)
	added := insertHandlerTestSkill(t, "audit-add-fail-new", "body")
	failAuditInsertFor(t, agentID)

	w := httptest.NewRecorder()
	testHandler.AddAgentSkills(w, skillAuditReq(http.MethodPost, "/api/agents/"+agentID+"/skills/add", map[string]any{
		"skill_ids": []string{added},
	}, agentID))
	assertAuditFailure(t, w)
	if got := agentSkillIDs(t, agentID); got != "" {
		t.Errorf("agent skills = %q; want none (rolled back)", got)
	}
}

// ── runtime skill toggle ────────────────────────────────────────────────────

func newRuntimeSkillAgent(t *testing.T) (agentID, runtimeID string) {
	t.Helper()
	runtimeID = createRuntimeLocalSkillTestRuntime(t, testUserID)
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id
		)
		VALUES ($1, $2, '', 'local', '{}'::jsonb, $3, 'private', 'private', 1, $4)
		RETURNING id
	`, testWorkspaceID, "Audit Runtime Skill "+t.Name(), runtimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID) })
	return agentID, runtimeID
}

func setRuntimeSkillEnabled(agentID, runtimeID string, enabled bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testHandler.SetAgentRuntimeSkillEnabled(w, skillAuditReq(http.MethodPut, "/api/agents/"+agentID+"/runtime-skills/enabled", map[string]any{
		"runtime_id": runtimeID, "root": "provider", "key": "review", "name": "Review Helper", "enabled": enabled,
	}, agentID))
	return w
}

func TestSetAgentRuntimeSkillEnabled_RecordsAuditRow(t *testing.T) {
	agentID, runtimeID := newRuntimeSkillAgent(t)
	if w := setRuntimeSkillEnabled(agentID, runtimeID, false); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "agent.runtime_skill.update", agentID, "agent")
	if got := fmt.Sprint(row.After["disabled_runtime_skills"]); !strings.Contains(got, "review") {
		t.Errorf("after = %v; want the disabled skill key", row.After)
	}
}

func TestSetAgentRuntimeSkillEnabled_AuditInsertFailureKeepsSetting(t *testing.T) {
	agentID, runtimeID := newRuntimeSkillAgent(t)
	failAuditInsertFor(t, agentID)

	assertAuditFailure(t, setRuntimeSkillEnabled(agentID, runtimeID, false))
	var disabled string
	dbfx.QueryRow(t, `SELECT disabled_runtime_skills::text FROM agent WHERE id = $1`, agentID).Scan(&disabled)
	if disabled != "[]" {
		t.Errorf("disabled_runtime_skills = %s; want [] (rolled back)", disabled)
	}
}

// ── agent MCP binding ───────────────────────────────────────────────────────

func TestAddAgentMcpServer_RecordsAuditRow(t *testing.T) {
	agentID := createHandlerTestAgent(t, "audit-mcp-add-agent", nil)
	serverID := createWorkspaceMcpServerForTest(t, "audit-mcp-add-"+t.Name(), workspaceMcpTestEntry)

	w := httptest.NewRecorder()
	testHandler.AddAgentMcpServer(w, skillAuditReq(http.MethodPost, "/api/agents/"+agentID+"/mcp-servers", map[string]any{
		"server_id": serverID,
	}, agentID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	row := onlyAuditRow(t, "agent.mcp_server.add", agentID, "agent")
	if row.After["mcp_server_ids"] != serverID {
		t.Errorf("after = %v; want %s", row.After, serverID)
	}
	assertNoneStored(t, row, workspaceMcpTestSecret, "linear.example")
}

func TestAddAgentMcpServer_AuditInsertFailureAddsNothing(t *testing.T) {
	agentID := createHandlerTestAgent(t, "audit-mcp-add-fail-agent", nil)
	serverID := createWorkspaceMcpServerForTest(t, "audit-mcp-add-fail-"+t.Name(), workspaceMcpTestEntry)
	failAuditInsertFor(t, agentID)

	w := httptest.NewRecorder()
	testHandler.AddAgentMcpServer(w, skillAuditReq(http.MethodPost, "/api/agents/"+agentID+"/mcp-servers", map[string]any{
		"server_id": serverID,
	}, agentID))
	assertAuditFailure(t, w)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_mcp_server WHERE agent_id = $1`, agentID); n != 0 {
		t.Errorf("agent_mcp_server rows = %d; want 0 (rolled back)", n)
	}
}
