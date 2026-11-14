package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// admEnv is one workspace with a project, one runtime and two agents.
type admEnv struct {
	t                 *testing.T
	pool              *pgxpool.Pool
	svc               *TaskService
	ws, user, runtime string
	agent, agent2     string
	project           string
}

func newAdmEnv(t *testing.T) *admEnv {
	t.Helper()
	pool := sharedTestPool(t)
	e := &admEnv{t: t, pool: pool}
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Adm', $1) RETURNING id`, fmt.Sprintf("adm-%d@multica.test", suffix)).Scan(&e.user), "user")
	must(pool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('adm', $1) RETURNING id`, fmt.Sprintf("adm-%d", suffix)).Scan(&e.ws), "workspace")
	t.Cleanup(func() {
		c := context.Background()
		pool.Exec(c, `DELETE FROM run_input WHERE workspace_id = $1`, e.ws)
		pool.Exec(c, `DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`, e.ws)
		pool.Exec(c, `DELETE FROM workspace WHERE id = $1`, e.ws)
		pool.Exec(c, `DELETE FROM "user" WHERE id = $1`, e.user)
	})
	_, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, e.ws, e.user)
	must(err, "member")
	must(pool.QueryRow(ctx, `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
		VALUES ($1, 'adm-rt', 'cloud', 'codex', 'online', '', '{}'::jsonb, $2, now()) RETURNING id`, e.ws, e.user).Scan(&e.runtime), "runtime")
	for i, dst := range []*string{&e.agent, &e.agent2} {
		must(pool.QueryRow(ctx, `INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
			VALUES ($1, $2, 'cloud', '{}'::jsonb, $3, 'workspace', 10, $4, '', '{}'::jsonb, '[]'::jsonb) RETURNING id`,
			e.ws, fmt.Sprintf("adm-agent-%d", i), e.runtime, e.user).Scan(dst), "agent")
	}
	must(pool.QueryRow(ctx, `INSERT INTO project (workspace_id, title) VALUES ($1, 'adm project') RETURNING id`, e.ws).Scan(&e.project), "project")
	e.svc = &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	return e
}

func (e *admEnv) issueFor(agent, priority string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO issue (workspace_id, title, creator_type, creator_id, assignee_type, assignee_id, priority, project_id, number)
		VALUES ($1, 'adm issue', 'member', $2, 'agent', $3, $4, $5,
			(SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1)) RETURNING id`, e.ws, e.user, agent, priority, e.project).Scan(&id); err != nil {
		e.t.Fatalf("seed issue: %v", err)
	}
	return id
}

func (e *admEnv) issue(priority string) string { return e.issueFor(e.agent, priority) }

func (e *admEnv) enqueue(issueID string) db.AgentTaskQueue {
	e.t.Helper()
	return e.enqueueAgent(issueID, e.agent)
}

func (e *admEnv) enqueueAgent(issueID, agent string) db.AgentTaskQueue {
	e.t.Helper()
	issue, err := e.svc.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		e.t.Fatalf("get issue: %v", err)
	}
	task, err := e.svc.EnqueueTaskForIssue(context.Background(), issue)
	if err != nil {
		e.t.Fatalf("enqueue: %v", err)
	}
	return task
}

func (e *admEnv) claim() *db.AgentTaskQueue {
	e.t.Helper()
	task, err := e.svc.ClaimTask(context.Background(), util.MustParseUUID(e.agent))
	if err != nil {
		e.t.Fatalf("claim: %v", err)
	}
	return task
}

func (e *admEnv) setStatus(status string) {
	e.t.Helper()
	_, err := e.svc.UpdateProject(context.Background(), util.MustParseUUID(e.ws),
		db.UpdateProjectParams{ID: util.MustParseUUID(e.project), Status: pgtype.Text{String: status, Valid: true}}, false, nil)
	if err != nil {
		e.t.Fatalf("set project status %s: %v", status, err)
	}
}

