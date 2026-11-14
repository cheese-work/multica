package ciwatch

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run Outcome

type outEnv struct {
	pool *pgxpool.Pool
	l    *Labeler
	ws   string
	repo string
	t0   time.Time
	now  time.Time
}

func newOutEnv(t *testing.T) *outEnv {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	e := &outEnv{pool: pool, ws: uuid.NewString(), repo: "r" + uuid.NewString()[:8],
		t0: time.Now().UTC().Truncate(time.Second).Add(-time.Hour)}
	e.now = e.t0.Add(90 * time.Second)
	e.l = NewLabeler(pool, func() time.Time { return e.now })
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM system1_observation WHERE workspace_id=$1`,
			`DELETE FROM ci_flake_increment WHERE workspace_id=$1`,
			`DELETE FROM ci_watch_action WHERE workspace_id=$1`,
			`DELETE FROM ci_signature_owner WHERE workspace_id=$1`,
			`DELETE FROM github_workflow_run WHERE workspace_id=$1`,
			`DELETE FROM issue WHERE workspace_id=$1`,
			`DELETE FROM github_pull_request WHERE workspace_id=$1`,
			`DELETE FROM workspace WHERE id=$1`,
		} {
			_, _ = pool.Exec(ctx, q, e.ws)
		}
		pool.Close()
	})
	return e
}

func (e *outEnv) workspace(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO workspace (id,name,slug) VALUES ($1,'w',$2)`, e.ws, e.ws); err != nil {
		t.Fatal(err)
	}
}

type obsSpec struct {
	runID     int64
	attempt   int32
	class     string
	choice    string
	sig       string
	sha       string
	at        time.Time
	predicted string
}

const wfPath = ".github/workflows/ci.yml"

func (e *outEnv) seed(t *testing.T, s obsSpec) string {
	t.Helper()
	in, _ := json.Marshal(map[string]any{
		"repo": "o/" + e.repo, "run_id": s.runID, "run_attempt": s.attempt, "head_sha": s.sha,
		"workflow_path": wfPath, "branch": "main", "signature_hash": s.sig,
	})
	var id string
	err := e.pool.QueryRow(context.Background(), `INSERT INTO system1_observation
		(workspace_id, kind, subject_id, class, decision_source, inputs, choice_key, predicted_key, created_at)
		VALUES ($1,'ci_event',$2,$3,'rule',$4,$5,NULLIF($6,''),$7) RETURNING id::text`,
		e.ws, subjectID(e.run(s.runID, s.attempt, "failure")).Bytes, s.class, in, s.choice, s.predicted, s.at).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *outEnv) run(id int64, attempt int32, conclusion string) Run {
	return Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: id, RunAttempt: attempt,
		WorkflowPath: wfPath, Event: "push", HeadBranch: "main", HeadSHA: "sha1", Conclusion: conclusion}
}

type obsRow struct {
	Label    string
	TTG      *int64
	Reversed bool
}

