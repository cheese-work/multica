package handler

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9605 (S1a) acceptance: the step run is built through the real status-move
// path (a project workflow handoff), a side event arrives while it is queued,
// and the run is claimed through the daemon claim handler and rendered with
// daemon.BuildPrompt. The step run keeps its prompt, its workflow role evidence,
// its PR binding, its originator and its connected-app overlay; the side input
// is answered by a run of its own.

// realStep is a queued step run made by moving an issue into a handoff step.
type realStep struct {
	issueFoldFixture
	projectID string
	stepID    string
	handoff   string // the handoff comment id; "" for an assignment-level run
}

// stepEvidence is everything about a step run that a side comment must not change.
type stepEvidence struct {
	Trigger, Kind, Note, Step, PullRequest string
	Originator, Accountable, Source        string
	Overlay, Apps                          string
	Roles                                  int
	Status                                 string
}

func newStepProject(t *testing.T, name string) realStep {
	t.Helper()
	seedTestCatalog(t)
	runtimeID := dbfx.Runtime(t, name+" runtime")
	agentID := dbfx.Agent(t, name+" agent", runtimeID, testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to", "instructions": "",
		"custom_env": testutil.Raw("'{}'::jsonb"), "custom_args": testutil.Raw("'[]'::jsonb"),
		"max_concurrent_tasks": 8,
	})
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2) ON CONFLICT DO NOTHING`, agentID, testWorkspaceID)
	projectID := deliveryProject(t, name+" project", agentID)
	issueID := dbfx.Issue(t, name+" issue", testutil.Cols{"project_id": projectID, "status": "todo"})
	t.Cleanup(func() { dbfx.Exec(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID) })
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	return realStep{
		issueFoldFixture: issueFoldFixture{issue: issue, agent: agent, issueID: issueID, agentID: agentID, runtime: runtimeID},
		projectID:        projectID,
	}
}

// moveIn moves the issue into the project's handoff step as a member. With a
// handoff comment the run is a status handoff run; without one it is an
// assignment-level run. It stores the PR binding and the overlay a run would
// carry, so a rewrite shows.
func (s *realStep) moveIn(t *testing.T, withHandoffComment bool) {
	t.Helper()
	body := map[string]any{"status": "in_progress"}
	if withHandoffComment {
		body["handoff_comment"] = map[string]any{"content": "review the migration"}
	}
	issueWriters[0].write(t, s.issueID, body, map[string]string{"X-Actor-Source": "interactive_jwt"}).Want(http.StatusOK)
	var count int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
		AND (trigger_comment_id IS NULL OR trigger_kind = 'status_handoff')`, s.issueID, s.agentID).Scan(&count)
	if count != 1 {
		t.Fatalf("the status move queued %d step runs, want 1", count)
	}
	dbfx.QueryRow(t, `SELECT id::text, COALESCE(trigger_comment_id::text, '') FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
		AND (trigger_comment_id IS NULL OR trigger_kind = 'status_handoff')`, s.issueID, s.agentID).Scan(&s.stepID, &s.handoff)
	dbfx.Exec(t, `UPDATE agent_task_queue SET pull_request_id = gen_random_uuid(),
		runtime_mcp_overlay = '{"mcpServers":{"step":{"url":"https://example.test/step"}}}'::jsonb,
		runtime_connected_apps = '[{"toolkit":"github"}]'::jsonb WHERE id = $1`, s.stepID)
	if got := s.evidence(t); got.Roles == 0 || got.Originator == "" || got.Step == "" {
		t.Fatalf("the status move left a step run without role evidence or originator: %+v", got)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(s.issueID))
	if err != nil {
		t.Fatal(err)
	}
	s.issue = issue
}

func (s realStep) evidence(t *testing.T) stepEvidence {
	t.Helper()
	var e stepEvidence
	dbfx.QueryRow(t, `SELECT COALESCE(trigger_comment_id::text, ''), COALESCE(trigger_kind, ''), COALESCE(handoff_note, ''),
			COALESCE(workflow_step, ''), COALESCE(pull_request_id::text, ''),
			COALESCE(originator_user_id::text, ''), COALESCE(accountable_user_id::text, ''), COALESCE(originator_source, ''),
			COALESCE(runtime_mcp_overlay::text, ''), COALESCE(runtime_connected_apps::text, ''), status,
			(SELECT count(*) FROM activity_log WHERE issue_id = t.issue_id AND action = 'workflow_role' AND details->>'task_id' = t.id::text)
		FROM agent_task_queue t WHERE id = $1`, s.stepID).Scan(
		&e.Trigger, &e.Kind, &e.Note, &e.Step, &e.PullRequest, &e.Originator, &e.Accountable, &e.Source, &e.Overlay, &e.Apps, &e.Status, &e.Roles)
	return e
}