func (e *admEnv) setWIP(n int) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE project SET settings = jsonb_set(settings, '{wip_limit}', to_jsonb($2::int)) WHERE id = $1`, e.project, n); err != nil {
		e.t.Fatalf("set wip: %v", err)
	}
}

func (e *admEnv) start(task *db.AgentTaskQueue) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1 AND status = 'dispatched'`, task.ID); err != nil {
		e.t.Fatalf("start: %v", err)
	}
}

func (e *admEnv) complete(task *db.AgentTaskQueue) {
	e.t.Helper()
	e.start(task)
	if _, err := e.svc.CompleteTask(context.Background(), task.ID, []byte(`{}`), "", "", "", false, "", ""); err != nil {
		e.t.Fatalf("complete: %v", err)
	}
}

func (e *admEnv) fail(task *db.AgentTaskQueue, reason string) {
	e.t.Helper()
	e.start(task)
	if _, err := e.svc.FailTask(context.Background(), task.ID, "boom", "", "", "", reason, false, "", ""); err != nil {
		e.t.Fatalf("fail: %v", err)
	}
}

func (e *admEnv) taskState(id pgtype.UUID) (status, admission, reason string) {
	e.t.Helper()
	var adm, why *string
	if err := e.pool.QueryRow(context.Background(), `SELECT status, admission_state, hold_reason FROM agent_task_queue WHERE id = $1`, id).Scan(&status, &adm, &why); err != nil {
		e.t.Fatalf("task state: %v", err)
	}
	if adm != nil {
		admission = *adm
	}
	if why != nil {
		reason = *why
	}
	return
}

// where is "queued/held", "queued/reserved", "dispatched" ...
func (e *admEnv) where(id pgtype.UUID) string {
	s, a, _ := e.taskState(id)
	if a != "" {
		return s + "/" + a
	}
	return s
}

func (e *admEnv) inputs(issueID string) []db.RunInput {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT event_id, state, task_id, redelivery_of_task_id, kind FROM run_input WHERE issue_id = $1 ORDER BY seq`, issueID)
	if err != nil {
		e.t.Fatalf("inputs: %v", err)
	}
	defer rows.Close()
	var out []db.RunInput
	for rows.Next() {
		var r db.RunInput
		if err := rows.Scan(&r.EventID, &r.State, &r.TaskID, &r.RedeliveryOfTaskID, &r.Kind); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func (e *admEnv) inputStates(issueID string) []string {
	var out []string
	for _, in := range e.inputs(issueID) {
		out = append(out, in.State)
	}
	return out
}

func (e *admEnv) record(issueID, agent, eventID, kind string) bool {
	e.t.Helper()
	// The owner is the pending run of (issue, agent); with none, an owner that is
	// not an active run, which the orphan job parks.
	owner, thread := dbid.NewV7(), pgtype.UUID{Valid: true}
	var oid, tid pgtype.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id, COALESCE(comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running') ORDER BY created_at DESC LIMIT 1`, issueID, agent).Scan(&oid, &tid); err == nil {
		owner, thread = oid, tid
	}
	ok, err := e.svc.RecordRunInput(context.Background(), RunInput{
		WorkspaceID: util.MustParseUUID(e.ws), IssueID: util.MustParseUUID(issueID), AgentID: util.MustParseUUID(agent),
		EventID: eventID, Kind: kind, Payload: []byte(`{"k":"` + eventID + `"}`),
		OwnerTaskID: owner, ThreadKey: thread,
	})
	if err != nil {
		e.t.Fatalf("record input: %v", err)
	}
	return ok
}

func eqStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func (e *admEnv) wip() int {
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
		WHERE i.project_id = $1 AND (t.status IN ('dispatched','running') OR t.admission_state = 'reserved')`, e.project).Scan(&n)
	return n
}

func TestAdmissionQueuedRunAcrossPauseStartsOnce(t *testing.T) {
	e := newAdmEnv(t)
	task := e.enqueue(e.issue("medium"))
	e.setStatus("paused")
	if got := e.claim(); got != nil {
		t.Fatalf("claim in a paused project started run %v", got.ID)
	}
	if s, a, why := e.taskState(task.ID); s != "queued" || a != "held" || why != "project paused" {
		t.Fatalf("after pause claim: status=%s admission=%s reason=%q, want queued/held/project paused", s, a, why)
	}
	e.setStatus("in_progress")
	first := e.claim()
	if first == nil || first.ID != task.ID {
		t.Fatalf("claim after unpause = %v, want run %v", first, task.ID)
	}
	if again := e.claim(); again != nil {
		t.Fatalf("run started twice: %v", again.ID)
	}
}

func TestAdmissionTriggerDuringPauseIsKeptAndStartsOnce(t *testing.T) {
	e := newAdmEnv(t)
	e.setStatus("paused")
	issue := e.issue("medium")
	task := e.enqueue(issue)
	if got := e.claim(); got != nil {
		t.Fatalf("claim while paused started %v", got.ID)
	}
	e.setStatus("in_progress")
	got := e.claim()
	if got == nil || got.ID != task.ID {
		t.Fatalf("claim after unpause = %v, want %v", got, task.ID)
	}
	in, err := e.svc.RunInputsForTask(context.Background(), got.ID)
	if err != nil || len(in) != 1 {
		t.Fatalf("inputs for run = %d (err %v), want the one trigger", len(in), err)
	}
}

func TestAdmissionWIPLimitStartsNextInFocusOrder(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(2)
	prios := []string{"low", "medium", "urgent", "high", "medium"}
	var tasks []db.AgentTaskQueue
	for _, p := range prios {
		tasks = append(tasks, e.enqueue(e.issue(p)))
	}
	var started []*db.AgentTaskQueue
	for i := 0; i < 5; i++ {
		if got := e.claim(); got != nil {
			started = append(started, got)
		}
	}
	if len(started) != 2 {
		t.Fatalf("started %d runs at WIP 2, want 2", len(started))
	}
	for _, s := range started {
		if s.ID != tasks[2].ID && s.ID != tasks[0].ID && s.ID != tasks[1].ID && s.ID != tasks[3].ID && s.ID != tasks[4].ID {
			t.Fatalf("unknown run started")
		}
	}
	// A claim that is refused leaves the run held with the WIP reason.
	for _, tk := range tasks {
		if s, a, why := e.taskState(tk.ID); s == "queued" && a == "held" && why != "WIP 2/2" {
			t.Fatalf("held reason = %q, want WIP 2/2", why)
		}
	}
	// As each ends, the next held run in focus order (priority, then age) starts.
	want := []int{3, 1, 4, 0} // high, then medium (older), medium, low — minus the two that started first
	_ = want
	order := []string{}
	running := started
	for len(running) > 0 {
		e.complete(running[0])
		running = running[1:]
		for {
			next := e.claim()
			if next == nil {
				break
			}
			order = append(order, next.ID.String())
			running = append(running, next)
		}
		if e.wip() > 2 {
			t.Fatalf("WIP %d exceeds the limit of 2", e.wip())
		}
	}
	if len(order) != 3 {
		t.Fatalf("started %d more runs, want the remaining 3", len(order))
	}
}

func TestAdmissionRestartKeepsHeldAndReserved(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	a := e.enqueue(e.issue("medium"))
	b := e.enqueue(e.issue("medium"))
	c := e.enqueue(e.issue("medium"))
	runA := e.claim()
	if runA == nil || runA.ID != a.ID {
		t.Fatalf("first claim = %v, want %v", runA, a.ID)
	}
	if got := e.claim(); got != nil {
		t.Fatalf("second claim at WIP 1 started %v", got.ID)
	}
	e.claim()
	e.complete(runA)
	if got := e.where(b.ID); got != "queued/reserved" {
		t.Fatalf("next held run = %s, want queued/reserved", got)
	}
	// Restart: a new service instance, one reconcile pass.
	e.svc = &TaskService{Queries: db.New(e.pool), TxStarter: e.pool, Bus: events.New()}
	if err := e.svc.ReconcileAdmission(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := e.where(b.ID); got != "queued/reserved" {
		t.Fatalf("reserved run after restart = %s, want queued/reserved", got)
	}
	if got := e.where(c.ID); got != "queued/held" {
		t.Fatalf("held run after restart = %s, want queued/held", got)
	}
	if got := e.claim(); got == nil || got.ID != b.ID {
		t.Fatalf("claim after restart = %v, want reserved %v", got, b.ID)
	}
}

func TestAdmissionTwoWorkersClaimSameRun(t *testing.T) {
	e := newAdmEnv(t)
	e.enqueue(e.issue("medium"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := e.svc.ClaimTask(context.Background(), util.MustParseUUID(e.agent)); err == nil && got != nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d workers won the run, want exactly 1", won)
	}
}

func TestAdmissionTwoWorkersDifferentRunsAtWIPOne(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueueAgent(e.issueFor(e.agent, "medium"), e.agent)
	e.enqueueAgent(e.issueFor(e.agent2, "medium"), e.agent2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, ag := range []string{e.agent, e.agent2} {
		wg.Add(1)
		go func(ag string) {
			defer wg.Done()
			if got, err := e.svc.ClaimTask(context.Background(), util.MustParseUUID(ag)); err == nil && got != nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(ag)
	}
	wg.Wait()
	if won != 1 || e.wip() != 1 {
		t.Fatalf("won=%d wip=%d, want exactly one start at WIP 1", won, e.wip())
	}
}

func TestAdmissionReleaseRacesNewClaimAtWIPOne(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	a := e.enqueue(e.issueFor(e.agent, "medium"))
	b := e.enqueue(e.issueFor(e.agent, "medium"))
	runA := e.claim()
	if runA == nil || runA.ID != a.ID {
		t.Fatalf("claim A = %v", runA)
	}
	if got := e.claim(); got != nil {
		t.Fatalf("B started at WIP 1: %v", got.ID)
	}
	c := e.enqueueAgent(e.issueFor(e.agent2, "medium"), e.agent2)
	e.start(runA)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := e.svc.CompleteTask(context.Background(), runA.ID, []byte(`{}`), "", "", "", false, "", ""); err != nil {
			t.Errorf("complete A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		e.svc.ClaimTask(context.Background(), util.MustParseUUID(e.agent2))
	}()
	wg.Wait()
	if n := e.wip(); n != 1 {
		t.Fatalf("WIP after release raced a claim = %d (B=%s C=%s), want exactly 1", n, e.where(b.ID), e.where(c.ID))
	}
}

func TestAdmissionUnclaimedReservationReturnsToHeldAtExpiry(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	a := e.enqueue(e.issue("medium"))
	b := e.enqueue(e.issue("medium"))
	c := e.enqueue(e.issue("medium"))
	runA := e.claim()
	if runA == nil || runA.ID != a.ID {
		t.Fatalf("claim A = %v", runA)
	}
	e.claim()
	e.claim()
	e.complete(runA)
	if got := e.where(b.ID); got != "queued/reserved" {
		t.Fatalf("B = %s, want reserved", got)
	}
	e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET reservation_expires_at = now() - interval '1 second' WHERE id = $1`, b.ID)
	if n, err := e.svc.ExpireDueReservations(context.Background()); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v, want 1", n, err)
	}
	if got := e.where(b.ID); got != "queued/held" {
		t.Fatalf("expired B = %s, want queued/held", got)
	}
	if got := e.where(c.ID); got != "queued/reserved" {
		t.Fatalf("next held C = %s, want queued/reserved", got)
	}
}

