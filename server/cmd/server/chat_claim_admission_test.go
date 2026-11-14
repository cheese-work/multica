package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2894 step 8: atomic admission, claim half. At most one task per DM is
// active whatever its agent or runtime (index from step 7); the claim
// predicate refuses the second one without an error, and ONLY the DM
// alternative ignores the agent. Issue and quick-create serialization stay
// per agent.

type admissionAgent struct{ agentID, runtimeID string }

// newAdmissionAgent is a dedicated agent on its own online runtime, so a claim
// never picks up another test's leftover tasks.
func newAdmissionAgent(t *testing.T, label string) admissionAgent {
	t.Helper()
	var a admissionAgent
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
		VALUES ($1, NULL, $2, 'local', 'che2911_admission', 'online', '{}'::jsonb, '{}'::jsonb, now())
		RETURNING id::text`, testWorkspaceID, "admission runtime "+label).Scan(&a.runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'local', '{}'::jsonb, $3, 'workspace', 20, $4)
		RETURNING id::text`, testWorkspaceID, "che2911-admission-"+label, a.runtimeID, testUserID).Scan(&a.agentID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, a.agentID)
		testPool.Exec(ctx(), `DELETE FROM agent WHERE id = $1`, a.agentID)
		testPool.Exec(ctx(), `DELETE FROM agent_runtime WHERE id = $1`, a.runtimeID)
	})
	return a
}

func newAdmissionDM(t *testing.T, owner admissionAgent) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, session_id, runtime_id, work_dir)
		VALUES ($1, $2, $3, 'admission', 'S_adm', $4, '/tmp/admission') RETURNING id::text`,
		testWorkspaceID, owner.agentID, testUserID, owner.runtimeID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, id)
		testPool.Exec(ctx(), `DELETE FROM chat_session WHERE id = $1`, id)
	})
	return id
}

// addTask inserts a task for agent a. Exactly one of dm / issue may be set;
// both empty makes a quick-create-shaped task.
func addAdmissionTask(t *testing.T, a admissionAgent, dm, issue, status string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, issue_id, status)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5) RETURNING id::text`,
		a.agentID, a.runtimeID, dm, issue, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func taskStatus(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(ctx(), `SELECT status FROM agent_task_queue WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func admissionService() *service.TaskService {
	return service.NewTaskService(db.New(testPool), testPool, nil, events.New())
}

func claimRuntime(t *testing.T, svc *service.TaskService, a admissionAgent) (string, error) {
	t.Helper()
	got, err := svc.ClaimTaskForRuntime(ctx(), ruleEUUID(t, a.runtimeID))
	if err != nil || got == nil {
		return "", err
	}
	return util.UUIDToString(got.ID), nil
}

func newAdmissionIssue(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, number)
		VALUES ($1, 'admission', 'member', $2, (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id::text`, testWorkspaceID, testUserID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(ctx(), `DELETE FROM issue WHERE id = $1`, id) })
	return id
}

