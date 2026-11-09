package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4149 (L5 of CHE-2769): agent outcomes T6-T8 on POST /api/chat/outcome (bound to the task
// token), T9 at task finish, T10 in the complete / fail / cancel paths, T11 per ask.
// Assertions go through the handlers, raw SQL and the L1 projection.

// outcome posts one agent outcome as the given task.
func (dm attentionDM) outcome(t *testing.T, taskID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/chat/outcome", body)
	if taskID != "" {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", taskID)
		req.Header.Set("X-Agent-ID", dm.agentID)
	}
	w := httptest.NewRecorder()
	testHandler.PostChatOutcome(w, req)
	return w
}

func (dm attentionDM) mustOutcome(t *testing.T, taskID string, body map[string]any) outcomeResult {
	t.Helper()
	w := dm.outcome(t, taskID, body)
	if w.Code != http.StatusOK {
		t.Fatalf("outcome %v: %d %s, want 200", body, w.Code, w.Body.String())
	}
	var res outcomeResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func (dm attentionDM) wantRefused(t *testing.T, taskID string, body map[string]any) {
	t.Helper()
	if w := dm.outcome(t, taskID, body); w.Code != http.StatusBadRequest {
		t.Fatalf("outcome %v: %d %s, want 400", body, w.Code, w.Body.String())
	}
}

// projection is the L1 view of this DM's chain.
func (dm attentionDM) projection(t *testing.T) service.ChatAttention {
	t.Helper()
	all, err := service.LoadChatAttention(t.Context(), testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(testUserID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.RootID == dm.root {
			return c
		}
	}
	return service.ChatAttention{}
}

func (dm attentionDM) openedAt(t *testing.T, id string) string {
	t.Helper()
	var s string
	dbfx.QueryRow(t, `SELECT opened_at::text FROM chat_attention_item WHERE id = $1`, id).Scan(&s)
	return s
}

func (dm attentionDM) ageAsk(t *testing.T, id string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE chat_attention_item SET opened_at = now() - interval '5 hours' WHERE id = $1`, id)
}

func (dm attentionDM) openAsks(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM chat_attention_item WHERE chain_root_session_id = $1 AND kind = 'ask' AND resolved_at IS NULL`, dm.root)
}

// repliesTo sends a reply that targets ask and puts the task that consumes it in the running state.
func (dm attentionDM) repliesTo(t *testing.T, ask string) string {
	t.Helper()
	res := dm.mustSend(t, "reply", map[string]any{"ask_ids": []string{ask}})
	markTaskRunning(t, t.Context(), res.TaskID)
	if st := dm.state(t, ask); st.WaitingTsk == nil || *st.WaitingTsk != res.TaskID {
		t.Fatalf("setup: ask %s not waiting on task %s", ask, res.TaskID)
	}
	return res.TaskID
}

func (dm attentionDM) wantNotWaiting(t *testing.T, ask string) {
	t.Helper()
	st := dm.state(t, ask)
	if st.WaitingMsg != nil || st.WaitingTsk != nil {
		t.Fatalf("ask %s still waiting: %v / %v", ask, st.WaitingMsg, st.WaitingTsk)
	}
	if st.ResolvedBy != nil {
		t.Fatalf("ask %s resolved (%s); want open", ask, *st.ResolvedBy)
	}
}

type outcomeResult struct {
	AskIDs []string `json:"ask_ids"`
}

// A4 / E7: Cheese replies to B; the agent's `question --ask B` returns B with its original age.
func TestQuestionOutcomeReturnsTheWaitingAskWithItsOriginalAge(t *testing.T) {
	dm := newAttentionDM(t)
	b := dm.item(t, "ask")
	dm.ageAsk(t, b)
	opened := dm.openedAt(t, b)
	task := dm.repliesTo(t, b)
	if c := dm.projection(t); c.Counts.Waiting != 1 || c.Counts.Ask != 0 {
		t.Fatalf("while the agent works: counts = %+v, want one waiting ask", c.Counts)
	}

	res := dm.mustOutcome(t, task, map[string]any{"kind": "question", "ask": b})
	if !slices.Equal(res.AskIDs, []string{b}) {
		t.Fatalf("outcome ask_ids = %v, want [%s]", res.AskIDs, b)
	}
	dm.wantNotWaiting(t, b)
	if got := dm.openedAt(t, b); got != opened {
		t.Fatalf("opened_at %s -> %s; a continued ask keeps its age", opened, got)
	}
	if n := dm.openAsks(t); n != 1 {
		t.Fatalf("%d open asks, want the same one", n)
	}
	c := dm.projection(t)
	if c.Counts.Ask != 1 || c.Counts.Waiting != 0 || c.StaleLevel != service.AttentionYellow {
		t.Fatalf("after the question: %+v, want one ask, not waiting, yellow (5 h old)", c)
	}
}

// T6 defaults: with neither --ask nor --new the agent continues its only open ask, or opens one.
func TestQuestionOutcomeDefaults(t *testing.T) {
	t.Run("no open ask opens one", func(t *testing.T) {
		dm := newAttentionDM(t)
		task := dm.sendRunning(t, "go")
		res := dm.mustOutcome(t, task, map[string]any{"kind": "question"})
		if len(res.AskIDs) != 1 {
			t.Fatalf("ask_ids = %v, want the new ask", res.AskIDs)
		}
		var agent, user, root, source string
		dbfx.QueryRow(t, `SELECT agent_id::text, user_id::text, chain_root_session_id::text, source_task_id::text FROM chat_attention_item WHERE id = $1`, res.AskIDs[0]).
			Scan(&agent, &user, &root, &source)
		if agent != dm.agentID || user != testUserID || root != dm.root || source != task {
			t.Fatalf("new ask = agent %s user %s root %s task %s, want %s %s %s %s", agent, user, root, source, dm.agentID, testUserID, dm.root, task)
		}
		if c := dm.projection(t); c.Counts.Ask != 1 {
			t.Fatalf("counts = %+v, want one ask", c.Counts)
		}
	})
	t.Run("one open ask is continued", func(t *testing.T) {
		dm := newAttentionDM(t)
		ask := dm.item(t, "ask")
		task := dm.sendRunning(t, "go")
		res := dm.mustOutcome(t, task, map[string]any{"kind": "question"})
		if !slices.Equal(res.AskIDs, []string{ask}) || dm.openAsks(t) != 1 {
			t.Fatalf("ask_ids = %v, open asks = %d, want only %s", res.AskIDs, dm.openAsks(t), ask)
		}
	})
	t.Run("--new opens another beside an open ask", func(t *testing.T) {
		dm := newAttentionDM(t)
		ask := dm.item(t, "ask")
		task := dm.sendRunning(t, "go")
		res := dm.mustOutcome(t, task, map[string]any{"kind": "question", "new": true})
		if len(res.AskIDs) != 1 || res.AskIDs[0] == ask || dm.openAsks(t) != 2 {
			t.Fatalf("ask_ids = %v, open asks = %d, want a second ask", res.AskIDs, dm.openAsks(t))
		}
		// A retry of the same outcome from the same task opens nothing more.
		dm.mustOutcome(t, task, map[string]any{"kind": "question", "new": true})
		if n := dm.openAsks(t); n != 2 {
			t.Fatalf("%d open asks after a duplicate --new, want 2", n)
		}
	})
}

// A5: done --ask resolves the named ask and nothing else, decisions included.
func TestDoneOutcomeResolvesOnlyTheNamedAsk(t *testing.T) {
	dm := newAttentionDM(t)
	ask := dm.item(t, "ask")
	later := dm.item(t, "later")
	requestID, _, _ := seedDMDecision(t, dm.agentID, dm.sessionID, "Ask", "open", "1 minute")
	task := dm.sendRunning(t, "go")

	dm.mustOutcome(t, task, map[string]any{"kind": "done", "ask_ids": []string{ask}})

	if st := dm.state(t, ask); st.ResolvedBy == nil || *st.ResolvedBy != "agent_done" {
		t.Fatalf("ask resolved_by = %v, want agent_done", st.ResolvedBy)
	}
	if st := dm.state(t, later); st.ResolvedBy != nil {
		t.Fatalf("done --ask resolved the later item (%s)", *st.ResolvedBy)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM decision_request WHERE id = $1`, requestID).Scan(&status)
	if status != "open" {
		t.Fatalf("decision status = %s, want open", status)
	}
	if c := dm.projection(t); c.Counts.Decision != 1 || c.Counts.Ask != 0 {
		t.Fatalf("counts = %+v, want the decision still open and no ask", c.Counts)
	}
}

// A15 / B2: two asks from one agent; done --ask B leaves A open with its age.
func TestDoneOutcomeLeavesTheOtherAskOpenWithItsAge(t *testing.T) {
	dm := newAttentionDM(t)
	a, b := dm.item(t, "ask"), dm.item(t, "ask")
	dm.ageAsk(t, a)
	opened := dm.openedAt(t, a)
	task := dm.sendRunning(t, "go")

	dm.mustOutcome(t, task, map[string]any{"kind": "done", "ask_ids": []string{b}})

	if st := dm.state(t, b); st.ResolvedBy == nil || *st.ResolvedBy != "agent_done" {
		t.Fatalf("B resolved_by = %v, want agent_done", st.ResolvedBy)
	}
	if st := dm.state(t, a); st.ResolvedBy != nil {
		t.Fatalf("A resolved (%s); done --ask B must not touch it", *st.ResolvedBy)
	}
	if got := dm.openedAt(t, a); got != opened {
		t.Fatalf("A opened_at %s -> %s", opened, got)
	}
}

// A7 / E20 / T11: an older task's outcome does not undo a newer one on the same ask, and a
// repeated outcome from the same task has no second effect. Other asks named by the stale outcome
// are still applied.
func TestStaleAndDuplicateOutcomesHaveNoEffectOnThatAsk(t *testing.T) {
	t.Skip("CHE-4873 quarantine (CHE-4909): seeds two active tasks in one chat session; idx_agent_task_queue_one_active_per_chat rejects it")
	dm := newAttentionDM(t)
	x, y := dm.item(t, "ask"), dm.item(t, "ask")
	older := dm.sendRunning(t, "first")
	newer := dm.sendRunning(t, "second")

	dm.mustOutcome(t, newer, map[string]any{"kind": "question", "ask": x})

	// The older run finishes late and says done for X and Y.
	dm.mustOutcome(t, older, map[string]any{"kind": "done", "ask_ids": []string{x, y}})
	if st := dm.state(t, x); st.ResolvedBy != nil {
		t.Fatalf("stale done resolved X (%s)", *st.ResolvedBy)
	}
	if st := dm.state(t, y); st.ResolvedBy == nil || *st.ResolvedBy != "agent_done" {
		t.Fatalf("Y resolved_by = %v; a stale outcome for X must not discard Y", st.ResolvedBy)
	}

	// The same task and the same ask again: nothing happens, so a waiting mark set since stays.
	dbfx.Exec(t, `UPDATE chat_attention_item SET waiting_task_id = $2, waiting_message_id = gen_random_uuid() WHERE id = $1`, x, newer)
	dm.mustOutcome(t, newer, map[string]any{"kind": "question", "ask": x})
	if st := dm.state(t, x); st.WaitingTsk == nil {
		t.Fatal("a duplicate outcome from the same task cleared waiting again")
	}
}

// A10: a task that finishes with no outcome keeps its reply, becomes an unclassified turn, and is
// not rerun.
func TestFinishWithoutOutcomeRecordsAnUnclassifiedTurn(t *testing.T) {
	dm := newAttentionDM(t)
	task := dm.sendRunning(t, "go")
	tasksBefore := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, dm.sessionID)

	dm.complete(t, task, 0, 0)

	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'assistant' AND content = 'done'`, dm.sessionID); n != 1 {
		t.Fatalf("%d stored replies, want 1 unchanged", n)
	}
	if got := dm.projection(t).Unclassified; !slices.Equal(got, []string{service.TurnKey(task)}) {
		t.Fatalf("unclassified = %v, want [turn:%s]", got, task)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, dm.sessionID); n != tasksBefore {
		t.Fatalf("%d tasks after finish, was %d: the turn must not be rerun", n, tasksBefore)
	}
}

// T8 / E26: a turn that ended with an outcome, and a turn that finished before L5 shipped, are not
// unclassified.
func TestClassifiedAndPreRolloutTurnsAreNotUnclassified(t *testing.T) {
	t.Run("info outcome", func(t *testing.T) {
		dm := newAttentionDM(t)
		task := dm.sendRunning(t, "go")
		dm.mustOutcome(t, task, map[string]any{"kind": "info"})
		dm.complete(t, task, 0, 0)
		if got := dm.projection(t).Unclassified; len(got) != 0 {
			t.Fatalf("unclassified = %v, want none after an info outcome", got)
		}
	})
	t.Run("done outcome", func(t *testing.T) {
		dm := newAttentionDM(t)
		ask := dm.item(t, "ask")
		task := dm.sendRunning(t, "go")
		dm.mustOutcome(t, task, map[string]any{"kind": "done", "ask_ids": []string{ask}})
		dm.complete(t, task, 0, 0)
		if got := dm.projection(t).Unclassified; len(got) != 0 {
			t.Fatalf("unclassified = %v, want none after a done outcome", got)
		}
	})
	t.Run("finished before rollout", func(t *testing.T) {
		dm := newAttentionDM(t)
		task := dm.sendRunning(t, "go")
		// Written straight to the table: the settle path never ran, as for a pre-L5 task.
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, task)
		if got := dm.projection(t).Unclassified; len(got) != 0 {
			t.Fatalf("unclassified = %v, want none for a turn L5 never saw", got)
		}
	})
	t.Run("failed task", func(t *testing.T) {
		dm := newAttentionDM(t)
		task := dm.sendRunning(t, "go")
		dm.fail(t, task)
		if got := dm.projection(t).Unclassified; len(got) != 0 {
			t.Fatalf("unclassified = %v, want none: a failure is the agent-stuck chip, not a turn", got)
		}
	})
}

// A17 / T10: Cheese replied to B; however the task ends, B comes back to Cheese with its age.
func TestEveryTaskEndingReturnsTheWaitingAskToCheese(t *testing.T) {
	endings := map[string]func(dm attentionDM, t *testing.T, task string){
		"info outcome": func(dm attentionDM, t *testing.T, task string) {
			dm.mustOutcome(t, task, map[string]any{"kind": "info"})
			dm.complete(t, task, 0, 0)
		},
		"no outcome": func(dm attentionDM, t *testing.T, task string) { dm.complete(t, task, 0, 0) },
		"invalid outcome": func(dm attentionDM, t *testing.T, task string) {
			dm.wantRefused(t, task, map[string]any{"kind": "done"})
			dm.complete(t, task, 0, 0)
		},
		"failed": func(dm attentionDM, t *testing.T, task string) { dm.fail(t, task) },
		"cancelled": func(dm attentionDM, t *testing.T, task string) {
			testutil.Call(t, testHandler.CancelTaskByUser, cancelTaskByUserRequest(t, testUserID, task)).Want(http.StatusOK)
		},
	}
	for name, end := range endings {
		t.Run(name, func(t *testing.T) {
			dm := newAttentionDM(t)
			b := dm.item(t, "ask")
			dm.ageAsk(t, b)
			opened := dm.openedAt(t, b)
			task := dm.repliesTo(t, b)

			end(dm, t, task)

			dm.wantNotWaiting(t, b)
			if got := dm.openedAt(t, b); got != opened {
				t.Fatalf("opened_at %s -> %s", opened, got)
			}
			if c := dm.projection(t); c.Counts.Ask != 1 || c.Counts.Waiting != 0 {
				t.Fatalf("counts = %+v, want the ask back with Cheese", c.Counts)
			}
		})
	}
}

// A18: invalid outcomes change no ask; the turn then finishes unclassified.
func TestInvalidOutcomesChangeNothingAndLeaveTheTurnUnclassified(t *testing.T) {
	dm := newAttentionDM(t)
	a, b := dm.item(t, "ask"), dm.item(t, "ask")
	// Another DM, so another chain.
	fAgent := dbfx.Agent(t, "Foreign "+t.Name(), handlerTestRuntimeID(t), testutil.Cols{})
	fSession := dbfx.ChatSession(t, fAgent)
	foreign := attentionDM{autoCompactDM{agentID: fAgent, sessionID: fSession}, ""}
	dbfx.QueryRow(t, `SELECT chain_root_session_id FROM chat_session WHERE id = $1`, fSession).Scan(&foreign.root)
	dbfx.Cleanup(t, `DELETE FROM chat_attention_item WHERE chain_root_session_id = $1`, foreign.root)
	foreignAsk := foreign.item(t, "ask")
	task := dm.sendRunning(t, "go")

	dm.wantRefused(t, task, map[string]any{"kind": "done"})                                        // done needs --ask
	dm.wantRefused(t, task, map[string]any{"kind": "done", "ask_ids": []string{}})                 // empty list
	dm.wantRefused(t, task, map[string]any{"kind": "question"})                                    // two open asks, no id
	dm.wantRefused(t, task, map[string]any{"kind": "question", "ask": b, "new": true})             // both forms
	dm.wantRefused(t, task, map[string]any{"kind": "question", "ask": foreignAsk})                 // another chain's ask
	dm.wantRefused(t, task, map[string]any{"kind": "done", "ask_ids": []string{a, foreignAsk}})    // one bad id voids the call
	dm.wantRefused(t, task, map[string]any{"kind": "done", "ask_ids": []string{uuid.NewString()}}) // unknown id
	dm.wantRefused(t, task, map[string]any{"kind": "shout"})                                       // unknown kind

	for _, id := range []string{a, b} {
		if st := dm.state(t, id); st.ResolvedBy != nil || st.WaitingMsg != nil {
			t.Fatalf("ask %s changed by an invalid outcome: %+v", id, st)
		}
	}
	if st := foreign.state(t, foreignAsk); st.ResolvedBy != nil {
		t.Fatalf("a foreign ask was resolved: %+v", st)
	}
	if n := dm.openAsks(t); n != 2 {
		t.Fatalf("%d open asks, want 2: an invalid question must not open one", n)
	}

	dm.complete(t, task, 0, 0)
	if got := dm.projection(t).Unclassified; !slices.Equal(got, []string{service.TurnKey(task)}) {
		t.Fatalf("unclassified = %v, want [turn:%s]", got, task)
	}
}

// The endpoint is bound to a running chat task: no token, an unknown task, and a finished task
// are all refused.
func TestOutcomeNeedsARunningChatTask(t *testing.T) {
	dm := newAttentionDM(t)
	ask := dm.item(t, "ask")
	task := dm.sendRunning(t, "go")
	body := map[string]any{"kind": "done", "ask_ids": []string{ask}}

	if w := dm.outcome(t, "", body); w.Code != http.StatusForbidden {
		t.Fatalf("no task token: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := dm.outcome(t, uuid.NewString(), body); w.Code != http.StatusNotFound {
		t.Fatalf("unknown task: %d %s, want 404", w.Code, w.Body.String())
	}
	dm.complete(t, task, 0, 0)
	if w := dm.outcome(t, task, body); w.Code != http.StatusConflict {
		t.Fatalf("finished task: %d %s, want 409", w.Code, w.Body.String())
	}
	if st := dm.state(t, ask); st.ResolvedBy != nil {
		t.Fatalf("a refused outcome resolved the ask (%s)", *st.ResolvedBy)
	}
}

// Every terminal report takes the chat session row first, then the task row. An outcome that
// arrives while a report holds the session must wait for it, see the final status and be refused.
// The settling transaction here takes the locks in the real order; an outcome that locked the task
// row first would deadlock with it (PostgreSQL aborts one side with 40P01).
func TestOutcomeRacingTaskSettlementIsRefusedAndChangesNothing(t *testing.T) {
	dm := newAttentionDM(t)
	ask := dm.item(t, "ask")
	task := dm.sendRunning(t, "go")

	settling, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = settling.Rollback(context.Background()) }()
	if _, err := settling.Exec(t.Context(), `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, dm.sessionID); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- dm.outcome(t, task, map[string]any{"kind": "done", "ask_ids": []string{ask}}) }()

	select {
	case w := <-done:
		t.Fatalf("outcome finished while the session was held: %d %s", w.Code, w.Body.String())
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := settling.Exec(t.Context(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, task); err != nil {
		t.Fatalf("settling the task while an outcome waits: %v", err)
	}
	if err := settling.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := <-done; w.Code != http.StatusConflict {
		t.Fatalf("outcome after settlement: %d %s, want 409", w.Code, w.Body.String())
	}
	if st := dm.state(t, ask); st.ResolvedBy != nil {
		t.Fatalf("a delayed outcome resolved the ask after settlement (%s)", *st.ResolvedBy)
	}
}

// The real /complete handler and an outcome for the same task, released together: neither may fail
// with a deadlock, and the ask is either resolved by an outcome that won or left alone by one that
// lost, never half-applied.
func TestOutcomeAndRealCompletionDoNotDeadlock(t *testing.T) {
	dm := newAttentionDM(t)
	ask := dm.item(t, "ask")
	task := dm.sendRunning(t, "go")

	hold, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(t.Context(), `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, dm.sessionID); err != nil {
		t.Fatal(err)
	}

	outcome := make(chan *httptest.ResponseRecorder, 1)
	complete := make(chan *httptest.ResponseRecorder, 1)
	go func() { outcome <- dm.outcome(t, task, map[string]any{"kind": "done", "ask_ids": []string{ask}}) }()
	go func() {
		req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+task+"/complete", map[string]any{"output": "done"}, testWorkspaceID, dm.daemonID)
		w := httptest.NewRecorder()
		testHandler.CompleteTask(w, withURLParam(req, "taskId", task))
		complete <- w
	}()
	time.Sleep(500 * time.Millisecond)
	if err := hold.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if w := <-complete; w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s, want 200", w.Code, w.Body.String())
	}
	w := <-outcome
	if w.Code != http.StatusOK && w.Code != http.StatusConflict {
		t.Fatalf("outcome: %d %s, want 200 or 409", w.Code, w.Body.String())
	}
	if resolved := dm.state(t, ask).ResolvedBy != nil; resolved != (w.Code == http.StatusOK) {
		t.Fatalf("outcome %d but ask resolved=%v", w.Code, resolved)
	}
}

