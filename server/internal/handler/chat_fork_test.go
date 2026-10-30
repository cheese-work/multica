package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func forkChatRequest(test *testing.T, sessionID, key string, extra ...map[string]any) *http.Request {
	test.Helper()
	body := map[string]any{"idempotency_key": key}
	for _, fields := range extra {
		for name, value := range fields {
			body[name] = value
		}
	}
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/fork", body)
	return withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
}

func cleanupChatFork(test *testing.T, childID string) {
	test.Helper()
	dbfx.Cleanup(test, `DELETE FROM chat_session WHERE id = $1`, childID)
	dbfx.Cleanup(test, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, childID)
	dbfx.Cleanup(test, `DELETE FROM attachment WHERE chat_session_id = $1`, childID)
	dbfx.Cleanup(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, childID)
}

func forkMessage(test *testing.T, sessionID string, columns testutil.Cols) string {
	test.Helper()
	columns["chat_session_id"] = sessionID
	return dbfx.Insert(test, "chat_message", columns)
}

func forkAttachment(test *testing.T, sessionID, messageID, name string) string {
	test.Helper()
	return dbfx.Insert(test, "attachment", testutil.Cols{
		"workspace_id": testWorkspaceID, "chat_session_id": sessionID, "chat_message_id": messageID,
		"uploader_type": "member", "uploader_id": testUserID, "filename": name,
		"url": "https://cdn.example.test/" + uuid.NewString() + "/" + name, "content_type": "text/plain", "size_bytes": 12,
	})
}

// seedForkHistory writes one sentence per row kind a fork must copy or skip,
// and returns the source rows the fork copies, in list order.
// forkSourceOnWire is the fork's source-side boundary as the client sees it: the
// last source message the fork copied. fork_last_message_id names the child's
// own copy of it instead.
func forkSourceOnWire(test *testing.T, session ChatSessionResponse) any {
	test.Helper()
	encoded, err := json.Marshal(session)
	if err != nil {
		test.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		test.Fatal(err)
	}
	return wire["fork_source_message_id"]
}

func seedForkHistory(test *testing.T, agentID, sourceID string) (copied []string, attachmentID string) {
	test.Helper()
	at := func(ago string) testutil.Cols {
		return testutil.Cols{"created_at": testutil.Raw("now() - interval '" + ago + "'")}
	}
	row := func(ago, role, content string, extra testutil.Cols) string {
		columns := at(ago)
		columns["role"], columns["content"] = role, content
		for name, value := range extra {
			columns[name] = value
		}
		return forkMessage(test, sourceID, columns)
	}
	copied = append(copied, row("10 minutes", "user", "Source goal", nil))
	row("9 minutes", "user", "hidden kickoff", testutil.Cols{"message_kind": "onboarding_kickoff"})
	copied = append(copied, row("8 minutes", "assistant", "Welcome", testutil.Cols{"message_kind": "onboarding_opening"}))
	copied = append(copied, row("7 minutes", "assistant", "", testutil.Cols{"failure_reason": "agent crashed", "elapsed_ms": 1200}))
	row("6 minutes", "assistant", "", testutil.Cols{"message_kind": "no_response"})
	row("5 minutes 30 seconds", "user", "/new", testutil.Cols{"message_kind": "channel_command"})
	equalTime := testutil.Cols{"created_at": testutil.Raw("date_trunc('second', now()) - interval '5 minutes'")}
	first := forkMessage(test, sourceID, testutil.Cols{"role": "assistant", "content": "equal one", "created_at": equalTime["created_at"]})
	second := forkMessage(test, sourceID, testutil.Cols{"role": "assistant", "content": "equal two", "created_at": equalTime["created_at"]})
	var equalOrder []string
	rows, err := testPool.Query(context.Background(), `SELECT id FROM chat_message WHERE id IN ($1, $2) ORDER BY created_at, id`, first, second)
	if err != nil {
		test.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			test.Fatal(err)
		}
		equalOrder = append(equalOrder, id)
	}
	rows.Close()
	copied = append(copied, equalOrder...)
	doneTask := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sourceID, "status": "completed", "runtime_id": handlerTestRuntimeID(test)})
	copied = append(copied, row("4 minutes", "assistant", "Reply with actions", testutil.Cols{
		"task_id": doneTask, "quick_actions": `[{"label":"Next","prompt":"next"}]`,
	}))
	copied = append(copied, row("3 minutes", "assistant", "Earlier summary", testutil.Cols{"message_kind": "continuation_summary"}))
	withLink := row("2 minutes", "user", "placeholder", nil)
	attachmentID = forkAttachment(test, sourceID, withLink, "notes.txt")
	dbfx.Exec(test, `UPDATE chat_message SET content = $2 WHERE id = $1`, withLink,
		"See [notes](/api/attachments/"+attachmentID+"/download) please")
	copied = append(copied, withLink)
	attachmentOnly := row("1 minute 30 seconds", "user", "", nil)
	forkAttachment(test, sourceID, attachmentOnly, "image.png")
	copied = append(copied, attachmentOnly)
	running := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sourceID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	queued := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sourceID, "status": "queued", "runtime_id": handlerTestRuntimeID(test)})
	copied = append(copied, row("1 minute 10 seconds", "user", "current turn", testutil.Cols{"task_id": running}))
	row("1 minute", "user", "queued follow-up", testutil.Cols{"task_id": queued})
	return copied, attachmentID
}

