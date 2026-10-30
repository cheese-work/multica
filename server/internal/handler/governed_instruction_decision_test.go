package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// CHE-2457: an agent may write a governed field when the request names an
// answered decision bound to the exact change: the field, decision_id,
// before_sha256, after_sha256. Runs against all four governed fields through
// the real update handlers. The vote path stays as CHE-1862 left it.

// decisionWriteFixture is an owner-approved card whose ask names before and
// after for field, plus the workspace setting that lets the card's agent write it.
type decisionWriteFixture struct {
	askedRecordFixture
	field string
}

func newDecisionWriteFixture(t *testing.T, field, before, after string) decisionWriteFixture {
	t.Helper()
	resetDecisionLease(t)
	fixture := newCardFixture(t)
	options := `[{"label":"` + decisiontx.ApproveWriteLabel + `","what_happens":"apply"},{"label":"Reject","what_happens":"drop"}]`
	dbfx.Exec(t, `UPDATE decision_request SET options = $2::jsonb, ask = $3 WHERE id = $1`,
		fixture.decision.ID, options, decisiontx.BoundWriteDecision(field, before, after))
	dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, fixture.issue.WorkspaceID, parseUUID(fixture.humanID))
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: decisiontx.ApproveWriteLabel}); err != nil {
		t.Fatal(err)
	}
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	setting, _ := json.Marshal(map[string]any{"delegated_governed_writes": DelegatedWritesConfig{
		AllowedAgents: []string{fixture.agentID}, AllowedFields: []string{field}, RequiresHumanDecision: true,
	}})
	dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(setting))
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
		dbfx.Exec(t, `DELETE FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID)
	})
	return decisionWriteFixture{fixture, field}
}

func (f decisionWriteFixture) write(tg ownerCASTarget, id, content, before string, mutate ...func(map[string]any)) *httptest.ResponseRecorder {
	body := map[string]any{
		tg.field:        content,
		"decision_id":   uuidToString(f.decision.ID),
		"before_sha256": before,
		"after_sha256":  testDigest(content),
	}
	for _, m := range mutate {
		m(body)
	}
	w := httptest.NewRecorder()
	tg.handle(w, asAgentActor(tg.req(testUserID, id, body), f.agentID, f.taskID))
	return w
}

func (f decisionWriteFixture) used(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_bound_write WHERE decision_id = $1`, f.decision.ID)
}

// eachDecisionCase seeds a target holding initial, approves the change to
// next, and runs fn.
func eachDecisionCase(t *testing.T, initial, next string, fn func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string)) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tg := range voteTargets() {
		t.Run(tg.name, func(t *testing.T) {
			id := tg.seed(t, initial)
			fn(t, tg, newDecisionWriteFixture(t, tg.name, testDigest(initial), testDigest(next)), id)
		})
	}
}

func TestDecisionGate_AcceptedWriteIsSingleUse(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after — café")+"\n"+voteHuman+"\n"
	eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		w := f.write(tg, id, next, testDigest(initial))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != next {
			t.Fatalf("value not written: %q", got)
		}
		if resp := decodeCASResponse(t, w); resp["before_digest"] != testDigest(initial) || resp["after_digest"] != testDigest(next) {
			t.Fatalf("receipt digests missing from response: %v", resp)
		}
		if f.used(t) != 1 {
			t.Fatal("accepted write left no audit row")
		}
		// Same decision, same change, moved back: refused, value stays.
		again := f.write(tg, id, initial, testDigest(next))
		if again.Code != http.StatusForbidden && again.Code != http.StatusConflict {
			t.Fatalf("reuse: expected 403 or 409, got %d: %s", again.Code, again.Body.String())
		}
		if got := tg.read(t, id); got != next {
			t.Fatalf("reused decision changed the value to %q", got)
		}
	})
}

func TestDecisionGate_WrongHashRefused(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman, voteAgent("after")+"\n"+voteHuman
	for name, mutate := range map[string]func(map[string]any){
		"after hash is not the content's":   func(b map[string]any) { b["after_sha256"] = testDigest("something else") },
		"before hash is not the live value": func(b map[string]any) { b["before_sha256"] = testDigest("not live") },
		"content differs from the approved one": func(b map[string]any) {
			for k := range b {
				if k != "decision_id" && k != "before_sha256" && k != "after_sha256" {
					b[k] = voteAgent("sneaky") + "\n" + voteHuman
					b["after_sha256"] = testDigest(b[k].(string))
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
				wantDenied(t, tg, id, initial, f.write(tg, id, next, testDigest(initial), mutate))
				if f.used(t) != 0 {
					t.Fatal("refused write consumed the decision")
				}
			})
		})
	}
}

func TestDecisionGate_HumanBlockChangeRefused(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman, voteAgent("after")+"\n"+"<!-- governed:human -->\nrule removed\n<!-- /governed:human -->"
	eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		wantDenied(t, tg, id, initial, f.write(tg, id, next, testDigest(initial)))
		if f.used(t) != 0 {
			t.Fatal("refused write consumed the decision")
		}
	})
}

func TestDecisionGate_UnapprovedDecisionRefused(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman, voteAgent("after")+"\n"+voteHuman
	cases := map[string]func(*testing.T, decisionWriteFixture){
		"agent not in the allow list": func(t *testing.T, f decisionWriteFixture) {
			dbfx.Exec(t, `UPDATE workspace SET settings = '{}'::jsonb WHERE id = $1`, testWorkspaceID)
		},
		"human is not an owner": func(t *testing.T, f decisionWriteFixture) {
			dbfx.Exec(t, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, f.issue.WorkspaceID, parseUUID(f.humanID))
		},
		"answer chose another option": func(t *testing.T, f decisionWriteFixture) {
			dbfx.Exec(t, `UPDATE decision_answer SET content_snapshot = 'Reject' WHERE decision_request_id = $1`, f.decision.ID)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
				mutate(t, f)
				wantDenied(t, tg, id, initial, f.write(tg, id, next, testDigest(initial)))
				if f.used(t) != 0 {
					t.Fatal("refused write consumed the decision")
				}
			})
		})
	}
}

func TestDecisionGate_VoteAndDecisionTogetherAreBadRequest(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman, voteAgent("after")+"\n"+voteHuman
	eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		w := f.write(tg, id, next, testDigest(initial), func(b map[string]any) { b["vote_issue"] = f.issueID })
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != initial {
			t.Fatalf("value must stay %q, got %q", initial, got)
		}
	})
}

// An approval names one field. Identical bytes in another field do not
// carry it over: the ask's field must be the write's field.
func TestDecisionGate_ApprovalForAnotherFieldRefused(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman, voteAgent("after")+"\n"+voteHuman
	eachDecisionCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		other := "project.description"
		if tg.name == other {
			other = "squad.instructions"
		}
		dbfx.Exec(t, `UPDATE decision_request SET ask = $2 WHERE id = $1`, f.decision.ID,
			decisiontx.BoundWriteDecision(other, testDigest(initial), testDigest(next)))
		wantDenied(t, tg, id, initial, f.write(tg, id, next, testDigest(initial)))
		if f.used(t) != 0 {
			t.Fatal("refused write consumed the decision")
		}
	})
}