func (e *outEnv) row(t *testing.T, id string) obsRow {
	t.Helper()
	var r obsRow
	err := e.pool.QueryRow(context.Background(), `SELECT coalesce(outcome_label,''), time_to_green_ms, reversed_at IS NOT NULL
		FROM system1_observation WHERE id=$1`, id).Scan(&r.Label, &r.TTG, &r.Reversed)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *outEnv) addRerunAction(t *testing.T, runID int64, state string, expected int) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO ci_watch_action
		(workspace_id, repo_owner, repo_name, original_run_id, action_kind, state, expected_attempt)
		VALUES ($1,'o',$2,$3,'rerun',$4,$5)`, e.ws, e.repo, runID, state, expected)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *outEnv) flakes(t *testing.T, sig string) int {
	t.Helper()
	n, err := e.l.FlakeCount7d(context.Background(), e.ws, "o", e.repo, sig)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOutcomeSelfHealedAndTimeToGreen(t *testing.T) {
	e := newOutEnv(t)
	id := e.seed(t, obsSpec{runID: 1, attempt: 1, class: "code_or_test", choice: "P2", sig: "s1", sha: "sha1", at: e.t0})
	if err := e.l.RunCompleted(context.Background(), e.run(2, 1, "success")); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, id)
	if r.Label != LabelSelfHealed || r.TTG == nil || *r.TTG != 90000 {
		t.Fatalf("got %+v, want self_healed ttg 90000", r)
	}
	// a second green changes nothing
	e.now = e.now.Add(time.Minute)
	if err := e.l.RunCompleted(context.Background(), e.run(3, 1, "success")); err != nil {
		t.Fatal(err)
	}
	if r2 := e.row(t, id); r2.TTG == nil || *r2.TTG != 90000 || r2.Label != LabelSelfHealed {
		t.Fatalf("relabelled: %+v", r2)
	}
}

func TestOutcomeOtherWorkflowOrBranchIsNotLabelled(t *testing.T) {
	e := newOutEnv(t)
	id := e.seed(t, obsSpec{runID: 1, attempt: 1, class: "code_or_test", choice: "P2", sig: "s1", sha: "sha1", at: e.t0})
	r := e.run(2, 1, "success")
	r.WorkflowPath = ".github/workflows/other.yml"
	r2 := e.run(3, 1, "success")
	r2.HeadBranch = "dev"
	r3 := e.run(4, 1, "success")
	r3.RepoName = "other-" + e.repo
	for _, x := range []Run{r, r2, r3} {
		if err := e.l.RunCompleted(context.Background(), x); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.row(t, id); got.Label != "" || got.TTG != nil {
		t.Fatalf("labelled by an unrelated run: %+v", got)
	}
}

func TestOutcomeFixedByPR(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	id := e.seed(t, obsSpec{runID: 1, attempt: 1, class: "code_or_test", choice: "P2", sig: "s1", sha: "sha1", at: e.t0})
	e.workspace(t)
	var issue, pr string
	if err := e.pool.QueryRow(ctx, `INSERT INTO issue (workspace_id,title,creator_type,creator_id) VALUES ($1,'t','agent',$1) RETURNING id::text`, e.ws).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	mergedAt := e.t0.Add(30 * time.Second)
	if err := e.pool.QueryRow(ctx, `INSERT INTO github_pull_request (workspace_id,installation_id,repo_owner,repo_name,pr_number,title,state,html_url,merged_at,pr_created_at,pr_updated_at)
		VALUES ($1,1,'o',$2,7,'fix','merged','u',$3,now(),now()) RETURNING id::text`, e.ws, e.repo, mergedAt).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO issue_pull_request (issue_id,pull_request_id) VALUES ($1,$2)`, issue, pr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_signature_owner (workspace_id,repo_owner,repo_name,signature_hash,issue_id,state) VALUES ($1,'o',$2,'s1',$3,'open')`, e.ws, e.repo, issue); err != nil {
		t.Fatal(err)
	}
	if err := e.l.RunCompleted(ctx, e.run(2, 1, "success")); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, id); r.Label != LabelFixedByPR || r.TTG == nil || *r.TTG != 90000 {
		t.Fatalf("got %+v, want fixed_by_pr", r)
	}
}

func TestOutcomeMergedBeforeFailureIsNotAFix(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	id := e.seed(t, obsSpec{runID: 1, attempt: 1, class: "code_or_test", choice: "P2", sig: "s1", sha: "sha1", at: e.t0})
	e.workspace(t)
	var issue, pr string
	if err := e.pool.QueryRow(ctx, `INSERT INTO issue (workspace_id,title,creator_type,creator_id) VALUES ($1,'t','agent',$1) RETURNING id::text`, e.ws).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO github_pull_request (workspace_id,installation_id,repo_owner,repo_name,pr_number,title,state,html_url,merged_at,pr_created_at,pr_updated_at)
		VALUES ($1,1,'o',$2,7,'old','merged','u',$3,now(),now()) RETURNING id::text`, e.ws, e.repo, e.t0.Add(-time.Hour)).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	_, _ = e.pool.Exec(ctx, `INSERT INTO issue_pull_request (issue_id,pull_request_id) VALUES ($1,$2)`, issue, pr)
	_, _ = e.pool.Exec(ctx, `INSERT INTO ci_signature_owner (workspace_id,repo_owner,repo_name,signature_hash,issue_id,state) VALUES ($1,'o',$2,'s1',$3,'open')`, e.ws, e.repo, issue)
	if err := e.l.RunCompleted(ctx, e.run(2, 1, "success")); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, id); r.Label != LabelSelfHealed {
		t.Fatalf("got %+v, want self_healed", r)
	}
}