type forkedRow struct {
	ID, Role, Content, Kind, QuickActions string
	CreatedAt                             string
	FailureReason                         *string
	ElapsedMs                             *int64
	HasTask                               bool
}

func forkRows(test *testing.T, where string, argument any) []forkedRow {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id::text, role, content, message_kind, quick_actions::text, created_at::text, failure_reason, elapsed_ms, task_id IS NOT NULL
		FROM chat_message WHERE `+where+` ORDER BY created_at, id`, argument)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var result []forkedRow
	for rows.Next() {
		var row forkedRow
		if err := rows.Scan(&row.ID, &row.Role, &row.Content, &row.Kind, &row.QuickActions, &row.CreatedAt, &row.FailureReason, &row.ElapsedMs, &row.HasTask); err != nil {
			test.Fatal(err)
		}
		result = append(result, row)
	}
	return result
}

func TestForkChatSessionCopiesVisibleHistory(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-copy", []byte("[]"))
	runtimeID := handlerTestRuntimeID(test)
	projectID := dbfx.Project(test, "Fork project")
	sourceID := dbfx.ChatSession(test, agentID, testutil.Cols{
		"title": "Explore", "project_id": projectID, "session_id": "provider-source", "work_dir": "/tmp/fork-source", "runtime_id": runtimeID,
	})
	copied, sourceAttachmentID := seedForkHistory(test, agentID, sourceID)
	var sourceBefore string
	dbfx.QueryRow(test, `SELECT row_to_json(session)::text FROM chat_session session WHERE id = $1`, sourceID).Scan(&sourceBefore)
	sourceMessages := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sourceID)

	var result ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "operation-one")).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(test, result.Session.ID)

	var sourceAfter string
	dbfx.QueryRow(test, `SELECT row_to_json(session)::text FROM chat_session session WHERE id = $1`, sourceID).Scan(&sourceAfter)
	if sourceBefore != sourceAfter || dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sourceID) != sourceMessages {
		test.Fatal("fork changed the source DM")
	}
	child, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(result.Session.ID))
	if err != nil {
		test.Fatal(err)
	}
	if child.Title != "Explore (fork)" || child.Status != "active" || child.SessionID.Valid || child.WorkDir.Valid || child.RuntimeID.Valid ||
		child.ContinuedFromSessionID.Valid || uuidToString(child.ForkedFromSessionID) != sourceID || uuidToString(child.ProjectID) != projectID {
		test.Fatalf("fork child did not start fresh: %+v", child)
	}

	sourceRows := forkRows(test, `id = ANY($1::uuid[])`, copied)
	childRows := forkRows(test, `chat_session_id = $1`, result.Session.ID)
	if len(childRows) != len(copied) {
		test.Fatalf("copied %d rows, want %d: %+v", len(childRows), len(copied), childRows)
	}
	for index, source := range sourceRows {
		copy := childRows[index]
		wantKind := source.Kind
		if wantKind == "onboarding_opening" {
			wantKind = "message"
		}
		wantContent := source.Content
		if strings.Contains(wantContent, "/api/attachments/") {
			wantContent = copy.Content
			if strings.Contains(copy.Content, sourceAttachmentID) || !strings.Contains(copy.Content, "/api/attachments/") {
				test.Fatalf("attachment link was not rewritten: %q", copy.Content)
			}
		}
		if copy.Role != source.Role || copy.Content != wantContent || copy.Kind != wantKind || copy.CreatedAt != source.CreatedAt ||
			!reflect.DeepEqual(copy.FailureReason, source.FailureReason) || !reflect.DeepEqual(copy.ElapsedMs, source.ElapsedMs) ||
			copy.HasTask || copy.QuickActions != "[]" {
			test.Fatalf("row %d copy = %+v, source = %+v", index, copy, source)
		}
	}
	var byID []string
	dbfx.QueryRow(test, `SELECT array_agg(id::text ORDER BY id) FROM chat_message WHERE chat_session_id = $1`, result.Session.ID).Scan(&byID)
	for index, row := range childRows {
		if byID[index] != row.ID {
			test.Fatal("copied ids are not generated in source order")
		}
	}
	last := childRows[len(childRows)-1].ID
	if uuidToString(child.ForkLastMessageID) != last || !child.ForkMessageCount.Valid || int(child.ForkMessageCount.Int32) != len(copied) {
		test.Fatalf("fork fields = last %s count %+v, want %s and %d", uuidToString(child.ForkLastMessageID), child.ForkMessageCount, last, len(copied))
	}
	if result.Session.ForkLastMessageID == nil || *result.Session.ForkLastMessageID != last || result.Session.ForkMessageCount == nil ||
		int(*result.Session.ForkMessageCount) != len(copied) || result.Session.ForkedFromSessionID == nil || *result.Session.ForkedFromSessionID != sourceID {
		test.Fatalf("response fork fields = %+v", result.Session)
	}
	if got := forkSourceOnWire(test, result.Session); got != copied[len(copied)-1] {
		test.Fatalf("response fork_source_message_id = %v, want the last copied source message %s", got, copied[len(copied)-1])
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE id = $1 AND last_read_at = (SELECT max(created_at) FROM chat_message WHERE chat_session_id = $1)`, result.Session.ID); count != 1 {
		test.Fatal("last_read_at is not the newest copied time")
	}
	if result.PreviousSessionID != sourceID || !strings.Contains(result.URL, result.Session.ID) || result.Message != nil {
		test.Fatalf("fork response = %+v", result)
	}

	var attachments []struct{ URL, MessageID string }
	rows, err := testPool.Query(context.Background(), `
		SELECT url, chat_message_id::text FROM attachment
		WHERE chat_session_id = $1 AND task_id IS NULL AND chat_message_id IN (SELECT id FROM chat_message WHERE chat_session_id = $1)
		ORDER BY created_at, id`, result.Session.ID)
	if err != nil {
		test.Fatal(err)
	}
	for rows.Next() {
		var attachment struct{ URL, MessageID string }
		if err := rows.Scan(&attachment.URL, &attachment.MessageID); err != nil {
			test.Fatal(err)
		}
		attachments = append(attachments, attachment)
	}
	rows.Close()
	if len(attachments) != 2 || dbfx.Count(test, `SELECT count(*) FROM attachment a JOIN attachment s ON s.url = a.url WHERE a.chat_session_id = $1 AND s.chat_session_id = $2`, result.Session.ID, sourceID) != 2 {
		test.Fatalf("attachments not copied onto the same stored files: %+v", attachments)
	}
	var linked string
	dbfx.QueryRow(test, `SELECT id::text FROM attachment WHERE chat_session_id = $1 AND filename = 'notes.txt'`, result.Session.ID).Scan(&linked)
	if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND content LIKE '%/api/attachments/' || $2 || '/download%'`, result.Session.ID, linked) != 1 {
		test.Fatal("copied text does not link the new attachment id")
	}

	for _, status := range []string{"active", "all"} {
		var sessions []ChatSessionResponse
		request := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/?status="+status, nil))
		testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
		found := false
		for _, session := range sessions {
			if session.ID == result.Session.ID {
				found = true
				if session.UnreadCount != 0 || session.ForkMessageCount == nil || session.ForkLastMessageID == nil ||
					forkSourceOnWire(test, session) != copied[len(copied)-1] {
					test.Fatalf("%s list shows the fork wrong: %+v", status, session)
				}
			}
		}
		if !found {
			test.Fatalf("%s list lost the fork", status)
		}
	}
}

func TestForkChatSessionWithMessageStartsFirstTurn(test *testing.T) {
	ctx := context.Background()
	agentID, sourceID, _, _ := setupDirectChatSession(test, ctx, "Src")
	forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "Plan the schema", "created_at": testutil.Raw("now() - interval '2 minutes'")})
	forkMessage(test, sourceID, testutil.Cols{"role": "assistant", "content": "Use Postgres", "created_at": testutil.Raw("now() - interval '1 minute'")})
	sourceTasks := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sourceID)
	message := "  try   the SQLite\n variant instead of postgres today  "

	var result, replay, otherText ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "with-message", map[string]any{"message": message})).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(test, result.Session.ID)
	if result.Session.Title != "Src (fork: try the SQLite variant instead of postgr…)" {
		test.Fatalf("title = %q", result.Session.Title)
	}
	rows := forkRows(test, `chat_session_id = $1`, result.Session.ID)
	if len(rows) != 3 || rows[2].Role != "user" || rows[2].Content != strings.TrimSpace(message) || !rows[2].HasTask || rows[2].Kind != "message" {
		test.Fatalf("child rows = %+v", rows)
	}
	if result.Message == nil || result.Message.ID != rows[2].ID || result.Session.ForkLastMessageID == nil || *result.Session.ForkLastMessageID != rows[1].ID || *result.Session.ForkMessageCount != 2 {
		test.Fatalf("fork point or response message wrong: %+v", result)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND chat_input_task_id = id AND status = 'queued'`, result.Session.ID); count != 1 {
		test.Fatalf("child turns = %d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message m JOIN agent_task_queue t ON t.id = m.task_id WHERE m.chat_session_id = $1 AND t.chat_session_id = $1`, result.Session.ID); count != 1 {
		test.Fatal("the first message is not the input of the child turn")
	}
	if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sourceID) != sourceTasks ||
		dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sourceID) != 2 {
		test.Fatal("the fork sent something in the source")
	}

	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "with-message", map[string]any{"message": message})).Want(http.StatusOK).JSON(&replay)
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "with-message", map[string]any{"message": "something else"})).Want(http.StatusOK).JSON(&otherText)
	for _, again := range []ChatForkResponse{replay, otherText} {
		if again.Session.ID != result.Session.ID || again.Message == nil || again.Message.ID != result.Message.ID {
			test.Fatalf("replay = %+v, want child %s message %s", again, result.Session.ID, result.Message.ID)
		}
	}
	if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, result.Session.ID) != 3 ||
		dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, result.Session.ID) != 1 {
		test.Fatal("a replay copied or sent again")
	}

	var short, blank ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "short", map[string]any{"message": "hi there"})).Want(http.StatusCreated).JSON(&short)
	cleanupChatFork(test, short.Session.ID)
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "blank", map[string]any{"message": " \n\t "})).Want(http.StatusCreated).JSON(&blank)
	cleanupChatFork(test, blank.Session.ID)
	if short.Session.Title != "Src (fork: hi there)" || blank.Session.Title != "Src (fork)" || blank.Message != nil {
		test.Fatalf("titles = %q / %q, blank message = %+v", short.Session.Title, blank.Session.Title, blank.Message)
	}
	if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, blank.Session.ID) != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, blank.Session.ID) != 2 {
		test.Fatal("a fork without a message sent something")
	}
	_ = agentID
}

