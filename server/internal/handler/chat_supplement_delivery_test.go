package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// newChatSupplementDaemonClient serves the real claim and ack handlers to the
// real daemon client, authenticated as the runtime owner.
func newChatSupplementDaemonClient(t *testing.T) *daemon.Client {
	t.Helper()
	return newObservedChatSupplementDaemonClient(t, nil)
}

// newObservedChatSupplementDaemonClient also hands every request to observe
// before the handler reads it.
func newObservedChatSupplementDaemonClient(t *testing.T, observe func(*http.Request)) *daemon.Client {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Header.Set("X-User-ID", testUserID)
			req.Header.Set("X-Workspace-ID", testWorkspaceID)
			if observe != nil {
				observe(req)
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Post("/api/daemon/tasks/{taskId}/supplements/claim", testHandler.ClaimTaskSupplement)
	r.Post("/api/daemon/tasks/{taskId}/supplements/{commentId}/ack", testHandler.AckTaskSupplement)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return daemon.NewClient(srv.URL)
}

type chatReceiptRow struct {
	status, reason string
	attempts       int
	delivered      bool
	ackedAfter     bool
	resentTaskID   string
	resentUncert   bool
}

func readChatReceipt(t *testing.T, f chatSteerFixture) chatReceiptRow {
	t.Helper()
	var row chatReceiptRow
	dbfx.QueryRow(t, `
		SELECT status, COALESCE(failure_reason, ''), attempt_count, delivered_at IS NOT NULL,
		       acked_after_resend_at IS NOT NULL, COALESCE(resent_task_id::text, ''), resent_uncertain
		FROM chat_task_supplement WHERE message_id = $1 AND task_id = $2
	`, f.messageID, f.runningID).Scan(&row.status, &row.reason, &row.attempts, &row.delivered,
		&row.ackedAfter, &row.resentTaskID, &row.resentUncert)
	return row
}

func steeredFixture(t *testing.T) chatSteerFixture {
	t.Helper()
	f := newChatSteerFixture(t, "capable")
	steerChat(t, f.sessionID, f.queuedID, testUserID).Want(http.StatusOK)
	return f
}

func claimSteer(t *testing.T, client *daemon.Client, f chatSteerFixture) *daemon.TaskSupplement {
	t.Helper()
	sup, err := client.ClaimTaskSupplement(context.Background(), f.runningID)
	if err != nil || sup == nil {
		t.Fatalf("claim = %+v, %v", sup, err)
	}
	return sup
}

func endChatTurn(t *testing.T, taskID string) {
	t.Helper()
	if _, err := testHandler.TaskService.CancelTask(context.Background(), parseUUID(taskID)); err != nil {
		t.Fatalf("end turn: %v", err)
	}
}

func resendChat(t *testing.T, f chatSteerFixture, userID string, confirm bool) *testutil.Response {
	t.Helper()
	req := newRequestAs(userID, http.MethodPost, "/api/chat/sessions/"+f.sessionID+"/messages/"+f.messageID+"/supplement/resend",
		map[string]any{"confirm_possible_duplicate": confirm})
	req = withURLParams(req, "sessionId", f.sessionID, "messageId", f.messageID)
	return testutil.Call(t, testHandler.ResendChatSupplement, chatPendingCtxAs(t, req, userID))
}

func wantConflictCode(t *testing.T, resp *testutil.Response, code string) {
	t.Helper()
	var body chatErrorBody
	resp.Want(http.StatusConflict).JSON(&body)
	if body.Code != code {
		t.Fatalf("code = %q, want %q", body.Code, code)
	}
}

func TestChatSupplementClaimAndAckThroughDaemonClient(t *testing.T) {
	f := steeredFixture(t)
	client := newChatSupplementDaemonClient(t)
	ctx := context.Background()

	sup := claimSteer(t, client, f)
	if sup.CommentID != f.messageID || sup.AuthorType != "member" || sup.Content != "steer me" {
		t.Fatalf("claimed = %+v", sup)
	}
	if row := readChatReceipt(t, f); row.status != "delivering" || row.attempts != 1 {
		t.Fatalf("after claim = %+v", row)
	}
	if again, err := client.ClaimTaskSupplement(ctx, f.runningID); err != nil || again != nil {
		t.Fatalf("second claim = %+v, %v", again, err)
	}
	for range 2 {
		if err := client.AckTaskSupplement(ctx, f.runningID, f.messageID, true, ""); err != nil {
			t.Fatalf("ack delivered: %v", err)
		}
	}
	if err := client.AckTaskSupplement(ctx, f.runningID, f.messageID, false, protocol.TaskSupplementFailureTimeout); err != nil {
		t.Fatalf("late negative ack: %v", err)
	}
	if row := readChatReceipt(t, f); row.status != "delivered" || !row.delivered || row.reason != "" {
		t.Fatalf("after acks = %+v", row)
	}
}

func TestChatSupplementNegativeAckLeavesReceiptUncertain(t *testing.T) {
	for _, tc := range []struct{ sent, stored string }{
		{protocol.TaskSupplementFailureTimeout, protocol.TaskSupplementFailureTimeout},
		{protocol.TaskSupplementFailureTurnEnded, protocol.TaskSupplementFailureTurnEnded},
		{protocol.TaskSupplementFailureTurnNotStarted, protocol.TaskSupplementFailureTurnNotStarted},
		{protocol.TaskSupplementFailureProviderRejected, protocol.TaskSupplementFailureProviderRejected},
		{"some_future_code", protocol.TaskSupplementFailureProviderRejected},
	} {
		t.Run(tc.sent, func(t *testing.T) {
			f := steeredFixture(t)
			client := newChatSupplementDaemonClient(t)
			claimSteer(t, client, f)
			if err := client.AckTaskSupplement(context.Background(), f.runningID, f.messageID, false, tc.sent); err != nil {
				t.Fatalf("negative ack: %v", err)
			}
			if row := readChatReceipt(t, f); row.status != "uncertain" || row.reason != tc.stored {
				t.Fatalf("after negative ack = %+v", row)
			}
			wantConflictCode(t, resendChat(t, f, testUserID, false), "supplement_delivery_uncertain")
		})
	}
}

func TestChatSupplementTerminalSettlement(t *testing.T) {
	client := newChatSupplementDaemonClient(t)
	ctx := context.Background()

	t.Run("never claimed", func(t *testing.T) {
		f := steeredFixture(t)
		endChatTurn(t, f.runningID)
		if row := readChatReceipt(t, f); row.status != "failed" || row.reason != "turn_ended" {
			t.Fatalf("never-claimed receipt = %+v", row)
		}
		err := client.AckTaskSupplement(ctx, f.runningID, f.messageID, true, "")
		if err == nil || !strings.Contains(err.Error(), "409") {
			t.Fatalf("ack on failed receipt = %v, want 409", err)
		}
	})
	t.Run("claimed", func(t *testing.T) {
		f := steeredFixture(t)
		claimSteer(t, client, f)
		endChatTurn(t, f.runningID)
		if row := readChatReceipt(t, f); row.status != "uncertain" || row.reason != "turn_ended_unconfirmed" {
			t.Fatalf("claimed receipt = %+v", row)
		}
		if err := client.AckTaskSupplement(ctx, f.runningID, f.messageID, true, ""); err != nil {
			t.Fatalf("late ack after settlement: %v", err)
		}
		if row := readChatReceipt(t, f); row.status != "delivered" || row.ackedAfter {
			t.Fatalf("late-acked receipt = %+v", row)
		}
	})
}

func TestChatSupplementClaimContentCarriesAttachments(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		attach     bool
	}{
		{"text only", "steer me", false},
		{"mixed", "steer me", true},
		{"attachment only", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChatSteerFixture(t, "capable")
			dbfx.Exec(t, `UPDATE chat_message SET content = $2 WHERE id = $1`, f.messageID, tc.text)
			var attID string
			if tc.attach {
				attID = seedMessageAttachment(t, f.sessionID, f.messageID, "plan.png", "2026-10-01T00:00:00Z")
			}
			steerChat(t, f.sessionID, f.queuedID, testUserID).Want(http.StatusOK)
			sup := claimSteer(t, newChatSupplementDaemonClient(t), f)
			block := "Attachments on this message:\n- id=" + attID + " filename=\"plan.png\" content_type=image/png\n" +
				"Use `multica attachment download <id>` to fetch each file locally before referring to it.\n"
			switch {
			case !tc.attach:
				if sup.Content != "steer me" {
					t.Fatalf("content = %q", sup.Content)
				}
			case tc.text == "":
				if !strings.HasPrefix(sup.Content, block) {
					t.Fatalf("attachment-only content = %q", sup.Content)
				}
			default:
				if !strings.HasPrefix(sup.Content, "steer me\n\n"+block) {
					t.Fatalf("mixed content = %q", sup.Content)
				}
			}
			if !tc.attach {
				return
			}
			// The agent fetches the steered file with its task identity.
			req := withURLParam(newRequest(http.MethodGet, "/api/attachments/"+attID, nil), "id", attID)
			req.Header.Set("X-Actor-Source", "task_token")
			var att AttachmentResponse
			testutil.Call(t, testHandler.GetAttachmentByID, req).Want(http.StatusOK).JSON(&att)
			if att.ID != attID {
				t.Fatalf("downloaded attachment = %+v", att)
			}
		})
	}
}

