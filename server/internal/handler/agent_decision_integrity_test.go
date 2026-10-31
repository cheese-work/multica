package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAgentDecisionImmutableProvenance(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	opinion := f.opinion(f.leaderID, "PASS")
	f.commit(t, f.leaderID, opinion)
	var id, body, task string
	dbfx.QueryRow(t, `SELECT comment_id::text,full_body,source_task_id::text FROM governed_vote_record WHERE decision_instance_id=$1 AND decision_event='commit'`, f.instance).Scan(&id, &body, &task)
	if task != f.tasks[f.leaderID] || !strings.HasPrefix(body, "Commit: decision:"+f.instance) {
		t.Fatal("lost casting-task snapshot")
	}
	dbfx.Exec(t, `UPDATE comment SET content='edited away' WHERE id=$1`, id)
	var snapshot string
	dbfx.QueryRow(t, `SELECT full_body FROM governed_vote_record WHERE comment_id=$1`, id).Scan(&snapshot)
	if snapshot != body {
		t.Fatal("comment edit changed immutable commitment")
	}
	for _, sql := range []string{
		`UPDATE decision_agent_scope SET waived_gate='security' WHERE instance_id=$1`,
		`UPDATE decision_request SET authority='human' WHERE id=$1`,
		`DELETE FROM decision_request WHERE id=$1`,
		`DELETE FROM decision_agent_scope WHERE instance_id=$1`,
		`UPDATE governed_vote_record SET full_body='forged' WHERE decision_instance_id=$1`,
		`UPDATE governed_vote_record SET decision_event='vote',decision_verdict='PASS' WHERE decision_instance_id=$1`,
		`DELETE FROM governed_vote_record WHERE decision_instance_id=$1`,
	} {
		_, err := testPool.Exec(context.Background(), sql, f.instance)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("immutable operation %s: %v", sql, err)
		}
	}
}

