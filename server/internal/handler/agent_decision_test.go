package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type agentDecisionFixture struct {
	voteFixture
	instance, proposal string
	scope              map[string]any
}

func newAgentDecisionFixture(t *testing.T) agentDecisionFixture {
	t.Helper()
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	f := agentDecisionFixture{voteFixture: newVoteFixture(t), instance: uuid.NewString()}
	for _, task := range f.tasks {
		dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, f.issueID, task)
	}
	f.scope = map[string]any{
		"work_author_id": f.writerID, "waived_gate": "screenshot_evidence", "route": "cosign",
		"signers":  []string{f.leaderID, f.peerID},
		"evidence": []map[string]string{{"url": "https://example.com/frozen.md", "sha256": boundAfter}},
	}
	// Request/scope rows are not owned by the generic comment fixture.
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue WHERE id = $1`, f.issueID)
		dbfx.Exec(t, `DELETE FROM governed_vote_record WHERE issue_id = $1`, f.issueID)
		if dbfx.Count(t, `SELECT count(*) FROM pg_class WHERE relname='decision_agent_scope' AND relnamespace='public'::regnamespace`) != 0 {
			dbfx.Exec(t, `DELETE FROM decision_agent_scope WHERE issue_id = $1`, f.issueID)
		}
		dbfx.Exec(t, `DELETE FROM decision_request WHERE issue_id = $1`, f.issueID)
	})
	return f
}

func (f agentDecisionFixture) body(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(f.scope)
	if err != nil {
		t.Fatal(err)
	}
	return "Proposal: decision:" + f.instance + " " + boundAfter + "\n\n```decision\n" + string(raw) + "\n```\n"
}

func (f agentDecisionFixture) post(t *testing.T, agent, content string, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := asAgentActor(withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": content}), "id", f.issueID), agent, f.tasks[agent])
	req.Header.Set("X-Actor-Source", "task_token")
	w := httptest.NewRecorder()
	testHandler.CreateComment(w, req)
	if w.Code != status {
		t.Fatalf("comment status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	return w
}

func (f *agentDecisionFixture) propose(t *testing.T) {
	t.Helper()
	f.proposal = f.body(t)
	f.post(t, f.writerID, f.proposal, http.StatusCreated)
}

func (f agentDecisionFixture) opinion(agent, verdict string) string {
	return fmt.Sprintf("decision_instance: %s\ncandidate_sha256: %s\nwaived_gate: screenshot_evidence\nsigner_agent_id: %s\nproposal_sha256: %s\nevidence: [{\"url\":\"https://example.com/frozen.md\",\"sha256\":%q}]\nverdict: %s\nindependence: formed before reading any other signer's opinion\nnonce: 1234567890abcdef1234567890abcdef\n\nReasoned opinion.\n", f.instance, boundAfter, agent, testDigest(f.proposal), boundAfter, verdict)
}

func (f agentDecisionFixture) commit(t *testing.T, agent, opinion string) {
	t.Helper()
	f.post(t, agent, "Commit: decision:"+f.instance+" "+boundAfter+" "+testDigest(opinion), http.StatusCreated)
}

func (f agentDecisionFixture) reveal(t *testing.T, agent, verdict, opinion string, status int) {
	t.Helper()
	f.post(t, agent, "Vote: "+verdict+" decision:"+f.instance+" "+boundAfter+"\n\n```opinion\n"+opinion+"```\n", status)
}

func TestAgentDecisionExcludedGates(t *testing.T) {
	for _, gate := range []string{"test_first", "merge", "deploy", "spend", "publication", "irreversible", "product_direction", "security", "unknown"} {
		t.Run(gate, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			f.scope["waived_gate"] = gate
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
	}
}

func TestAgentDecisionInstanceBindingAndSnapshot(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	var authority, body, task, author string
	dbfx.QueryRow(t, `SELECT r.authority, s.proposal_body, s.proposer_task_id::text, s.work_author_id::text FROM decision_request r JOIN decision_agent_scope s ON s.instance_id=r.id WHERE r.id=$1`, f.instance).Scan(&authority, &body, &task, &author)
	if authority != "agents" || body != f.proposal || task != f.taskID || author != f.writerID {
		t.Fatalf("lost proposal provenance: %s %s %s %s", authority, body, task, author)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_answer WHERE decision_request_id=$1`, f.instance); n != 0 {
		t.Fatalf("invented %d human answers", n)
	}
	f.post(t, f.writerID, f.proposal, http.StatusBadRequest)
	f.post(t, f.leaderID, "Commit: decision:"+uuid.NewString()+" "+boundAfter+" "+boundBefore, http.StatusBadRequest)
	f.post(t, f.leaderID, "Commit: decision:"+f.instance+" "+boundBefore+" "+boundBefore, http.StatusBadRequest)
	other := f
	other.issueID = dbfx.Issue(t, "foreign decision issue")
	other.post(t, f.leaderID, "Commit: decision:"+f.instance+" "+boundAfter+" "+boundBefore, http.StatusBadRequest)
}

