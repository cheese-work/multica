package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// compactFixture is one active DM that holds a resume session (S_old), plus a
// running chat task and the mat_ token the daemon would hand that task.
type compactFixture struct {
	sessionID, taskID, token string
}

func newCompactFixture(t *testing.T) compactFixture {
	t.Helper()
	ctx := context.Background()
	var agentID, runtimeID, sessionID, taskID string
	if err := testPool.QueryRow(ctx, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, session_id, runtime_id, work_dir)
		VALUES ($1, $2, $3, 'compact', 'S_old', $4, '/tmp/compact-work') RETURNING id`,
		testWorkspaceID, agentID, testUserID, runtimeID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	token := chatTaskToken(t, sessionID, "running")
	if err := testPool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return compactFixture{sessionID: sessionID, taskID: taskID, token: token}
}

func postCompact(t *testing.T, token, summary string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"summary": summary})
	req, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/chat/compact", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// T1: the DM that asked to compact is still that DM afterwards.
func TestChatCompactKeepsTheSameDM(t *testing.T) {
	fx := newCompactFixture(t)
	ctx := context.Background()

	status, body := postCompact(t, fx.token, "Goal: ship in-place compact.")
	if status != http.StatusOK {
		t.Fatalf("compact = %d %s, want 200", status, body)
	}

	var gotStatus string
	var continuedInto, sessionPtr, runtimePtr, workDir *string
	var epoch int64
	if err := testPool.QueryRow(ctx, `
		SELECT status, continued_into_session_id::text, session_id, runtime_id::text, work_dir, compaction_epoch
		FROM chat_session WHERE id = $1`, fx.sessionID).Scan(&gotStatus, &continuedInto, &sessionPtr, &runtimePtr, &workDir, &epoch); err != nil {
		t.Fatal(err)
	}
	if gotStatus != "active" || continuedInto != nil {
		t.Errorf("status = %q, continued_into = %v; want active and NULL", gotStatus, continuedInto)
	}
	if sessionPtr != nil || runtimePtr != nil {
		t.Errorf("resume pointer = %v/%v, want NULL/NULL", sessionPtr, runtimePtr)
	}
	if workDir == nil || *workDir != "/tmp/compact-work" {
		t.Errorf("work_dir = %v, want kept", workDir)
	}
	if epoch != 1 {
		t.Errorf("compaction_epoch = %d, want 1", epoch)
	}

	var summaries int
	var summaryTask *string
	if err := testPool.QueryRow(ctx, `
		SELECT count(*), max(task_id::text) FROM chat_message
		WHERE chat_session_id = $1 AND message_kind = 'compaction_summary'`, fx.sessionID).Scan(&summaries, &summaryTask); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || summaryTask == nil || *summaryTask != fx.taskID {
		t.Errorf("summaries = %d (task %v), want 1 owned by %s", summaries, summaryTask, fx.taskID)
	}
	var successors int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_session WHERE continued_from_session_id = $1`, fx.sessionID).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if successors != 0 {
		t.Errorf("successor DMs = %d, want 0", successors)
	}
}

// T2: a retry from the same task adds no second summary and no second epoch.
func TestChatCompactRetryIsIdempotent(t *testing.T) {
	fx := newCompactFixture(t)
	for i := 0; i < 2; i++ {
		if status, body := postCompact(t, fx.token, "Same summary."); status != http.StatusOK {
			t.Fatalf("compact #%d = %d %s, want 200", i+1, status, body)
		}
	}
	var summaries int
	var epoch int64
	if err := testPool.QueryRow(ctx(), `
		SELECT (SELECT count(*) FROM chat_message WHERE chat_session_id = cs.id AND message_kind = 'compaction_summary'), cs.compaction_epoch
		FROM chat_session cs WHERE cs.id = $1`, fx.sessionID).Scan(&summaries, &epoch); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || epoch != 1 {
		t.Errorf("summaries = %d, epoch = %d; want 1 and 1", summaries, epoch)
	}
}

