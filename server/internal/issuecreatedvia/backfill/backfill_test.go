package backfill

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

var testPool *pgxpool.Pool

// isolatedDB gives the test an empty, fully migrated database: the shared test
// database may lag behind the migrations, and the backfill scans whole tables.
func isolatedDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica_test?sslmode=disable"
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("database not available: %v", err)
	}
	conn.Close(context.Background())
	return testutil.IsolatedDB(t, dsn)
}

// scenario is one workspace holding one issue per rule row of the CHE-6376 plan.
type scenario struct {
	f  *testutil.Fixture
	ws pgtype.UUID
	// want maps a fixture name to the value the backfill must write; ""
	// marks a row that stays unchanged (rule 7, rule 10).
	want map[string]string
	ids  map[string]string
	// old is the created_via each row starts with ("" = NULL).
	old map[string]string
}

func uuidOf(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	ctx := context.Background()
	f0 := testutil.New(testPool, "", "")
	user := f0.User(t, "Backfill User", fmt.Sprintf("backfill-%s@multica.ai", uuid.NewString()))
	ws := f0.Workspace(t, "Backfill WS", "backfill-"+uuid.NewString()[:8], testutil.Cols{"issue_prefix": "BKF"})
	f := testutil.New(testPool, ws, user)
	f.Member(t, ws, user, "owner")
	rt := f.Runtime(t, "bf-runtime")
	agent := f.Agent(t, "bf-agent", rt)
	s := &scenario{f: f, ws: uuidOf(t, ws), want: map[string]string{}, ids: map[string]string{}, old: map[string]string{}}

	task := func(over testutil.Cols) string {
		cols := testutil.Cols{"runtime_id": rt}
		for k, v := range over {
			cols[k] = v
		}
		return f.Task(t, agent, cols)
	}
	add := func(name, want, old string, over testutil.Cols) string {
		cols := testutil.Cols{}
		for k, v := range over {
			cols[k] = v
		}
		if old != "" {
			cols["created_via"] = old
		}
		id := f.Issue(t, name, cols)
		s.want[name], s.ids[name], s.old[name] = want, id, old
		return id
	}
	agentMade := func(taskID string) testutil.Cols {
		return testutil.Cols{"creator_type": "agent", "creator_id": agent, "origin_type": "agent_create", "origin_id": taskID}
	}
	memberComment := func() string {
		return f.Comment(t, add("host "+uuid.NewString()[:6], "form", "", nil), "please do it")
	}
	agentComment := func() string {
		host := add("host "+uuid.NewString()[:6], "form", "", nil)
		return f.Comment(t, host, "done", testutil.Cols{"author_type": "agent", "author_id": agent})
	}

	// Rule 1 and 2.
	qc := task(nil)
	add("1 quick_create", "dialog", "agent", testutil.Cols{"creator_type": "agent", "creator_id": agent, "origin_type": "quick_create", "origin_id": qc})
	add("2 autopilot origin", "autopilot", "", testutil.Cols{"creator_type": "agent", "creator_id": agent, "origin_type": "autopilot", "origin_id": uuid.NewString()})
	add("2 autopilot task", "autopilot", "agent", agentMade(task(testutil.Cols{"trigger_evidence_kind": "autopilot_run"})))

	// Rule 3 and 4.
	chat := f.ChatSession(t, agent)
	dmTask := task(testutil.Cols{"chat_session_id": chat})
	add("3 dm", "dm", "agent", agentMade(dmTask))
	sameRunTask := task(testutil.Cols{"chat_session_id": chat})
	// The parent itself has no same-run parent, so it is a DM issue too.
	parent := add("4 same-run parent", "dm", "agent", agentMade(sameRunTask))
	child := agentMade(sameRunTask)
	child["parent_issue_id"] = parent
	add("4 dm child of same-run parent", "agent", "agent", child)
	older := add("older parent", "form", "", nil)
	olderChild := agentMade(dmTask)
	olderChild["parent_issue_id"] = older
	add("3 dm child of older parent", "dm", "agent", olderChild)

	// Rule 5, 5b and 6.
	add("5 comment", "comment", "agent", agentMade(task(testutil.Cols{"trigger_evidence_kind": "comment", "trigger_comment_id": memberComment()})))
	add("5 comment by agent", "agent", "agent", agentMade(task(testutil.Cols{"trigger_evidence_kind": "comment", "trigger_comment_id": agentComment()})))
	add("5b legacy comment", "comment", "", agentMade(task(testutil.Cols{"trigger_comment_id": memberComment()})))
	add("6 wakeup with member comment", "agent", "agent", agentMade(task(testutil.Cols{"trigger_evidence_kind": "issue_wakeup", "trigger_comment_id": memberComment()})))
	add("6 legacy no evidence", "agent", "", agentMade(task(nil)))

	// Rule 7, 8 and 9.
	add("7 slack", "", "", testutil.Cols{"origin_type": "slack_chat", "origin_id": uuid.NewString()})
	add("9 member", "form", "", nil)

	// Rule 10: no origin stamp, and a stamp whose task row is gone.
	add("10 no origin stamp", "", "agent", testutil.Cols{"creator_type": "agent", "creator_id": agent})
	add("10 task row missing", "", "agent", agentMade(uuid.NewString()))

	// Platform create paths (S1 PR 1969): old rows are found by their link.
	rel := add("release issue", "autopilot", "", nil)
	if _, err := testPool.Exec(ctx, `INSERT INTO github_release_deploy
		(workspace_id, repo_owner, repo_name, source_sha, image_digest, old_sha, run_id, issue_id)
		VALUES ($1, 'o', 'r', 'sha', 'digest', 'old', 1, $2)`, ws, rel); err != nil {
		t.Fatal(err)
	}
	add("ci-watch issue", "autopilot", "", testutil.Cols{"metadata": testutil.Raw(`'{"ci_watch_count": 3}'::jsonb`)})
	// An agent-made issue that carries the counter keeps its task rule.
	add("agent issue with ci_watch_count", "agent", "agent", func() testutil.Cols {
		c := agentMade(task(nil))
		c["metadata"] = testutil.Raw(`'{"ci_watch_count": 1}'::jsonb`)
		return c
	}())

	// Outside the domain: already classified.
	add("outside domain", "", "dialog", nil)
	return s
}