type failingQueryStarter struct {
	inner txStarter
	name  string
}

func (starter failingQueryStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingQueryTx{Tx: transaction, name: starter.name}, nil
}

type failingQueryTx struct {
	pgx.Tx
	name string
}

func (transaction failingQueryTx) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	if strings.Contains(query, "-- name: "+transaction.name+" ") {
		return continuationFailureRow{}
	}
	return transaction.Tx.QueryRow(ctx, query, arguments...)
}

func (transaction failingQueryTx) Exec(ctx context.Context, query string, arguments ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "-- name: "+transaction.name+" ") {
		return pgconn.CommandTag{}, errors.New("injected " + transaction.name + " failure")
	}
	return transaction.Tx.Exec(ctx, query, arguments...)
}

func TestForkChatSessionRollsBackFailedSteps(test *testing.T) {
	ctx := context.Background()
	agentID, sourceID, _, _ := setupDirectChatSession(test, ctx, "Rollback")
	forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "Keep this"})
	for _, name := range []string{"InsertForkChatMessages", "SetChatForkSource", "CreateChatTask", "CreateChatMessage", "PlaceForkFirstMessage"} {
		test.Run(name, func(test *testing.T) {
			handler := *testHandler
			handler.TxStarter = failingQueryStarter{inner: testHandler.TxStarter, name: name}
			testutil.Call(test, handler.ForkChatSession, forkChatRequest(test, sourceID, "failed-"+name, map[string]any{"message": "go"})).Want(http.StatusInternalServerError)
			if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID); count != 1 {
				test.Fatalf("failed fork left a child: %d sessions", count)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.agent_id = $1`, agentID); count != 1 {
				test.Fatalf("failed fork left copies: %d messages", count)
			}
		})
	}
}

func TestForkChatSessionTaskKeyUsesInputOwner(test *testing.T) {
	ctx := context.Background()
	agentID, sourceID, _, _ := setupDirectChatSession(test, ctx, "Task key")
	original := sendDirectChat(test, ctx, agentID, sourceID, "Fork this for me")
	markTaskRunning(test, ctx, original)
	taskRequest := func(taskID string, extra ...map[string]any) *http.Request {
		request := forkChatRequest(test, "", "", extra...)
		request.Header.Set("X-Actor-Source", "task_token")
		request.Header.Set("X-Task-ID", taskID)
		return request
	}
	var first, retried, other ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, taskRequest(original)).Want(http.StatusCreated).JSON(&first)
	cleanupChatFork(test, first.Session.ID)

	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'failed', failure_reason = 'timeout', completed_at = now() WHERE id = $1`, original)
	parent, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(original))
	if err != nil {
		test.Fatal(err)
	}
	retry, err := testHandler.TaskService.MaybeRetryFailedTask(ctx, parent)
	if err != nil || retry == nil || uuidToString(retry.ChatInputTaskID) != original {
		test.Fatalf("auto-retry clone = %+v, %v", retry, err)
	}
	markTaskRunning(test, ctx, uuidToString(retry.ID))
	testutil.Call(test, testHandler.ForkChatSession, taskRequest(uuidToString(retry.ID))).Want(http.StatusOK).JSON(&retried)
	if retried.Session.ID != first.Session.ID {
		test.Fatalf("auto-retry created a second child: %s vs %s", retried.Session.ID, first.Session.ID)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE forked_from_session_id = $1`, sourceID); count != 1 {
		test.Fatalf("children = %d, want 1", count)
	}
	testutil.Call(test, testHandler.ForkChatSession, taskRequest(uuidToString(retry.ID), map[string]any{"message": "agent text"})).Want(http.StatusBadRequest)

	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, uuidToString(retry.ID))
	next := sendDirectChat(test, ctx, agentID, sourceID, "Fork again")
	markTaskRunning(test, ctx, next)
	testutil.Call(test, testHandler.ForkChatSession, taskRequest(next)).Want(http.StatusCreated).JSON(&other)
	cleanupChatFork(test, other.Session.ID)
	if other.Session.ID == first.Session.ID {
		test.Fatal("a new turn reused the earlier turn's fork")
	}
}

func TestForkChatSessionAuthorizationAndValidation(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-auth", []byte("[]"))
	builderID := createHandlerTestAgent(test, "fork-builder", []byte("[]"))
	dbfx.Exec(test, `UPDATE agent SET kind = 'system', system_key = 'agent_builder:' || id::text WHERE id = $1`, builderID)
	for _, name := range []string{"other-human", "agent", "other-chat", "issue-task", "completed-task", "wrong-agent", "wrong-workspace", "malformed-task",
		"missing-key", "long-key", "nul-key", "summary", "nul-message", "long-message", "task-message", "archived", "channel", "empty", "builder"} {
		test.Run(name, func(test *testing.T) {
			owner := agentID
			if name == "builder" {
				owner = builderID
			}
			sourceID := dbfx.ChatSession(test, owner)
			if name != "empty" {
				forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "Something to copy"})
			}
			request := forkChatRequest(test, sourceID, "operation")
			want := http.StatusForbidden
			switch name {
			case "other-human":
				request.Header.Set("X-User-ID", uuid.NewString())
			case "agent":
				request.Header.Set("X-Agent-ID", agentID)
			case "other-chat", "issue-task", "completed-task", "wrong-agent", "wrong-workspace", "task-message":
				columns := testutil.Cols{"status": "running", "chat_session_id": sourceID, "runtime_id": handlerTestRuntimeID(test)}
				taskAgentID := agentID
				switch name {
				case "other-chat":
					columns["chat_session_id"] = dbfx.ChatSession(test, agentID)
				case "issue-task":
					delete(columns, "chat_session_id")
				case "completed-task":
					columns["status"] = "completed"
				case "wrong-agent":
					taskAgentID = createHandlerTestAgent(test, "fork-wrong-agent", []byte("[]"))
				case "wrong-workspace":
					request = request.WithContext(middleware.SetMemberContext(request.Context(), uuid.NewString(), db.Member{}))
				case "task-message":
					request = forkChatRequest(test, sourceID, "operation", map[string]any{"message": "from the agent"})
					want = http.StatusBadRequest
				}
				taskID := dbfx.Task(test, taskAgentID, columns)
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Task-ID", taskID)
			case "malformed-task":
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Task-ID", "not-a-uuid")
			case "missing-key", "long-key", "nul-key":
				key := map[string]string{"missing-key": "", "long-key": strings.Repeat("x", 129), "nul-key": "key\x00"}[name]
				request = forkChatRequest(test, sourceID, key)
				want = http.StatusBadRequest
			case "summary", "nul-message", "long-message":
				extra := map[string]map[string]any{
					"summary":      {"summary": "Goal: keep going."},
					"nul-message":  {"message": "go\x00"},
					"long-message": {"message": strings.Repeat("x", forkRequestMaxBytes)},
				}[name]
				request = forkChatRequest(test, sourceID, "operation", extra)
				want = http.StatusBadRequest
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, sourceID)
				want = http.StatusConflict
			case "channel":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, sourceID)
				want = http.StatusConflict
			case "empty", "builder":
				want = http.StatusConflict
			}
			before, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sourceID))
			if err != nil {
				test.Fatal(err)
			}
			response := testutil.Call(test, testHandler.ForkChatSession, request).Want(want)
			if name == "summary" && !strings.Contains(response.Text(), "summary is not accepted") {
				test.Fatalf("summary rejection = %s", response.Text())
			}
			if name == "empty" && !strings.Contains(response.Text(), "nothing to fork yet") {
				test.Fatalf("empty rejection = %s", response.Text())
			}
			after, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sourceID))
			if err != nil || before != after || dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE forked_from_session_id = $1`, sourceID) != 0 {
				test.Fatalf("rejected fork changed source or created a child: %+v, %v", after, err)
			}
		})
	}
}

