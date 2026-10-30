package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func (f agentDecisionFixture) vetoBody(reason string) string {
	return "Veto: decision:" + f.instance + " " + boundAfter + "\n\n" + reason + "\n"
}

// vetoStatus is safe off the test goroutine: it never calls t.Fatal.
func (f agentDecisionFixture) vetoStatus(agent, content string) int {
	req := asAgentActor(withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": content}), "id", f.issueID), agent, f.tasks[agent])
	req.Header.Set("X-Actor-Source", "task_token")
	w := httptest.NewRecorder()
	testHandler.CreateComment(w, req)
	return w.Code
}

func cleanDecisionC3Tables(t *testing.T, issueID string) {
	t.Helper()
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue WHERE id=$1`, issueID)
		for _, table := range []string{"decision_agent_undo", "decision_agent_reliance", "decision_agent_settlement"} {
			if dbfx.Count(t, `SELECT count(*) FROM pg_class WHERE relname=$1 AND relnamespace='public'::regnamespace`, table) > 0 {
				dbfx.Exec(t, "DELETE FROM "+table+" WHERE issue_id=$1", issueID)
			}
		}
	})
}

func prepareVeto(t *testing.T) agentDecisionFixture {
	t.Helper()
	f := prepareSettlement(t)
	cleanDecisionC3Tables(t, f.issueID)
	return f
}

func effective(t *testing.T, f agentDecisionFixture) {
	t.Helper()
	for _, agent := range []string{f.leaderID, f.peerID} {
		if err := settlementReveal(f, agent, f.opinion(agent, "PASS")); err != nil {
			t.Fatal(err)
		}
	}
}

func liveSettlements(t *testing.T, f agentDecisionFixture) int {
	return dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1 AND revoked_at IS NULL`, f.instance)
}

func TestAgentDecisionVetoRevokesEffectiveRecord(t *testing.T) {
	f := prepareVeto(t)
	effective(t, f)
	if liveSettlements(t, f) != 1 {
		t.Fatal("fixture did not settle")
	}
	f.post(t, f.leaderID, f.vetoBody("the screenshot is stale"), http.StatusCreated)
	if n := liveSettlements(t, f); n != 0 {
		t.Fatalf("%d live settlements after veto", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1 AND revoked_at IS NOT NULL AND revoked_by=$2`, f.instance, f.leaderID); n != 1 {
		t.Fatalf("revocation not attributed to the vetoer: %d", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM governed_vote_record WHERE decision_instance_id=$1 AND decision_event='veto'`, f.instance); n != 1 {
		t.Fatalf("veto events=%d", n)
	}
	lookup := callAgentDecision(t, f, http.MethodGet)
	if !strings.Contains(lookup.Body.String(), `"state":"revoked"`) {
		t.Fatalf("lookup after veto: %s", lookup.Body)
	}
}

func TestAgentDecisionVetoBeforeEffectLeavesRevokedTombstone(t *testing.T) {
	f := prepareVeto(t)
	if err := settlementReveal(f, f.leaderID, f.opinion(f.leaderID, "PASS")); err != nil {
		t.Fatal(err)
	}
	f.post(t, f.peerID, f.vetoBody("the peer vetoes before quorum"), http.StatusCreated)
	// The second reveal is refused on a revoked instance; even a direct engine
	// run cannot make it effective.
	if err := settlementReveal(f, f.peerID, f.opinion(f.peerID, "PASS")); err == nil {
		t.Fatal("reveal accepted on a vetoed instance")
	}
	if n := liveSettlements(t, f); n != 0 {
		t.Fatalf("vetoed instance has %d live settlements", n)
	}
}

func TestAgentDecisionVetoQuorumRace(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) { vetoQuorumRace(t, i) })
	}
}