func TestOutcomeRerunGreenAndRed(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	green := e.seed(t, obsSpec{runID: 10, attempt: 1, class: "infra_transient", choice: "P1", sig: "g", sha: "sha1", at: e.t0})
	red := e.seed(t, obsSpec{runID: 20, attempt: 1, class: "infra_transient", choice: "P1", sig: "r", sha: "sha1", at: e.t0})
	e.addRerunAction(t, 10, StateDone, 2)
	e.addRerunAction(t, 20, StateAmbiguous, 2)
	if err := e.l.RunCompleted(ctx, e.run(10, 2, "success")); err != nil {
		t.Fatal(err)
	}
	if err := e.l.RunCompleted(ctx, e.run(20, 2, "failure")); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, green); r.Label != LabelRerunGreen || r.Reversed {
		t.Fatalf("green: %+v", r)
	}
	if r := e.row(t, red); r.Label != LabelRerunRed || r.Reversed {
		t.Fatalf("red: %+v", r)
	}
	if e.flakes(t, "g") != 1 || e.flakes(t, "r") != 0 {
		t.Fatalf("flakes g=%d r=%d, want 1 and 0", e.flakes(t, "g"), e.flakes(t, "r"))
	}
}

func TestOutcomeManualRerunIsHumanOverride(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	id := e.seed(t, obsSpec{runID: 10, attempt: 1, class: "code_or_test", choice: "P2", sig: "m", sha: "sha1", at: e.t0})
	// the watcher's rerun was cancelled because a person got there first
	e.addRerunAction(t, 10, StateCancelled, 2)
	if err := e.l.RunCompleted(ctx, e.run(10, 2, "success")); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, id)
	if r.Label != LabelHumanOverride || !r.Reversed || r.TTG == nil {
		t.Fatalf("got %+v, want human_override with reversed_at and ttg", r)
	}
	if e.flakes(t, "m") != 1 {
		t.Fatalf("a manual green rerun counts for the flake counter")
	}
}

func TestOutcomeRateLimitRerunDoesNotCountAsFlake(t *testing.T) {
	e := newOutEnv(t)
	e.seed(t, obsSpec{runID: 10, attempt: 1, class: "infra_rate_limit", choice: "P1", sig: "rl", sha: "sha1", at: e.t0})
	e.addRerunAction(t, 10, StateDone, 2)
	if err := e.l.RunCompleted(context.Background(), e.run(10, 2, "success")); err != nil {
		t.Fatal(err)
	}
	if n := e.flakes(t, "rl"); n != 0 {
		t.Fatalf("rate-limit rerun counted: %d", n)
	}
}