func (s *scenario) current(t *testing.T, name string) string {
	t.Helper()
	var v *string
	if err := testPool.QueryRow(context.Background(), `SELECT created_via FROM issue WHERE id = $1`, s.ids[name]).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func (s *scenario) opts() Options { return Options{WorkspaceID: s.ws, BatchSize: 4} }

// expectedCounts is the per-value count the dry run must report. Rows with
// want "" are rule 7 (unknown) or rule 10 (unresolved) and are counted apart.
func (s *scenario) expectedCounts() map[string]int {
	counts := map[string]int{}
	for name, want := range s.want {
		switch name {
		case "outside domain":
		case "7 slack":
			counts["unknown"]++
		case "10 no origin stamp", "10 task row missing":
		default:
			counts[want]++
		}
	}
	return counts
}

func testDryRunReportsEveryRuleAndWritesNothing(t *testing.T) {
	s := newScenario(t)
	before := map[string]string{}
	for name := range s.ids {
		before[name] = s.current(t, name)
	}

	rep, err := DryRun(context.Background(), testPool, s.opts())
	if err != nil {
		t.Fatal(err)
	}
	for value, want := range s.expectedCounts() {
		if got := rep.Counts[value]; got != want {
			t.Errorf("counts[%s] = %d, want %d", value, got, want)
		}
	}
	if rep.Unresolved != 2 {
		t.Errorf("unresolved = %d, want 2", rep.Unresolved)
	}
	if len(rep.UnresolvedIssueIDs) != 2 {
		t.Errorf("unresolved ids = %v, want 2", rep.UnresolvedIssueIDs)
	}
	if rep.Legacy["comment"] != 1 || rep.Legacy["agent"] != 2 {
		t.Errorf("legacy = %v, want comment=1 agent=2", rep.Legacy)
	}
	if len(rep.LegacyCommentSamples) != 1 || rep.LegacyCommentSamples[0].IssueID != s.ids["5b legacy comment"] ||
		rep.LegacyCommentSamples[0].TriggerCommentID == "" {
		t.Errorf("legacy comment samples = %+v", rep.LegacyCommentSamples)
	}
	if rep.Domain != len(s.ids)-1 {
		t.Errorf("domain = %d, want %d (every fixture but the one outside it)", rep.Domain, len(s.ids)-1)
	}
	for name := range s.ids {
		if got := s.current(t, name); got != before[name] {
			t.Errorf("dry run wrote %s: %q -> %q", name, before[name], got)
		}
	}
}

func readReceipt(t *testing.T, path string) []receiptLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []receiptLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l receiptLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("receipt line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

func testApplySetsValuesOnceAndRevertRestores(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()
	receipt := filepath.Join(t.TempDir(), "receipt.jsonl")

	res, err := Apply(ctx, testPool, s.opts(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for name, want := range s.want {
		got := s.current(t, name)
		exp := want
		if exp == "" {
			exp = s.old[name] // unchanged
		}
		if got != exp {
			t.Errorf("after apply %s = %q, want %q", name, got, exp)
		}
		if got != s.old[name] {
			changed++
		}
	}
	if res.Written != changed {
		t.Errorf("written = %d, want %d", res.Written, changed)
	}
	if lines := readReceipt(t, receipt); len(lines) != changed {
		t.Errorf("receipt has %d lines, want %d", len(lines), changed)
	}

	second, err := Apply(ctx, testPool, s.opts(), filepath.Join(t.TempDir(), "second.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 {
		t.Errorf("second apply wrote %d rows, want 0", second.Written)
	}

	// A row edited after the apply is not reverted.
	if _, err := testPool.Exec(ctx, `UPDATE issue SET created_via = 'cli' WHERE id = $1`, s.ids["9 member"]); err != nil {
		t.Fatal(err)
	}
	rev, err := Revert(ctx, testPool, s.opts(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Reverted != changed-1 || rev.Skipped != 1 {
		t.Errorf("revert = %+v, want reverted %d skipped 1", rev, changed-1)
	}
	for name := range s.ids {
		want := s.old[name]
		if name == "9 member" {
			want = "cli"
		}
		if got := s.current(t, name); got != want {
			t.Errorf("after revert %s = %q, want %q", name, got, want)
		}
	}
}

// One migrated database serves every subtest: migrating costs about a minute.
// Each subtest builds its own workspace and scopes the run to it.
func TestBackfill(t *testing.T) {
	testPool = isolatedDB(t)
	t.Run("dry run reports every rule and writes nothing", testDryRunReportsEveryRuleAndWritesNothing)
	t.Run("apply sets values once and revert restores", testApplySetsValuesOnceAndRevertRestores)
}
