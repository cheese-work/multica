package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type hicFixture struct {
	agentID, taskID string
}

// newHICFixture seeds an agent every workspace member may invoke, with a
// running task on issueID.
func newHICFixture(t *testing.T, issueID string) hicFixture {
	t.Helper()
	agentID := createHandlerTestAgent(t, "hic-"+uuid.NewString()[:8], []byte("[]"))
	taskID := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
	return hicFixture{agentID, taskID}
}

func hicMember(t *testing.T, role string) string {
	t.Helper()
	id := dbfx.User(t, "HIC "+role, "hic-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(t, testWorkspaceID, id, role)
	return id
}

func hicResolve(t *testing.T, ctx context.Context, issueID, agentID string) (*humanInCharge, error) {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	return testHandler.resolveHumanInCharge(ctx, testHandler.Queries, issue, agent)
}

func wantHIC(t *testing.T, got *humanInCharge, err error, userID, source string) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolver error: %v", err)
	}
	if got == nil || got.UserID != userID || got.Source != source {
		t.Fatalf("human in charge = %+v, want user %s via %s", got, userID, source)
	}
}

func TestHumanInChargeResolverOrder(t *testing.T) {
	creator := hicMember(t, "member")
	requester := hicMember(t, "member")
	lead := hicMember(t, "member")
	project := dbfx.Project(t, "hic project", testutil.Cols{"lead_type": "member", "lead_id": lead})
	issue := dbfx.Issue(t, "hic order", testutil.Cols{"creator_id": creator, "requested_by_id": requester, "project_id": project})
	fx := newHICFixture(t, issue)
	ctx := context.Background()

	got, err := hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, creator, "creator")

	// An agent creator is not a candidate: step 2 takes over.
	dbfx.Exec(t, `UPDATE issue SET creator_type = 'agent', creator_id = $2 WHERE id = $1`, issue, fx.agentID)
	got, err = hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, requester, "requested_by")

	// A null requested_by_id skips step 2.
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = NULL WHERE id = $1`, issue)
	got, err = hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, lead, "project_lead")

	// An agent project lead is not a candidate: the workspace owner is.
	dbfx.Exec(t, `UPDATE project SET lead_type = 'agent', lead_id = $2 WHERE id = $1`, project, fx.agentID)
	got, err = hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, testUserID, "workspace_owner")
}

func TestHumanInChargeSkipsIneligibleCandidates(t *testing.T) {
	left := dbfx.User(t, "HIC left", "hic-"+uuid.NewString()+"@test.invalid") // not a member
	requester := hicMember(t, "member")
	issue := dbfx.Issue(t, "hic ineligible", testutil.Cols{"creator_id": left, "requested_by_id": requester})
	fx := newHICFixture(t, issue)
	ctx := context.Background()

	got, err := hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, requester, "requested_by")

	// A private agent owned by the owner: only the owner passes canInvokeAgent.
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, fx.agentID)
	got, err = hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, testUserID, "workspace_owner")

	// Nobody can invoke the agent: none, never an agent or a fallback.
	other := hicMember(t, "member")
	dbfx.Exec(t, `UPDATE agent SET owner_id = $2 WHERE id = $1`, fx.agentID, other)
	got, err = hicResolve(t, ctx, issue, fx.agentID)
	if err != nil || got != nil {
		t.Fatalf("want none, got %+v err=%v", got, err)
	}
}

func TestHumanInChargeLookupErrorFailsClosed(t *testing.T) {
	issue := dbfx.Issue(t, "hic error")
	fx := newHICFixture(t, issue)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := hicResolve(t, ctx, issue, fx.agentID)
	if err == nil || got != nil {
		t.Fatalf("lookup failure must return an error and no member, got %+v err=%v", got, err)
	}
}

// A failed project-lead lookup must fail closed, not fall through to the owner.
func TestHumanInChargeProjectLookupErrorFailsClosed(t *testing.T) {
	lead := hicMember(t, "member")
	project := dbfx.Project(t, "hic project", testutil.Cols{"lead_type": "member", "lead_id": lead})
	issue := dbfx.Issue(t, "hic project error", testutil.Cols{"creator_type": "agent", "creator_id": createHandlerTestAgent(t, "hic-creator-"+uuid.NewString()[:8], []byte("[]")), "project_id": project})
	fx := newHICFixture(t, issue)
	ctx := context.Background()

	got, err := hicResolve(t, ctx, issue, fx.agentID)
	wantHIC(t, got, err, lead, "project_lead") // healthy lookup resolves the lead

	real := testHandler.Queries
	testHandler.Queries = db.New(failQueryDBTX{DBTX: testPool, failOn: "FROM project", err: errors.New("project lookup failed")})
	t.Cleanup(func() { testHandler.Queries = real })
	issueRow, err := real.GetIssue(ctx, parseUUID(issue))
	if err != nil {
		t.Fatal(err)
	}
	agentRow, err := real.GetAgent(ctx, parseUUID(fx.agentID))
	if err != nil {
		t.Fatal(err)
	}
	got, err = testHandler.resolveHumanInCharge(ctx, testHandler.Queries, issueRow, agentRow)
	if err == nil || got != nil {
		t.Fatalf("project lookup failure must return an error and no member, got %+v err=%v", got, err)
	}
}

func hicCall(t *testing.T, issueID, taskID, agentID, source string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/human-in-charge", nil), "id", issueID)
	if source != "" {
		req.Header.Set("X-Actor-Source", source)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
	}
	return testutil.Call(t, testHandler.GetHumanInCharge, req)
}

func TestHumanInChargeEndpointTaskTokenAuth(t *testing.T) {
	issue := dbfx.Issue(t, "hic auth")
	other := dbfx.Issue(t, "hic auth other")
	fx := newHICFixture(t, issue)

	hicCall(t, issue, fx.taskID, fx.agentID, "").Want(http.StatusForbidden)           // member session
	hicCall(t, other, fx.taskID, fx.agentID, "task_token").Want(http.StatusForbidden) // task on another issue
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID)
	hicCall(t, issue, fx.taskID, fx.agentID, "task_token").Want(http.StatusForbidden) // task not running
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, fx.taskID)

	var out struct {
		Human  *humanInCharge `json:"human"`
		Reason string         `json:"reason"`
	}
	hicCall(t, issue, fx.taskID, fx.agentID, "task_token").Want(http.StatusOK).JSON(&out)
	if out.Human == nil || out.Human.UserID != testUserID {
		t.Fatalf("human = %+v, want creator %s", out.Human, testUserID)
	}

	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, fx.agentID, hicMember(t, "member"))
	out.Human = nil
	hicCall(t, issue, fx.taskID, fx.agentID, "task_token").Want(http.StatusOK).JSON(&out)
	if out.Human != nil || out.Reason == "" {
		t.Fatalf("want none with a reason, got %+v %q", out.Human, out.Reason)
	}
}

// CHE-2586 replay: a mention-triggered reviewer task that is not the assignee
// gets the same member on a member-assigned and a squad-assigned issue.
func TestHumanInChargeReplayCHE2586(t *testing.T) {
	requester := hicMember(t, "member")
	assigneeAgent := createHandlerTestAgent(t, "hic-assignee-"+uuid.NewString()[:8], []byte("[]"))
	squad := dbfx.Squad(t, "hic squad "+uuid.NewString()[:8], assigneeAgent)

	member := dbfx.Issue(t, "hic replay member", testutil.Cols{"creator_type": "agent", "creator_id": assigneeAgent, "requested_by_id": requester, "assignee_type": "member", "assignee_id": requester})
	squadIssue := dbfx.Issue(t, "hic replay squad", testutil.Cols{"creator_type": "agent", "creator_id": assigneeAgent, "requested_by_id": requester, "assignee_type": "squad", "assignee_id": squad})

	for _, issue := range []string{member, squadIssue} {
		fx := newHICFixture(t, issue) // the reviewer: not the assignee
		var out struct {
			Human *humanInCharge `json:"human"`
		}
		hicCall(t, issue, fx.taskID, fx.agentID, "task_token").Want(http.StatusOK).JSON(&out)
		if out.Human == nil || out.Human.UserID != requester {
			t.Fatalf("issue %s: human = %+v, want %s", issue, out.Human, requester)
		}
	}
}

// The resolver runs inside the issue write transaction, which waits on the issue
// lock while holding its own connection. It must read through the queries it is
// given: reaching for a second pooled connection lets concurrent writers
// exhaust the pool while they wait on one another.
func TestHumanInChargeResolverUsesGivenQueries(t *testing.T) {
	creator := hicMember(t, "member")
	issueID := dbfx.Issue(t, "hic given queries", testutil.Cols{"creator_id": creator})
	fx := newHICFixture(t, issueID)

	cfg := testPool.Config()
	cfg.MaxConns = 1
	starved, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer starved.Close()
	held, err := starved.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	h := *testHandler
	h.Queries = db.New(starved)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(fx.agentID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := h.resolveHumanInCharge(ctx, testHandler.Queries, issue, agent)
	wantHIC(t, got, err, creator, "creator")
}