// T3: an archived DM and a channel-bound DM refuse with 409 and stay unchanged.
func TestChatCompactRefusesArchivedAndChannelBoundDMs(t *testing.T) {
	t.Run("archived", func(t *testing.T) {
		fx := newCompactFixture(t)
		if _, err := testPool.Exec(ctx(), `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fx.sessionID); err != nil {
			t.Fatal(err)
		}
		if status, body := postCompact(t, fx.token, "x"); status != http.StatusConflict {
			t.Fatalf("archived compact = %d %s, want 409", status, body)
		}
		assertCompactUntouched(t, fx.sessionID)
	})
	t.Run("channel bound", func(t *testing.T) {
		fx := newCompactFixture(t)
		appID := "compact-" + uuid.NewString()
		var installID string
		if err := testPool.QueryRow(ctx(), `
			INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, installer_user_id)
			SELECT $1, agent_id, 'feishu', jsonb_build_object('app_id', $3::text), $4 FROM chat_session WHERE id = $2
			RETURNING id`, testWorkspaceID, fx.sessionID, appID, testUserID).Scan(&installID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			testPool.Exec(ctx(), `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, fx.sessionID)
			testPool.Exec(ctx(), `DELETE FROM channel_installation WHERE id = $1`, installID)
		})
		if _, err := testPool.Exec(ctx(), `
			INSERT INTO channel_chat_session_binding (chat_session_id, installation_id, channel_type, channel_chat_id, chat_type)
			VALUES ($1, $2, 'feishu', $3, 'p2p')`, fx.sessionID, installID, appID); err != nil {
			t.Fatal(err)
		}
		if status, body := postCompact(t, fx.token, "x"); status != http.StatusConflict {
			t.Fatalf("channel-bound compact = %d %s, want 409", status, body)
		}
		assertCompactUntouched(t, fx.sessionID)
	})
}

func assertCompactUntouched(t *testing.T, sessionID string) {
	t.Helper()
	var summaries int
	var sessionPtr *string
	var epoch int64
	if err := testPool.QueryRow(ctx(), `
		SELECT (SELECT count(*) FROM chat_message WHERE chat_session_id = cs.id AND message_kind = 'compaction_summary'), cs.session_id, cs.compaction_epoch
		FROM chat_session cs WHERE cs.id = $1`, sessionID).Scan(&summaries, &sessionPtr, &epoch); err != nil {
		t.Fatal(err)
	}
	if summaries != 0 || sessionPtr == nil || *sessionPtr != "S_old" || epoch != 0 {
		t.Errorf("refused DM changed: summaries=%d session_id=%v epoch=%d", summaries, sessionPtr, epoch)
	}
}

func ctx() context.Context { return context.Background() }

// compactAfterTaskEnds drives the interleaving where the caller task ends after
// the handler's first status check but before the compaction takes the DM lock:
// the DM row is held, the request is released and parks on that lock, the task
// is moved to `status`, then the DM is released.
func compactAfterTaskEnds(t *testing.T, fx compactFixture, status string) (int, []byte) {
	t.Helper()
	holder, err := testPool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx())
	if _, err := holder.Exec(ctx(), `SELECT 1 FROM chat_session WHERE id = $1 FOR UPDATE`, fx.sessionID); err != nil {
		t.Fatal(err)
	}
	type reply struct {
		status int
		body   []byte
	}
	done := make(chan reply, 1)
	go func() {
		s, b := postCompact(t, fx.token, "Summary from a task that already ended.")
		done <- reply{s, b}
	}()
	for waited := 0; ; waited++ {
		var parked int
		if err := testPool.QueryRow(ctx(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%FROM chat_session%' AND query ILIKE '%FOR NO KEY UPDATE%'`).Scan(&parked); err != nil {
			t.Fatal(err)
		}
		if parked > 0 {
			break
		}
		if waited > 500 {
			t.Fatal("compact request never reached the DM lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = $2, completed_at = now() WHERE id = $1`, fx.taskID, status); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx()); err != nil {
		t.Fatal(err)
	}
	r := <-done
	return r.status, r.body
}

// T4: a caller that was cancelled or completed before the compaction took its
// locks gets a conflict and changes nothing: no summary, no epoch bump.
func TestChatCompactRefusesTaskThatEndedBeforeTheLock(t *testing.T) {
	for _, ended := range []string{"cancelled", "completed"} {
		t.Run(ended, func(t *testing.T) {
			fx := newCompactFixture(t)
			if status, body := compactAfterTaskEnds(t, fx, ended); status != http.StatusConflict {
				t.Fatalf("compact after %s = %d %s, want 409", ended, status, body)
			}
			assertCompactUntouched(t, fx.sessionID)
		})
	}
}

// T5: a task whose row is being changed right now (a terminal transition holds
// it) is refused at once instead of waiting; the transition keeps its DM-last
// order, so waiting here could deadlock it.
func TestChatCompactDoesNotWaitOnATaskInTransition(t *testing.T) {
	fx := newCompactFixture(t)
	holder, err := testPool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx())
	if _, err := holder.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if status, body := postCompact(t, fx.token, "x"); status != http.StatusConflict {
		t.Fatalf("compact against a task in transition = %d %s, want 409", status, body)
	}
	assertCompactUntouched(t, fx.sessionID)
}