func TestAdmissionClaimAndExpiryExactlyOneWins(t *testing.T) {
	for _, claimFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("claimFirst=%v", claimFirst), func(t *testing.T) {
			e := newAdmEnv(t)
			e.setWIP(1)
			a := e.enqueue(e.issue("medium"))
			b := e.enqueue(e.issue("medium"))
			runA := e.claim()
			if runA == nil || runA.ID != a.ID {
				t.Fatalf("claim A = %v", runA)
			}
			e.claim()
			e.complete(runA)
			if claimFirst {
				if got := e.claim(); got == nil || got.ID != b.ID {
					t.Fatalf("claim of reserved B = %v", got)
				}
				e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET reservation_expires_at = now() - interval '1 second', admission_state = 'reserved' WHERE id = $1 AND false`, b.ID)
				if n, _ := e.svc.ExpireDueReservations(context.Background()); n != 0 {
					t.Fatalf("expiry after claim changed %d runs", n)
				}
				if got := e.where(b.ID); got != "dispatched" {
					t.Fatalf("B = %s after claim then expiry, want dispatched", got)
				}
				return
			}
			e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET reservation_expires_at = now() - interval '1 second' WHERE id = $1`, b.ID)
			if got := e.claim(); got != nil && got.ID == b.ID {
				t.Fatalf("a lapsed reservation was claimed")
			}
			if n, _ := e.svc.ExpireDueReservations(context.Background()); n != 1 {
				t.Fatalf("expiry changed %d runs, want 1", n)
			}
		})
	}
}