func TestAgentDecisionEveryInsertLocksInstance(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	a, b := f.opinion(f.leaderID, "PASS"), f.opinion(f.peerID, "PASS")
	f.commit(t, f.leaderID, a)
	f.commit(t, f.peerID, b)
	for _, line := range []string{
		"Proposal: decision:" + f.instance + " " + boundAfter,
		"Commit: decision:" + f.instance + " " + boundAfter + " " + boundBefore,
		"Vote: PASS decision:" + f.instance + " " + boundAfter,
		"Vote: BLOCK decision:" + f.instance + " " + boundAfter,
		"Retract: decision:" + f.instance,
		"Veto: decision:" + f.instance,
	} {
		t.Run(strings.Fields(line)[0]+strings.Fields(line)[1], func(t *testing.T) {
			ctx := context.Background()
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, err = db.New(tx).CreateComment(ctx, db.CreateCommentParams{ID: parseUUID(uuid.NewString()), IssueID: parseUUID(f.issueID), WorkspaceID: parseUUID(testWorkspaceID), AuthorType: "agent", AuthorID: parseUUID(f.leaderID), SourceTaskID: parseUUID(f.tasks[f.leaderID]), Content: line, Type: "comment"})
			if err != nil {
				t.Fatal(err)
			}
			probe, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Rollback(ctx)
			var available bool
			err = probe.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('agent-decision:' || $1,0))`, f.instance).Scan(&available)
			if err != nil || available {
				t.Fatalf("insert did not hold instance lock: %v / %v", available, err)
			}
		})
	}
}

func TestAgentDecisionRejectsUntrustedAndMalformedActions(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	for _, body := range []string{
		"Commit: decision:bad " + boundAfter + " " + boundBefore,
		"Commit: decision:" + f.instance + " " + boundAfter + " " + boundBefore + "\nleaked opinion",
		"Vote: PASS decision:" + f.instance + " " + boundAfter,
		"Veto: decision:" + f.instance + " reason owner",
		"Retract: decision:" + f.instance,
	} {
		f.post(t, f.leaderID, body, http.StatusBadRequest)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, f.tasks[f.leaderID])
	f.post(t, f.leaderID, "Commit: decision:"+f.instance+" "+boundAfter+" "+boundBefore, http.StatusBadRequest)
}

func newAdjudicationFixture(t *testing.T) agentDecisionFixture {
	t.Helper()
	f := newAgentDecisionFixture(t)
	t.Setenv("MULTICA_VOTE_GATE_LABS", `{"claude/claude-test":"anthropic","codex/gpt-test":"openai","gemini/gemini-test":"google"}`)
	rt := dbfx.Runtime(t, "adjudicator-runtime-"+safeTestName(t), testutil.Cols{"provider": "gemini"})
	judge := createHandlerTestAgent(t, "adjudicator-"+safeTestName(t), nil)
	dbfx.Exec(t, `UPDATE agent SET runtime_id=$1,model='gemini-test' WHERE id=$2`, rt, judge)
	task := createHandlerTestTaskForAgentOnIssue(t, judge, f.issueID)
	f.tasks[judge] = task
	f.retask(t, judge, rt, "gemini-test")
	point := "screenshot coverage"
	review := "Verdict: BLOCK\nGate: screenshot_evidence\nDisputed-point: " + point + "\nBlock-category: reversible_gate\nScreenshots are required for this gate.\n"
	rebuttal := "Rebuttal: screenshots add no evidence here\nGate: screenshot_evidence\nDisputed-point: " + point + "\n"
	var reviewer, author map[string]any
	if err := json.Unmarshal(f.post(t, f.leaderID, review, http.StatusCreated).Body.Bytes(), &reviewer); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(f.post(t, f.writerID, rebuttal, http.StatusCreated).Body.Bytes(), &author); err != nil {
		t.Fatal(err)
	}
	delete(f.scope, "signers")
	f.scope["route"] = "adjudicate"
	f.scope["adjudication"] = map[string]any{"reviewer_comment_id": reviewer["id"], "reviewer_task_id": f.tasks[f.leaderID], "reviewer_verdict": review,
		"author_comment_id": author["id"], "author_task_id": f.taskID, "author_rebuttal": rebuttal, "point": point, "adjudicator_agent_id": judge}
	return f
}

func TestAgentDecisionAdjudicationBindsThirdLabAndPoint(t *testing.T) {
	f := newAdjudicationFixture(t)
	f.propose(t)
	dispute := f.scope["adjudication"].(map[string]any)
	judge := dispute["adjudicator_agent_id"].(string)
	opinion := strings.Replace(f.opinion(judge, "PASS"), "\n\nReasoned opinion.", "\nupholds: author\ndisputed_point: screenshot coverage\n\nReasoned opinion.", 1)
	f.commit(t, judge, opinion)
	f.reveal(t, judge, "PASS", opinion, http.StatusCreated)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_decision_live_vote WHERE instance_id=$1 AND verdict='PASS'`, f.instance); n != 1 {
		t.Fatalf("adjudicator vote count %d", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_answer WHERE decision_request_id=$1`, f.instance); n != 0 {
		t.Fatal("adjudication invented human authority")
	}
}

func TestAgentDecisionAdjudicationRefusals(t *testing.T) {
	for _, kind := range []string{"not_third_lab", "forged_review", "wrong_author", "wrong_point", "missing_evidence", "cosigner_split"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdjudicationFixture(t)
			d := f.scope["adjudication"].(map[string]any)
			switch kind {
			case "not_third_lab":
				d["adjudicator_agent_id"] = f.peerID
			case "forged_review":
				d["reviewer_verdict"] = "forged signed verdict"
			case "wrong_author":
				d["author_task_id"] = f.tasks[f.peerID]
			case "wrong_point":
				d["point"] = "different point"
			case "missing_evidence":
				f.scope["evidence"] = []any{}
			case "cosigner_split":
				d["reviewer_verdict"] = "Vote: BLOCK decision:" + uuid.NewString() + " " + boundAfter
			}
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
	}
}
