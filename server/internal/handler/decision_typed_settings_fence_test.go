package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// stagePause blocks one named stage of a seam until released, and tells the
// test when the stage was reached.
type stagePause struct {
	at       string
	reached  chan struct{}
	release  chan struct{}
	once     sync.Once
	releaser sync.Once
}

func newStagePause(at string) *stagePause {
	return &stagePause{at: at, reached: make(chan struct{}), release: make(chan struct{})}
}

func (pause *stagePause) hook(stage string) {
	if stage != pause.at {
		return
	}
	pause.once.Do(func() { close(pause.reached) })
	<-pause.release
}

func (pause *stagePause) Release() { pause.releaser.Do(func() { close(pause.release) }) }

func (pause *stagePause) waitReached(test *testing.T) {
	test.Helper()
	select {
	case <-pause.reached:
	case <-time.After(10 * time.Second):
		test.Fatalf("stage %q was not reached", pause.at)
	}
}

// pauseTypedSend pauses the typed send at stage, until Release.
func pauseTypedSend(test *testing.T, stage string) *stagePause {
	pause := newStagePause(stage)
	typedSendHook = pause.hook
	test.Cleanup(func() {
		pause.Release()
		typedSendHook = func(string) {}
	})
	return pause
}

// pauseSettingsWrite pauses the owner's direct write at stage, until Release.
func pauseSettingsWrite(test *testing.T, stage string) *stagePause {
	pause := newStagePause(stage)
	settingsWriteHook = pause.hook
	test.Cleanup(func() {
		pause.Release()
		settingsWriteHook = func(string) {}
	})
	return pause
}

// watchSettingsWrite records when the write reaches stage, and never blocks it.
func watchSettingsWrite(test *testing.T, stage string) *stagePause {
	pause := newStagePause(stage)
	pause.release = make(chan struct{})
	pause.Release()
	settingsWriteHook = pause.hook
	test.Cleanup(func() { settingsWriteHook = func(string) {} })
	return pause
}

type callResult struct {
	status int
	body   string
}

// typedSingleCallBody is the single-call body of a card whose first option is a server_action.
func typedSingleCallBody(test *testing.T, fixture singleCallFixture) map[string]any {
	test.Helper()
	raw, err := json.Marshal(fixture.details)
	if err != nil {
		test.Fatal(err)
	}
	var ask map[string]any
	if err := json.Unmarshal(raw, &ask); err != nil {
		test.Fatal(err)
	}
	options := ask["options"].([]any)
	options[0].(map[string]any)["after_choice"] = "server_action"
	options[0].(map[string]any)["action"] = map[string]any{"type": "issue.status.set", "params": map[string]any{"status": "in_review"}}
	options[1].(map[string]any)["after_choice"] = "record_only"
	body := fixture.body()
	body["ask"] = ask
	return body
}

// startTypedSend posts a typed single-call card from another goroutine.
func startTypedSend(test *testing.T, fixture singleCallFixture) <-chan callResult {
	test.Helper()
	request := fixture.call(typedSingleCallBody(test, fixture))
	done := make(chan callResult, 1)
	go func() {
		recorder := httptest.NewRecorder()
		testHandler.RequestDecision(recorder, request)
		done <- callResult{recorder.Code, recorder.Body.String()}
	}()
	return done
}

// startOwnerWrite saves the settings through the owner's direct endpoint from another goroutine.
func startOwnerWrite(test *testing.T, settings map[string]any) <-chan callResult {
	test.Helper()
	request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": settings}), "id", testWorkspaceID)
	done := make(chan callResult, 1)
	go func() {
		recorder := httptest.NewRecorder()
		testHandler.UpdateWorkspace(recorder, request)
		done <- callResult{recorder.Code, recorder.Body.String()}
	}()
	return done
}

func startApproval(test *testing.T, request settingsRequest, handler *Handler) <-chan callResult {
	test.Helper()
	raw, err := json.Marshal(request.claim(test))
	if err != nil {
		test.Fatal(err)
	}
	call := request.action.decide("approve", request.action.humanID, "interactive_jwt")
	call.Body = readerCloser{bytes.NewReader(raw)}
	done := make(chan callResult, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ApproveDecisionAction(recorder, call)
		done <- callResult{recorder.Code, recorder.Body.String()}
	}()
	return done
}