func TestChatSupplementResendFailedReceipt(t *testing.T) {
	f := steeredFixture(t)
	endChatTurn(t, f.runningID)

	var first, again ResendChatSupplementResponse
	resendChat(t, f, testUserID, false).Want(http.StatusOK).JSON(&first)
	resendChat(t, f, testUserID, false).Want(http.StatusOK).JSON(&again)
	if first.TaskID == "" || again.TaskID != first.TaskID || first.Supplement.ResentTaskID == nil || *first.Supplement.ResentTaskID != first.TaskID {
		t.Fatalf("resends = %+v / %+v", first, again)
	}
	if got := chatTaskStatus(t, first.TaskID); got != "queued" {
		t.Fatalf("resend task status = %q", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, f.sessionID); n != 1 {
		t.Fatalf("queued tasks = %d, want 1", n)
	}
	inputs, err := testHandler.Queries.ListChatInputMessages(context.Background(), parseUUID(first.TaskID))
	if err != nil || len(inputs) != 1 || uuidToString(inputs[0].ID) != f.messageID {
		t.Fatalf("resend task input = %+v, %v", inputs, err)
	}
	if row := readChatReceipt(t, f); row.resentTaskID != first.TaskID || row.resentUncert {
		t.Fatalf("receipt after resend = %+v", row)
	}

	// The new run receives the message without the duplicate warning.
	var claimed claimAttachmentsResponse
	claimForRuntimeExpecting(t, testHandler, f.runtimeID, f.daemonID, http.StatusOK).JSON(&claimed)
	if claimed.Task == nil || !strings.Contains(claimed.Task.ChatMessage, "steer me") ||
		strings.Contains(claimed.Task.ChatMessage, service.ChatSupplementResentUncertainNotes) {
		t.Fatalf("resend claim = %+v", claimed.Task)
	}
}

// Steering a resent message is a new request: it joins the run that is active
// now, and never replays the receipt the resend consumed.
func TestSteerResentMessageJoinsCurrentRun(t *testing.T) {
	f := steeredFixture(t)
	endChatTurn(t, f.runningID)
	runB := sendDirectChat(t, context.Background(), f.agentID, f.sessionID, "second turn")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'dispatched', dispatched_at = now() WHERE id = $1`, runB)
	startChatTask(t, runB, protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1)
	var resent ResendChatSupplementResponse
	resendChat(t, f, testUserID, false).Want(http.StatusOK).JSON(&resent)

	var receipt ChatSupplementResponse
	steerChat(t, f.sessionID, resent.TaskID, testUserID).Want(http.StatusOK).JSON(&receipt)
	if receipt.TaskID != runB || receipt.Status != "pending" {
		t.Fatalf("steer of resent task = %+v, want a pending receipt for run %s", receipt, runB)
	}
	if got := chatTaskStatus(t, resent.TaskID); got != "cancelled" {
		t.Fatalf("resent task status = %q, want cancelled", got)
	}
	if row := readChatReceipt(t, f); row.status != "failed" || row.resentTaskID != resent.TaskID {
		t.Fatalf("consumed receipt changed: %+v", row)
	}
	var replay ChatSupplementResponse
	steerChat(t, f.sessionID, resent.TaskID, testUserID).Want(http.StatusOK).JSON(&replay)
	if replay.TaskID != runB {
		t.Fatalf("replayed steer = %+v", replay)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_task_supplement WHERE message_id = $1`, f.messageID); n != 2 {
		t.Fatalf("receipts = %d, want 2", n)
	}

	req := withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+f.sessionID+"/messages", nil), "sessionId", f.sessionID)
	var messages []ChatMessageResponse
	testutil.Call(t, testHandler.ListChatMessages, chatPendingCtxAs(t, req, testUserID)).Want(http.StatusOK).JSON(&messages)
	for _, m := range messages {
		if m.ID == f.messageID && (m.Supplement == nil || m.Supplement.TaskID != runB || m.Supplement.Status != "pending") {
			t.Fatalf("bubble receipt = %+v, want the run %s receipt", m.Supplement, runB)
		}
	}
	sup, err := newChatSupplementDaemonClient(t).ClaimTaskSupplement(context.Background(), runB)
	if err != nil || sup == nil || sup.CommentID != f.messageID {
		t.Fatalf("run B claim = %+v, %v", sup, err)
	}
}