func TestForkChatSessionLeavesDecisionAndTaskOwnership(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	decision, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	forkMessage(test, fixture.sessionID, testutil.Cols{"role": "user", "content": "Context"})
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"chat_session_id": fixture.sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	answerID := dbfx.Insert(test, "decision_answer", testutil.Cols{
		"decision_request_id": decision.Request.ID, "answered_by_task_id": taskID, "source_chat_session_id": fixture.sessionID,
		"chat_message_ids": []string{uuidToString(decision.Message.ID)}, "content_snapshot": "Approved", "effective_at": testutil.Raw("now()"),
	})
	tokenID := dbfx.Insert(test, "task_token", testutil.Cols{
		"token_hash": "fork-fixture-" + uuid.NewString(), "task_id": taskID, "agent_id": fixture.agentID,
		"workspace_id": testWorkspaceID, "user_id": fixture.humanID, "expires_at": testutil.Raw("now() + interval '1 hour'"),
	})
	var answerBefore, tokenBefore string
	dbfx.QueryRow(test, `SELECT row_to_json(answer)::text FROM decision_answer answer WHERE id = $1`, answerID).Scan(&answerBefore)
	dbfx.QueryRow(test, `SELECT row_to_json(token)::text FROM task_token token WHERE id = $1`, tokenID).Scan(&tokenBefore)
	before, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	request := forkChatRequest(test, fixture.sessionID, "decision-fork")
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Task-ID", taskID)
	var child ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, request).Want(http.StatusCreated).JSON(&child)
	cleanupChatFork(test, child.Session.ID)
	stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: decision.Request.ID})
	if err != nil || !reflect.DeepEqual(stored, decision.Request) {
		test.Fatalf("fork transferred or changed the decision: %+v, %v", stored, err)
	}
	after, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil || !reflect.DeepEqual(before, after) {
		test.Fatalf("fork changed the source task: %+v, %v", after, err)
	}
	var answerAfter, tokenAfter string
	dbfx.QueryRow(test, `SELECT row_to_json(answer)::text FROM decision_answer answer WHERE id = $1`, answerID).Scan(&answerAfter)
	dbfx.QueryRow(test, `SELECT row_to_json(token)::text FROM task_token token WHERE id = $1`, tokenID).Scan(&tokenAfter)
	if answerBefore != answerAfter || tokenBefore != tokenAfter {
		test.Fatal("fork changed the source approval or task token")
	}
	for label, query := range map[string]string{
		"decisions": `SELECT count(*) FROM decision_request WHERE chat_session_id = $1`,
		"tasks":     `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`,
		"approvals": `SELECT count(*) FROM decision_answer WHERE source_chat_session_id = $1`,
		"tokens":    `SELECT count(*) FROM task_token token JOIN agent_task_queue task ON task.id = token.task_id WHERE task.chat_session_id = $1`,
	} {
		if count := dbfx.Count(test, query, child.Session.ID); count != 0 {
			test.Fatalf("child inherited %s: %d", label, count)
		}
	}
}

