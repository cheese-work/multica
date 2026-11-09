package handler

import "testing"

// retractionState is the retraction columns of the answers on the fixture's request.
func (f episodeFixture) retractionState(t *testing.T) (delivered bool, outcome string) {
	t.Helper()
	var out *string
	dbfx.QueryRow(t, `SELECT retraction_delivered_at IS NOT NULL, retraction_outcome FROM decision_answer
		WHERE decision_request_id = $1 AND disclaimed_at IS NOT NULL`, f.requestID).Scan(&delivered, &out)
	if out != nil {
		outcome = *out
	}
	return delivered, outcome
}

// Design §4.5.2 and §9.4 trace E′ (reduced to the asking agent as recipient): an
// answer delivered before the episode is withdrawn after the system close. The
// retraction stays pending while the recipient has no runtime, and one delivery
// follows the rebind. The system close never skips it.
func TestDecisionEpisodeRetractionAfterNoResumeAgentIsPendingThenOneDelivery(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, f.agentID).Scan(&runtimeID)
	t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET runtime_id = $2 WHERE id = $1`, f.agentID, runtimeID) })
	dbfx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, f.agentID)
	f.deliverEpisodeOnly(t)
	if closed := f.closedEpisodes(t); len(closed) != 1 || closed[0].reason != "no_resume_agent" {
		t.Fatalf("closed episodes = %+v, want one closed no_resume_agent", closed)
	}

	// The blocked step hands the issue to the agent that clears blocks (Tofu in
	// production); here a stand-in without a runtime binding yet.
	tofu := dbfx.Agent(t, "Block owner "+workflowTestSuffix(), "")
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, f.issueID, tofu)
	f.pendingRetraction(t, false)
	runner := newDeliveryRunner(t)
	for range 3 {
		decisionPass(t, runner)
	}
	if delivered, outcome := f.retractionState(t); delivered || outcome != "" {
		t.Fatalf("while unbound: retraction delivered=%v outcome=%q, want pending with no outcome (no eager skip)", delivered, outcome)
	}
	tasksOf := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, f.issueID, tofu)
	}
	if got := tasksOf(); got != 0 {
		t.Fatalf("while unbound: %d tasks, want none", got)
	}

	dbfx.Exec(t, `UPDATE agent SET runtime_id = $2 WHERE id = $1`, tofu, handlerTestRuntimeID(t))
	for range 3 {
		decisionPass(t, runner)
	}
	delivered, outcome := f.retractionState(t)
	if !delivered || outcome != "run_enqueued" {
		t.Fatalf("after rebind: retraction delivered=%v outcome=%q, want one run_enqueued delivery", delivered, outcome)
	}
	if got := tasksOf(); got != 1 {
		t.Fatalf("after rebind: %d tasks, want exactly one", got)
	}
}
