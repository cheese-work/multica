package handler

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Exercise the dormant engine within the real comment transaction. Production
// has no switch to enable this call until the later lifecycle/pilot slices.
func settlementReveal(f agentDecisionFixture, agent, opinion string) error {
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// The HTTP path locks the issue row (LockWakeupIssue) before the instance
	// advisory lock; take them in the same order so races match production.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM issue WHERE id=$1 FOR UPDATE`, f.issueID); err != nil {
		return err
	}
	params := db.CreateCommentParams{ID: parseUUID(uuid.NewString()), WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(f.issueID), AuthorType: "agent", AuthorID: parseUUID(agent), SourceTaskID: parseUUID(f.tasks[agent]), Type: "comment", Content: "Vote: PASS decision:" + f.instance + " " + boundAfter + "\n\n```opinion\n" + opinion + "```\n"}
	annotation, err := prepareAgentDecision(ctx, tx, params, true)
	if err != nil {
		return err
	}
	comment, err := db.New(tx).CreateComment(ctx, params)
	if err != nil {
		return err
	}
	if err := annotateAgentDecision(ctx, tx, comment.ID, annotation); err != nil {
		return err
	}
	if err := settleAgentDecision(ctx, tx, params.WorkspaceID, f.instance); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func prepareSettlement(t *testing.T) agentDecisionFixture {
	t.Helper()
	enableAgentDecisions(t, true)
	f := newAgentDecisionFixture(t)
	// These tables do not exist on the red-run baseline.
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue WHERE id=$1`, f.issueID)
		for _, table := range []string{"decision_agent_reliance", "decision_agent_settlement"} {
			if dbfx.Count(t, `SELECT count(*) FROM pg_class WHERE relname=$1 AND relnamespace='public'::regnamespace`, table) > 0 {
				dbfx.Exec(t, "DELETE FROM "+table+" WHERE issue_id=$1", f.issueID)
			}
		}
	})
	f.propose(t)
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.commit(t, agent, f.opinion(agent, "PASS"))
	}
	return f
}

func TestAgentDecisionSettlementConcurrent(t *testing.T) {
	f := prepareSettlement(t)
	results := make(chan error, 2)
	for _, agent := range []string{f.leaderID, f.peerID} {
		go func(agent string) { results <- settlementReveal(f, agent, f.opinion(agent, "PASS")) }(agent)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	// Assert via pg_class first to make absence a behavioral assertion, not a
	// setup or collection failure in the pre-implementation run.
	if dbfx.Count(t, `SELECT count(*) FROM pg_class WHERE relname='decision_agent_settlement'`) == 0 {
		t.Fatal("both PASS reveals committed but no settlement storage exists")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1`, f.instance); n != 1 {
		t.Fatalf("settlements=%d, want exactly one", n)
	}
}