func waitResult(test *testing.T, done <-chan callResult, within time.Duration, what string) callResult {
	test.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(within):
		test.Fatalf("%s did not finish within %s", what, within)
		return callResult{}
	}
}

func assertPending(test *testing.T, done <-chan callResult, within time.Duration, what string) {
	test.Helper()
	select {
	case result := <-done:
		test.Fatalf("%s finished while the other side was open: %d %s", what, result.status, result.body)
	case <-time.After(within):
	}
}

// waitForFenceWaiter returns the backend that waits for an advisory lock, and
// fails when none does. It also proves that the waiter holds no row, tuple or
// transaction lock.
func waitForFenceWaiter(test *testing.T) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := testPool.QueryRow(context.Background(), `
SELECT pid FROM pg_locks
WHERE locktype = 'advisory' AND NOT granted
  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
LIMIT 1`).Scan(&pid)
		if err == nil {
			var held int
			dbfx.QueryRow(test, `
SELECT count(*) FROM pg_locks
WHERE pid = $1 AND granted AND locktype IN ('relation', 'tuple', 'transactionid', 'advisory')`, pid).Scan(&held)
			if held != 0 {
				test.Fatalf("the backend that waits at the fence holds %d locks, want none", held)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	test.Fatal("no backend waits at the fence")
}

func serverActionsValue(test *testing.T) string {
	test.Helper()
	return strings.Trim(string(storedSettings(test)["decision_server_actions"]), `"`)
}

// requestSettingOff files the card that turns decision_server_actions off, with the setting on.
func requestSettingOff(test *testing.T) settingsRequest {
	test.Helper()
	setWorkspaceSettings(test, `{"decision_server_actions":"on","compact_prompt":"old"}`)
	patch := map[string]any{"decision_server_actions": "off"}
	request, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(test, "decision_server_actions"), "after_sha256": patchDigest(test, patch),
	})
	makeApproverOwner(test, request.action, "owner")
	return request
}

func typedCards(test *testing.T, fixture singleCallFixture) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID)
}

// Red: a send paused after its read of "on" lets the approval commit "off", and the send then commits a typed card.
// Green: the approval waits for the send (refused at settings_fence, retried), then commits.
func TestTypedSendFenceApprovalRetriesUntilTheSendCommits(test *testing.T) {
	request := requestSettingOff(test)
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "after_setting_read")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)

	approval := startApproval(test, request, withVerifier(acceptingVerifier))
	assertPending(test, approval, 150*time.Millisecond, "the approval")
	pause.Release()

	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusCreated {
		test.Fatalf("send = %d %s, want 201", result.status, result.body)
	}
	result := waitResult(test, approval, 10*time.Second, "the approval")
	if result.status != http.StatusOK || !strings.Contains(result.body, "executed") {
		test.Fatalf("approval = %d %s, want 200 executed", result.status, result.body)
	}
	if got := serverActionsValue(test); got != "off" {
		test.Fatalf("decision_server_actions = %q, want off", got)
	}
	if cards := typedCards(test, fixture); cards != 1 {
		test.Fatalf("cards = %d, want the one the send committed", cards)
	}
}

// A send held past the retry window answers the approval 409 busy_retry, and nothing is used up.
func TestTypedSendFenceApprovalBusyRetryThenSecondApprove(test *testing.T) {
	request := requestSettingOff(test)
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "after_setting_read")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)

	handler := withVerifier(acceptingVerifier)
	status, body := request.approveRaw(test, handler)
	if status != http.StatusConflict || !strings.Contains(body, "busy_retry") {
		test.Fatalf("approval = %d %s, want 409 busy_retry", status, body)
	}
	request.action.assertUntouched(test)
	if got := serverActionsValue(test); got != "on" {
		test.Fatalf("decision_server_actions = %q after the refusal, want on", got)
	}
	pause.Release()
	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusCreated {
		test.Fatalf("send = %d %s, want 201", result.status, result.body)
	}
	if status, body := request.approveRaw(test, handler); status != http.StatusOK || !strings.Contains(body, "executed") {
		test.Fatalf("second approval = %d %s, want 200 executed", status, body)
	}
	if got := serverActionsValue(test); got != "off" {
		test.Fatalf("decision_server_actions = %q, want off", got)
	}
}