func TestForkChatSessionConcurrentRetry(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-concurrent", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "Race me"})
	requests := []*http.Request{forkChatRequest(test, sourceID, "same"), forkChatRequest(test, sourceID, "same"), forkChatRequest(test, sourceID, "different")}
	responses := make(chan *httptest.ResponseRecorder, len(requests))
	for _, request := range requests {
		go func(request *http.Request) {
			response := httptest.NewRecorder()
			testHandler.ForkChatSession(response, request)
			responses <- response
		}(request)
	}
	children := map[string]bool{}
	created, replayed := 0, 0
	for range requests {
		response := <-responses
		if response.Code == http.StatusCreated {
			created++
		} else if response.Code == http.StatusOK {
			replayed++
		} else {
			test.Errorf("concurrent fork: %d %s", response.Code, response.Body.String())
			continue
		}
		var child ChatForkResponse
		if err := json.Unmarshal(response.Body.Bytes(), &child); err != nil {
			test.Error(err)
			continue
		}
		if !children[child.Session.ID] {
			cleanupChatFork(test, child.Session.ID)
			children[child.Session.ID] = true
		}
	}
	if created != 2 || replayed != 1 || len(children) != 2 || dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE forked_from_session_id = $1`, sourceID) != 2 {
		test.Fatalf("concurrent operation counts: created=%d replayed=%d children=%d", created, replayed, len(children))
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.forked_from_session_id = $1`, sourceID); count != 2 {
		test.Fatalf("concurrent forks copied %d rows, want one per child", count)
	}
}

