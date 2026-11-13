package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	relOld    = "1111111111111111111111111111111111111111"
	relNew    = "2222222222222222222222222222222222222222"
	relDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// releaseKit: a workspace-scoped once rule on issue.labels_changed, filtered to
// the `release` label — the P4c steward trigger under test.
type releaseKit struct {
	customEnv
	ises  *IssueService
	label string
	rule  string
}

func newReleaseKit(t *testing.T) releaseKit {
	t.Helper()
	k := newCustomEnv(t)
	ises := NewIssueService(k.f.q, k.f.Pool, nil, nil, nil)
	label, err := k.f.q.EnsureIssueLabelByName(context.Background(), db.EnsureIssueLabelByNameParams{WorkspaceID: parseTestUUID(t, k.f.WorkspaceID), Name: "release", Color: "#444444"})
	if err != nil {
		t.Fatalf("ensure label: %v", err)
	}
	k.f.Exec(t, "UPDATE workspace SET issue_counter=(SELECT COALESCE(MAX(number),0) FROM issue WHERE workspace_id=$1) WHERE id=$1", k.f.WorkspaceID)
	k.f.Cleanup(t, "DELETE FROM issue_to_label WHERE label_id IN (SELECT id FROM issue_label WHERE workspace_id=$1)", k.f.WorkspaceID)
	k.f.Cleanup(t, "DELETE FROM issue_label WHERE workspace_id=$1", k.f.WorkspaceID)
	k.f.Cleanup(t, "DELETE FROM github_release_deploy WHERE workspace_id=$1", k.f.WorkspaceID)
	labelID := uuidString(label.ID)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["issue.labels_changed"]`)+`,"mode":"once","filters":{"labels":["`+labelID+`"]}`)
	return releaseKit{customEnv: k, ises: ises, label: labelID, rule: view.RuleKey}
}

func (k releaseKit) deploy(t *testing.T, sha string) (issueID string, created bool) {
	t.Helper()
	issue, created, err := k.ises.CreateRelease(context.Background(), ReleaseDeploy{
		WorkspaceID: parseTestUUID(t, k.f.WorkspaceID), RepoOwner: "cheese-work", RepoName: "multica",
		RunID: 1, OldSHA: relOld, NewSHA: sha, Digest: relDigest,
	})
	if err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	return uuidString(issue.ID), created
}

func (k releaseKit) addLabel(t *testing.T, issue, label string) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO issue_to_label(issue_id,label_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, issue, label)
}

func (k releaseKit) otherLabel(t *testing.T, name string) string {
	t.Helper()
	l, err := k.f.q.EnsureIssueLabelByName(context.Background(), db.EnsureIssueLabelByNameParams{WorkspaceID: parseTestUUID(t, k.f.WorkspaceID), Name: name, Color: "#444444"})
	if err != nil {
		t.Fatal(err)
	}
	return uuidString(l.ID)
}

func TestCreateReleaseIssueShape(t *testing.T) {
	k := newReleaseKit(t)
	id, created := k.deploy(t, relNew)
	if !created || id == "" || strings.HasPrefix(id, "0000") {
		t.Fatalf("created = %v id = %q, want a new issue", created, id)
	}
	var title, desc string
	var labelled int
	k.f.Pool.QueryRow(context.Background(), `SELECT title, COALESCE(description,'') FROM issue WHERE id=$1`, id).Scan(&title, &desc)
	k.f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_to_label WHERE issue_id=$1 AND label_id=$2`, id, k.label).Scan(&labelled)
	if want := "release(multica): 1111111..2222222"; title != want {
		t.Fatalf("title = %q, want %q", title, want)
	}
	for _, want := range []string{relNew, relDigest, relOld} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %s:\n%s", want, desc)
		}
	}
	if labelled != 1 {
		t.Fatalf("release label attached %d times, want 1", labelled)
	}
}

func TestCreateReleaseDeduplicatesADeploy(t *testing.T) {
	k := newReleaseKit(t)
	first, created := k.deploy(t, relNew)
	again, createdAgain := k.deploy(t, relNew)
	if !created || createdAgain || first != again {
		t.Fatalf("first %s created=%v, again %s created=%v; want one issue", first, created, again, createdAgain)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue WHERE workspace_id=$1 AND title LIKE 'release(%'`, k.f.WorkspaceID); n != 1 {
		t.Fatalf("%d release issues, want 1", n)
	}
}

// A write that fails right after the issue exists (a crash between creating the
// issue and recording its id) must not lose the release or open a second one.
func TestCreateReleaseRecoversAfterAFailureBetweenCreateAndLink(t *testing.T) {
	k := newReleaseKit(t)
	ctx := context.Background()
	k.f.Exec(t, `CREATE OR REPLACE FUNCTION release_link_fail() RETURNS trigger AS $$ BEGIN
		IF NEW.issue_id IS NOT NULL THEN RAISE EXCEPTION 'simulated crash'; END IF;
		RETURN NEW; END $$ LANGUAGE plpgsql`)
	k.f.Exec(t, `CREATE TRIGGER release_link_fail BEFORE INSERT OR UPDATE ON github_release_deploy FOR EACH ROW EXECUTE FUNCTION release_link_fail()`)
	armed := true
	disarm := func() {
		if armed {
			armed = false
			k.f.Exec(t, `DROP TRIGGER release_link_fail ON github_release_deploy`)
		}
	}
	t.Cleanup(func() { disarm(); k.f.Exec(t, `DROP FUNCTION IF EXISTS release_link_fail()`) })

	d := ReleaseDeploy{WorkspaceID: parseTestUUID(t, k.f.WorkspaceID), RepoOwner: "cheese-work", RepoName: "multica", RunID: 1, OldSHA: relOld, NewSHA: relNew, Digest: relDigest}
	if _, _, err := k.ises.CreateRelease(ctx, d); err == nil {
		t.Fatal("first try: want the simulated failure")
	}
	disarm()

	issue, _, err := k.ises.CreateRelease(ctx, d)
	if err != nil || !issue.ID.Valid {
		t.Fatalf("retry = %v, %v; want the release issue", issue.ID.Valid, err)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue WHERE workspace_id=$1 AND title LIKE 'release(%'`, k.f.WorkspaceID); n != 1 {
		t.Fatalf("%d release issues, want 1", n)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM github_release_deploy WHERE workspace_id=$1 AND issue_id=$2 AND labeled_at IS NOT NULL`, k.f.WorkspaceID, uuidString(issue.ID)); n != 1 {
		t.Fatalf("claim row linked and labeled = %d, want 1", n)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue_to_label WHERE issue_id=$1 AND label_id=$2`, uuidString(issue.ID), k.label); n != 1 {
		t.Fatalf("release label rows = %d, want 1", n)
	}
}