// Approval first: a send that checked "on" before the approval, and takes its locks after it, reads "off" and is refused.
func TestTypedSendFenceApprovalFirstRefusesTheSend(test *testing.T) {
	request := requestSettingOff(test)
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "before_fence")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)
	if status, body := request.approveRaw(test, withVerifier(acceptingVerifier)); status != http.StatusOK {
		test.Fatalf("approval = %d %s, want 200", status, body)
	}
	pause.Release()
	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusBadRequest || !strings.Contains(result.body, "server_actions_off") {
		test.Fatalf("send = %d %s, want 400 server_actions_off", result.status, result.body)
	}
	if cards := typedCards(test, fixture); cards != 0 {
		test.Fatalf("cards = %d, want 0", cards)
	}
}

// Direct-endpoint race (a): a send paused between its fence and its workspace row lock.
func TestTypedSendFenceOwnerWriteWaitsAtTheFenceWhileTheSendHoldsIt(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on"}`)
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "after_fence")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)

	write := startOwnerWrite(test, map[string]any{"decision_server_actions": "off"})
	waitForFenceWaiter(test)
	assertPending(test, write, 100*time.Millisecond, "the owner write")
	pause.Release()

	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusCreated {
		test.Fatalf("send = %d %s, want 201", result.status, result.body)
	}
	if result := waitResult(test, write, 10*time.Second, "the owner write"); result.status != http.StatusOK {
		test.Fatalf("write = %d %s, want 200", result.status, result.body)
	}
	if got := serverActionsValue(test); got != "off" {
		test.Fatalf("decision_server_actions = %q, want off", got)
	}
	if cards := typedCards(test, fixture); cards != 1 {
		test.Fatalf("cards = %d, want 1", cards)
	}
}

// Direct-endpoint race (b): the owner write paused between its fence and LockWorkspaceSettings.
func TestTypedSendFenceSendWaitsAtTheFenceWhileTheWriteHoldsIt(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on"}`)
	fixture := newSingleCallFixture(test)
	pause := pauseSettingsWrite(test, "before_settings_lock")
	write := startOwnerWrite(test, map[string]any{"decision_server_actions": "off"})
	pause.waitReached(test)

	send := startTypedSend(test, fixture)
	waitForFenceWaiter(test)
	assertPending(test, send, 100*time.Millisecond, "the send")
	pause.Release()

	if result := waitResult(test, write, 10*time.Second, "the owner write"); result.status != http.StatusOK {
		test.Fatalf("write = %d %s, want 200", result.status, result.body)
	}
	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusBadRequest || !strings.Contains(result.body, "server_actions_off") {
		test.Fatalf("send = %d %s, want 400 server_actions_off", result.status, result.body)
	}
	if got := serverActionsValue(test); got != "off" {
		test.Fatalf("decision_server_actions = %q, want off", got)
	}
	if cards := typedCards(test, fixture); cards != 0 {
		test.Fatalf("cards = %d, want 0", cards)
	}
}

// Direct-endpoint race (c): a send paused after its workspace row lock; the write waits at the fence and does not reach LockWorkspaceSettings.
func TestTypedSendFenceWriteDoesNotReachTheRowLockBeforeTheSendCommits(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on"}`)
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "after_row_lock")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)

	reachedLock := watchSettingsWrite(test, "before_settings_lock")
	write := startOwnerWrite(test, map[string]any{"decision_server_actions": "off"})
	waitForFenceWaiter(test)
	select {
	case <-reachedLock.reached:
		test.Fatal("the write reached LockWorkspaceSettings while the send was open")
	case <-time.After(150 * time.Millisecond):
	}
	pause.Release()

	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusCreated {
		test.Fatalf("send = %d %s, want 201", result.status, result.body)
	}
	if result := waitResult(test, write, 10*time.Second, "the owner write"); result.status != http.StatusOK {
		test.Fatalf("write = %d %s, want 200", result.status, result.body)
	}
	if got := serverActionsValue(test); got != "off" {
		test.Fatalf("decision_server_actions = %q, want off", got)
	}
	if cards := typedCards(test, fixture); cards != 1 {
		test.Fatalf("cards = %d, want 1", cards)
	}
}