// Batch terminators (sweepers, agent archival, runtime teardown) must take the chat session lock
// before they write a chat task, like every other terminal path. A holder of the session row (the
// outcome, mid status-check) then sees a status that cannot change under it: the batch waits,
// and the outcome is ordered strictly before the cancel.
func TestBatchTerminationWaitsForTheChatSessionLock(t *testing.T) {
	for name, cancel := range map[string]func(dm attentionDM) error{
		"archived agent": func(dm attentionDM) error {
			_, err := testHandler.TaskService.CancelTasksForArchivedAgent(context.Background(), parseUUID(dm.agentID))
			return err
		},
		"cancel all agent tasks": func(dm attentionDM) error {
			_, err := testHandler.TaskService.CancelTasksForAgent(context.Background(), parseUUID(dm.agentID))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { batchWaitsForSession(t, cancel) })
	}
}

func batchWaitsForSession(t *testing.T, cancel func(dm attentionDM) error) {
	dm := newAttentionDM(t)
	task := dm.sendRunning(t, "go")

	holder, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(t.Context(), `SELECT cs.id FROM agent_task_queue t JOIN chat_session cs ON cs.id = t.chat_session_id WHERE t.id = $1 FOR UPDATE OF cs`, task); err != nil {
		t.Fatal(err)
	}

	archived := make(chan error, 1)
	go func() { archived <- cancel(dm) }()
	select {
	case err := <-archived:
		t.Fatalf("archival cancelled a chat task while its session row was held (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	var status string
	if err := holder.QueryRow(t.Context(), `SELECT status FROM agent_task_queue WHERE id = $1`, task).Scan(&status); err != nil || status != "running" {
		t.Fatalf("task status while the session is held = %q (err=%v), want running", status, err)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-archived; err != nil {
		t.Fatalf("archival cancel: %v", err)
	}
}

// The real archival path and an outcome for the same task, released together: neither fails, and
// the ask is resolved only if the outcome won.
func TestOutcomeAndRealArchivalCancelDoNotConflict(t *testing.T) {
	dm := newAttentionDM(t)
	ask := dm.item(t, "ask")
	task := dm.sendRunning(t, "go")

	hold, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(t.Context(), `SELECT id FROM agent_task_queue WHERE id = $1 FOR NO KEY UPDATE`, task); err != nil {
		t.Fatal(err)
	}

	outcome := make(chan *httptest.ResponseRecorder, 1)
	archived := make(chan error, 1)
	go func() { outcome <- dm.outcome(t, task, map[string]any{"kind": "done", "ask_ids": []string{ask}}) }()
	go func() {
		_, err := testHandler.TaskService.CancelTasksForArchivedAgent(context.Background(), parseUUID(dm.agentID))
		archived <- err
	}()
	time.Sleep(500 * time.Millisecond)
	if err := hold.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := <-archived; err != nil {
		t.Fatalf("archival cancel: %v", err)
	}
	w := <-outcome
	if w.Code != http.StatusOK && w.Code != http.StatusConflict {
		t.Fatalf("outcome: %d %s, want 200 or 409", w.Code, w.Body.String())
	}
	if resolved := dm.state(t, ask).ResolvedBy != nil; resolved != (w.Code == http.StatusOK) {
		t.Fatalf("outcome %d but ask resolved=%v", w.Code, resolved)
	}
}

// Lock order is chat_session -> runtime -> agent -> task. A real chat send holds the session while it locks the
// agent; a revoke or runtime delete that locked the agent first and then queued for the session
// deadlocked with it (40P01). A gate holds the session so the send queues first and the teardown
// queues second; both handlers then run for real when the gate opens. Neither may fail.
func raceSendAgainstTeardown(t *testing.T, dm attentionDM, teardown func() error) {
	t.Helper()
	gate, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(t.Context(), `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, dm.sessionID); err != nil {
		t.Fatal(err)
	}

	sent := make(chan *httptest.ResponseRecorder, 1)
	torn := make(chan error, 1)
	go func() { sent <- dm.send(t, "racing the teardown") }()
	time.Sleep(500 * time.Millisecond)
	go func() { torn <- teardown() }()
	time.Sleep(500 * time.Millisecond)
	if err := gate.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	select {
	case w := <-sent:
		if w.Code/100 != 2 {
			t.Fatalf("send racing a teardown: %d %s, want 2xx", w.Code, w.Body.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("send did not finish: deadlock with the teardown")
	}
	select {
	case err := <-torn:
		if err != nil {
			t.Fatalf("teardown racing a send: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("teardown did not finish: deadlock with the send")
	}
}

func TestChatSendAndMemberRevocationDoNotDeadlock(t *testing.T) {
	dm := newAttentionDM(t)
	member := createPermissionTestMember(t, "revoke-race-"+dm.sessionID+"@example.com")
	dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = $1, visibility = 'public' WHERE id = $2`, member, dm.runtimeID)
	var memberRow string
	dbfx.QueryRow(t, `SELECT id FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, member).Scan(&memberRow)

	raceSendAgainstTeardown(t, dm, func() error {
		_, err := testHandler.revokeAndRemoveMember(context.Background(),
			parseUUID(testWorkspaceID), parseUUID(member), parseUUID(memberRow), parseUUID(testUserID))
		return err
	})
}

func TestChatSendAndRuntimeDeleteDoNotDeadlock(t *testing.T) {
	dm := newAttentionDM(t)
	raceSendAgainstTeardown(t, dm, func() error {
		body := map[string]any{"expected_active_agent_ids": []string{dm.agentID}}
		req := withURLParam(newRequest(http.MethodPost, "/api/runtimes/"+dm.runtimeID+"/unbind-agents-and-delete", body), "runtimeId", dm.runtimeID)
		w := httptest.NewRecorder()
		testHandler.UnbindAgentsAndDeleteRuntime(w, req)
		if w.Code != http.StatusOK {
			return fmt.Errorf("runtime delete: %d %s", w.Code, w.Body.String())
		}
		return nil
	})
}