// A confirmed resend of an uncertain receipt can queue behind the same run that
// may already hold the message; steering it back into that run is refused.
func TestSteerResentMessageRefusesSameRun(t *testing.T) {
	f := steeredFixture(t)
	client := newChatSupplementDaemonClient(t)
	claimSteer(t, client, f)
	if err := client.AckTaskSupplement(context.Background(), f.runningID, f.messageID, false, protocol.TaskSupplementFailureTimeout); err != nil {
		t.Fatalf("negative ack: %v", err)
	}
	var resent ResendChatSupplementResponse
	resendChat(t, f, testUserID, true).Want(http.StatusOK).JSON(&resent)

	wantConflictCode(t, steerChat(t, f.sessionID, resent.TaskID, testUserID), "supplement_not_resendable")
	if got := chatTaskStatus(t, resent.TaskID); got != "queued" {
		t.Fatalf("refused steer changed the resent task to %q", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_task_supplement WHERE message_id = $1`, f.messageID); n != 1 {
		t.Fatalf("receipts = %d, want 1", n)
	}
}

func TestChatSupplementResendAccess(t *testing.T) {
	f := steeredFixture(t)
	endChatTurn(t, f.runningID)
	privateAgentID, _, memberID := privateAgentTestFixture(t)
	// The existing creator gate answers a non-creator with 403.
	resendChat(t, f, memberID, false).Want(http.StatusForbidden)

	sessionID := insertChatSessionAs(t, privateAgentID, memberID)
	queuedID, messageID, _ := insertQueuedChatInputWithAttachment(t, privateAgentID, sessionID, "private")
	runningID := insertPendingChatTask(t, privateAgentID, sessionID, "completed")
	dbfx.InsertNoID(t, "chat_task_supplement", testutil.Cols{
		"message_id": messageID, "task_id": runningID, "workspace_id": testWorkspaceID,
		"chat_session_id": sessionID, "author_id": memberID, "status": "failed",
	}, `chat_session_id = $1`, sessionID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, queuedID)
	private := chatSteerFixture{sessionID: sessionID, messageID: messageID, runningID: runningID}
	resendChat(t, private, memberID, false).Want(http.StatusForbidden)
	if row := readChatReceipt(t, private); row.resentTaskID != "" {
		t.Fatalf("refused resend wrote %+v", row)
	}
}

// The ack is held back while the turn ends and the user resends: a late ack
// still lands, and both the user and the new run are told of the duplicate.
func TestChatSupplementLateAckAfterConfirmedResend(t *testing.T) {
	f := steeredFixture(t)
	client := newChatSupplementDaemonClient(t)
	claimSteer(t, client, f)
	endChatTurn(t, f.runningID)
	if row := readChatReceipt(t, f); row.status != "uncertain" {
		t.Fatalf("settled receipt = %+v", row)
	}
	wantConflictCode(t, resendChat(t, f, testUserID, false), "supplement_delivery_uncertain")

	var resent ResendChatSupplementResponse
	resendChat(t, f, testUserID, true).Want(http.StatusOK).JSON(&resent)
	if row := readChatReceipt(t, f); row.resentTaskID != resent.TaskID || !row.resentUncert {
		t.Fatalf("receipt after confirmed resend = %+v", row)
	}
	if err := client.AckTaskSupplement(context.Background(), f.runningID, f.messageID, true, ""); err != nil {
		t.Fatalf("late ack: %v", err)
	}
	if row := readChatReceipt(t, f); row.status != "delivered" || !row.ackedAfter || row.attempts != 1 {
		t.Fatalf("receipt after late ack = %+v", row)
	}
	if err := client.AckTaskSupplement(context.Background(), f.runningID, f.messageID, false, protocol.TaskSupplementFailureTimeout); err != nil {
		t.Fatalf("negative ack after resend: %v", err)
	}
	if row := readChatReceipt(t, f); row.status != "delivered" {
		t.Fatalf("negative ack after resend changed the receipt: %+v", row)
	}

	var claimed claimAttachmentsResponse
	claimForRuntimeExpecting(t, testHandler, f.runtimeID, f.daemonID, http.StatusOK).JSON(&claimed)
	if claimed.Task == nil || strings.Count(claimed.Task.ChatMessage, "steer me") != 1 ||
		strings.Count(claimed.Task.ChatMessage, service.ChatSupplementResentUncertainNotes) != 1 {
		t.Fatalf("new run input = %+v", claimed.Task)
	}
}

func TestChatSupplementAckBeforeResendRefusesResend(t *testing.T) {
	f := steeredFixture(t)
	client := newChatSupplementDaemonClient(t)
	claimSteer(t, client, f)
	endChatTurn(t, f.runningID)
	if err := client.AckTaskSupplement(context.Background(), f.runningID, f.messageID, true, ""); err != nil {
		t.Fatalf("ack: %v", err)
	}
	wantConflictCode(t, resendChat(t, f, testUserID, true), "supplement_not_resendable")
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, f.sessionID); n != 0 {
		t.Fatalf("refused resend queued %d tasks", n)
	}
}

func TestChatSupplementConcurrentResendAndAck(t *testing.T) {
	f := steeredFixture(t)
	client := newChatSupplementDaemonClient(t)
	claimSteer(t, client, f)
	endChatTurn(t, f.runningID)

	req := newRequest(http.MethodPost, "/resend", map[string]any{"confirm_possible_duplicate": true})
	req = chatPendingCtxAs(t, withURLParams(req, "sessionId", f.sessionID, "messageId", f.messageID), testUserID)
	var wg sync.WaitGroup
	var ackErr error
	var resendCode int
	wg.Add(2)
	go func() {
		defer wg.Done()
		ackErr = client.AckTaskSupplement(context.Background(), f.runningID, f.messageID, true, "")
	}()
	go func() {
		defer wg.Done()
		w := httptest.NewRecorder()
		testHandler.ResendChatSupplement(w, req)
		resendCode = w.Code
	}()
	wg.Wait()
	if ackErr != nil {
		t.Fatalf("ack: %v", ackErr)
	}
	row := readChatReceipt(t, f)
	queued := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, f.sessionID)
	switch resendCode {
	case http.StatusOK: // resend first, then the late ack
		if row.status != "delivered" || !row.ackedAfter || row.resentTaskID == "" || queued != 1 {
			t.Fatalf("resend-first outcome: %+v, queued %d", row, queued)
		}
	case http.StatusConflict: // ack first, resend refused
		if row.status != "delivered" || row.ackedAfter || row.resentTaskID != "" || queued != 0 {
			t.Fatalf("ack-first outcome: %+v, queued %d", row, queued)
		}
	default:
		t.Fatalf("resend status = %d", resendCode)
	}
}