func TestForkChatSessionSharedAttachmentFileSurvivesDelete(test *testing.T) {
	store := &mockStorage{files: map[string][]byte{}}
	original := testHandler.Storage
	testHandler.Storage = store
	test.Cleanup(func() { testHandler.Storage = original })
	agentID := createHandlerTestAgent(test, "fork-delete", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	withFile := forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "file"})
	key := "fork/" + uuid.NewString() + "/shared.txt"
	store.files[key] = []byte("data")
	sourceAttachment := dbfx.Insert(test, "attachment", testutil.Cols{
		"workspace_id": testWorkspaceID, "chat_session_id": sourceID, "chat_message_id": withFile, "uploader_type": "member",
		"uploader_id": testUserID, "filename": "shared.txt", "url": "https://cdn.example.com/" + key, "content_type": "text/plain", "size_bytes": 4,
	})
	deleteAttachment := func(id string) *testutil.Response {
		return testutil.Call(test, testHandler.DeleteAttachment, withURLParam(newRequest(http.MethodDelete, "/api/attachments/"+id, nil), "id", id))
	}
	stored := func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		_, ok := store.files[key]
		return ok
	}

	// A delete that races a fork waits for the fork's share lock, then sees
	// the copy and keeps the file.
	ctx := context.Background()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(ctx)
	childID := dbfx.ChatSession(test, agentID)
	childMessage := forkMessage(test, childID, testutil.Cols{"role": "user", "content": "file"})
	queries := testHandler.Queries.WithTx(holder)
	locked, err := queries.LockChatAttachmentsForFork(ctx, db.LockChatAttachmentsForForkParams{WorkspaceID: parseUUID(testWorkspaceID), ChatMessageIds: []pgtype.UUID{parseUUID(withFile)}})
	if err != nil || len(locked) != 1 {
		test.Fatalf("fork lock = %d rows, %v", len(locked), err)
	}
	copyID := uuid.NewString()
	if err := queries.InsertForkChatAttachments(ctx, db.InsertForkChatAttachmentsParams{
		Ids: []pgtype.UUID{parseUUID(copyID)}, ChatSessionID: parseUUID(childID), ChatMessageIds: []pgtype.UUID{parseUUID(childMessage)}, SourceIds: []pgtype.UUID{parseUUID(sourceAttachment)},
	}); err != nil {
		test.Fatal(err)
	}
	done := make(chan error, 1)
	var response *testutil.Response
	go func() {
		response = deleteAttachment(sourceAttachment)
		done <- nil
	}()
	waitForCommentMutationLock(test, "LockAttachmentRow", done)
	if err := holder.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	<-done
	if response.Code != http.StatusNoContent || !stored() {
		test.Fatalf("delete during fork: status %d, file kept %v", response.Code, stored())
	}
	if deleteAttachment(copyID).Code != http.StatusNoContent || stored() {
		test.Fatal("deleting the last row kept the stored file")
	}
}