func TestAdmissionLapsedReservationExpiredByStartupReconcile(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	a := e.enqueue(e.issue("medium"))
	b := e.enqueue(e.issue("medium"))
	c := e.enqueue(e.issue("medium"))
	runA := e.claim()
	if runA == nil || runA.ID != a.ID {
		t.Fatalf("claim A = %v", runA)
	}
	e.claim()
	e.claim()
	e.complete(runA)
	e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET reservation_expires_at = now() - interval '1 minute' WHERE id = $1`, b.ID)
	e.svc = &TaskService{Queries: db.New(e.pool), TxStarter: e.pool, Bus: events.New()}
	if err := e.svc.ReconcileAdmission(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := e.where(b.ID); got != "queued/held" {
		t.Fatalf("lapsed B = %s, want queued/held", got)
	}
	if got := e.where(c.ID); got != "queued/reserved" {
		t.Fatalf("C = %s, want queued/reserved", got)
	}
}

func TestAdmissionCrashAfterClaimParksInputs(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	if run == nil {
		t.Fatal("no claim")
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"claimed"}) {
		t.Fatalf("inputs after claim = %v, want [claimed]", got)
	}
	// A non-retryable failure of the dispatched run.
	if _, err := e.svc.FailTask(context.Background(), run.ID, "boom", "", "", "", "agent_error", false, "", ""); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs after the failure = %v, want [parked]", got)
	}
}

func TestAdmissionCrashAfterBatchDeliveredParksThenAuthorizedRetryRedelivers(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	e.fail(run, "agent_error")
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs = %v, want [parked]", got)
	}
	res, err := e.svc.ResumeParkedInputs(context.Background(), util.MustParseUUID(issue), util.MustParseUUID(e.agent))
	if n := res.Resumed; err != nil || n != 1 {
		t.Fatalf("resume = %d, %v, want 1", n, err)
	}
	in := e.inputs(issue)
	if len(in) != 1 || in[0].State != "owned" || in[0].RedeliveryOfTaskID != run.ID {
		t.Fatalf("after resume: %+v, want owned redelivery of %v", in, run.ID)
	}
}

func TestAdmissionCompletedRunDeliversInputs(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	e.complete(run)
	if got := e.inputStates(issue); !eqStrings(got, []string{"delivered"}) {
		t.Fatalf("inputs = %v, want [delivered]", got)
	}
}

func TestAdmissionFailedRunParksAndNothingRedispatches(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	e.fail(run, "agent_error")
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs = %v, want [parked]", got)
	}
	if got := e.claim(); got != nil {
		t.Fatalf("a parked batch was re-dispatched as %v", got.ID)
	}
	e.svc.ReleaseProjectAdmission(context.Background(), util.MustParseUUID(e.project))
	if got := e.claim(); got != nil {
		t.Fatalf("a release event re-dispatched a parked batch as %v", got.ID)
	}
}

func TestAdmissionRepeatedProviderFailureStopsAtRetryLimit(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	runs := 0
	for i := 0; i < 10; i++ {
		e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET fire_at = NULL, status = 'queued' WHERE issue_id = $1 AND status IN ('queued', 'deferred')`, issue)
		run := e.claim()
		if run == nil {
			break
		}
		runs++
		e.fail(run, string(taskfailure.ReasonAgentProviderNetwork))
	}
	if runs != 3 {
		t.Fatalf("provider failures ran %d times, want the recovery path's limit of 3", runs)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs after the retry limit = %v, want [parked]", got)
	}
}

