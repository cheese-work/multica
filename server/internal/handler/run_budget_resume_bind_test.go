package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// bindPath is one of the three paths that end a resume on a pending survivor.
type bindPath struct {
	name, trigger string
	squad         bool
	// slot: the survivor holds the assignment-level slot, so it has no comment.
	slot bool
}

var bindPaths = []bindPath{
	{name: "assignment", trigger: "comment", slot: true},
	{name: "mention", trigger: "mention"},
	{name: "squad leader", trigger: "squad leader callback", squad: true},
}

// bindCase is a linked refusal whose comment is C2, on the path's agent, with
// the cap raised so the only thing between it and a run is the survivor.
type bindCase struct {
	c      budgetCase
	id     pgtype.UUID
	agent  string
	squad  pgtype.UUID
	c1, c2 pgtype.UUID
}

func newBindCase(t *testing.T, installationID int64, p bindPath, refusalComment bool) bindCase {
	t.Helper()
	c, id := intentCase(t, installationID)
	b := bindCase{c: c, id: id, agent: c.agentID}
	if p.trigger == "mention" || p.squad {
		b.agent = secondAgent(t)
	}
	comment := func(text string) pgtype.UUID {
		var cid pgtype.UUID
		if err := testPool.QueryRow(context.Background(), `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
			VALUES ($1,$2,'member',$3,$4) RETURNING id`, testWorkspaceID, c.issueID, testUserID, text).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		return cid
	}
	b.c1, b.c2 = comment("first"), comment("second")
	if p.squad {
		if err := testPool.QueryRow(context.Background(), `INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
			VALUES ($1, 'bind-' || gen_random_uuid()::text, '', $2, $3) RETURNING id`, testWorkspaceID, b.agent, testUserID).Scan(&b.squad); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM squad WHERE id=$1`, b.squad) })
		dbfx.Exec(t, `UPDATE issue SET assignee_type='squad', assignee_id=$2 WHERE id=$1`, c.issueID, b.squad)
	}
	var stored any
	if refusalComment {
		stored = b.c2
	}
	dbfx.Exec(t, `UPDATE run_budget_refusal SET agent_id=$2, trigger=$3, trigger_comment_id=$4, actor_user_id=$5 WHERE id=$1`, id, b.agent, p.trigger, stored, testUserID)
	linkRefusal(t, id, resumeRowOf(t, id).Window)
	raiseCap(t, c)
	return b
}

// survivor inserts the pending run of the refusal's agent, triggered by comment.
func (b bindCase) survivor(t *testing.T, p bindPath, status string, comment pgtype.UUID) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	var squad any
	if p.squad {
		squad = b.squad
	}
	if err := testPool.QueryRow(context.Background(), `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, trigger_comment_id, is_leader_task, squad_id)
		SELECT $1, runtime_id, $2, $3, 0, $4, $5, $6 FROM agent WHERE id=$1 RETURNING id`, b.agent, b.c.issueID, status, comment, p.squad, squad).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// taskInput is the comments a run will answer: its trigger and its planned ids.
func taskInput(t *testing.T, task pgtype.UUID) (trigger pgtype.UUID, planned []pgtype.UUID, originator pgtype.UUID) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `SELECT trigger_comment_id, coalesced_comment_ids, originator_user_id FROM agent_task_queue WHERE id=$1`, task).Scan(&trigger, &planned, &originator); err != nil {
		t.Fatal(err)
	}
	return
}

func answers(t *testing.T, task, comment pgtype.UUID) bool {
	t.Helper()
	trigger, planned, _ := taskInput(t, task)
	if trigger == comment {
		return true
	}
	for _, p := range planned {
		if p == comment {
			return true
		}
	}
	return false
}

func issueRuns(t *testing.T, c budgetCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND task_kind IS DISTINCT FROM 'budget_recovery'`, c.issueID)
}

// own is the comment the path's survivor answers before the resume.
func (b bindCase) own(p bindPath) pgtype.UUID {
	if p.slot {
		return pgtype.UUID{}
	}
	return b.c1
}

// A queued run for another comment is not the refused work: the resume folds
// the stored comment into it, in its transaction, and only then ends on it.
func TestRunBudgetResumeQueuedSurvivorOfAnotherCommentTakesTheStoredComment(t *testing.T) {
	for i, p := range bindPaths {
		t.Run(p.name, func(t *testing.T) {
			b := newBindCase(t, int64(2860200+i), p, true)
			dbfx.Exec(t, `UPDATE run_budget_refusal SET taint_ids=ARRAY[$2]::text[] WHERE id=$1`, b.id, joinTaint)
			survivor := b.survivor(t, p, "queued", b.own(p))
			before := issueRuns(t, b.c)
			resume(t, b.id)
			wantResume(t, b.id, "run_queued", "")
			if got := wantTaskOf(t, b.id); got != survivor {
				t.Fatalf("refusal ends on %s, want the survivor %s", uuidToString(got), uuidToString(survivor))
			}
			if !answers(t, survivor, b.c2) {
				t.Fatal("the refusal ended on a run that never takes the refused comment")
			}
			if !p.slot && !answers(t, survivor, b.c1) {
				t.Fatal("the survivor lost its own comment")
			}
			if n := issueRuns(t, b.c); n != before {
				t.Fatalf("runs = %d, want the %d before the resume: no second run", n, before)
			}
			if _, _, originator := taskInput(t, survivor); uuidToString(originator) != testUserID {
				t.Fatalf("survivor originator %s, want the refusal's actor %s", uuidToString(originator), testUserID)
			}
			var taint []string
			if err := testPool.QueryRow(context.Background(), `SELECT taint_ids FROM agent_task_queue WHERE id=$1`, survivor).Scan(&taint); err != nil {
				t.Fatal(err)
			}
			wantTaint(t, taint, joinTaint)
		})
	}
}