func TestForkChatSessionListsNewForkFirstAndKeepsTitle(test *testing.T) {
	ctx := context.Background()
	withStubLLM(test, stubLLMCompletion(test, http.StatusOK, "Generated Title"))
	agentID, sourceID, _, _ := setupDirectChatSession(test, ctx, "Old DM")
	forkMessage(test, sourceID, testutil.Cols{"role": "assistant", "content": "Hello from last week", "created_at": testutil.Raw("now() - interval '7 days'")})
	recentID := dbfx.ChatSession(test, agentID, testutil.Cols{"title": "Recent DM"})
	forkMessage(test, recentID, testutil.Cols{"role": "user", "content": "Recent", "created_at": testutil.Raw("now() - interval '1 hour'")})

	var child ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "list-order")).Want(http.StatusCreated).JSON(&child)
	cleanupChatFork(test, child.Session.ID)
	for _, status := range []string{"active", "all"} {
		var sessions []ChatSessionResponse
		request := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/?status="+status, nil))
		testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
		position := map[string]int{}
		for index, session := range sessions {
			position[session.ID] = index
		}
		if position[child.Session.ID] > position[recentID] {
			test.Fatalf("%s list put the new fork below an older DM: %v", status, position)
		}
	}

	request := newRequest(http.MethodPost, "/api/chat/sessions/"+child.Session.ID+"/messages", map[string]any{"content": "First question in the fork"})
	testutil.Call(test, testHandler.SendChatMessage, withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", child.Session.ID))).Want(http.StatusCreated)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if title := chatSessionTitleFromDB(test, parseUUID(child.Session.ID)); title != "Old DM (fork)" {
			test.Fatalf("first send auto-titled the fork: %q", title)
		}
	}
}

func TestForkChatSessionCopiesLargeDM(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-large", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID, testutil.Cols{"title": "Large"})
	dbfx.Exec(test, `
		INSERT INTO chat_message (chat_session_id, role, content, created_at)
		SELECT $1, CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END, repeat('x', 4096), now() - make_interval(secs => 4000 - n)
		FROM generate_series(1, 2000) AS n`, sourceID)
	dbfx.Exec(test, `
		INSERT INTO attachment (workspace_id, chat_session_id, chat_message_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
		SELECT $2, $1, m.id, 'member', $3, 'file.txt', 'https://cdn.example.com/large/' || m.id, 'text/plain', 1
		FROM (SELECT id FROM chat_message WHERE chat_session_id = $1 ORDER BY created_at LIMIT 200) AS m`, sourceID, testWorkspaceID, testUserID)
	started := time.Now()
	var child ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "large")).Want(http.StatusCreated).JSON(&child)
	elapsed := time.Since(started)
	cleanupChatFork(test, child.Session.ID)
	if child.Session.ForkMessageCount == nil || *child.Session.ForkMessageCount != 2000 ||
		dbfx.Count(test, `SELECT count(*) FROM attachment WHERE chat_session_id = $1`, child.Session.ID) != 200 {
		test.Fatalf("large copy incomplete: %+v", child.Session)
	}
	test.Logf("fork copy of 2000 messages (4 KiB) and 200 attachments took %s", elapsed)
}

func TestForkChatSessionFirstMessageSortsAfterCopiedHistory(test *testing.T) {
	ctx := context.Background()
	_, sourceID, _, _ := setupDirectChatSession(test, ctx, "Late source")
	// A source send that commits after the fork transaction starts carries a
	// later time than the fork's transaction-start now(); this row stands in
	// for it.
	forkMessage(test, sourceID, testutil.Cols{"role": "assistant", "content": "Committed during the fork", "created_at": testutil.Raw("now() + interval '1 minute'")})
	var child, replay ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "late", map[string]any{"message": "After the copy"})).Want(http.StatusCreated).JSON(&child)
	cleanupChatFork(test, child.Session.ID)
	rows := forkRows(test, `chat_session_id = $1`, child.Session.ID)
	if len(rows) != 2 || rows[0].Content != "Committed during the fork" || rows[1].Content != "After the copy" || child.Message == nil || child.Message.ID != rows[1].ID {
		test.Fatalf("first message does not sort after the copied history: %+v", rows)
	}
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "late", map[string]any{"message": "After the copy"})).Want(http.StatusOK).JSON(&replay)
	if replay.Message == nil || replay.Message.ID != child.Message.ID {
		test.Fatalf("replay lost the first message: %+v", replay.Message)
	}
}