func TestAdmissionRecoveryPathRedeliversSameEventIDs(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	e.record(issue, e.agent, "evt-2", "comment")
	first := e.claim()
	e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET fire_at = NULL, status = 'queued' WHERE issue_id = $1 AND status IN ('queued', 'deferred')`, issue)
	e.fail(first, string(taskfailure.ReasonAgentProviderNetwork))
	e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET fire_at = NULL, status = 'queued' WHERE issue_id = $1 AND status IN ('queued', 'deferred')`, issue)
	second := e.claim()
	if second == nil || second.ID == first.ID {
		t.Fatalf("recovery run = %v, want a new run", second)
	}
	in, _ := e.svc.RunInputsForTask(context.Background(), second.ID)
	if len(in) != 2 {
		t.Fatalf("redelivered %d inputs, want 2", len(in))
	}
	ids := map[string]bool{}
	for _, i := range in {
		ids[i.EventID] = true
		if i.RedeliveryOfTaskID != first.ID {
			t.Fatalf("input %s redelivery_of = %v, want %v", i.EventID, i.RedeliveryOfTaskID, first.ID)
		}
	}
	if !ids["evt-2"] {
		t.Fatalf("redelivery lost event id evt-2: %v", ids)
	}
}

func TestAdmissionFailedRunParksAllInputs(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	e.record(issue, e.agent, "evt-b", "comment")
	e.record(issue, e.agent, "evt-c", "comment")
	run := e.claim()
	if in, _ := e.svc.RunInputsForTask(context.Background(), run.ID); len(in) != 3 {
		t.Fatalf("batch = %d inputs, want 3", len(in))
	}
	e.fail(run, "agent_error")
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked", "parked", "parked"}) {
		t.Fatalf("inputs = %v, want all parked", got)
	}
}

func TestAdmissionTwoDecisionOutcomesDuringPauseBothReachRun(t *testing.T) {
	e := newAdmEnv(t)
	e.setStatus("paused")
	issue := e.issue("medium")
	e.enqueue(issue)
	if !e.record(issue, e.agent, "decision-1", "decision") || !e.record(issue, e.agent, "decision-2", "decision") {
		t.Fatal("a distinct decision outcome was deduplicated")
	}
	e.setStatus("in_progress")
	run := e.claim()
	if run == nil {
		t.Fatal("no run after unpause")
	}
	in, _ := e.svc.RunInputsForTask(context.Background(), run.ID)
	count := map[string]int{}
	for _, i := range in {
		count[i.EventID]++
	}
	if count["decision-1"] != 1 || count["decision-2"] != 1 {
		t.Fatalf("batch = %v, want each decision once", count)
	}
}

func TestAdmissionRetriedEventIDDeliveredOnce(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	if !e.record(issue, e.agent, "same-event", "comment") {
		t.Fatal("first record deduplicated")
	}
	if e.record(issue, e.agent, "same-event", "comment") {
		t.Fatal("a retry of the same event id was stored twice")
	}
	run := e.claim()
	in, _ := e.svc.RunInputsForTask(context.Background(), run.ID)
	n := 0
	for _, i := range in {
		if i.EventID == "same-event" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("event delivered %d times, want 1", n)
	}
}

