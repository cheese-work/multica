package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3580 (layer 2 of CHE-3250): an agent may change workflow step
// instructions when the request carries a PASS receipt from one reviewer of
// another lab: receipt_issue, before_sha256, after_sha256, no decision_id.
// Hashes are the workflowStepsHash recipe; the receipt line names
// workflow.instructions:<workflow id>.

type workflowReceiptFixture struct {
	voteFixture
	workflowID, label, before, reviewerID string
	line                                  string
	steps                                 []any
	after                                 string
}

// openReceiptDelegation is the owner's setting that lets a receipt alone write:
// requires_human_decision stored as an explicit false.
func openReceiptDelegation() receiptFixture { return receiptFixture{humanDecision: "false"} }

func newWorkflowReceiptFixture(t *testing.T, initial string) workflowReceiptFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Receipt write "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "handler": map[string]any{"type": "member", "id": testUserID}, "instructions": initial},
		{"status_key": "blocked"},
	})
	r := workflowReceiptFixture{voteFixture: newVoteFixture(t), workflowID: wf.ID}
	r.reviewerID = r.leaderID
	r.label = "workflow.instructions:" + wf.ID
	r.before = canonicalStepsHash(t, wf.ID, liveSteps(t, wf.ID))
	r.propose(t, setInstructions("agent text"))
	openReceiptDelegation().delegate(t, r.writerID, "workflow.instructions")
	return r
}

// propose sets the steps the writer will send and the PASS line that signs them.
func (r *workflowReceiptFixture) propose(t *testing.T, edit func(map[string]any)) {
	t.Helper()
	r.steps = editedSteps(t, r.workflowID, "in_progress", edit)
	r.after = canonicalStepsHash(t, r.workflowID, r.steps)
	r.line = "Vote: PASS " + r.label + " " + r.after + " from " + r.before
}

// review posts the proposal (writer) and the reviewer's PASS line.
func (r workflowReceiptFixture) review(t *testing.T) {
	t.Helper()
	r.say(t, "agent", r.writerID, 60, "Proposal: "+r.label+" "+r.after)
	r.say(t, "agent", r.reviewerID, 30, r.line)
}

func (r workflowReceiptFixture) body(mutate ...func(map[string]any)) map[string]any {
	body := map[string]any{"steps": r.steps, "receipt_issue": r.issueID, "before_sha256": r.before, "after_sha256": r.after}
	for _, m := range mutate {
		m(body)
	}
	return body
}

func (r workflowReceiptFixture) agent(path string, body map[string]any, params ...string) *http.Request {
	req := withURLParams(workflowRequest("PATCH", path, body), append([]string{"id", r.workflowID}, params...)...)
	req.Header.Set("X-Actor-Source", "task_token")
	return asAgentActor(req, r.writerID, r.taskID)
}

func (r workflowReceiptFixture) patch(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateIssueWorkflow, r.agent("/api/issue-workflows/"+r.workflowID, body))
}

func (r workflowReceiptFixture) stored(t *testing.T) string {
	t.Helper()
	for _, raw := range liveSteps(t, r.workflowID) {
		if step := raw.(map[string]any); step["status_key"] == "in_progress" {
			return step["instructions"].(string)
		}
	}
	t.Fatal("workflow has no in_progress step")
	return ""
}