// Concurrent redeliveries of one deploy: one issue, and every caller gets it.
func TestCreateReleaseConcurrentRedeliveryOpensOneIssue(t *testing.T) {
	k := newReleaseKit(t)
	d := ReleaseDeploy{WorkspaceID: parseTestUUID(t, k.f.WorkspaceID), RepoOwner: "cheese-work", RepoName: "multica", RunID: 1, OldSHA: relOld, NewSHA: relNew, Digest: relDigest}
	const n = 8
	var wg sync.WaitGroup
	ids, errs := make([]string, n), make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			issue, _, err := k.ises.CreateRelease(context.Background(), d)
			ids[i], errs[i] = uuidString(issue.ID), err
		}()
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] || !strings.Contains(ids[i], "-") {
			t.Fatalf("call %d = %q, %v; want the same issue id %q and no error", i, ids[i], errs[i], ids[0])
		}
	}
	if c := k.f.Count(t, `SELECT count(*) FROM issue WHERE workspace_id=$1 AND title LIKE 'release(%'`, k.f.WorkspaceID); c != 1 {
		t.Fatalf("%d release issues, want 1", c)
	}
}

func TestReleaseIssueGroupsMergedPullRequests(t *testing.T) {
	k := newReleaseKit(t)
	for i, pr := range [][2]string{{"feat(github): add x", "feat"}, {"fix(api): y", "fix"}, {"feat(cli): z", "cli"}, {"fix(skill): w", "skill"}, {"chore: v", "other"}} {
		k.f.Insert(t, "github_pull_request", testutil.Cols{
			"workspace_id": k.f.WorkspaceID, "installation_id": 1, "repo_owner": "cheese-work", "repo_name": "multica",
			"pr_number": 100 + i, "title": pr[0], "state": "merged", "html_url": "https://example.test/pr/" + pr[1],
			"merged_at": "now()", "pr_created_at": "now()", "pr_updated_at": "now()",
		})
	}
	k.f.Cleanup(t, "DELETE FROM github_pull_request WHERE workspace_id=$1", k.f.WorkspaceID)
	id, _ := k.deploy(t, relNew)
	var desc string
	k.f.Pool.QueryRow(context.Background(), `SELECT description FROM issue WHERE id=$1`, id).Scan(&desc)
	for _, want := range []string{"### feat", "### fix", "### CLI", "### skill", "feat(github): add x", "feat(cli): z"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q:\n%s", want, desc)
		}
	}
	// A CLI-scoped feat lists under CLI, not feat.
	if feat := section(desc, "### feat"); strings.Contains(feat, "feat(cli)") {
		t.Errorf("feat section holds the CLI PR:\n%s", feat)
	}
}

func section(desc, head string) string {
	i := strings.Index(desc, head)
	if i < 0 {
		return ""
	}
	rest := desc[i+len(head):]
	if j := strings.Index(rest, "### "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// T1: an issue the intake path creates wakes the steward exactly once.
func TestReleaseIssueT1CreatedIssueWakesStewardOnce(t *testing.T) {
	k := newReleaseKit(t)
	k.deploy(t, relNew)
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
}

// T2: another label change after the first run starts no run.
func TestReleaseIssueT2LaterLabelChangeStartsNoRun(t *testing.T) {
	k := newReleaseKit(t)
	id, _ := k.deploy(t, relNew)
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
	k.finish(t)
	k.addLabel(t, id, k.otherLabel(t, "other"))
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
}

// T3: removing and re-adding `release` starts no second run.
func TestReleaseIssueT3ReaddingReleaseStartsNoRun(t *testing.T) {
	k := newReleaseKit(t)
	id, _ := k.deploy(t, relNew)
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
	k.finish(t)
	k.f.Exec(t, `DELETE FROM issue_to_label WHERE issue_id=$1 AND label_id=$2`, id, k.label)
	k.tick(t)
	k.addLabel(t, id, k.label)
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
}

// T4: an issue without `release` starts no run.
func TestReleaseIssueT4IssueWithoutReleaseStartsNoRun(t *testing.T) {
	k := newReleaseKit(t)
	k.addLabel(t, k.issue, k.otherLabel(t, "other"))
	k.tick(t)
	k.wantRuns(t, k.rule, 0)
}

// T5: another label first, then `release`, wakes exactly once.
func TestReleaseIssueT5OtherLabelThenReleaseWakesOnce(t *testing.T) {
	k := newReleaseKit(t)
	k.addLabel(t, k.issue, k.otherLabel(t, "other"))
	k.tick(t)
	k.wantRuns(t, k.rule, 0)
	k.addLabel(t, k.issue, k.label)
	k.tick(t)
	k.wantRuns(t, k.rule, 1)
}