func TestAgentDecisionSignerRules(t *testing.T) {
	for _, kind := range []string{"author", "duplicate", "same_lab", "unknown_lab", "unknown_author_lab"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			switch kind {
			case "author":
				f.scope["signers"] = []string{f.writerID, f.leaderID}
			case "duplicate":
				f.scope["signers"] = []string{f.leaderID, f.leaderID}
			case "same_lab":
				f.retask(t, f.leaderID, f.runtimeOf(t, f.peerID), "claude-test")
			case "unknown_lab":
				f.retask(t, f.peerID, f.runtimeOf(t, f.peerID), "unknown")
			case "unknown_author_lab":
				f.retask(t, f.writerID, f.runtimeOf(t, f.writerID), "unknown")
			}
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
	}
}

func TestAgentDecisionRevealValidation(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "instance", "digest", "gate", "signer", "proposal", "evidence", "verdict", "independence", "nonce", "hash", "two_blocks", "two_votes"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			f.propose(t)
			opinion := f.opinion(f.leaderID, "PASS")
			switch kind {
			case "missing":
				opinion = strings.Replace(opinion, "waived_gate: screenshot_evidence\n", "", 1)
			case "duplicate":
				opinion = "verdict: PASS\n" + opinion
			case "instance":
				opinion = strings.Replace(opinion, f.instance, uuid.NewString(), 1)
			case "digest":
				opinion = strings.Replace(opinion, "candidate_sha256: "+boundAfter, "candidate_sha256: "+boundBefore, 1)
			case "gate":
				opinion = strings.Replace(opinion, "waived_gate: screenshot_evidence", "waived_gate: design_lock_label", 1)
			case "signer":
				opinion = strings.Replace(opinion, f.leaderID, f.peerID, 1)
			case "proposal":
				opinion = strings.Replace(opinion, testDigest(f.proposal), boundBefore, 1)
			case "evidence":
				opinion = strings.Replace(opinion, "https://example.com/frozen.md", "https://example.com/other.md", 1)
			case "verdict":
				opinion = strings.Replace(opinion, "verdict: PASS", "verdict: BLOCK", 1)
			case "independence":
				opinion = strings.Replace(opinion, "formed before reading any other signer's opinion", "read the other opinion", 1)
			case "nonce":
				opinion = strings.Replace(opinion, "1234567890abcdef1234567890abcdef", "1234", 1)
			}
			f.commit(t, f.leaderID, opinion)
			f.commit(t, f.peerID, f.opinion(f.peerID, "PASS"))
			switch kind {
			case "hash":
				opinion += "Changed after committing.\n"
			case "two_blocks":
				opinion += "```\n```opinion\nextra\n"
			case "two_votes":
				opinion += "```\nVote: BLOCK decision:" + f.instance + " " + boundAfter + "\n```opinion\n"
			}
			f.reveal(t, f.leaderID, "PASS", opinion, http.StatusBadRequest)
		})
	}
}

func TestAgentDecisionEarlyRevealIsVoid(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	first := f.opinion(f.leaderID, "PASS")
	f.commit(t, f.leaderID, first)
	// Persist the premature reveal as an auditable void, never a counted vote.
	f.reveal(t, f.leaderID, "PASS", first, http.StatusBadRequest)
	if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, f.issueID); got != 2 {
		t.Fatalf("refused reveal created a comment: %d comments", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governed_vote_record WHERE decision_instance_id=$1 AND decision_event='void' AND source_task_id=$2 AND full_body LIKE 'Vote: PASS%'`, f.instance, f.tasks[f.leaderID]); got != 1 {
		t.Fatalf("missing rejected-attempt provenance: %d records", got)
	}
	f.commit(t, f.peerID, f.opinion(f.peerID, "PASS"))
	f.reveal(t, f.leaderID, "PASS", first, http.StatusBadRequest)
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_decision_live_vote WHERE instance_id=$1 AND verdict='PASS'`, f.instance); got != 0 {
		t.Fatalf("early reveal counted %d PASS", got)
	}
}

func TestAgentDecisionRetractionAndConflictingVote(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	a, b := f.opinion(f.leaderID, "PASS"), f.opinion(f.peerID, "PASS")
	f.commit(t, f.leaderID, a)
	f.commit(t, f.peerID, b)
	f.reveal(t, f.leaderID, "PASS", a, http.StatusCreated)
	f.reveal(t, f.leaderID, "BLOCK", a, http.StatusBadRequest)
	f.reveal(t, f.peerID, "PASS", b, http.StatusCreated)
	f.post(t, f.leaderID, "Retract: decision:"+f.instance, http.StatusCreated)
	f.post(t, f.leaderID, "Retract: decision:"+f.instance, http.StatusBadRequest)
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_decision_live_vote WHERE instance_id=$1 AND verdict='PASS'`, f.instance); got != 1 {
		t.Fatalf("retracted PASS counted: %d", got)
	}
}
