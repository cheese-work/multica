package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-3490 (layer 1 of CHE-3250): an agent may write agent.instructions when
// the request carries a PASS receipt from one reviewer of another lab:
// receipt_issue, before_sha256, after_sha256. The reviewer's `Vote: PASS ...
// from <before>` line is the receipt; each receipt is used once.

type receiptFixture struct {
	voteFixture
	tg                  ownerCASTarget
	id, label, initial  string
	next, nextDigest    string
	beforeDigest, issue string
	reviewerID          string
	reviewerLine        string
	humanDecision       string // stored requires_human_decision, a JSON literal
}

func receiptTarget(t *testing.T) ownerCASTarget {
	t.Helper()
	for _, tg := range voteTargets() {
		if tg.name == "agent.instructions" {
			return tg
		}
	}
	t.Fatal("no agent.instructions target")
	return ownerCASTarget{}
}

func newReceiptFixture(t *testing.T) receiptFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	r := receiptFixture{voteFixture: newVoteFixture(t), tg: receiptTarget(t), humanDecision: "false"}
	r.initial, r.next = voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after")+"\n"+voteHuman+"\n"
	r.id = r.tg.seed(t, r.initial)
	r.label = "agent.instructions:" + r.id
	r.beforeDigest, r.nextDigest = testDigest(r.initial), testDigest(r.next)
	r.issue, r.reviewerID = r.issueID, r.leaderID
	r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + r.beforeDigest
	r.delegate(t, r.writerID, "agent.instructions")
	return r
}

// delegate stores the workspace setting that lets agentID write field, with
// the owner's explicit requires_human_decision choice.
func (r receiptFixture) delegate(t *testing.T, agentID, field string) {
	t.Helper()
	human := r.humanDecision
	if human == "" {
		human = "true" // the default stays closed unless a test opts in
	}
	r.delegateWith(t, agentID, field, `"requires_human_decision": `+human)
}

// delegateWith stores the setting with extra raw JSON members; pass "" to omit
// requires_human_decision from the stored value.
func (r receiptFixture) delegateWith(t *testing.T, agentID, field, extra string) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	agent, _ := json.Marshal([]string{agentID})
	fields, _ := json.Marshal([]string{field})
	members := `"allowed_agents": ` + string(agent) + `, "allowed_fields": ` + string(fields)
	if extra != "" {
		members += ", " + extra
	}
	dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID,
		`{"delegated_governed_writes": {`+members+`}}`)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
	})
}

// review posts the proposal (writer) and the reviewer's PASS line.
func (r receiptFixture) review(t *testing.T) {
	t.Helper()
	r.say(t, "agent", r.writerID, 60, "Proposal: "+r.label+" "+r.nextDigest)
	r.say(t, "agent", r.reviewerID, 30, r.reviewerLine)
}

func (r receiptFixture) send(content, before string, mutate ...func(map[string]any)) *httptest.ResponseRecorder {
	body := map[string]any{
		r.tg.field:      content,
		"receipt_issue": r.issue,
		"before_sha256": before,
		"after_sha256":  testDigest(content),
	}
	for _, m := range mutate {
		m(body)
	}
	w := httptest.NewRecorder()
	r.tg.handle(w, asAgentActor(r.tg.req(testUserID, r.id, body), r.writerID, r.taskID))
	return w
}

func (r receiptFixture) used(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM governed_pass_receipt WHERE target_id = $1`, r.id)
}

func (r receiptFixture) wantRefused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden && w.Code != http.StatusConflict {
		t.Fatalf("expected 403 or 409, got %d: %s", w.Code, w.Body.String())
	}
	if got := r.tg.read(t, r.id); got != r.initial {
		t.Fatalf("value must stay %q, got %q", r.initial, got)
	}
	if r.used(t) != 0 {
		t.Fatal("refused write consumed the receipt")
	}
}

func TestPassReceipt_OtherFieldsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	t.Setenv("MULTICA_VOTE_GATE_FIELDS", voteAllFields)
	initial, next := voteAgent("before")+"\n"+voteHuman, voteAgent("after")+"\n"+voteHuman
	for _, tg := range voteTargets() {
		if tg.name == "agent.instructions" {
			continue
		}
		t.Run(tg.name, func(t *testing.T) {
			f := newVoteFixture(t)
			// Delegated and receipted: only the field scope can refuse this write.
			receiptFixture{voteFixture: f}.delegate(t, f.writerID, tg.name)
			id := tg.seed(t, initial)
			f.say(t, "agent", f.writerID, 60, "Proposal: "+tg.name+":"+id+" "+testDigest(next))
			f.say(t, "agent", f.leaderID, 30, "Vote: PASS "+tg.name+":"+id+" "+testDigest(next)+" from "+testDigest(initial))
			body := map[string]any{tg.field: next, "receipt_issue": f.issueID, "before_sha256": testDigest(initial), "after_sha256": testDigest(next)}
			w := httptest.NewRecorder()
			tg.handle(w, asAgentActor(tg.req(testUserID, id, body), f.writerID, f.taskID))
			wantDenied(t, tg, id, initial, w)
		})
	}
}
