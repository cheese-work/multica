package handler

import (
	"context"
	"net/http/httptest"
	"testing"
)

// CHE-5318 merge preservation: the fork audits UpdateSkill inside the write
// transaction; upstream v0.8.2 records a skill version in the same handler.
// Both must land together, and a failed audit must roll the version back.

func TestUpdateSkill_AuditAndVersionLandTogether(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-version-ok", "old body")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM skill_version WHERE skill_id = $1`, id)
		testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, id)
	})

	w := httptest.NewRecorder()
	testHandler.UpdateSkill(w, skillAuditReq("PUT", "/api/skills/"+id, map[string]any{"content": "new body"}, id))
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	onlyAuditRow(t, "skill.update", id, "skill")
	if n := dbfx.Count(t, `SELECT count(*) FROM skill_version WHERE skill_id = $1`, id); n < 1 {
		t.Errorf("skill_version rows = %d; want the update recorded", n)
	}
}

func TestUpdateSkill_AuditFailureRollsBackVersion(t *testing.T) {
	id := insertHandlerTestSkill(t, "audit-version-fail", "old body")
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM skill_version WHERE skill_id = $1`, id) })
	failAuditInsertFor(t, id)

	w := httptest.NewRecorder()
	testHandler.UpdateSkill(w, skillAuditReq("PUT", "/api/skills/"+id, map[string]any{"content": "new body"}, id))
	assertAuditFailure(t, w)

	if n := dbfx.Count(t, `SELECT count(*) FROM skill_version WHERE skill_id = $1 AND source = 'update'`, id); n != 0 {
		t.Errorf("update version rows = %d; want 0 (rolled back with the audit failure)", n)
	}
}