// A settings write that changes no fenced key takes no fence: it does not wait for a send that holds the shared side.
func TestTypedSendFenceUnfencedWritesDoNotWait(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on","compact_prompt":"old"}`)
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "after_fence")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)

	write := startOwnerWrite(test, map[string]any{"decision_server_actions": "on", "compact_prompt": "new"})
	if result := waitResult(test, write, 3*time.Second, "the owner write of an unfenced key"); result.status != http.StatusOK {
		test.Fatalf("write = %d %s, want 200", result.status, result.body)
	}
	pause.Release()
	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusCreated {
		test.Fatalf("send = %d %s, want 201", result.status, result.body)
	}
}

// No other settings writer can change a fenced key, so none needs the fence: each reconcile
// of the direct write leaves decision_server_actions and standing_answers as the body had them,
// for a body that carries them and for one that names other keys.
func TestTypedSettingsFenceOtherWritersLeaveFencedKeys(test *testing.T) {
	fenced := func() map[string]any {
		return map[string]any{"decision_server_actions": "on", "standing_answers": map[string]any{"review_cap": "split"}}
	}
	reconciles := map[string]func(stored, incoming map[string]any) error{
		"autolink":        func(stored, incoming map[string]any) error { reconcilePRMergeSettings(stored, incoming); return nil },
		"delegated_write": func(stored, incoming map[string]any) error { reconcileDelegatedWrites(stored, incoming); return nil },
		"jev":             func(stored, incoming map[string]any) error { reconcileAgentDecisionJev(stored, incoming); return nil },
		"routing":         func(stored, incoming map[string]any) error { reconcileRoutingAgent(stored, incoming); return nil },
		"labellers":       func(stored, incoming map[string]any) error { reconcileSystem1Labellers(stored, incoming); return nil },
		"background":      func(stored, incoming map[string]any) error { return reconcileBackgroundAgents(stored, incoming, false) },
		"background_machine": func(stored, incoming map[string]any) error {
			return reconcileBackgroundAgents(stored, incoming, true)
		},
	}
	for name, reconcile := range reconciles {
		stored, incoming := fenced(), fenced()
		stored["github_auto_link_prs_enabled"], incoming["github_auto_link_prs_enabled"] = true, false
		_ = reconcile(stored, incoming)
		for _, key := range typedFencedKeys {
			if !reflect.DeepEqual(incoming[key], fenced()[key]) {
				test.Errorf("%s changed %s: %v", name, key, incoming[key])
			}
		}
		// A body that omits the keys: no reconcile adds, sets or invents one.
		stored, incoming = fenced(), map[string]any{"compact_prompt": "new"}
		_ = reconcile(stored, incoming)
		for _, key := range typedFencedKeys {
			if _, present := incoming[key]; present {
				test.Errorf("%s added %s to a body that omitted it: %v", name, key, incoming[key])
			}
		}
	}
}

