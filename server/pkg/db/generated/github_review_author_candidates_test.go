package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-6888: ListGitHubReviewAuthorCandidates put the task-key expression inside
// an OR, which forced a scan of every row of agent_task_queue (110,243 rows on
// C00). The tests seed a workspace with noiseTasks other tasks and read what the
// executor did, not how long it took.

const (
	noiseIssues      = 400
	noiseTasksPerIss = 50
	noiseTasks       = noiseIssues * noiseTasksPerIss // 20,000 tasks in the workspace
	linkedIssueTasks = 40                             // other tasks on the issue the PR links to
)

// Bounds, derived from the fixture:
//   - the branch arm reads the linked issue's tasks only: linkedIssueTasks + 1
//     rows through idx_agent_task_queue_issue_id, once per probe;
//   - the key arm reads one task through the task-key index;
//   - every other node (PR, link, author, agent lookups) touches a handful of
//     rows. 10 x (linkedIssueTasks + 1) = 410 leaves room for those and sits
//     49x below the 20,000 rows a scan of the table reads.
const (
	maxRowsExamined = 10 * (linkedIssueTasks + 1)
	// Indexed, the lookup reads about 12 buffers (a few index pages and heap
	// pages for 41 tasks of one issue); a scan of 20,000 tasks reads 500 to 1,000.
	// 100 is 8x the indexed read and 5x below the lowest scan.
	maxBuffers = 100
)

type reviewAuthorFixture struct {
	tx                pgx.Tx
	ws, otherWS       string
	a1, a2, otherAuth string
	linkedIssue       string
}

func openReviewAuthorTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Skip(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) }) // the seed never leaves the transaction
	return ctx, tx
}

func (f *reviewAuthorFixture) one(t *testing.T, ctx context.Context, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := f.tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
	return id
}

