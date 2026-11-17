package ghstack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func storePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Skipf("skipping DB test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skipping DB test: database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var storeSeq int

type storeFx struct {
	pool *pgxpool.Pool
	inst int64
}

func newStoreFx(t *testing.T) *storeFx {
	storeSeq++
	return &storeFx{pool: storePool(t), inst: 9_100_000 + int64(os.Getpid()%1000)*100 + int64(storeSeq)}
}

// workspace inserts a workspace with the given settings JSON, bound to the
// installation, and returns its id.
func (f *storeFx) workspace(t *testing.T, settings string) string {
	t.Helper()
	ctx := context.Background()
	storeSeq++
	var id string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, description, issue_prefix, settings) VALUES ($1,$2,'','GHX',$3::jsonb) RETURNING id::text`,
		"ghx", fmt.Sprintf("ghx-%d-%d-%d", os.Getpid(), f.inst, storeSeq), settings).Scan(&id); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1::uuid`, id) })
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO github_installation (workspace_id, installation_id, account_login) VALUES ($1::uuid,$2,'acme')`,
		id, f.inst); err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	return id
}

// linkedIssue inserts PR N for the workspace and an issue linked to it.
func (f *storeFx) linkedIssue(t *testing.T, ws string, pr int) string {
	t.Helper()
	ctx := context.Background()
	var prID, issueID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at)
		 VALUES ($1::uuid,$2,'acme','widgets',$3,'t','open','https://x',now(),now()) RETURNING id::text`,
		ws, f.inst, pr).Scan(&prID); err != nil {
		t.Fatalf("seed pr: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO issue (workspace_id,title,creator_type,creator_id) VALUES ($1::uuid,'i','member',gen_random_uuid()) RETURNING id::text`,
		ws).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1::uuid,$2::uuid)`, issueID, prID); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	return issueID
}

func (f *storeFx) reports(t *testing.T, issue string) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT details FROM activity_log WHERE issue_id=$1::uuid AND action=$2 ORDER BY created_at`, issue, ReportAction)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestStoreModesTakeLowestAcrossBoundWorkspaces(t *testing.T) {
	f := newStoreFx(t)
	a := f.workspace(t, `{"github_stack_autolink":{"mode":"link"}}`)
	b := f.workspace(t, `{"github_stack_autolink":{"mode":"report"}}`)
	r, err := NewStore(f.pool).Resolve(context.Background(), f.inst, "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if r.Final != ModeReport {
		t.Fatalf("final = %q, want report (lowest bound workspace)", r.Final)
	}
	got := map[string]Mode{}
	for _, w := range r.Workspaces {
		got[w.WorkspaceID] = w.Mode
	}
	if got[a] != ModeLink || got[b] != ModeReport || len(got) != 2 {
		t.Fatalf("per-workspace modes = %v", got)
	}
}

func TestStoreModeIsOffWithNoBinding(t *testing.T) {
	f := newStoreFx(t)
	r, err := NewStore(f.pool).Resolve(context.Background(), f.inst, "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if r.Final != ModeOff {
		t.Fatalf("final = %q, want off when nothing is bound", r.Final)
	}
}

func TestStoreModeEnvKillForcesOff(t *testing.T) {
	f := newStoreFx(t)
	f.workspace(t, `{"github_stack_autolink":{"mode":"link"}}`)
	t.Setenv("GITHUB_STACK_AUTOLINK", "0")
	r, err := NewStore(f.pool).Resolve(context.Background(), f.inst, "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if r.Final != ModeOff {
		t.Fatalf("final = %q, want off under the env kill", r.Final)
	}
}