func TestOutcomeGreenRerunOnNewCommitIsNotAFlake(t *testing.T) {
	e := newOutEnv(t)
	e.seed(t, obsSpec{runID: 10, attempt: 1, class: "code_or_test", choice: "P2", sig: "nc", sha: "sha1", at: e.t0})
	e.addRerunAction(t, 10, StateDone, 2)
	r := e.run(10, 2, "success")
	r.HeadSHA = "sha2"
	if err := e.l.RunCompleted(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if n := e.flakes(t, "nc"); n != 0 {
		t.Fatalf("new commit counted: %d", n)
	}
}

// Third failure-then-green in 7 days: the counter reaches 3 and Classify
// returns flaky for the next failure with that signature.
func TestOutcomeFlakeCounterThirdIncrementClassifiesFlaky(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	ev := &FailureEvent{Conclusion: "failure", Evidence: EvidenceFull, SignatureHash: "flk", FailedJobCount: 1,
		Jobs: []JobEvidence{{Name: "test", Excerpt: "--- FAIL: TestX"}}}
	if d := Classify(ev, History{FlakeCount7d: e.flakes(t, "flk")}); d.Class != ClassCode {
		t.Fatalf("before any flake: %s", d.Class)
	}
	for i := int64(1); i <= 3; i++ {
		e.seed(t, obsSpec{runID: i, attempt: 1, class: "code_or_test", choice: "P2", sig: "flk", sha: "sha1", at: e.t0})
		e.addRerunAction(t, i, StateDone, 2)
		if err := e.l.RunCompleted(ctx, e.run(i, 2, "success")); err != nil {
			t.Fatal(err)
		}
		if got := e.flakes(t, "flk"); got != int(i) {
			t.Fatalf("after green rerun %d counter = %d", i, got)
		}
		if i < 3 {
			if d := Classify(ev, History{FlakeCount7d: e.flakes(t, "flk")}); d.Class == ClassFlaky {
				t.Fatalf("flaky too early at %d", i)
			}
		}
	}
	if d := Classify(ev, History{FlakeCount7d: e.flakes(t, "flk")}); d.Class != ClassFlaky || d.Protocol != "P3" {
		t.Fatalf("after 3 increments: %s/%s, want flaky/P3", d.Class, d.Protocol)
	}
	// redelivered webhook: no double count
	if err := e.l.RunCompleted(ctx, e.run(3, 2, "success")); err != nil {
		t.Fatal(err)
	}
	if got := e.flakes(t, "flk"); got != 3 {
		t.Fatalf("redelivery counted: %d", got)
	}
	// increments older than 7 days drop out
	if _, err := e.pool.Exec(ctx, `UPDATE ci_flake_increment SET created_at = now() - interval '8 days' WHERE workspace_id=$1 AND ref='1'`, e.ws); err != nil {
		t.Fatal(err)
	}
	if got := e.flakes(t, "flk"); got != 2 {
		t.Fatalf("old increment still counted: %d", got)
	}
}

func TestOutcomeShaFlipsCountOnceAtTwoFlips(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	e.seed(t, obsSpec{runID: 1, attempt: 1, class: "unknown", choice: "P2", sig: "fl", sha: "sha1", at: e.t0})
	add := func(runID int64, concl string, at time.Time) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `INSERT INTO github_workflow_run (workspace_id,installation_id,repo_owner,repo_name,run_id,run_attempt,status,conclusion,workflow_path,head_sha,head_branch,created_at)
			VALUES ($1,1,'o',$2,$3,1,'completed',$4,$5,'sha1','main',$6)`, e.ws, e.repo, runID, concl, wfPath, at); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(runID int64, concl string) {
		t.Helper()
		if err := e.l.RunCompleted(ctx, e.run(runID, 1, concl)); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "failure", e.t0)
	add(2, "success", e.t0.Add(time.Minute)) // flip 1
	complete(2, "success")
	if got := e.flakes(t, "fl"); got != 0 {
		t.Fatalf("one flip counted: %d", got)
	}
	add(3, "failure", e.t0.Add(2*time.Minute)) // flip 2
	complete(3, "failure")
	if got := e.flakes(t, "fl"); got != 1 {
		t.Fatalf("after 2 flips counter = %d, want 1", got)
	}
	add(4, "success", e.t0.Add(3*time.Minute)) // flip 3: same sha, no second increment
	complete(4, "success")
	if got := e.flakes(t, "fl"); got != 1 {
		t.Fatalf("third flip counted again: %d", got)
	}
}

func TestOutcomeOverrideByIssue(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	id := e.seed(t, obsSpec{runID: 1, attempt: 1, class: "code_or_test", choice: "P2", sig: "ov", sha: "sha1", at: e.t0})
	other := e.seed(t, obsSpec{runID: 2, attempt: 1, class: "code_or_test", choice: "P2", sig: "zz", sha: "sha1", at: e.t0})
	e.workspace(t)
	var issue string
	if err := e.pool.QueryRow(ctx, `INSERT INTO issue (workspace_id,title,creator_type,creator_id) VALUES ($1,'t','agent',$1) RETURNING id::text`, e.ws).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	_, _ = e.pool.Exec(ctx, `INSERT INTO ci_signature_owner (workspace_id,repo_owner,repo_name,signature_hash,issue_id,state) VALUES ($1,'o',$2,'ov',$3,'open')`, e.ws, e.repo, issue)
	if err := e.l.OverrideByIssue(ctx, e.ws, issue); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, id); r.Label != LabelHumanOverride || !r.Reversed {
		t.Fatalf("got %+v", r)
	}
	if r := e.row(t, other); r.Label != "" || r.Reversed {
		t.Fatalf("other signature touched: %+v", r)
	}
	// a later green keeps the override label but still records time to green
	if err := e.l.RunCompleted(ctx, e.run(9, 1, "success")); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, id); r.Label != LabelHumanOverride || r.TTG == nil {
		t.Fatalf("after green: %+v", r)
	}
}

func TestOutcomeSetByRun(t *testing.T) {
	e := newOutEnv(t)
	id := e.seed(t, obsSpec{runID: 5, attempt: 1, class: "superseded", choice: "P0", sig: "", sha: "sha1", at: e.t0})
	if err := e.l.Set(context.Background(), e.run(5, 1, "failure"), LabelSelfHealed); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, id); r.Label != LabelSelfHealed {
		t.Fatalf("got %+v", r)
	}
}