// A second agent's DM task waits behind an active DM task: the poll returns
// no task and no error, the candidate stays queued, and the next poll after
// the first settles claims it.
func TestClaimDMTaskOfOtherAgentWaitsWithoutError(t *testing.T) {
	svc := admissionService()
	r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
	dm := newAdmissionDM(t, r1)
	first := addAdmissionTask(t, r1, dm, "", "running")
	compaction := addAdmissionTask(t, r2, dm, "", "queued")
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET purpose = 'compaction' WHERE id = $1`, compaction); err != nil {
		t.Fatal(err)
	}

	got, err := claimRuntime(t, svc, r2)
	if err != nil || got != "" {
		t.Fatalf("claim beside an active DM task = %q, %v; want no task, no error", got, err)
	}
	if s := taskStatus(t, compaction); s != "queued" {
		t.Fatalf("waiting task status = %s, want queued", s)
	}

	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if got, err := claimRuntime(t, svc, r2); err != nil || got != compaction {
		t.Fatalf("claim after settle = %q, %v; want %s", got, err, compaction)
	}
}

// Two claimers on different runtimes start together: exactly one DM task
// becomes dispatched, nobody sees an error (the loser hits the unique index),
// and the loser is claimed after the winner settles.
func TestClaimBarrierTwoAgentsOneDMExactlyOneActive(t *testing.T) {
	svc := admissionService()
	for i := 0; i < 25; i++ {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
			dm := newAdmissionDM(t, r1)
			t1 := addAdmissionTask(t, r1, dm, "", "queued")
			t2 := addAdmissionTask(t, r2, dm, "", "queued")

			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			got := make([]string, 2)
			for i, a := range []admissionAgent{r1, r2} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					got[i], errs[i] = claimRuntime(t, svc, a)
				}()
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("claimer %d error: %v", i, err)
				}
			}
			var active int
			if err := testPool.QueryRow(ctx(), `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'dispatched'`, dm).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != 1 {
				t.Fatalf("active tasks on the DM = %d, want exactly 1 (t1=%s t2=%s)", active, taskStatus(t, t1), taskStatus(t, t2))
			}

			// Settle the winner; the other is claimed on its next poll.
			winner, loser, loserAgent := t1, t2, r2
			if got[1] != "" {
				winner, loser, loserAgent = t2, t1, r1
			}
			if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, winner); err != nil {
				t.Fatal(err)
			}
			if g, err := claimRuntime(t, svc, loserAgent); err != nil || g != loser {
				t.Fatalf("successor claim = %q, %v; want %s", g, err, loser)
			}
		})
	}
}

// Deterministic form of the race: the first claim is held uncommitted while
// the second runs, so the second really waits on the unique index and then
// fails with 23505. The service maps that to "no task this poll".
func TestClaimUniqueViolationMapsToNoTaskAndKeepsCandidateQueued(t *testing.T) {
	svc := admissionService()
	r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
	dm := newAdmissionDM(t, r1)
	addAdmissionTask(t, r1, dm, "", "queued")
	loser := addAdmissionTask(t, r2, dm, "", "queued")

	tx, err := testPool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx())
	if _, err := tx.Exec(ctx(), `UPDATE agent_task_queue SET status = 'dispatched' WHERE agent_id = $1 AND chat_session_id = $2`, r1.agentID, dm); err != nil {
		t.Fatal(err)
	}

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := claimRuntime(t, svc, r2)
		done <- result{id, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("claim returned while the other claimer was uncommitted: %+v", r)
	case <-time.After(500 * time.Millisecond): // the claim is waiting on the index
	}
	if err := tx.Commit(ctx()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.id != "" {
			t.Fatalf("claim after the other committed = %q, %v; want no task, no error", r.id, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("claim did not return")
	}
	if s := taskStatus(t, loser); s != "queued" {
		t.Fatalf("loser status = %s, want queued", s)
	}
}

// A retry child of a compaction task, queued while another agent's DM task is
// active, waits like any other DM task.
func TestClaimRetryChildWaitsBehindOtherAgentsDMTask(t *testing.T) {
	svc := admissionService()
	r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
	dm := newAdmissionDM(t, r1)
	addAdmissionTask(t, r1, dm, "", "running")
	parent := addAdmissionTask(t, r2, dm, "", "failed")
	var child string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, parent_task_id, purpose, attempt)
		SELECT agent_id, runtime_id, chat_session_id, 'queued', id, 'compaction', 2 FROM agent_task_queue WHERE id = $1
		RETURNING id::text`, parent).Scan(&child); err != nil {
		t.Fatal(err)
	}
	if got, err := claimRuntime(t, svc, r2); err != nil || got != "" {
		t.Fatalf("retry child claim = %q, %v; want no task, no error", got, err)
	}
	if s := taskStatus(t, child); s != "queued" {
		t.Fatalf("child status = %s, want queued", s)
	}
}

