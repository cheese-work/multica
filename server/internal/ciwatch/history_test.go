package ciwatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

// Observer.Hist feeds the counts a pipeline-only caller cannot know into the
// stored features.
func TestObserverStoresLoadedHistory(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{}
	p := pipelineFor(ev, ob, failingRunGH())
	var gotSig string
	p.Observer.Hist = func(_ context.Context, _ Run, sig string) (History, error) {
		gotSig = sig
		return History{SameSignature24h: 2, SameSignature7d: 5, Last10: []string{"failure", "success"}, OwnerIssueOpen: true}, nil
	}
	if _, err := p.Process(context.Background(), realInput(), History{}); err != nil {
		t.Fatal(err)
	}
	if len(gotSig) != 64 {
		t.Fatalf("history loader asked for signature %q", gotSig)
	}
	for _, r := range ob.rows {
		var f map[string]any
		if err := json.Unmarshal(r.Inputs, &f); err != nil {
			t.Fatal(err)
		}
		if f["same_signature_24h"] != 2.0 || f["same_signature_7d"] != 5.0 || f["owner_issue_open"] != true ||
			!reflect.DeepEqual(f["last_10_conclusions"], []any{"failure", "success"}) {
			t.Fatalf("features do not carry the loaded history: %v", f)
		}
	}
}

func TestObserverHistoryErrorFailsTheDelivery(t *testing.T) {
	p := pipelineFor(&memEvents{}, &memObs{}, failingRunGH())
	p.Observer.Hist = func(context.Context, Run, string) (History, error) { return History{}, errors.New("db down") }
	if _, err := p.Process(context.Background(), realInput(), History{}); err == nil {
		t.Fatal("a failed history lookup must fail the delivery so it is redelivered")
	}
}

// Needs a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run LabelerHistory
func TestLabelerHistory(t *testing.T) {
	e := newOutEnv(t)
	ctx := context.Background()
	e.workspace(t)
	const sig = "sig-history"
	insertEvent := func(runID int64, signature string, age time.Duration) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, signature_hash, created_at)
			VALUES ($1,'o',$2,$3,1,$4,$5)`, e.ws, e.repo, runID, signature, e.now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.pool.Exec(ctx, `DELETE FROM ci_failure_event WHERE workspace_id=$1`, e.ws) })
	}
	insertEvent(1, sig, time.Hour)           // 24h and 7d
	insertEvent(2, sig, 3*24*time.Hour)      // 7d only
	insertEvent(3, sig, 10*24*time.Hour)     // outside both
	insertEvent(4, "other-sig", time.Minute) // another signature
	insertEvent(5, sig, time.Minute)         // this very run: not counted

	// 12 mirrored runs of the workflow; the 10 newest are reported newest first.
	for i := int64(1); i <= 12; i++ {
		concl := "success"
		if i%2 == 0 {
			concl = "failure"
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO github_workflow_run (workspace_id, installation_id, repo_owner, repo_name, run_id, run_attempt, status_rank, conclusion, workflow_path, head_branch, created_at)
			VALUES ($1,1,'o',$2,$3,1,2,$4,$5,'main',$6)`, e.ws, e.repo, 100+i, concl, wfPath, e.now.Add(-time.Duration(20-i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// A run of another workflow must not show up.
	if _, err := e.pool.Exec(ctx, `INSERT INTO github_workflow_run (workspace_id, installation_id, repo_owner, repo_name, run_id, run_attempt, status_rank, conclusion, workflow_path, head_branch, created_at)
		VALUES ($1,1,'o',$2,200,1,2,'cancelled','.github/workflows/other.yml','main',$3)`, e.ws, e.repo, e.now); err != nil {
		t.Fatal(err)
	}

	run := e.run(5, 1, "failure")
	got, err := e.l.History(ctx, run, sig)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"failure", "success", "failure", "success", "failure", "success", "failure", "success", "failure", "success"}
	if got.SameSignature24h != 1 || got.SameSignature7d != 2 || !reflect.DeepEqual(got.Last10, want) || got.OwnerIssueOpen {
		t.Fatalf("got %+v", got)
	}

	// An open owner issue flips OwnerIssueOpen; a closed one does not.
	var issue string
	if err := e.pool.QueryRow(ctx, `INSERT INTO issue (workspace_id,title,creator_type,creator_id) VALUES ($1,'t','agent',$1) RETURNING id::text`, e.ws).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_signature_owner (workspace_id,repo_owner,repo_name,signature_hash,issue_id,state) VALUES ($1,'o',$2,$3,$4,'closed')`, e.ws, e.repo, sig, issue); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.l.History(ctx, run, sig); got.OwnerIssueOpen {
		t.Fatal("a closed owner must not count as open")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE ci_signature_owner SET state='open' WHERE workspace_id=$1`, e.ws); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.l.History(ctx, run, sig); !got.OwnerIssueOpen {
		t.Fatal("an open owner must count as open")
	}
}

// Replay of run 42164833838 (the rate-limit failure that motivated the
// watcher) from its real job log: class and protocol only, no action.
func TestReplayRun42164833838ClassifiesAsRateLimit(t *testing.T) {
	gh := &fakeGH{jobsByPage: [][]Job{{failedJob(127718756968, "cache-policy", t0)}},
		logs: map[int64]func(context.Context) io.Reader{127718756968: text(rateLimitLog)}}
	ev, ob := &memEvents{}, &memObs{}
	p := pipelineFor(ev, ob, gh)
	in := realInput()
	if _, err := p.Process(context.Background(), in, History{}); err != nil {
		t.Fatal(err)
	}
	if len(ob.rows) != 1 {
		t.Fatalf("%d observations, want 1", len(ob.rows))
	}
	for _, r := range ob.rows {
		if r.Class != string(ClassRateLimit) || r.ChoiceKey != "P1" {
			t.Fatalf("class=%q protocol=%q, want infra_rate_limit P1", r.Class, r.ChoiceKey)
		}
	}
}