func (r workflowReceiptFixture) used(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM governed_pass_receipt WHERE target_id = $1`, r.workflowID)
}

func (r workflowReceiptFixture) wantRefused(t *testing.T, resp *testutil.Response, initial string) {
	t.Helper()
	if resp.Code != http.StatusForbidden && resp.Code != http.StatusConflict {
		t.Fatalf("expected 403 or 409, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := r.stored(t); got != initial {
		t.Fatalf("instructions must stay %q, got %q", initial, got)
	}
	if r.used(t) != 0 {
		t.Fatal("refused write consumed the receipt")
	}
}

func TestWorkflowPassReceipt_FailedChecksStayRefused(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *workflowReceiptFixture)
		mut   func(map[string]any)
	}{
		{name: "reviewer is of the writer's lab", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.retask(t, r.reviewerID, r.runtimeOf(t, r.writerID), "claude-test")
			r.review(t)
		}},
		{name: "writer votes for itself", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.reviewerID = r.writerID
			r.review(t)
		}},
		{name: "reviewer lab unknown", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.retask(t, r.reviewerID, r.runtimeOf(t, r.reviewerID), "gpt-unlisted")
			r.review(t)
		}},
		{name: "writer lab unknown", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.retask(t, r.writerID, r.runtimeOf(t, r.writerID), "claude-unlisted")
			r.review(t)
		}},
		{name: "stale before digest", setup: func(t *testing.T, r *workflowReceiptFixture) { r.review(t) },
			mut: func(b map[string]any) { b["before_sha256"] = testDigest("not the live workflow") }},
		{name: "wrong after digest", setup: func(t *testing.T, r *workflowReceiptFixture) { r.review(t) },
			mut: func(b map[string]any) { b["after_sha256"] = testDigest("something else") }},
		{name: "receipt names another before digest", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.line = "Vote: PASS " + r.label + " " + r.after + " from " + testDigest("older workflow")
			r.review(t)
		}},
		{name: "PASS before the proposal", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.say(t, "agent", r.reviewerID, 90, r.line)
			r.say(t, "agent", r.writerID, 60, "Proposal: "+r.label+" "+r.after)
		}},
		{name: "no proposal", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.say(t, "agent", r.reviewerID, 30, r.line)
		}},
		{name: "BLOCK present", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.review(t)
			r.say(t, "agent", r.peerID, 10, "Vote: BLOCK "+r.label+" "+r.after)
		}},
		{name: "receipt for another workflow", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.label = "workflow.instructions:00000000-0000-0000-0000-000000000000"
			r.line = "Vote: PASS " + r.label + " " + r.after + " from " + r.before
			r.review(t)
		}},
		{name: "member PASS does not count", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.say(t, "agent", r.writerID, 60, "Proposal: "+r.label+" "+r.after)
			r.say(t, "member", testUserID, 30, r.line)
		}},
		{name: "writer not in allowed_agents", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.review(t)
			openReceiptDelegation().delegate(t, r.peerID, "workflow.instructions")
		}},
		{name: "field not in allowed_fields", setup: func(t *testing.T, r *workflowReceiptFixture) {
			r.review(t)
			openReceiptDelegation().delegate(t, r.writerID, "project.description")
		}},
		{name: "receipt and decision together", setup: func(t *testing.T, r *workflowReceiptFixture) { r.review(t) },
			mut: func(b map[string]any) { b["decision_id"] = "00000000-0000-0000-0000-000000000000" }},
		{name: "receipt issue missing", setup: func(t *testing.T, r *workflowReceiptFixture) { r.review(t) },
			mut: func(b map[string]any) { delete(b, "receipt_issue") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newWorkflowReceiptFixture(t, "")
			c.setup(t, &r)
			var mut []func(map[string]any)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			resp := r.patch(t, r.body(mut...))
			if resp.Code == http.StatusBadRequest && c.name == "receipt and decision together" {
				return
			}
			r.wantRefused(t, resp, "")
		})
	}
}

// A structural edit is workflow.steps: a receipt cannot delegate it, even when
// the writer is delegated workflow.steps and the receipt names that field.
func TestWorkflowPassReceipt_StepsFieldRefused(t *testing.T) {
	r := newWorkflowReceiptFixture(t, "")
	r.propose(t, func(step map[string]any) { step["next_status_key"] = "done" })
	r.label = "workflow.steps:" + r.workflowID
	r.line = "Vote: PASS " + r.label + " " + r.after + " from " + r.before
	openReceiptDelegation().delegate(t, r.writerID, "workflow.steps")
	r.review(t)
	r.wantRefused(t, r.patch(t, r.body()), "")
}

func TestWorkflowPassReceipt_HumanBlockChangeRefused(t *testing.T) {
	r := newWorkflowReceiptFixture(t, protectedStart)
	r.propose(t, setInstructions(strings.Replace(protectedStart, "Never merge.", "Always merge.", 1)))
	r.review(t)
	r.wantRefused(t, r.patch(t, r.body()), protectedStart)
}

// requires_human_decision is the owner's policy: true or omitted keeps the
// receipt-only write closed on both workflow routes; only explicit false opens it.
func TestWorkflowPassReceipt_RequiresHumanDecision(t *testing.T) {
	for _, extra := range []string{`"requires_human_decision": true`, ""} {
		for _, route := range []string{"PATCH", "PUT handoff"} {
			t.Run(route+" "+extra, func(t *testing.T) {
				r := newWorkflowReceiptFixture(t, "")
				r.review(t)
				receiptFixture{}.delegateWith(t, r.writerID, "workflow.instructions", extra)
				var resp *testutil.Response
				if route == "PATCH" {
					resp = r.patch(t, r.body())
				} else {
					var step map[string]any
					for _, raw := range r.steps {
						if s := raw.(map[string]any); s["status_key"] == "in_progress" {
							step = s
						}
					}
					for k, v := range r.body() {
						if k != "steps" {
							step[k] = v
						}
					}
					req := r.agent("/api/issue-workflows/"+r.workflowID+"/handoffs/in_progress", step, "status", "in_progress")
					req.Method = http.MethodPut
					resp = testutil.Call(t, testHandler.SetIssueWorkflowHandoff, req)
				}
				r.wantRefused(t, resp, "")
			})
		}
	}
}