// Stale-dispatch reclaim keeps the same row dispatched (no second active row)
// while another agent's task for the DM is queued, and that task stays back.
func TestStaleDispatchReclaimKeepsOneActiveWhileOtherAgentQueued(t *testing.T) {
	svc := admissionService()
	r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
	dm := newAdmissionDM(t, r1)
	stale := addAdmissionTask(t, r1, dm, "", "dispatched")
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET dispatched_at = now() - interval '1 hour', prepare_lease_expires_at = NULL WHERE id = $1`, stale); err != nil {
		t.Fatal(err)
	}
	waiting := addAdmissionTask(t, r2, dm, "", "queued")

	if got, err := claimRuntime(t, svc, r1); err != nil || got != stale {
		t.Fatalf("reclaim = %q, %v; want %s", got, err, stale)
	}
	if got, err := claimRuntime(t, svc, r2); err != nil || got != "" {
		t.Fatalf("other agent claim = %q, %v; want no task, no error", got, err)
	}
	if s := taskStatus(t, waiting); s != "queued" {
		t.Fatalf("waiting status = %s, want queued", s)
	}
}

// Regression: only the DM alternative ignores the agent.
func TestClaimTwoAgentsOnOneIssueBothActive(t *testing.T) {
	svc := admissionService()
	r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
	issue := newAdmissionIssue(t)
	addAdmissionTask(t, r1, "", issue, "queued")
	addAdmissionTask(t, r2, "", issue, "queued")
	for _, a := range []admissionAgent{r1, r2} {
		if got, err := claimRuntime(t, svc, a); err != nil || got == "" {
			t.Fatalf("issue task of %s = %q, %v; want claimed", a.agentID, got, err)
		}
	}
}

func TestClaimTwoAgentsQuickCreateBothActive(t *testing.T) {
	svc := admissionService()
	r1, r2 := newAdmissionAgent(t, "r1"), newAdmissionAgent(t, "r2")
	addAdmissionTask(t, r1, "", "", "queued")
	addAdmissionTask(t, r2, "", "", "queued")
	for _, a := range []admissionAgent{r1, r2} {
		if got, err := claimRuntime(t, svc, a); err != nil || got == "" {
			t.Fatalf("quick-create task of %s = %q, %v; want claimed", a.agentID, got, err)
		}
	}
}

func TestClaimSameAgentIssueAndQuickCreateStillSerialize(t *testing.T) {
	svc := admissionService()
	a := newAdmissionAgent(t, "r1")
	issue := newAdmissionIssue(t)
	addAdmissionTask(t, a, "", issue, "running")
	secondIssue := addAdmissionTask(t, a, "", issue, "queued")
	addAdmissionTask(t, a, "", "", "running")
	secondQuick := addAdmissionTask(t, a, "", "", "queued")
	if got, err := claimRuntime(t, svc, a); err != nil || got != "" {
		t.Fatalf("same-agent second tasks claim = %q, %v; want none", got, err)
	}
	for _, id := range []string{secondIssue, secondQuick} {
		if s := taskStatus(t, id); s != "queued" {
			t.Fatalf("task %s status = %s, want queued", id, s)
		}
	}
}

// Regression: a single agent's own DM serialization is unchanged.
func TestClaimSingleAgentDMStillSerializes(t *testing.T) {
	svc := admissionService()
	a := newAdmissionAgent(t, "r1")
	dm := newAdmissionDM(t, a)
	first := addAdmissionTask(t, a, dm, "", "running")
	second := addAdmissionTask(t, a, dm, "", "queued")
	if got, err := claimRuntime(t, svc, a); err != nil || got != "" {
		t.Fatalf("claim behind own running task = %q, %v; want none", got, err)
	}
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if got, err := claimRuntime(t, svc, a); err != nil || got != second {
		t.Fatalf("claim after settle = %q, %v; want %s", got, err, second)
	}
}
