package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghstack"
)

type recEnqueuer struct {
	mu   sync.Mutex
	jobs []ghstack.Job
}

func (r *recEnqueuer) Enqueue(j ghstack.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = append(r.jobs, j)
}

func (r *recEnqueuer) numbers() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int
	for _, j := range r.jobs {
		out = append(out, j.PR)
	}
	sort.Ints(out)
	return out
}

func stackPRBody(action string, number int, installationID int64, base string, merged bool) map[string]any {
	body := buildMergedPRWebhookBody("CHE-1", number, "acme", "widgets", installationID)
	body["action"] = action
	pr := body["pull_request"].(map[string]any)
	pr["base"] = map[string]any{"ref": base}
	pr["head"].(map[string]any)["ref"] = "feat/b"
	state := "open"
	if action == "closed" {
		state = "closed"
	}
	pr["state"], pr["merged"] = state, merged
	if !merged {
		pr["merged_at"] = nil
	}
	return body
}

func stackTriggerHandler(t *testing.T, installationID int64, enq StackAutolinkEnqueuer) *Handler {
	t.Helper()
	seedAllowlistRow(t, installationID, "acme", nil)
	h, _ := allowlistWebhookHandler(t, "")
	h.StackAutolink = enq
	return h
}

func deliverPR(t *testing.T, h *Handler, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	if err := h.handlePullRequestEvent(context.Background(), raw); err != nil {
		t.Fatalf("handlePullRequestEvent(%v): %v", body["action"], err)
	}
}

func TestStackTriggerEnqueuesOneJobPerAction(t *testing.T) {
	const inst = int64(41670001)
	rec := &recEnqueuer{}
	h := stackTriggerHandler(t, inst, rec)
	for i, action := range []string{"opened", "reopened", "ready_for_review", "synchronize"} {
		n := 100 + i
		deliverPR(t, h, stackPRBody(action, n, inst, "feat/a", false))
		if got := rec.numbers(); len(got) != i+1 || got[len(got)-1] < 100 {
			t.Fatalf("%s: jobs = %v, want exactly one more", action, got)
		}
	}
	rec.mu.Lock()
	want := ghstack.Job{InstallationID: inst, Owner: "acme", Repo: "widgets", PR: 100}
	got := rec.jobs[0]
	rec.mu.Unlock()
	if got != want {
		t.Fatalf("first job = %+v, want %+v", got, want)
	}
}

func TestStackTriggerEditedOnlyOnBaseChange(t *testing.T) {
	const inst = int64(41670002)
	rec := &recEnqueuer{}
	h := stackTriggerHandler(t, inst, rec)
	title := stackPRBody("edited", 110, inst, "feat/a", false)
	title["changes"] = map[string]any{"title": map[string]any{"from": "old"}}
	deliverPR(t, h, title)
	if len(rec.numbers()) != 0 {
		t.Fatalf("a title edit enqueued %v", rec.numbers())
	}
	base := stackPRBody("edited", 111, inst, "feat/a", false)
	base["changes"] = map[string]any{"base": map[string]any{"ref": map[string]any{"from": "main"}}}
	deliverPR(t, h, base)
	if got := rec.numbers(); len(got) != 1 || got[0] != 111 {
		t.Fatalf("base edit jobs = %v, want [111]", got)
	}
}

func TestStackTriggerIgnoresOtherActions(t *testing.T) {
	const inst = int64(41670003)
	rec := &recEnqueuer{}
	h := stackTriggerHandler(t, inst, rec)
	for _, action := range []string{"labeled", "assigned", "converted_to_draft"} {
		deliverPR(t, h, stackPRBody(action, 120, inst, "feat/a", false))
	}
	deliverPR(t, h, stackPRBody("closed", 121, inst, "feat/a", false)) // closed, not merged
	if len(rec.numbers()) != 0 {
		t.Fatalf("jobs = %v, want none", rec.numbers())
	}
}

// A merge re-evaluates the open layers above it (through refreshOpenPRsOnBase),
// not the merged PR itself and not an unrelated PR.
func TestStackTriggerMergeReevaluatesLayersAbove(t *testing.T) {
	const inst = int64(41670004)
	rec := &recEnqueuer{}
	h := stackTriggerHandler(t, inst, rec)
	seed := func(n int, state, base string) {
		t.Helper()
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, snapshot_base_ref, pr_created_at, pr_updated_at)
			 VALUES ($1::uuid,$2,'acme','widgets',$3,'t',$4,'https://x',$5,now(),now())`,
			testWorkspaceID, inst, n, state, base); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE installation_id=$1`, inst)
	})
	seed(131, "open", "feat/b")   // above the merged PR (base = its head)
	seed(132, "open", "main")     // on the merged PR's base
	seed(133, "open", "other")    // unrelated
	seed(134, "closed", "feat/b") // not open
	deliverPR(t, h, stackPRBody("closed", 130, inst, "main", true))
	if got := rec.numbers(); len(got) != 2 || got[0] != 131 || got[1] != 132 {
		t.Fatalf("jobs = %v, want [131 132]", got)
	}
}

type failingRunner struct{ ran chan struct{} }

func (f failingRunner) Run(context.Context, ghstack.Job) error {
	f.ran <- struct{}{}
	panic("stacks API exploded")
}

type linkedAll struct{}

func (linkedAll) IsLinked(context.Context, ghstack.Job) (bool, error) { return true, nil }
func (linkedAll) ListLinkedStacked(context.Context, ghstack.Job, int) ([]ghstack.Job, error) {
	return nil, nil
}

// A stacks API failure (here the worst case, a panic) never changes the
// webhook response.
func TestStackTriggerFailureDoesNotChangeWebhookResponse(t *testing.T) {
	const inst = int64(41670005)
	ran := make(chan struct{}, 1)
	d := &ghstack.Dispatcher{Runner: failingRunner{ran}, Links: linkedAll{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.Start(ctx)
	h := stackTriggerHandler(t, inst, d)
	code := postAllowlistWebhook(t, h, "pull_request", "che4167-fail-"+strconv.Itoa(int(inst)), stackPRBody("opened", 140, inst, "feat/a", false))
	t.Cleanup(func() { _ = intakeRows(t, "che4167-fail-"+strconv.Itoa(int(inst))) })
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("the link job never ran")
	}
}