// The owner CAS path writes governed fields in other columns (workspace.context): it leaves
// workspace.settings, and so both fenced keys, as they were.
func TestTypedSettingsFenceOwnerCASLeavesFencedKeys(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on","standing_answers":{"review_cap":"split"}}`)
	before := storedSettings(test)
	var current string
	dbfx.QueryRow(test, `SELECT COALESCE(context, '') FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&current)
	ctx := context.Background()
	transaction, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	_, reason, err := ownerCASWriteTx(ctx, transaction, ownerCASWorkspaceContext, testWorkspaceID, testWorkspaceID, "context set by the CAS path", hermesExceptionDigest([]byte(current)))
	if err != nil || reason != hermesExceptionRejectNone {
		test.Fatalf("owner CAS write: reason %v, err %v", reason, err)
	}
	var settings []byte
	if err := transaction.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings); err != nil {
		test.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(settings, &after); err != nil {
		test.Fatal(err)
	}
	for _, key := range typedFencedKeys {
		if !bytes.Equal(after[key], before[key]) {
			test.Errorf("owner CAS changed %s: %s, want %s", key, after[key], before[key])
		}
	}
}

// The owner's passkey approval of a patch that names no fenced key leaves both keys as they were,
// and takes no fence: it commits while a send holds the fence.
func TestTypedSettingsFenceUnfencedPatchLeavesFencedKeys(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on","standing_answers":{"review_cap":"split"},"compact_prompt":"old"}`)
	patch := map[string]any{"compact_prompt": "new"}
	request, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(test, "compact_prompt"), "after_sha256": patchDigest(test, patch),
	})
	makeApproverOwner(test, request.action, "owner")
	fixture := newSingleCallFixture(test)
	pause := pauseTypedSend(test, "after_setting_read")
	send := startTypedSend(test, fixture)
	pause.waitReached(test)

	result := waitResult(test, startApproval(test, request, withVerifier(acceptingVerifier)), 3*time.Second, "the approval of an unfenced patch")
	if result.status != http.StatusOK || !strings.Contains(result.body, "executed") {
		test.Fatalf("approval = %d %s, want 200 executed", result.status, result.body)
	}
	pause.Release()
	waitResult(test, send, 10*time.Second, "the send")
	stored := storedSettings(test)
	if got := string(stored["decision_server_actions"]); got != `"on"` {
		test.Fatalf("decision_server_actions = %s, want \"on\"", got)
	}
	if got := string(stored["standing_answers"]); !strings.Contains(got, "split") {
		test.Fatalf("standing_answers = %s, want review_cap split", got)
	}
}

// startPendingIntentTypedSend posts a typed card from the lane's open intent (the
// pending-intent branch, not the single-call one) from another goroutine.
func startPendingIntentTypedSend(test *testing.T, fixture requestCoreFixture) <-chan callResult {
	test.Helper()
	body := map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": typedAsk(fixture)}
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	done := make(chan callResult, 1)
	go func() {
		recorder := httptest.NewRecorder()
		testHandler.RequestDecision(recorder, request)
		done <- callResult{recorder.Code, recorder.Body.String()}
	}()
	return done
}

// CHE-9716 (A5e F12). A pending-intent send whose preflight read "on" and whose
// owner then committed "off" must refuse inside its own transaction and leave no card.
func TestPendingIntentTypedSendRefusesWhenSettingIsOffAfterPreflight(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on"}`)
	fixture := newRequestCoreFixture(test, false)
	pause := pauseTypedSend(test, "before_fence")
	send := startPendingIntentTypedSend(test, fixture)
	pause.waitReached(test) // the API preflight has passed with the setting on

	setWorkspaceSettings(test, `{"decision_server_actions":"off"}`) // the owner commits off
	pause.Release()

	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusBadRequest || !strings.Contains(result.body, "server_actions_off") {
		test.Fatalf("send = %d %s, want 400 server_actions_off", result.status, result.body)
	}
	if cards := requestCount(test, fixture); cards != 0 {
		test.Fatalf("cards = %d, want 0", cards)
	}
}

// CHE-9716 (A5e F12). A pending-intent typed send holds the shared fence, so an owner
// write that turns the setting off waits for the send to commit.
func TestPendingIntentTypedSendHoldsTheFenceAgainstAnOwnerWrite(test *testing.T) {
	setWorkspaceSettings(test, `{"decision_server_actions":"on"}`)
	fixture := newRequestCoreFixture(test, false)
	pause := pauseTypedSend(test, "after_setting_read")
	send := startPendingIntentTypedSend(test, fixture)
	pause.waitReached(test)

	write := startOwnerWrite(test, map[string]any{"decision_server_actions": "off"})
	waitForFenceWaiter(test)
	assertPending(test, write, 100*time.Millisecond, "the owner write")
	pause.Release()

	if result := waitResult(test, send, 10*time.Second, "the send"); result.status != http.StatusCreated {
		test.Fatalf("send = %d %s, want 201", result.status, result.body)
	}
	if result := waitResult(test, write, 10*time.Second, "the owner write"); result.status != http.StatusOK {
		test.Fatalf("write = %d %s, want 200", result.status, result.body)
	}
	if got := serverActionsValue(test); got != "off" {
		test.Fatalf("decision_server_actions = %q, want off", got)
	}
	if cards := requestCount(test, fixture); cards != 1 {
		test.Fatalf("cards = %d, want 1", cards)
	}
}
