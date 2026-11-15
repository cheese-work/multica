package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// L5a: the governed-field write paths (owner CAS, vote receipt, decision
// binding) write their audit row in the same transaction as the swap.

// auditRowCount counts the rows already written for the target; workspace.context
// targets the shared test workspace, so earlier tests leave rows behind.
func auditRowCount(t *testing.T, tg ownerCASTarget, id string) int {
	t.Helper()
	return len(auditRowsFor(t, tg.name, id))
}

func requireGovernedAuditRow(t *testing.T, tg ownerCASTarget, id string, rowsBefore int, wantActor, before, after string) {
	t.Helper()
	rows := auditRowsFor(t, tg.name, id)
	if len(rows) != rowsBefore+1 {
		t.Fatalf("audit rows for %s = %d, want %d", tg.name, len(rows), rowsBefore+1)
	}
	row := rows[len(rows)-1]
	if row.ActorType != wantActor {
		t.Errorf("actor_type = %q, want %q", row.ActorType, wantActor)
	}
	if wantActor == "agent" && row.TaskID == "" {
		t.Error("agent row carries no task_id")
	}
	b, _ := row.Before[tg.field].(map[string]any)
	a, _ := row.After[tg.field].(map[string]any)
	if b == nil || a == nil || b["sha256"] == nil {
		t.Fatalf("text fields must be stored as digest summaries; before=%v after=%v", row.Before, row.After)
	}
	if int(b["len"].(float64)) != len(before) || int(a["len"].(float64)) != len(after) {
		t.Errorf("lengths = %v/%v, want %d/%d", b["len"], a["len"], len(before), len(after))
	}
	if strings.Contains(auditRowText(row), after) || strings.Contains(auditRowText(row), "initial") {
		t.Errorf("row leaks instruction text: %s", auditRowText(row))
	}
}

func auditRowText(r auditRow) string { return fmt.Sprint(r.Before, r.After) }

func TestOwnerCAS_WritesMemberAuditRow(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		initial, next := "initial\n", "second version\nwith two lines\n"
		id := tg.seed(t, initial)
		rowsBefore := auditRowCount(t, tg, id)
		if w := tg.do(testUserID, id, next, testDigest(initial)); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		requireGovernedAuditRow(t, tg, id, rowsBefore, "member", initial, next)
	})
}

func TestOwnerCAS_AuditInsertFailureRollsBackTheSwap(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		initial := "initial\n"
		id := tg.seed(t, initial)
		failAuditInsertFor(t, id)
		w := tg.do(testUserID, id, "changed\n", testDigest(initial))
		if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") == "" {
			t.Fatalf("expected retryable 500, got %d (Retry-After %q): %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		if got := tg.read(t, id); got != initial {
			t.Fatalf("swap must roll back with its audit row, stored = %q", got)
		}
	})
}

func TestDecisionGate_WritesAgentAuditRow(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after — café")+"\n"+voteHuman+"\n"
	eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		rowsBefore := auditRowCount(t, tg, id)
		if w := f.write(tg, id, next, testDigest(initial)); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		requireGovernedAuditRow(t, tg, id, rowsBefore, "agent", initial, next)
	})
}

func TestDecisionGate_AuditInsertFailureKeepsValueAndDecisionUnspent(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after")+"\n"+voteHuman+"\n"
	eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		failAuditInsertFor(t, id)
		if w := f.write(tg, id, next, testDigest(initial)); w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != initial {
			t.Fatalf("value must stay, got %q", got)
		}
		if f.used(t) != 0 {
			t.Fatal("decision was consumed although the write rolled back")
		}
	})
}

func TestVoteGate_WritesAgentAuditRow(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after — café")+"\n"+voteHuman+"\n"
	eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		f.approve(t, label, testDigest(next))
		rowsBefore := auditRowCount(t, tg, id)
		if w := f.write(tg, id, next, testDigest(initial)); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		requireGovernedAuditRow(t, tg, id, rowsBefore, "agent", initial, next)
	})
}

func TestVoteGate_AuditInsertFailureKeepsValue(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after")+"\n"+voteHuman+"\n"
	eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		f.approve(t, label, testDigest(next))
		failAuditInsertFor(t, id)
		if w := f.write(tg, id, next, testDigest(initial)); w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != initial {
			t.Fatalf("value must stay, got %q", got)
		}
	})
}

// ── runtime access changes ───────────────────────────────────────────────

func runtimeAccessRequest(confirmed bool, ownerID, runtimeID string) (func(w http.ResponseWriter, r *http.Request), *http.Request) {
	if confirmed {
		req := newRequestAs(ownerID, http.MethodPost, "/api/runtimes/"+runtimeID+"/revoke-workspace-access",
			map[string]any{"expected_nonowner_agent_ids": []string{}, "expected_task_ids": []string{}})
		return testHandler.RevokeRuntimeWorkspaceAccess, withURLParam(req, "runtimeId", runtimeID)
	}
	req := newRequestAs(ownerID, http.MethodPatch, "/api/runtimes/"+runtimeID, map[string]any{"visibility": "private"})
	return testHandler.UpdateAgentRuntime, withURLParam(req, "runtimeId", runtimeID)
}

func TestRuntimeAccessChange_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, confirmed := range []bool{false, true} {
		kind := map[bool]string{false: "runtime.access.update", true: "runtime.access.revoke"}[confirmed]
		t.Run(kind, func(t *testing.T) {
			runtimeID, ownerID, _ := runtimeVisibilityFixture(t)
			dbfx.Exec(t, `UPDATE agent_runtime SET visibility = 'public' WHERE id = $1`, runtimeID)
			handle, req := runtimeAccessRequest(confirmed, ownerID, runtimeID)
			w := httptest.NewRecorder()
			handle(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			rows := auditRowsFor(t, kind, runtimeID)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if row.ActorType != "member" || row.TargetType != "runtime" {
				t.Errorf("actor/target = %s/%s, want member/runtime", row.ActorType, row.TargetType)
			}
			if row.Before["visibility"] != "public" || row.After["visibility"] != "private" {
				t.Errorf("visibility diff = %v -> %v, want public -> private", row.Before, row.After)
			}
		})
	}
}

func TestRuntimeAccessChange_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "patch", true: "confirmed"}[confirmed], func(t *testing.T) {
			runtimeID, ownerID, _ := runtimeVisibilityFixture(t)
			dbfx.Exec(t, `UPDATE agent_runtime SET visibility = 'public' WHERE id = $1`, runtimeID)
			failAuditInsertFor(t, runtimeID)
			handle, req := runtimeAccessRequest(confirmed, ownerID, runtimeID)
			w := httptest.NewRecorder()
			handle(w, req)
			if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") == "" {
				t.Fatalf("expected retryable 500, got %d: %s", w.Code, w.Body.String())
			}
			var vis string
			dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&vis)
			if vis != "public" {
				t.Fatalf("visibility = %q after failed audit; the change must roll back", vis)
			}
		})
	}
}