func TestOutcomeShadowReport(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	in := e.t0.Add(time.Minute)
	e.seed(t, obsSpec{runID: 1, attempt: 1, class: "infra_rate_limit", choice: "P1", sig: "a", sha: "x", at: in, predicted: "P1"})
	e.seed(t, obsSpec{runID: 2, attempt: 1, class: "infra_rate_limit", choice: "P1", sig: "a", sha: "x", at: in, predicted: "P4"})
	e.seed(t, obsSpec{runID: 3, attempt: 1, class: "code_or_test", choice: "P2", sig: "b", sha: "x", at: in})
	e.seed(t, obsSpec{runID: 4, attempt: 1, class: "owned_elsewhere", choice: "P0", sig: "", sha: "x", at: in})
	old := e.seed(t, obsSpec{runID: 5, attempt: 1, class: "code_or_test", choice: "P2", sig: "old", sha: "x", at: e.t0.Add(-48 * time.Hour)})
	_ = old
	for i := 0; i < 6; i++ { // 6 more signatures so the top list must cut at 5
		e.seed(t, obsSpec{runID: int64(100 + i), attempt: 1, class: "unknown", choice: "P2", sig: string(rune('c' + i)), sha: "x", at: in})
	}
	if _, err := e.pool.Exec(ctx, `UPDATE system1_observation SET reversed_at=now(), outcome_label='human_override' WHERE workspace_id=$1 AND inputs->>'signature_hash'='b'`, e.ws); err != nil {
		t.Fatal(err)
	}
	for i, reason := range []string{"cap", "cap", "breaker"} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO ci_watch_action (workspace_id,repo_owner,repo_name,original_run_id,action_kind,state,reason,updated_at)
			VALUES ($1,'o',$2,$3,'rerun','cancelled',$4,$5)`, e.ws, e.repo, 1000+i, reason, in); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := e.l.ShadowReport(ctx, e.ws, e.t0, e.t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Events != 10 {
		t.Errorf("Events = %d, want 10", rep.Events)
	}
	if rep.ClassMix["infra_rate_limit"] != 2 || rep.ClassMix["owned_elsewhere"] != 1 || rep.ClassMix["unknown"] != 6 {
		t.Errorf("ClassMix = %v", rep.ClassMix)
	}
	if want := map[string]int{"P0": 1, "P1": 2, "P2": 7}; !reflect.DeepEqual(rep.ProtocolMix, want) {
		t.Errorf("ProtocolMix = %v, want %v", rep.ProtocolMix, want)
	}
	if rep.JevPredicted != 2 || rep.JevAgreed != 1 {
		t.Errorf("Jev predicted/agreed = %d/%d, want 2/1", rep.JevPredicted, rep.JevAgreed)
	}
	if rep.Overrides != 1 {
		t.Errorf("Overrides = %d, want 1", rep.Overrides)
	}
	if rep.Capped != 2 || rep.Cancelled["cap"] != 2 || rep.Cancelled["breaker"] != 1 {
		t.Errorf("Capped %d Cancelled %v", rep.Capped, rep.Cancelled)
	}
	if len(rep.TopSignatures) != 5 || rep.TopSignatures[0] != (SignatureCount{"a", 2}) {
		t.Errorf("TopSignatures = %v", rep.TopSignatures)
	}
}

// Observe records the keys the labeler joins on, and labels a superseded run.
func TestOutcomeObserveRecordsJoinKeysAndLabelsSuperseded(t *testing.T) {
	w := &fakeObs{}
	set := &fakeSetter{}
	run := obsRun()
	run.HeadSHA = "deadbeef"
	err := Observer{W: w, Labels: set}.Observe(context.Background(), run, Result{Kind: KindP0, Reason: ReasonSuperseded, Label: LabelSelfHealed}, History{})
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	_ = json.Unmarshal(w.got[0].Inputs, &in)
	for k, want := range map[string]any{"repo": "cheese-work/multica", "run_id": float64(run.RunID), "run_attempt": float64(1), "head_sha": "deadbeef"} {
		if in[k] != want {
			t.Errorf("inputs[%s] = %v, want %v", k, in[k], want)
		}
	}
	if set.label != LabelSelfHealed {
		t.Errorf("label set = %q, want self_healed", set.label)
	}
}

type fakeSetter struct{ label string }

func (f *fakeSetter) Set(_ context.Context, _ Run, label string) error { f.label = label; return nil }