func TestForkChatSessionRejectsBodyPaddedPastTheCap(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-padded", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "Something to copy"})
	request := forkChatRequest(test, sourceID, "padded")
	body := `{"idempotency_key":"padded"}` + strings.Repeat(" ", forkRequestMaxBytes+1)
	request.Body = io.NopCloser(strings.NewReader(body))
	request.ContentLength = int64(len(body))
	testutil.Call(test, testHandler.ForkChatSession, request).Want(http.StatusBadRequest)
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE forked_from_session_id = $1`, sourceID); count != 0 {
		test.Fatalf("a padded body created %d children", count)
	}
}

func TestForkTitleStripsMarkdown(test *testing.T) {
	cases := map[string]string{
		"[/office-hours](slash://skill/x) try":    "Src (fork: /office-hours try)",
		"> docker pull x":                         "Src (fork: docker pull x)",
		"# Heading text":                          "Src (fork: Heading text)",
		"- item one":                              "Src (fork: item one)",
		"1. first":                                "Src (fork: first)",
		"```sh\ndocker pull x\n```":               "Src (fork: docker pull x)",
		"> ```sh\n> docker pull x\n> ```":         "Src (fork: docker pull x)",
		"see [docs](https://example.com/a_b) now": "Src (fork: see docs now)",
		"": "Src (fork)",
	}
	for in, want := range cases {
		if got := forkTitle("Src", in); got != want {
			test.Errorf("forkTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func forkDraft(test *testing.T, sessionID, uploaderID string, over testutil.Cols) string {
	test.Helper()
	columns := testutil.Cols{
		"workspace_id": testWorkspaceID, "chat_session_id": sessionID, "uploader_type": "member", "uploader_id": uploaderID,
		"filename": "draft.png", "url": "https://cdn.example.test/" + uuid.NewString() + "/draft.png", "content_type": "image/png", "size_bytes": 12,
	}
	for name, value := range over {
		columns[name] = value
	}
	return dbfx.Insert(test, "attachment", columns)
}

func TestForkChatSessionMovesDraftAttachmentsToTheChild(test *testing.T) {
	ctx := context.Background()
	agentID, sourceID, _, _ := setupDirectChatSession(test, ctx, "Src")
	otherDM := dbfx.ChatSession(test, agentID)
	history := forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "earlier"})
	mine := forkDraft(test, sourceID, testUserID, nil)
	imageOnly := forkDraft(test, sourceID, testUserID, nil)
	foreign := forkDraft(test, sourceID, uuid.NewString(), nil)
	bound := forkDraft(test, sourceID, testUserID, testutil.Cols{"chat_message_id": history})
	elsewhere := forkDraft(test, otherDM, testUserID, nil)

	var result, replay ChatForkResponse
	body := map[string]any{"message": "look at this", "attachment_ids": []string{mine, foreign, bound, elsewhere}}
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "with-image", body)).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(test, result.Session.ID)
	if !reflect.DeepEqual(result.AttachmentIDs, []string{mine}) || result.Message == nil {
		test.Fatalf("response = %+v, want only %s bound", result, mine)
	}
	if dbfx.Count(test, `SELECT count(*) FROM attachment WHERE id = $1 AND chat_session_id = $2 AND chat_message_id = $3`, mine, result.Session.ID, result.Message.ID) != 1 {
		test.Fatal("the draft is not bound to the child's first message")
	}
	if dbfx.Count(test, `SELECT count(*) FROM attachment WHERE id = ANY($1::uuid[]) AND chat_session_id = $2`, []string{foreign, bound, elsewhere}, result.Session.ID) != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM attachment WHERE id = $1 AND chat_session_id = $2 AND chat_message_id = $3`, bound, sourceID, history) != 1 {
		test.Fatal("an attachment the caller does not own as a source draft was moved")
	}

	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "with-image", body)).Want(http.StatusOK).JSON(&replay)
	if replay.Session.ID != result.Session.ID || !reflect.DeepEqual(replay.AttachmentIDs, []string{mine}) ||
		dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'user'`, result.Session.ID) != 2 {
		test.Fatalf("replay = %+v, want the same child and no second send", replay)
	}

	var only ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "image-only", map[string]any{"attachment_ids": []string{imageOnly}})).Want(http.StatusCreated).JSON(&only)
	cleanupChatFork(test, only.Session.ID)
	if only.Message == nil || only.Message.Content != "" || !reflect.DeepEqual(only.AttachmentIDs, []string{imageOnly}) ||
		dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, only.Session.ID) != 1 {
		test.Fatalf("attachment-only fork = %+v", only)
	}
	if dbfx.Count(test, `SELECT count(*) FROM attachment WHERE chat_session_id = $1 AND chat_message_id IS NULL`, sourceID) != 1 {
		test.Fatal("the source kept a draft that was not the caller's (only the foreign one should remain)")
	}

	var nothing ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "unbindable", map[string]any{"attachment_ids": []string{foreign}})).Want(http.StatusCreated).JSON(&nothing)
	cleanupChatFork(test, nothing.Session.ID)
	if nothing.Message != nil {
		test.Fatalf("a fork whose only attachment was unbindable sent an empty message: %+v", nothing.Message)
	}
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "bad-id", map[string]any{"attachment_ids": []string{"nope"}})).Want(http.StatusBadRequest)
}

func TestForkChatSessionTaskScopedRejectsAttachments(test *testing.T) {
	ctx := context.Background()
	agentID, sourceID, _, _ := setupDirectChatSession(test, ctx, "Task attach")
	original := sendDirectChat(test, ctx, agentID, sourceID, "Fork this for me")
	markTaskRunning(test, ctx, original)
	request := forkChatRequest(test, "", "", map[string]any{"attachment_ids": []string{uuid.NewString()}})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Task-ID", original)
	testutil.Call(test, testHandler.ForkChatSession, request).Want(http.StatusBadRequest)
}