func (s realStep) mention(text string) string {
	return "[@Agent](mention://agent/" + s.agentID + ") " + text
}

func postIssueComment(t *testing.T, issueID, content, parentID string) string {
	t.Helper()
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	var out struct {
		ID string `json:"id"`
	}
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", body), "id", issueID)
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&out)
	return out.ID
}

// wantUnchanged fails unless the queued step run row still holds what the status
// move stored. Call it before the claim: completion clears the overlay.
func (s realStep) wantUnchanged(t *testing.T, before stepEvidence) {
	t.Helper()
	if after := s.evidence(t); after != before {
		t.Fatalf("step run changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

// wantKeptStepRun fails unless the claimed run renders the step prompt with the
// workflow block, the handoff's originator and the handoff's trigger.
func (s realStep) wantKeptStepRun(t *testing.T, before stepEvidence, claimed daemon.Task) {
	t.Helper()
	wantStepPrompt(t, claimed)
	prompt := daemon.BuildPrompt(claimed, "codex")
	if !strings.Contains(prompt, "[Project workflow]") {
		t.Fatalf("step prompt lost the project workflow block:\n%s", prompt)
	}
	if claimed.InitiatorID != before.Originator {
		t.Fatalf("claimed step run initiator = %q, want the handoff's %q", claimed.InitiatorID, before.Originator)
	}
	if claimed.TriggerCommentID != before.Trigger || claimed.TriggerKind != before.Kind || claimed.HandoffNote != before.Note {
		t.Fatalf("claimed step run trigger = %q kind=%q note=%q, want %q %q %q", claimed.TriggerCommentID, claimed.TriggerKind, claimed.HandoffNote, before.Trigger, before.Kind, before.Note)
	}
	if before.Kind == "status_handoff" && !strings.Contains(prompt, "Handoff context") {
		t.Fatalf("status handoff prompt lost its handoff context:\n%s", prompt)
	}
}

// wantCommentRun fails unless the claim set holds a run of its own for the
// comment: a comment prompt carrying the comment, replying in its thread.
func wantCommentRun(t *testing.T, claimed map[string]daemon.Task, stepID, commentID, text string) daemon.Task {
	t.Helper()
	for id, task := range claimed {
		if id == stepID || task.TriggerCommentID != commentID {
			continue
		}
		prompt := daemon.BuildPrompt(task, "codex")
		if !strings.Contains(prompt, "[NEW COMMENT]") || !strings.Contains(prompt, text) {
			t.Fatalf("comment run does not render the comment:\n%s", prompt)
		}
		if task.TriggerKind == "status_handoff" {
			t.Fatalf("comment run claimed as a status handoff: %+v", task)
		}
		return task
	}
	t.Fatalf("no claimed run for comment %s (claimed %d runs)", commentID, len(claimed))
	return daemon.Task{}
}

// A comment in another thread while the step run is queued gets a run of its
// own; the step run is claimed with its own prompt and evidence.
func TestAcceptanceCrossThreadCommentKeepsStepRun(t *testing.T) {
	for _, withHandoff := range []bool{false, true} {
		name := "assignment"
		if withHandoff {
			name = "status_handoff"
		}
		t.Run(name, func(t *testing.T) {
			s := newStepProject(t, "accept cross "+name)
			s.moveIn(t, withHandoff)
			before := s.evidence(t)

			commentID := postIssueComment(t, s.issueID, s.mention("a side question"), "")

			s.wantUnchanged(t, before)
			claimed := s.claimAll(t)
			step, ok := claimed[s.stepID]
			if !ok {
				t.Fatalf("the step run was not claimed (claimed %d runs)", len(claimed))
			}
			s.wantKeptStepRun(t, before, step)
			wantCommentRun(t, claimed, s.stepID, commentID, "a side question")
		})
	}
}

// A comment run queued first does not stop the move into the step from queuing
// its step run, and the step run is not folded into the comment run.
func TestAcceptanceCommentRunQueuedFirstKeepsStepRun(t *testing.T) {
	for _, withHandoff := range []bool{false, true} {
		name := "assignment"
		if withHandoff {
			name = "status_handoff"
		}
		t.Run(name, func(t *testing.T) {
			s := newStepProject(t, "accept first "+name)
			commentID := postIssueComment(t, s.issueID, s.mention("a question before the move"), "")
			s.moveIn(t, withHandoff)
			before := s.evidence(t)

			s.wantUnchanged(t, before)
			claimed := s.claimAll(t)
			step, ok := claimed[s.stepID]
			if !ok {
				t.Fatalf("the step run was not claimed (claimed %d runs)", len(claimed))
			}
			s.wantKeptStepRun(t, before, step)
			wantCommentRun(t, claimed, s.stepID, commentID, "a question before the move")
		})
	}
}

// A comment in the handoff thread while the status handoff run is queued leaves
// the run intact, is not receipted as delivered by the step claim, and is
// replayed as a run of its own when the step run completes.
func TestAcceptanceSameThreadCommentKeepsStatusHandoffRun(t *testing.T) {
	s := newStepProject(t, "accept same thread")
	s.moveIn(t, true)
	before := s.evidence(t)

	reply := postIssueComment(t, s.issueID, s.mention("one more thing"), s.handoff)

	s.wantUnchanged(t, before)
	step := claimNext(t, s.runtime)
	if step == nil || step.ID != s.stepID {
		t.Fatalf("first claim = %+v, want the step run %s", step, s.stepID)
	}
	s.wantKeptStepRun(t, before, *step)
	if prompt := daemon.BuildPrompt(*step, "codex"); strings.Contains(prompt, "one more thing") {
		t.Fatalf("the step prompt answers the same-thread comment:\n%s", prompt)
	}
	if delivered := uuidsToStrings(s.task(t, s.stepID).DeliveredCommentIds); slices.Contains(delivered, reply) {
		t.Fatalf("the step claim receipted the reply as delivered: %v", delivered)
	}

	dbfx.Exec(t, "UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1", s.stepID)
	completed := s.task(t, s.stepID)
	testHandler.reconcileCommentsOnCompletion(context.Background(), &completed)

	followUp := claimNext(t, s.runtime)
	if followUp == nil {
		t.Fatal("the reply was lost: no run claimed after the step run completed")
	}
	if followUp.ID == s.stepID || followUp.TriggerCommentID != reply {
		t.Fatalf("follow-up run = %+v, want a run triggered by the reply %s", followUp, reply)
	}
	if prompt := daemon.BuildPrompt(*followUp, "codex"); !strings.Contains(prompt, "[NEW COMMENT]") || !strings.Contains(prompt, "one more thing") {
		t.Fatalf("follow-up run does not render the reply:\n%s", prompt)
	}
}

// A planned comment (a worker's reply, or the loser of an enqueue race) registers
// on a queued status handoff holder without touching the run, and the replay
// answers it. A queued comment run still refuses the append.
func TestAcceptanceRegisterOnQueuedStatusHandoffHolder(t *testing.T) {
	s := newStepProject(t, "accept register")
	s.moveIn(t, true)
	before := s.evidence(t)
	reply := dbfx.Comment(t, s.issueID, "worker reply", testutil.Cols{"parent_id": s.handoff})

	ok, err := testHandler.registerPlannedCommentForActiveTask(context.Background(), s.issue, s.agent.ID, parseUUID(reply), pgtype.Text{})
	if err != nil || !ok {
		t.Fatalf("register on the queued status handoff run = %v, %v; want registered", ok, err)
	}
	if !slices.Contains(coalescedIDs(s.task(t, s.stepID)), reply) {
		t.Fatalf("the reply is not planned on the holder: %v", coalescedIDs(s.task(t, s.stepID)))
	}

	s.wantUnchanged(t, before)
	step := claimNext(t, s.runtime)
	if step == nil || step.ID != s.stepID {
		t.Fatalf("first claim = %+v, want the step run", step)
	}
	s.wantKeptStepRun(t, before, *step)
	if len(step.CoalescedComments) != 0 {
		t.Fatalf("step claim embedded %d coalesced comments the prompt never renders", len(step.CoalescedComments))
	}
	if delivered := uuidsToStrings(s.task(t, s.stepID).DeliveredCommentIds); slices.Contains(delivered, reply) {
		t.Fatalf("the step claim receipted the reply as delivered: %v", delivered)
	}
	dbfx.Exec(t, "UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1", s.stepID)
	completed := s.task(t, s.stepID)
	testHandler.reconcileCommentsOnCompletion(context.Background(), &completed)
	followUp := claimNext(t, s.runtime)
	if followUp == nil || followUp.TriggerCommentID != reply {
		t.Fatalf("follow-up run = %+v, want a run triggered by the reply %s", followUp, reply)
	}
	if prompt := daemon.BuildPrompt(*followUp, "codex"); !strings.Contains(prompt, "worker reply") {
		t.Fatalf("follow-up run does not render the reply:\n%s", prompt)
	}
}

// The post-insert sibling fold and the budget-resume fold leave a real step run
// alone, and both runs are claimed with their own prompts.
func TestAcceptanceSiblingAndResumeFoldsKeepStepRun(t *testing.T) {
	s := newStepProject(t, "accept folds")
	s.moveIn(t, false)
	before := s.evidence(t)

	// Post-insert sibling fold: a fresh comment run next to the queued step run.
	siblingComment := dbfx.Comment(t, s.issueID, "sibling question")
	siblingID := dbfx.Task(t, s.agentID, testutil.Cols{"runtime_id": s.runtime, "issue_id": s.issueID, "status": "queued", "trigger_comment_id": siblingComment})
	if testHandler.collapseIntoQueuedSibling(context.Background(), s.issue, s.task(t, siblingID)) {
		t.Fatal("the comment run was collapsed into the queued step run")
	}

	// Budget-resume fold: the refused comment is not bound to the step run.
	refused := dbfx.Comment(t, s.issueID, "refused question")
	refusal := db.RunBudgetRefusal{TriggerCommentID: parseUUID(refused), AgentID: s.agent.ID, Trigger: "mention"}
	prep := testHandler.prepareBind(context.Background(), refusal, s.issue)
	if prep == nil {
		t.Fatal("no bind prep for the refused comment")
	}
	var held error
	if err := testHandler.underIssueLock(context.Background(), s.issue.ID, func(q *db.Queries) error {
		_, held = testHandler.bindSurvivor(context.Background(), q, refusal, parseUUID(s.stepID), prep)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if held != errResumeSurvivorHeld {
		t.Fatalf("bindSurvivor = %v, want errResumeSurvivorHeld", held)
	}

	s.wantUnchanged(t, before)
	claimed := s.claimAll(t)
	step, ok := claimed[s.stepID]
	if !ok {
		t.Fatalf("the step run was not claimed (claimed %d runs)", len(claimed))
	}
	s.wantKeptStepRun(t, before, step)
	wantCommentRun(t, claimed, s.stepID, siblingComment, "sibling question")
}

// A decision answer while the recipient's only queued run is a step run is
// delivered as a comment run of its own that replies in the decision thread; the
// step run keeps its prompt, with the project workflow block. The squad leader's
// run is covered the same way.
func TestAcceptanceDecisionAnswerKeepsStepRun(t *testing.T) {
	for _, squad := range []bool{false, true} {
		name := "agent"
		if squad {
			name = "squad"
		}
		t.Run(name, func(t *testing.T) {
			resetDecisionLease(t)
			fixture, recorded := readyDecisionDelivery(t)
			runtimeID := dbfx.Runtime(t, "decision step runtime "+name)
			dbfx.Exec(t, `UPDATE agent SET runtime_id = $1, max_concurrent_tasks = 8 WHERE id = $2`, runtimeID, fixture.agentID)
			projectID := deliveryProject(t, "decision step project "+name, fixture.agentID)
			dbfx.Exec(t, `UPDATE issue SET project_id = $1, status = 'in_progress' WHERE id = $2`, projectID, fixture.issueID)
			cols := testutil.Cols{"issue_id": fixture.issueID, "status": "queued", "runtime_id": runtimeID, "workflow_step": "in_progress"}
			if squad {
				squadID := dbfx.Insert(t, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Decision step squad", "creator_id": testUserID, "leader_id": fixture.agentID})
				dbfx.Exec(t, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
				cols["is_leader_task"] = true
				cols["squad_id"] = squadID
			}
			stepID := dbfx.Task(t, fixture.agentID, cols)
			t.Cleanup(func() { dbfx.Exec(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", fixture.issueID) })

			_, runner := newDecisionRunnerFixture(t)
			decisionPass(t, runner)
			stored, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: recorded.Answer.ID})
			if err != nil {
				t.Fatal(err)
			}
			if stored.WakeOutcome.String != "run_enqueued" {
				t.Fatalf("wake outcome = %q, want run_enqueued", stored.WakeOutcome.String)
			}
			step := (issueFoldFixture{}).task(t, stepID)
			if step.TriggerCommentID.Valid || len(step.CoalescedCommentIds) != 0 || step.TriggerKind.Valid {
				t.Fatalf("the decision answer rewrote the step run: trigger=%v coalesced=%v", step.TriggerCommentID, coalescedIDs(step))
			}

			claimed := claimAllOn(t, runtimeID)
			claimedStep, ok := claimed[stepID]
			if !ok {
				t.Fatalf("the step run was not claimed (claimed %d runs)", len(claimed))
			}
			wantStepPrompt(t, claimedStep)
			if prompt := daemon.BuildPrompt(claimedStep, "codex"); !strings.Contains(prompt, "[Project workflow]") {
				t.Fatalf("step prompt lost the project workflow block:\n%s", prompt)
			}
			wantCommentRun(t, claimed, stepID, uuidToString(recorded.Comment.ID), recorded.Comment.Content)
		})
	}
}