func TestStoreModeIsReadFreshEachCall(t *testing.T) {
	f := newStoreFx(t)
	ws := f.workspace(t, `{"github_stack_autolink":{"mode":"link"}}`)
	s := NewStore(f.pool)
	if r, _ := s.Resolve(context.Background(), f.inst, "acme", "widgets"); r.Final != ModeLink {
		t.Fatalf("first final = %q, want link", r.Final)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE workspace SET settings='{"github_stack_autolink":{"mode":"off"}}'::jsonb WHERE id=$1::uuid`, ws); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Resolve(context.Background(), f.inst, "acme", "widgets"); r.Final != ModeOff {
		t.Fatalf("second final = %q, want off (setting lowered)", r.Final)
	}
}

func TestStoreReportWritesOneActivityPerLinkedIssue(t *testing.T) {
	f := newStoreFx(t)
	ws := f.workspace(t, `{}`)
	issue := f.linkedIssue(t, ws, 7)
	s := NewStore(f.pool)
	o := Outcome{Job: Job{InstallationID: f.inst, Owner: "acme", Repo: "widgets", PR: 7}, BaseRef: "feat/a", Code: OutcomeWouldCreate,
		Workspaces: []WorkspaceMode{{WorkspaceID: ws, Mode: ModeReport}}}
	if err := s.Report(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	got := f.reports(t, issue)
	if len(got) != 1 {
		t.Fatalf("reports = %v, want one", got)
	}
	if got[0]["code"] != "would_create" || got[0]["base_ref"] != "feat/a" || got[0]["pr"] != float64(7) {
		t.Fatalf("details = %v", got[0])
	}
}

func TestStoreReportDedupesIdenticalAndKeepsDistinct(t *testing.T) {
	f := newStoreFx(t)
	ws := f.workspace(t, `{}`)
	issue := f.linkedIssue(t, ws, 7)
	s := NewStore(f.pool)
	mk := func(base string, c Code) Outcome {
		return Outcome{Job: Job{InstallationID: f.inst, Owner: "acme", Repo: "widgets", PR: 7}, BaseRef: base, Code: c,
			Workspaces: []WorkspaceMode{{WorkspaceID: ws, Mode: ModeReport}}}
	}
	for _, o := range []Outcome{
		mk("feat/a", OutcomeWouldCreate), mk("feat/a", OutcomeWouldCreate), // identical: one entry
		mk("feat/b", OutcomeWouldCreate), // new base ref
		mk("feat/a", OutcomeCreated),     // new code
	} {
		if err := s.Report(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.reports(t, issue)); n != 3 {
		t.Fatalf("reports = %d, want 3 (identical report deduped)", n)
	}
}

func TestStoreReportSkipsWorkspacesNotInOutcome(t *testing.T) {
	f := newStoreFx(t)
	on := f.workspace(t, `{}`)
	off := f.workspace(t, `{"github_stack_autolink":{"mode":"off"}}`)
	onIssue := f.linkedIssue(t, on, 7)
	offIssue := f.linkedIssue(t, off, 7)
	o := Outcome{Job: Job{InstallationID: f.inst, Owner: "acme", Repo: "widgets", PR: 7}, BaseRef: "main", Code: OutcomeConflict,
		Workspaces: []WorkspaceMode{{WorkspaceID: on, Mode: ModeReport}}}
	if err := NewStore(f.pool).Report(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := len(f.reports(t, onIssue)); n != 1 {
		t.Fatalf("eligible workspace reports = %d, want 1", n)
	}
	if n := len(f.reports(t, offIssue)); n != 0 {
		t.Fatalf("off workspace reports = %d, want 0", n)
	}
}

func TestStoreReportWithNoLinkedIssueIsANoop(t *testing.T) {
	f := newStoreFx(t)
	ws := f.workspace(t, `{}`)
	o := Outcome{Job: Job{InstallationID: f.inst, Owner: "acme", Repo: "widgets", PR: 99}, BaseRef: "main", Code: OutcomeConflict,
		Workspaces: []WorkspaceMode{{WorkspaceID: ws, Mode: ModeReport}}}
	if err := NewStore(f.pool).Report(context.Background(), o); err != nil {
		t.Fatalf("no linked issue must not be an error: %v", err)
	}
}