func vetoQuorumRace(t *testing.T, i int) {
	{
		f := prepareVeto(t)
		if err := settlementReveal(f, f.leaderID, f.opinion(f.leaderID, "PASS")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var vetoCode int
		wg.Add(2)
		go func() { defer wg.Done(); _ = settlementReveal(f, f.peerID, f.opinion(f.peerID, "PASS")) }()
		go func() { defer wg.Done(); vetoCode = f.vetoStatus(f.leaderID, f.vetoBody("race")) }()
		wg.Wait()
		if vetoCode != http.StatusCreated {
			t.Fatalf("veto status=%d", vetoCode)
		}
		if n := liveSettlements(t, f); n != 0 {
			t.Fatalf("iteration %d: %d live settlements survived the veto", i, n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1`, f.instance); n != 1 {
			t.Fatalf("iteration %d: settlement rows=%d, want one revoked row", i, n)
		}
	}
}

func TestAgentDecisionVetoRelianceRace(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) { vetoRelianceRace(t, i) })
	}
}

func vetoRelianceRace(t *testing.T, i int) {
	{
		f := prepareVeto(t)
		effective(t, f)
		var wg sync.WaitGroup
		var rely *httptest.ResponseRecorder
		var vetoCode int
		wg.Add(2)
		go func() {
			defer wg.Done()
			rely = httptest.NewRecorder()
			testHandler.RelyAgentDecision(rely, agentDecisionRequest(f, http.MethodPost))
		}()
		go func() { defer wg.Done(); vetoCode = f.vetoStatus(f.leaderID, f.vetoBody("race")) }()
		wg.Wait()
		if vetoCode != http.StatusCreated {
			t.Fatalf("veto status=%d", vetoCode)
		}
		reliances := dbfx.Count(t, `SELECT count(*) FROM decision_agent_reliance WHERE instance_id=$1`, f.instance)
		undos := dbfx.Count(t, `SELECT count(*) FROM decision_agent_undo u JOIN decision_agent_reliance r ON r.id=u.reliance_id WHERE u.instance_id=$1 AND u.agent_id=$2`, f.instance, f.writerID)
		switch {
		case strings.Contains(rely.Body.String(), `"decided":true`):
			if reliances != 1 || undos != 1 {
				t.Fatalf("iteration %d: accepted reliance not listed in the undo assignment (reliances=%d undos=%d)", i, reliances, undos)
			}
		default:
			if reliances != 0 || undos != 0 {
				t.Fatalf("iteration %d: refused reliance left rows (reliances=%d undos=%d)", i, reliances, undos)
			}
		}
		again := httptest.NewRecorder()
		testHandler.RelyAgentDecision(again, agentDecisionRequest(f, http.MethodPost))
		if strings.Contains(again.Body.String(), `"decided":true`) {
			t.Fatalf("iteration %d: reliance accepted after veto", i)
		}
	}
}

func TestAgentDecisionUndoAssignmentNotifiesRelyingAgent(t *testing.T) {
	f := prepareVeto(t)
	effective(t, f)
	if rely := callAgentDecision(t, f, http.MethodPost); !strings.Contains(rely.Body.String(), `"decided":true`) {
		t.Fatalf("rely: %s", rely.Body)
	}
	f.post(t, f.leaderID, f.vetoBody("undo what relied on this"), http.StatusCreated)
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_undo WHERE instance_id=$1 AND agent_id=$2 AND task_id=$3`, f.instance, f.writerID, f.tasks[f.writerID]); n != 1 {
		t.Fatalf("undo assignments=%d", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_type='agent' AND recipient_id=$1 AND issue_id=$2 AND type='agent_decision_undo'`, f.writerID, f.issueID); n != 1 {
		t.Fatalf("undo notifications=%d", n)
	}
}

func TestAgentDecisionRevokedNeverEffectiveAgain(t *testing.T) {
	f := prepareVeto(t)
	effective(t, f)
	f.post(t, f.leaderID, f.vetoBody("revoke"), http.StatusCreated)
	ctx := context.Background()
	for name, statement := range map[string]string{
		"unrevoke": `UPDATE decision_agent_settlement SET revoked_at=NULL,revoked_by=NULL WHERE instance_id=$1`,
		"rewrite":  `UPDATE decision_agent_settlement SET effective_at=now() WHERE instance_id=$1`,
		"delete":   `DELETE FROM decision_agent_settlement WHERE instance_id=$1`,
	} {
		if _, err := testPool.Exec(ctx, statement, f.instance); err == nil {
			t.Fatalf("%s of a revoked settlement succeeded", name)
		}
	}
	// Later protocol actions and a re-run of the engine cannot resurrect it.
	f.post(t, f.leaderID, "Retract: decision:"+f.instance, http.StatusBadRequest)
	f.post(t, f.peerID, "Commit: decision:"+f.instance+" "+boundAfter+" "+boundBefore, http.StatusBadRequest)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := settleAgentDecision(ctx, tx, parseUUID(testWorkspaceID), f.instance); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := liveSettlements(t, f); n != 0 {
		t.Fatalf("revoked instance became effective again: %d", n)
	}
}

func TestAgentDecisionRetractAfterEffectIsRefused(t *testing.T) {
	f := prepareVeto(t)
	effective(t, f)
	f.post(t, f.leaderID, "Retract: decision:"+f.instance, http.StatusBadRequest)
	if n := liveSettlements(t, f); n != 1 {
		t.Fatalf("refused retraction changed the record: %d", n)
	}
}

func TestAgentDecisionVetoRefusals(t *testing.T) {
	for _, kind := range []string{"stranger", "author", "no_task_token", "digest", "no_reason", "unknown_instance", "reason_too_long"} {
		t.Run(kind, func(t *testing.T) {
			f := prepareVeto(t)
			effective(t, f)
			stranger := createHandlerTestAgent(t, "stranger-"+safeTestName(t), nil)
			f.tasks[stranger] = createHandlerTestTaskForAgentOnIssue(t, stranger, f.issueID)
			body, agent := f.vetoBody("reason"), f.leaderID
			switch kind {
			case "stranger":
				agent = stranger
			case "author":
				agent = f.writerID
			case "digest":
				body = strings.Replace(body, boundAfter, boundBefore, 1)
			case "no_reason":
				body = "Veto: decision:" + f.instance + " " + boundAfter + "\n"
			case "unknown_instance":
				body = strings.Replace(body, f.instance, uuid.NewString(), 1)
			case "reason_too_long":
				body = f.vetoBody(strings.Repeat("x", 4097))
			}
			req := asAgentActor(withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": body}), "id", f.issueID), agent, f.tasks[agent])
			if kind != "no_task_token" {
				req.Header.Set("X-Actor-Source", "task_token")
			}
			w := httptest.NewRecorder()
			testHandler.CreateComment(w, req)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
				t.Fatalf("%s veto status=%d: %s", kind, w.Code, w.Body)
			}
			if n := liveSettlements(t, f); n != 1 {
				t.Fatalf("%s veto changed the record: %d", kind, n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM governed_vote_record WHERE decision_instance_id=$1 AND decision_event='veto'`, f.instance); n != 0 {
				t.Fatalf("%s veto left %d veto events", kind, n)
			}
		})
	}
}

func TestAgentDecisionMemberVeto(t *testing.T) {
	f := prepareVeto(t)
	effective(t, f)
	post := func(content string) int {
		req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": content}), "id", f.issueID)
		w := httptest.NewRecorder()
		testHandler.CreateComment(w, req)
		return w.Code
	}
	// A workspace owner may veto; an ordinary member may not.
	other := dbfx.User(t, "plain member", "plain-"+safeTestName(t)+"@example.com")
	dbfx.Member(t, testWorkspaceID, other, "member")
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": f.vetoBody("not an owner")}), "id", f.issueID)
	req.Header.Set("X-User-ID", other)
	w := httptest.NewRecorder()
	testHandler.CreateComment(w, req)
	if w.Code != http.StatusForbidden && w.Code != http.StatusBadRequest {
		t.Fatalf("plain member veto status=%d", w.Code)
	}
	if liveSettlements(t, f) != 1 {
		t.Fatal("plain member veto changed the record")
	}
	if code := post(f.vetoBody("owner decision")); code != http.StatusCreated {
		t.Fatalf("owner veto status=%d", code)
	}
	if liveSettlements(t, f) != 0 {
		t.Fatal("owner veto did not revoke")
	}
}

func TestAgentDecisionSupersession(t *testing.T) {
	f := prepareVeto(t)
	effective(t, f)
	next := f
	next.instance = uuid.NewString()
	next.scope = map[string]any{}
	for k, v := range f.scope {
		next.scope[k] = v
	}
	next.scope["supersedes"] = f.instance
	raw, _ := json.Marshal(next.scope)
	proposal := "Proposal: decision:" + next.instance + " " + boundBefore + "\n\n```decision\n" + string(raw) + "\n```\n"
	f.post(t, f.writerID, proposal, http.StatusCreated)
	if n := liveSettlements(t, f); n != 0 {
		t.Fatalf("superseded instance still live: %d", n)
	}
	var reason string
	dbfx.QueryRow(t, `SELECT revoke_reason FROM decision_agent_settlement WHERE instance_id=$1`, f.instance).Scan(&reason)
	if !strings.Contains(reason, next.instance) {
		t.Fatalf("revocation does not name the successor: %q", reason)
	}
	// Refused: unknown target, and a target on another issue.
	for name, target := range map[string]string{"unknown": uuid.NewString(), "foreign": f.instance} {
		other := next
		other.instance = uuid.NewString()
		other.scope = map[string]any{}
		for k, v := range f.scope {
			other.scope[k] = v
		}
		other.scope["supersedes"] = target
		if name == "foreign" {
			other.issueID = dbfx.Issue(t, "foreign supersession issue")
		}
		raw, _ := json.Marshal(other.scope)
		line := "Proposal: decision:" + other.instance + " " + boundBefore + "\n\n```decision\n" + string(raw) + "\n```\n"
		if name == "foreign" {
			other.tasks = f.tasks
			dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=$1 WHERE id=$2`, other.issueID, f.tasks[f.writerID])
			t.Cleanup(func() {
				dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=$1 WHERE id=$2`, f.issueID, f.tasks[f.writerID])
			})
		}
		other.post(t, f.writerID, line, http.StatusBadRequest)
	}
}

func TestAgentDecisionAdjudicationSettlement(t *testing.T) {
	for _, verdict := range []string{"PASS", "BLOCK"} {
		t.Run(verdict, func(t *testing.T) {
			enableAgentDecisions(t, true)
			f := newAdjudicationFixture(t)
			cleanDecisionC3Tables(t, f.issueID)
			f.propose(t)
			judge := f.scope["adjudication"].(map[string]any)["adjudicator_agent_id"].(string)
			upholds := map[string]string{"PASS": "author", "BLOCK": "reviewer"}[verdict]
			opinion := strings.Replace(f.opinion(judge, verdict), "\n\nReasoned opinion.", "\nupholds: "+upholds+"\ndisputed_point: screenshot coverage\n\nReasoned opinion.", 1)
			f.commit(t, judge, opinion)
			ctx := context.Background()
			f.reveal(t, judge, verdict, opinion, http.StatusCreated)
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := settleAgentDecision(ctx, tx, parseUUID(testWorkspaceID), f.instance); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			want := 0
			if verdict == "PASS" {
				want = 1
			}
			if n := liveSettlements(t, f); n != want {
				t.Fatalf("adjudicator %s upholding %s: settlements=%d, want %d", verdict, upholds, n, want)
			}
		})
	}
}