// A dispatched run of another thread cannot take planned input: the refusal
// never ends on it without the comment. It stays resume_pending, and once the
// run has started the sweep queues the run for the stored comment.
func TestRunBudgetResumeDispatchedSurvivorOfAnotherThreadLeavesTheRefusalPending(t *testing.T) {
	for i, p := range bindPaths {
		t.Run(p.name, func(t *testing.T) {
			b := newBindCase(t, int64(2860210+i), p, true)
			survivor := b.survivor(t, p, "dispatched", b.own(p))
			resume(t, b.id)
			wantResume(t, b.id, "resume_pending", "")
			if answers(t, survivor, b.c2) {
				t.Fatal("the comment was registered on a run of another thread")
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, survivor)
			testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
			wantResume(t, b.id, "run_queued", "")
			if got := refusedRunOf(t, b.c); got.Comment != b.c2 {
				t.Fatalf("fresh run answers %s, want the stored comment %s", uuidToString(got.Comment), uuidToString(b.c2))
			}
		})
	}
}

// A dispatched run of the stored comment's own thread takes it as planned input.
func TestRunBudgetResumeDispatchedSurvivorOfTheSameThreadRegistersTheStoredComment(t *testing.T) {
	for i, p := range bindPaths {
		if p.slot {
			continue // the assignment slot has no thread
		}
		t.Run(p.name, func(t *testing.T) {
			b := newBindCase(t, int64(2860220+i), p, true)
			dbfx.Exec(t, `UPDATE comment SET parent_id=$2 WHERE id=$1`, b.c2, b.c1)
			survivor := b.survivor(t, p, "dispatched", b.c1)
			before := issueRuns(t, b.c)
			resume(t, b.id)
			wantResume(t, b.id, "run_queued", "")
			if got := wantTaskOf(t, b.id); got != survivor {
				t.Fatalf("refusal ends on %s, want the survivor %s", uuidToString(got), uuidToString(survivor))
			}
			if !answers(t, survivor, b.c2) {
				t.Fatal("the refusal ended on a run that never takes the refused comment")
			}
			if n := issueRuns(t, b.c); n != before {
				t.Fatalf("runs = %d, want the %d before the resume: no second run", n, before)
			}
		})
	}
}

// The survivor already answers the stored comment, or the refusal stored none:
// it is the refused work as it stands and nothing is folded.
func TestRunBudgetResumeSurvivorOfTheSameOrNoCommentBindsUntouched(t *testing.T) {
	for i, p := range bindPaths {
		if !p.slot { // the assignment slot holds no comment to be the same
			t.Run(p.name+"/same comment", func(t *testing.T) {
				b := newBindCase(t, int64(2860230+i), p, true)
				survivor := b.survivor(t, p, "queued", b.c2)
				resume(t, b.id)
				wantResume(t, b.id, "run_queued", "")
				if _, planned, _ := taskInput(t, survivor); len(planned) != 0 {
					t.Fatalf("survivor planned = %v, want none", planned)
				}
			})
		}
		t.Run(p.name+"/no stored comment", func(t *testing.T) {
			b := newBindCase(t, int64(2860240+i), p, false)
			survivor := b.survivor(t, p, "queued", b.own(p))
			resume(t, b.id)
			wantResume(t, b.id, "run_queued", "")
			if trigger, planned, _ := taskInput(t, survivor); trigger != b.own(p) || len(planned) != 0 {
				t.Fatalf("survivor trigger %s planned %v, want untouched", uuidToString(trigger), planned)
			}
		})
	}
}

// A survivor that cannot take the comment (it is on another head) does not end
// the refusal: it stays resume_pending until the survivor finishes, and then the
// sweep queues the run for the stored comment.
func TestRunBudgetResumeSurvivorThatCannotTakeTheCommentLeavesTheRefusalPending(t *testing.T) {
	p := bindPaths[0]
	b := newBindCase(t, 2860250, p, true)
	var pr string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'multica-ai', 'multica', 2860250, 'review PR', 'open', 'https://example.test/pr', now(), now(), 'new-head') RETURNING id`, testWorkspaceID).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, b.c.issueID, pr)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, pr)
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr)
	})
	survivor := b.survivor(t, p, "queued", b.own(p))
	dbfx.Exec(t, `UPDATE agent_task_queue SET context = jsonb_build_object('head_sha', 'old-head') WHERE id=$1`, survivor)
	resume(t, b.id)
	wantResume(t, b.id, "resume_pending", "")
	if answers(t, survivor, b.c2) {
		t.Fatal("the comment was folded into a run on another head")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, survivor)
	testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
	wantResume(t, b.id, "run_queued", "")
	if got := refusedRunOf(t, b.c); got.Comment != b.c2 {
		t.Fatalf("fresh run answers %s, want the stored comment %s", uuidToString(got.Comment), uuidToString(b.c2))
	}
}