func (f *reviewAuthorFixture) exec(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := f.tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func (f *reviewAuthorFixture) agent(t *testing.T, ctx context.Context, ws, name string) string {
	return f.one(t, ctx, `INSERT INTO agent (workspace_id, name, runtime_mode) VALUES ($1, $2, 'cloud') RETURNING id::text`, ws, name)
}

func (f *reviewAuthorFixture) issue(t *testing.T, ctx context.Context, ws string, number int) string {
	return f.one(t, ctx, `INSERT INTO issue (workspace_id, title, creator_type, creator_id, number) VALUES ($1, 'c6888', 'member', gen_random_uuid(), $2) RETURNING id::text`, ws, number)
}

// task inserts a finished task (a finished task needs no runtime).
func (f *reviewAuthorFixture) task(t *testing.T, ctx context.Context, agent, issue, branch string, createdAt time.Time) string {
	return f.one(t, ctx, `INSERT INTO agent_task_queue (agent_id, issue_id, status, branch_name, created_at, completed_at)
		VALUES ($1, $2, 'completed', $3, $4, $4) RETURNING id::text`, agent, issue, branch, createdAt)
}

// pr inserts a pull request whose head branch is branch, created at prCreated.
func (f *reviewAuthorFixture) pr(t *testing.T, ctx context.Context, ws, branch string, number int, prCreated time.Time, linkTo string) string {
	id := f.one(t, ctx, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, branch)
		VALUES ($1, 1, 'acme', 'widget', $2, 't', 'open', 'u', $3, $3, 'h', $4) RETURNING id::text`, ws, number, prCreated, branch)
	if linkTo != "" {
		f.exec(t, ctx, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, linkTo, id)
	}
	return id
}

func taskKey(taskID string) string {
	return taskID[len(taskID)-12:]
}

func seedReviewAuthors(t *testing.T) (context.Context, *reviewAuthorFixture) {
	t.Helper()
	ctx, tx := openReviewAuthorTx(t)
	f := &reviewAuthorFixture{tx: tx}
	f.ws = f.one(t, ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('che6888', 'che6888-scale', 'C68') RETURNING id::text`)
	f.otherWS = f.one(t, ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('che6888b', 'che6888-other', 'C6B') RETURNING id::text`)
	f.a1 = f.agent(t, ctx, f.ws, "a1")
	f.a2 = f.agent(t, ctx, f.ws, "a2")
	f.otherAuth = f.agent(t, ctx, f.otherWS, "a3")
	f.linkedIssue = f.issue(t, ctx, f.ws, 1)

	// noiseTasks tasks the lookups must not read: 400 issues x 50 tasks.
	f.exec(t, ctx, fmt.Sprintf(`
		WITH i AS (
			INSERT INTO issue (workspace_id, title, creator_type, creator_id, number)
			SELECT $1::uuid, 'c6888 noise', 'member', gen_random_uuid(), 1000 + g FROM generate_series(1, %d) g
			RETURNING id
		)
		INSERT INTO agent_task_queue (agent_id, issue_id, status, branch_name, created_at, completed_at)
		SELECT $2::uuid, i.id, 'completed', 'agent/a1/noise-' || n, now() - interval '2 hours', now() - interval '2 hours'
		FROM i, generate_series(1, %d) n`, noiseIssues, noiseTasksPerIss), f.ws, f.a1)
	// linkedIssueTasks other tasks on the issue the PRs link to.
	f.exec(t, ctx, fmt.Sprintf(`
		INSERT INTO agent_task_queue (agent_id, issue_id, status, branch_name, created_at, completed_at)
		SELECT $1::uuid, $2::uuid, 'completed', 'agent/a1/other-' || n, now() - interval '2 hours', now() - interval '2 hours'
		FROM generate_series(1, %d) n`, linkedIssueTasks), f.a2, f.linkedIssue)
	for _, table := range []string{"agent_task_queue", "issue", "github_pull_request", "issue_pull_request"} {
		f.exec(t, ctx, "ANALYZE "+table)
	}
	return ctx, f
}

func uuidOf(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *reviewAuthorFixture) candidates(t *testing.T, ctx context.Context, pr, ws, key string) []string {
	t.Helper()
	got, err := New(f.tx).ListGitHubReviewAuthorCandidates(ctx, ListGitHubReviewAuthorCandidatesParams{
		PullRequestID: uuidOf(t, pr), WorkspaceID: uuidOf(t, ws), TaskKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(got))
	for _, u := range got {
		out = append(out, u.String())
	}
	return out
}

func wantAgents(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[strings.ToLower(g)] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

// The behaviour the split must keep, on the 20,000-task fixture.
func TestListGitHubReviewAuthorCandidatesBehavior(t *testing.T) {
	ctx, f := seedReviewAuthors(t)
	prCreated := time.Now()
	early := prCreated.Add(-time.Hour)

	// Key arm: the PR branch carries the task key of a1's task; nothing links the PR.
	keyTask := f.task(t, ctx, f.a1, f.issue(t, ctx, f.ws, 2), "agent/a1/x-"+"unrelated", early)
	keyPR := f.pr(t, ctx, f.ws, "agent/a1/slug-"+taskKey(keyTask), 1, prCreated, "")
	wantAgents(t, "key match", f.candidates(t, ctx, keyPR, f.ws, taskKey(keyTask)), f.a1)

	// Another workspace gets nothing for the same PR and key.
	wantAgents(t, "other workspace", f.candidates(t, ctx, keyPR, f.otherWS, taskKey(keyTask)))

	// Branch arm: a1's task on the linked issue has the PR's branch and predates the PR.
	branchPR := f.pr(t, ctx, f.ws, "agent/a1/branch-only", 2, prCreated, f.linkedIssue)
	f.task(t, ctx, f.a1, f.linkedIssue, "agent/a1/branch-only", early)
	wantAgents(t, "branch match", f.candidates(t, ctx, branchPR, f.ws, ""), f.a1)

	// An empty key uses the branch arm only: a2's task whose key would match
	// the PR branch is not a candidate while the key is empty.
	a2Task := f.task(t, ctx, f.a2, f.issue(t, ctx, f.ws, 3), "agent/a2/elsewhere", early)
	emptyKeyPR := f.pr(t, ctx, f.ws, "agent/a2/slug-"+taskKey(a2Task), 3, prCreated, f.linkedIssue)
	wantAgents(t, "empty key, no branch match", f.candidates(t, ctx, emptyKeyPR, f.ws, ""))
	wantAgents(t, "same PR with its key", f.candidates(t, ctx, emptyKeyPR, f.ws, taskKey(a2Task)), f.a2)

	// Both arms land on the same agent: one row, not two.
	bothTask := f.task(t, ctx, f.a1, f.issue(t, ctx, f.ws, 4), "agent/a1/else", early)
	bothPR := f.pr(t, ctx, f.ws, "agent/a1/both-"+taskKey(bothTask), 4, prCreated, f.linkedIssue)
	f.task(t, ctx, f.a1, f.linkedIssue, "agent/a1/both-"+taskKey(bothTask), early)
	wantAgents(t, "both arms same agent", f.candidates(t, ctx, bothPR, f.ws, taskKey(bothTask)), f.a1)

	// Two arms, two agents: both come back (the caller then refuses to guess).
	f.task(t, ctx, f.a2, f.linkedIssue, "agent/a1/both-"+taskKey(bothTask), early)
	wantAgents(t, "arms disagree", f.candidates(t, ctx, bothPR, f.ws, taskKey(bothTask)), f.a1, f.a2)

	// A branch task created after the PR is not the author.
	latePR := f.pr(t, ctx, f.ws, "agent/a1/late", 5, prCreated, f.linkedIssue)
	f.task(t, ctx, f.a1, f.linkedIssue, "agent/a1/late", prCreated.Add(time.Hour))
	wantAgents(t, "task after the PR", f.candidates(t, ctx, latePR, f.ws, ""))

	// A task on an issue the PR is not linked to is not a branch match.
	unlinkedPR := f.pr(t, ctx, f.ws, "agent/a1/unlinked", 6, prCreated, "")
	f.task(t, ctx, f.a1, f.issue(t, ctx, f.ws, 5), "agent/a1/unlinked", early)
	wantAgents(t, "unlinked issue", f.candidates(t, ctx, unlinkedPR, f.ws, ""))
}

// The cost: key and branch arms together touch at most maxRowsExamined rows and
// maxBuffers buffers of a 20,000-task workspace, and never seq scan the table.
func TestListGitHubReviewAuthorCandidatesDoesNotScanTasks(t *testing.T) {
	ctx, f := seedReviewAuthors(t)
	prCreated := time.Now()
	early := prCreated.Add(-time.Hour)

	keyTask := f.task(t, ctx, f.a1, f.issue(t, ctx, f.ws, 2), "agent/a1/elsewhere", early)
	f.task(t, ctx, f.a1, f.linkedIssue, "agent/a1/both-"+taskKey(keyTask), early)
	pr := f.pr(t, ctx, f.ws, "agent/a1/both-"+taskKey(keyTask), 1, prCreated, f.linkedIssue)
	f.exec(t, ctx, "ANALYZE agent_task_queue")

	spy := &sqlSpy{DBTX: f.tx}
	if _, err := New(spy).ListGitHubReviewAuthorCandidates(ctx, ListGitHubReviewAuthorCandidatesParams{
		PullRequestID: uuidOf(t, pr), WorkspaceID: uuidOf(t, f.ws), TaskKey: taskKey(keyTask),
	}); err != nil {
		t.Fatal(err)
	}
	cost, err := explainCost(ctx, f.tx, spy.sql, spy.args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d tasks in the workspace: rows examined %.0f (max %d), buffers %d (max %d), seq scans %v",
		noiseTasks, cost.RowsExamined, maxRowsExamined, cost.Buffers, maxBuffers, cost.SeqScans)
	for _, rel := range cost.SeqScans {
		if rel == "agent_task_queue" {
			t.Errorf("seq scan on agent_task_queue")
		}
	}
	if cost.RowsExamined > maxRowsExamined {
		t.Errorf("examined %.0f rows, want <= %d", cost.RowsExamined, maxRowsExamined)
	}
	if cost.Buffers > maxBuffers {
		t.Errorf("read %d buffers, want <= %d", cost.Buffers, maxBuffers)
	}

	// Negative control: the same fixture with the key expression made
	// non-indexable must break the bound by more than 10x, or the bound above
	// proves nothing.
	const indexable = "replace(task.id::text, '-', '')"
	if !strings.Contains(spy.sql, indexable) {
		t.Fatalf("control: %q not found in the production SQL", indexable)
	}
	control, err := explainCost(ctx, f.tx, strings.ReplaceAll(spy.sql, indexable, "replace(task.id::text || '', '-', '')"), spy.args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("control (non-indexable key expression): rows examined %.0f, buffers %d, seq scans %v", control.RowsExamined, control.Buffers, control.SeqScans)
	if control.RowsExamined <= 10*maxRowsExamined {
		t.Errorf("control examined %.0f rows, want > %d: the fixture no longer separates an indexed plan from a scan", control.RowsExamined, 10*maxRowsExamined)
	}
}
