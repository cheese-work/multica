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
	r := receiptFixture{voteFixture: newVoteFixture(t), tg: receiptTarget(t)}
	r.initial, r.next = voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after")+"\n"+voteHuman+"\n"
	r.id = r.tg.seed(t, r.initial)
	r.label = "agent.instructions:" + r.id
	r.beforeDigest, r.nextDigest = testDigest(r.initial), testDigest(r.next)
	r.issue, r.reviewerID = r.issueID, r.leaderID
	r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + r.beforeDigest
	r.delegate(t, r.writerID, "agent.instructions")
	return r
}

// delegate stores the workspace setting that lets agentID write field.
func (r receiptFixture) delegate(t *testing.T, agentID, field string) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	setting, _ := json.Marshal(map[string]any{"delegated_governed_writes": DelegatedWritesConfig{
		AllowedAgents: []string{agentID}, AllowedFields: []string{field}, RequiresHumanDecision: true,
	}})
	dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(setting))
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

func TestPassReceipt_ValidReceiptWritesOnceAndLeavesAuditRow(t *testing.T) {
	r := newReceiptFixture(t)
	r.review(t)
	w := r.send(r.next, r.beforeDigest)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := r.tg.read(t, r.id); got != r.next {
		t.Fatalf("value not written: %q", got)
	}
	if r.used(t) != 1 {
		t.Fatal("accepted write left no receipt row")
	}
	// The value is restored out of band, then the same signed change is
	// retried: the used receipt is refused with 409 and nothing is written.
	dbfx.Exec(t, `UPDATE agent SET instructions = $2 WHERE id = $1`, r.id, r.initial)
	again := r.send(r.next, r.beforeDigest)
	if again.Code != http.StatusConflict {
		t.Fatalf("reuse: expected 409, got %d: %s", again.Code, again.Body.String())
	}
	if got := r.tg.read(t, r.id); got != r.initial {
		t.Fatalf("reused receipt changed the value to %q", got)
	}
	if r.used(t) != 1 {
		t.Fatalf("reuse changed the receipt rows: %d", r.used(t))
	}
}

// The design's receipt is one comment by the reviewer: Proposal, then PASS.
func TestPassReceipt_ProposalAndPassInOneComment(t *testing.T) {
	r := newReceiptFixture(t)
	r.say(t, "agent", r.reviewerID, 30, "Proposal: "+r.label+" "+r.nextDigest+"\n"+r.reviewerLine)
	if w := r.send(r.next, r.beforeDigest); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPassReceipt_PassBeforeProposalInOneCommentRefused(t *testing.T) {
	r := newReceiptFixture(t)
	r.say(t, "agent", r.reviewerID, 30, r.reviewerLine+"\nProposal: "+r.label+" "+r.nextDigest)
	r.wantRefused(t, r.send(r.next, r.beforeDigest))
}

func TestPassReceipt_FailedChecksStayRefused(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, r receiptFixture)
		mut    func(map[string]any)
		before string
	}{
		{name: "reviewer is of the writer's lab", setup: func(t *testing.T, r receiptFixture) {
			r.retask(t, r.reviewerID, r.runtimeOf(t, r.writerID), "claude-test")
			r.review(t)
		}},
		{name: "writer votes for itself", setup: func(t *testing.T, r receiptFixture) {
			r.reviewerID = r.writerID
			r.review(t)
		}},
		{name: "reviewer lab unknown", setup: func(t *testing.T, r receiptFixture) {
			r.retask(t, r.reviewerID, r.runtimeOf(t, r.reviewerID), "gpt-unlisted")
			r.review(t)
		}},
		{name: "writer lab unknown", setup: func(t *testing.T, r receiptFixture) {
			r.retask(t, r.writerID, r.runtimeOf(t, r.writerID), "claude-unlisted")
			r.review(t)
		}},
		// The receipt is real for the digests it names, so only the live-value
		// and new-value checks can refuse these two.
		{name: "stale before digest", before: testDigest("not the live value"),
			setup: func(t *testing.T, r receiptFixture) {
				r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + testDigest("not the live value")
				r.review(t)
			}},
		{name: "wrong after digest",
			setup: func(t *testing.T, r receiptFixture) {
				r.nextDigest = testDigest("something else")
				r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + r.beforeDigest
				r.review(t)
			},
			mut: func(b map[string]any) { b["after_sha256"] = testDigest("something else") }},
		{name: "receipt names another before digest", setup: func(t *testing.T, r receiptFixture) {
			r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + testDigest("older value")
			r.review(t)
		}},
		{name: "PASS before the proposal", setup: func(t *testing.T, r receiptFixture) {
			r.say(t, "agent", r.reviewerID, 90, r.reviewerLine)
			r.say(t, "agent", r.writerID, 60, "Proposal: "+r.label+" "+r.nextDigest)
		}},
		{name: "no proposal", setup: func(t *testing.T, r receiptFixture) {
			r.say(t, "agent", r.reviewerID, 30, r.reviewerLine)
		}},
		{name: "BLOCK present", setup: func(t *testing.T, r receiptFixture) {
			r.review(t)
			r.say(t, "agent", r.peerID, 10, "Vote: BLOCK "+r.label+" "+r.nextDigest)
		}},
		{name: "receipt for another target", setup: func(t *testing.T, r receiptFixture) {
			r.label = "agent.instructions:00000000-0000-0000-0000-000000000000"
			r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + r.beforeDigest
			r.review(t)
		}},
		{name: "member PASS does not count", setup: func(t *testing.T, r receiptFixture) {
			r.say(t, "agent", r.writerID, 60, "Proposal: "+r.label+" "+r.nextDigest)
			r.say(t, "member", testUserID, 30, r.reviewerLine)
		}},
		{name: "writer not in allowed_agents", setup: func(t *testing.T, r receiptFixture) {
			r.review(t)
			r.delegate(t, r.peerID, "agent.instructions")
		}},
		{name: "field not in allowed_fields", setup: func(t *testing.T, r receiptFixture) {
			r.review(t)
			r.delegate(t, r.writerID, "project.description")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReceiptFixture(t)
			c.setup(t, r)
			before := r.beforeDigest
			if c.before != "" {
				before = c.before
			}
			var mut []func(map[string]any)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			r.wantRefused(t, r.send(r.next, before, mut...))
		})
	}
}

func TestPassReceipt_HumanBlockChangeRefused(t *testing.T) {
	r := newReceiptFixture(t)
	changed := voteAgent("after") + "\n<!-- governed:human -->\nrule removed\n<!-- /governed:human -->"
	r.nextDigest = testDigest(changed)
	r.reviewerLine = "Vote: PASS " + r.label + " " + r.nextDigest + " from " + r.beforeDigest
	r.review(t)
	r.wantRefused(t, r.send(changed, r.beforeDigest))
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

func TestPassReceipt_ReceiptAndVoteTogetherAreBadRequest(t *testing.T) {
	r := newReceiptFixture(t)
	r.review(t)
	w := r.send(r.next, r.beforeDigest, func(b map[string]any) { b["vote_issue"] = r.issue })
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
