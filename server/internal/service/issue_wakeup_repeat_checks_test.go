package service

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-5136: an agent that registers a second checks_finished rule on a PR head
// whose first rule already fired gets a refusal, not another one-shot run.
func TestWakeupChecksFinishedRepeatOnUnchangedHeadIsRefused(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	s.Tasks.PRSnapshotsEnabled = true
	ctx := context.Background()
	member := parseTestUUID(t, f.UserID)
	var pr string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO github_pull_request(workspace_id,installation_id,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,head_sha,snapshot_head_sha,checks_rollup_state)
		VALUES($1,1,'multica-ai','wakeup-repeat',(extract(epoch from clock_timestamp())*1000)::bigint % 100000,'PR','open','https://example.test/pr',now(),now(),'aaa','aaa','FAILURE') RETURNING id`, f.WorkspaceID).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM github_pull_request WHERE id=$1", pr)
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	runtime := testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')")
	sourceTask := func(agentID string) string {
		return f.Task(t, agentID, testutil.Cols{"issue_id": issue, "status": "running", "runtime_id": runtime})
	}
	rule := func(agentID string) (string, error) {
		src := sourceTask(agentID)
		w, err := s.Create(ctx, issue, member, parseTestUUID(t, src), WakeupInput{AgentID: agentID, Kind: "event", Instruction: "Look at CI",
			Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
		return util.UUIDToString(w.ID), err
	}

	// A first create is accepted, and it fires: checks finished on head aaa.
	first, err := rule(agent)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	wakeTick(t, f, s, parseTestUUID(t, first))
	if n := wakeRuns(t, f, parseTestUUID(t, first)); n != 1 {
		t.Fatalf("first rule started %d runs, want 1", n)
	}

	// Another agent's rule is not affected.
	other := f.privateAgentOwnedBy(t, f.UserID, "other")
	if _, err := rule(other); err != nil {
		t.Fatalf("another agent's create refused: %v", err)
	}

	// The second create on the same head is refused with the exact text.
	_, err = rule(agent)
	want := "head aaa unchanged since the last checks_finished fire; wait for a new head or use --until-status"
	if !errors.Is(err, ErrWakeupHeadUnchanged) || err.Error() != want {
		t.Fatalf("repeat create err = %v, want %q", err, want)
	}
	if n := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE issue_id=$1", issue); n != 2 {
		t.Fatalf("refusal left %d rules, want 2", n)
	}

	// A new head is accepted.
	f.Exec(t, "UPDATE github_pull_request SET head_sha='bbb',snapshot_head_sha='bbb' WHERE id=$1", pr)
	if _, err := rule(agent); err != nil {
		t.Fatalf("create after a new head: %v", err)
	}
}