func TestAgentDecisionSettlementInsertFailureRollsBackVote(t *testing.T) {
	f := prepareSettlement(t)
	if err := settlementReveal(f, f.leaderID, f.opinion(f.leaderID, "PASS")); err != nil {
		t.Fatal(err)
	}
	if dbfx.Count(t, `SELECT count(*) FROM pg_class WHERE relname='decision_agent_settlement'`) == 0 {
		t.Fatal("settlement failure cannot close the gate: storage is missing")
	}
	dbfx.Exec(t, `CREATE FUNCTION fail_c2_settlement() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected settlement failure'; END $$`)
	dbfx.Exec(t, `CREATE TRIGGER fail_c2_settlement BEFORE INSERT ON decision_agent_settlement FOR EACH ROW EXECUTE FUNCTION fail_c2_settlement()`)
	t.Cleanup(func() {
		dbfx.Exec(t, `DROP TRIGGER fail_c2_settlement ON decision_agent_settlement`)
		dbfx.Exec(t, `DROP FUNCTION fail_c2_settlement()`)
	})
	if err := settlementReveal(f, f.peerID, f.opinion(f.peerID, "PASS")); err == nil {
		t.Fatal("failed settlement accepted vote")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM governed_vote_record WHERE decision_instance_id=$1 AND decision_event='vote'`, f.instance); n != 1 {
		t.Fatalf("votes=%d, want first vote only", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatalf("gate has %d settlements after failure", n)
	}
}

func agentDecisionRequest(f agentDecisionFixture, method string) *http.Request {
	path := "/api/issues/" + f.issueID + "/agent-decisions?gate=screenshot_evidence&candidate_sha256=" + boundAfter
	req := asAgentActor(withURLParam(newRequest(method, path, map[string]any{"gate": "screenshot_evidence", "candidate_sha256": boundAfter, "purpose": "gate check"}), "id", f.issueID), f.writerID, f.tasks[f.writerID])
	req.Header.Set("X-Actor-Source", "task_token")
	return req
}

func callAgentDecision(t *testing.T, f agentDecisionFixture, method string) *httptest.ResponseRecorder {
	t.Helper()
	req := agentDecisionRequest(f, method)
	w := httptest.NewRecorder()
	if method == http.MethodGet {
		testHandler.LookupAgentDecision(w, req)
	} else {
		testHandler.RelyAgentDecision(w, req)
	}
	return w
}

func TestAgentDecisionReliance(t *testing.T) {
	f := prepareSettlement(t)
	for _, agent := range []string{f.leaderID, f.peerID} {
		if err := settlementReveal(f, agent, f.opinion(agent, "PASS")); err != nil {
			t.Fatal(err)
		}
	}
	if dbfx.Count(t, `SELECT count(*) FROM pg_class WHERE relname='decision_agent_settlement'`) == 0 {
		t.Fatal("reliance has no effective record storage")
	}
	lookup := callAgentDecision(t, f, http.MethodGet)
	if lookup.Code != 200 || !strings.Contains(lookup.Body.String(), `"authorization":false`) {
		t.Fatalf("lookup=%d %s", lookup.Code, lookup.Body)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_reliance WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatal("lookup registered reliance")
	}
	rely := callAgentDecision(t, f, http.MethodPost)
	if rely.Code != 200 || !strings.Contains(rely.Body.String(), `"decided":true`) {
		t.Fatalf("rely=%d %s", rely.Code, rely.Body)
	}
	var task, agent, purpose string
	dbfx.QueryRow(t, `SELECT task_id::text,agent_id::text,purpose FROM decision_agent_reliance WHERE instance_id=$1`, f.instance).Scan(&task, &agent, &purpose)
	if task != f.tasks[f.writerID] || agent != f.writerID || purpose != "gate check" {
		t.Fatalf("unbound reliance: %s %s %s", task, agent, purpose)
	}
	dbfx.Exec(t, `CREATE FUNCTION fail_c2_reliance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected reliance failure'; END $$`)
	dbfx.Exec(t, `CREATE TRIGGER fail_c2_reliance BEFORE INSERT ON decision_agent_reliance FOR EACH ROW EXECUTE FUNCTION fail_c2_reliance()`)
	t.Cleanup(func() {
		dbfx.Exec(t, `DROP TRIGGER fail_c2_reliance ON decision_agent_reliance`)
		dbfx.Exec(t, `DROP FUNCTION fail_c2_reliance()`)
	})
	rely = callAgentDecision(t, f, http.MethodPost)
	if rely.Code != 200 || !strings.Contains(rely.Body.String(), `"decided":false`) {
		t.Fatalf("insert failure authorized reliance: %s", rely.Body)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_reliance WHERE instance_id=$1`, f.instance); n != 1 {
		t.Fatalf("failed insert left %d rows", n)
	}
}

// Commit faults occur after the reliance insert but before authorization.
type failedRelianceCommit struct{ pgx.Tx }

func (tx failedRelianceCommit) Commit(context.Context) error {
	return errors.New("injected commit failure")
}

type failedRelianceCommitStarter struct{ txStarter }

func (s failedRelianceCommitStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.txStarter.Begin(ctx)
	return failedRelianceCommit{tx}, err
}

func TestAgentDecisionRelianceCommitFailure(t *testing.T) {
	f := prepareSettlement(t)
	dbfx.Exec(t, `INSERT INTO decision_agent_settlement(instance_id,workspace_id,issue_id) VALUES($1,$2,$3)`, f.instance, testWorkspaceID, f.issueID)
	h := *testHandler
	h.TxStarter = failedRelianceCommitStarter{h.TxStarter}
	w := httptest.NewRecorder()
	h.RelyAgentDecision(w, agentDecisionRequest(f, http.MethodPost))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"decided":false`) {
		t.Fatalf("commit failure authorized: %s", w.Body)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_reliance WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatalf("commit failure left %d rows", n)
	}
}

func TestAgentDecisionRelianceRefusals(t *testing.T) {
	for _, kind := range []string{"missing", "revoked", "future", "digest", "gate", "issue", "task", "agent", "human", "purpose"} {
		t.Run(kind, func(t *testing.T) {
			f := prepareSettlement(t)
			if kind != "missing" {
				dbfx.Exec(t, `INSERT INTO decision_agent_settlement(instance_id,workspace_id,issue_id,effective_at,revoked_at)
    VALUES($1,$2,$3,CASE WHEN $4='future' THEN now()+interval '1 hour' ELSE now() END,CASE WHEN $4='revoked' THEN now() ELSE NULL END)`, f.instance, testWorkspaceID, f.issueID, kind)
			}
			req := agentDecisionRequest(f, http.MethodPost)
			switch kind {
			case "digest", "gate", "purpose":
				binding := map[string]any{"gate": "screenshot_evidence", "candidate_sha256": boundAfter, "purpose": "gate check"}
				switch kind {
				case "digest":
					binding["candidate_sha256"] = boundBefore
				case "gate":
					binding["gate"] = "security"
				case "purpose":
					binding["purpose"] = ""
				}
				req = asAgentActor(withURLParam(newRequest(http.MethodPost, "/", binding), "id", f.issueID), f.writerID, f.tasks[f.writerID])
				req.Header.Set("X-Actor-Source", "task_token")
			case "issue":
				req = withURLParam(req, "id", dbfx.Issue(t, "Different issue"))
			case "task":
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, f.tasks[f.writerID])
			case "agent":
				req.Header.Set("X-Agent-ID", f.peerID)
			case "human":
				req.Header.Del("X-Actor-Source")
			}
			w := httptest.NewRecorder()
			testHandler.RelyAgentDecision(w, req)
			if strings.Contains(w.Body.String(), `"decided":true`) {
				t.Fatalf("%s authorized: %s", kind, w.Body)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_reliance WHERE instance_id=$1`, f.instance); n != 0 {
				t.Fatalf("%s registered %d reliance rows", kind, n)
			}
		})
	}
}

func TestAgentDecisionRelianceUsesInstanceLock(t *testing.T) {
	f := prepareSettlement(t)
	dbfx.Exec(t, `INSERT INTO decision_agent_settlement(instance_id,workspace_id,issue_id) VALUES($1,$2,$3)`, f.instance, testWorkspaceID, f.issueID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	blocker, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if err := lockAgentDecision(ctx, blocker, f.instance); err != nil {
		t.Fatal(err)
	}
	// A short deadline under the existing instance lock must refuse and leave no
	// reliance row. This proves the consumer uses the comment/veto lock key.
	req := agentDecisionRequest(f, http.MethodPost)
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(f.issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = testHandler.relyAgentDecision(req.WithContext(short), issue, agentDecisionBinding{Gate: "screenshot_evidence", CandidateSHA256: boundAfter, Purpose: "gate check"})
	if err == nil {
		t.Fatal("reliance bypassed the held instance lock")
	}

	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_reliance WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatal("reliance bypassed the instance lock")
	}
}