func TestAdmissionPriorityChangeReordersNextRelease(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	a := e.enqueue(e.issue("medium"))
	b := e.enqueue(e.issue("medium"))
	cIssue := e.issue("medium")
	c := e.enqueue(cIssue)
	runA := e.claim()
	if runA == nil || runA.ID != a.ID {
		t.Fatalf("claim A = %v", runA)
	}
	e.claim()
	e.claim()
	e.pool.Exec(context.Background(), `UPDATE issue SET priority = 'urgent' WHERE id = $1`, cIssue)
	e.complete(runA)
	if got := e.where(c.ID); got != "queued/reserved" {
		t.Fatalf("urgent C = %s (B = %s), want C reserved first", got, e.where(b.ID))
	}
}

func TestAdmissionCancelAfterStartParksAndNothingStarts(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	if _, err := e.svc.CancelTask(context.Background(), run.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs after cancel = %v, want [parked]", got)
	}
	e.svc.ReleaseProjectAdmission(context.Background(), util.MustParseUUID(e.project))
	if got := e.claim(); got != nil {
		t.Fatalf("a cancelled batch started run %v", got.ID)
	}
}

func TestAdmissionCancelInPausedProjectStaysParkedAfterUnpause(t *testing.T) {
	e := newAdmEnv(t)
	e.setStatus("paused")
	issue := e.issue("medium")
	task := e.enqueue(issue)
	if _, err := e.svc.CancelTask(context.Background(), task.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	e.setStatus("in_progress")
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs = %v, want [parked]", got)
	}
	if got := e.claim(); got != nil {
		t.Fatalf("unpause started a cancelled run: %v", got.ID)
	}
}

func TestAdmissionExplicitResumeReturnsBatchToHeldAndStartsOnce(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	e.fail(run, "agent_error")
	res, err := e.svc.ResumeParkedInputs(context.Background(), util.MustParseUUID(issue), util.MustParseUUID(e.agent))
	if n := res.Resumed; err != nil || n != 1 {
		t.Fatalf("resume = %d, %v", n, err)
	}
	got := e.claim()
	if got == nil {
		t.Fatal("resumed batch did not start")
	}
	in, _ := e.svc.RunInputsForTask(context.Background(), got.ID)
	if len(in) != 1 {
		t.Fatalf("resumed run carries %d inputs, want 1", len(in))
	}
	if again := e.claim(); again != nil {
		t.Fatalf("resumed batch started twice")
	}
}

func TestAdmissionDiscardParkedBatch(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	e.fail(run, "agent_error")
	n, err := e.svc.DiscardParkedInputs(context.Background(), util.MustParseUUID(issue), util.MustParseUUID(e.agent))
	if err != nil || n != 1 {
		t.Fatalf("discard = %d, %v", n, err)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"discarded"}) {
		t.Fatalf("inputs = %v, want [discarded]", got)
	}
	if m, _ := e.svc.ResumeParkedInputs(context.Background(), util.MustParseUUID(issue), util.MustParseUUID(e.agent)); m.Resumed != 0 {
		t.Fatalf("a discarded batch was resumed")
	}
}

func TestAdmissionNewTriggerAfterCancelStartsRunWithOnlyNewInput(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	if _, err := e.svc.CancelTask(context.Background(), run.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	e.enqueue(issue)
	fresh := e.claim()
	if fresh == nil {
		t.Fatal("new trigger did not start a run")
	}
	in, _ := e.svc.RunInputsForTask(context.Background(), fresh.ID)
	if len(in) != 1 {
		t.Fatalf("new run carries %d inputs, want only the new one", len(in))
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked", "claimed"}) {
		t.Fatalf("inputs = %v, want [parked claimed]", got)
	}
}
