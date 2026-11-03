package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2908 (CHE-2894 step 5): settings.compact_agent_id is a human-admin field.
// Plan v3 obligations CEO-9(a)/SEC-3, SEC-1 rule 4 and ENG-8.

// logSink captures slog records so a test can assert which events were emitted.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records returns the captured records whose message is msg.
func (s *logSink) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(s.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func captureLogs(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

var compactHostSeq atomic.Int64

type compactAgentFixture struct {
	t *testing.T
}

func newCompactAgentFixture(t *testing.T) *compactAgentFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	var settings []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'compact_agent_id' WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, settings, testWorkspaceID)
	})
	return &compactAgentFixture{t: t}
}

// eligibleAgent is workspace-visible, not archived, on a public workspace runtime.
func (f *compactAgentFixture) eligibleAgent(name string) string {
	f.t.Helper()
	runtimeID := dbfx.Runtime(f.t, name+"-runtime", testutil.Cols{"visibility": "public"})
	return dbfx.Agent(f.t, name, runtimeID, testutil.Cols{
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
}

func (f *compactAgentFixture) stored() string {
	f.t.Helper()
	var value *string
	dbfx.QueryRow(f.t, `SELECT settings ->> 'compact_agent_id' FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&value)
	if value == nil {
		return ""
	}
	return *value
}

func (f *compactAgentFixture) seed(agentID string) {
	f.t.Helper()
	dbfx.Exec(f.t, `UPDATE workspace SET settings = jsonb_set(settings, '{compact_agent_id}', to_jsonb($1::text)) WHERE id = $2`, agentID, testWorkspaceID)
}

type machineKind int

const (
	humanActor machineKind = iota
	taskTokenActor
	cloudNodeActor
)

func (f *compactAgentFixture) send(method string, body map[string]any, actor machineKind) *httptest.ResponseRecorder {
	f.t.Helper()
	req := withURLParam(newRequest(method, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID)
	switch actor {
	case taskTokenActor:
		hostAgentID := createHandlerTestAgent(f.t, fmt.Sprintf("compact-agent-host-%d", compactHostSeq.Add(1)), nil)
		req = asAgentActor(req, hostAgentID, createHandlerTestTaskForAgent(f.t, hostAgentID))
	case cloudNodeActor:
		req = asCloudNodeActor(req)
	}
	rec := httptest.NewRecorder()
	testHandler.UpdateWorkspace(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	code, _ := body["code"].(string)
	return code
}

// compactionTask queues a task for agentID on a fresh DM with the given status
// and purpose ("" is an ordinary task).
func (f *compactAgentFixture) compactionTask(agentID, status, purpose string) string {
	f.t.Helper()
	session := dbfx.ChatSession(f.t, agentID)
	cols := testutil.Cols{
		"runtime_id":      handlerTestRuntimeID(f.t),
		"status":          status,
		"chat_session_id": session,
	}
	if purpose != "" {
		cols["purpose"] = purpose
	}
	if status == "running" {
		cols["started_at"] = testutil.Raw("now()")
	}
	return dbfx.Task(f.t, agentID, cols)
}

func (f *compactAgentFixture) taskStatus(taskID string) string {
	f.t.Helper()
	var status string
	dbfx.QueryRow(f.t, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	return status
}

func TestCompactAgentIDHumanAdminSetAndReset(t *testing.T) {
	f := newCompactAgentFixture(t)
	logs := captureLogs(t)
	agentA := f.eligibleAgent("compact-agent-a")
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || '{"keep":"me"}'::jsonb WHERE id = $1`, testWorkspaceID)

	rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": strings.ToUpper(agentA)}, humanActor)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.stored(); got != agentA {
		t.Fatalf("stored compact_agent_id = %q, want canonical %q", got, agentA)
	}
	var keep string
	dbfx.QueryRow(t, `SELECT settings ->> 'keep' FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&keep)
	if keep != "me" {
		t.Fatalf("the dedicated field dropped another settings key: keep = %q", keep)
	}
	events := logs.records(t, compactAgentChangedEvent)
	if len(events) != 1 {
		t.Fatalf("want 1 %s event after the set, got %d", compactAgentChangedEvent, len(events))
	}
	if events[0]["actor_id"] != testUserID || events[0]["old_value"] != "" || events[0]["new_value"] != agentA {
		t.Fatalf("set event = %v, want actor %s old \"\" new %s", events[0], testUserID, agentA)
	}

	rec = f.send(http.MethodPatch, map[string]any{"compact_agent_id": nil}, humanActor)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.stored(); got != "" {
		t.Fatalf("reset left compact_agent_id = %q", got)
	}
	events = logs.records(t, compactAgentChangedEvent)
	if len(events) != 2 || events[1]["old_value"] != agentA || events[1]["new_value"] != "" {
		t.Fatalf("reset event = %v", events)
	}
}

func TestCompactAgentIDHumanWritesOnNestedSettingsPaths(t *testing.T) {
	f := newCompactAgentFixture(t)
	agentA := f.eligibleAgent("compact-agent-nested-a")
	agentB := f.eligibleAgent("compact-agent-nested-b")

	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		if rec := f.send(method, map[string]any{"settings": map[string]any{"compact_agent_id": agentA}}, humanActor); rec.Code != http.StatusOK {
			t.Fatalf("%s nested set: status %d: %s", method, rec.Code, rec.Body.String())
		}
		if got := f.stored(); got != agentA {
			t.Fatalf("%s nested set stored %q, want %q", method, got, agentA)
		}
		// A settings replacement that omits the key clears it.
		if rec := f.send(method, map[string]any{"settings": map[string]any{"other": 1}}, humanActor); rec.Code != http.StatusOK {
			t.Fatalf("%s key-omitting replacement: status %d: %s", method, rec.Code, rec.Body.String())
		}
		if got := f.stored(); got != "" {
			t.Fatalf("%s key-omitting replacement left %q", method, got)
		}
	}
	// A→B in one nested write.
	f.seed(agentA)
	if rec := f.send(http.MethodPut, map[string]any{"settings": map[string]any{"compact_agent_id": agentB}}, humanActor); rec.Code != http.StatusOK {
		t.Fatalf("nested change: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.stored(); got != agentB {
		t.Fatalf("nested change stored %q, want %q", got, agentB)
	}
}

func TestCompactAgentIDEligibility(t *testing.T) {
	f := newCompactAgentFixture(t)
	logs := captureLogs(t)

	archived := f.eligibleAgent("compact-agent-archived")
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, archived)
	private := f.eligibleAgent("compact-agent-private")
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', visibility = 'private' WHERE id = $1`, private)
	privateRuntime := f.eligibleAgent("compact-agent-private-runtime")
	dbfx.Exec(t, `UPDATE agent_runtime SET visibility = 'private' WHERE id = (SELECT runtime_id FROM agent WHERE id = $1)`, privateRuntime)
	noRuntime := f.eligibleAgent("compact-agent-no-runtime")
	dbfx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, noRuntime)

	otherWorkspace := dbfx.Workspace(t, "Compact Other", "compact-other-"+strings.ToLower(testWorkspaceID[:8]))
	otherRuntime := dbfx.Runtime(t, "compact-other-runtime", testutil.Cols{"workspace_id": otherWorkspace, "visibility": "public"})
	foreign := dbfx.Agent(t, "compact-agent-foreign", otherRuntime, testutil.Cols{
		"workspace_id": otherWorkspace, "visibility": "workspace", "permission_mode": "public_to",
	})

	cases := []struct {
		name     string
		value    any
		wantCode string
		leak     string
	}{
		{"archived", archived, "compact_agent_ineligible", archived},
		{"private agent", private, "compact_agent_ineligible", private},
		{"private runtime", privateRuntime, "compact_agent_ineligible", privateRuntime},
		{"no runtime", noRuntime, "compact_agent_ineligible", noRuntime},
		{"other workspace", foreign, "compact_agent_ineligible", foreign},
		{"unknown id", "11111111-1111-4111-8111-111111111111", "compact_agent_ineligible", ""},
		{"not a uuid", "not-an-id", "invalid_compact_agent", ""},
		{"empty string", "", "invalid_compact_agent", ""},
		{"number", 7, "invalid_compact_agent", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": tc.value}, humanActor)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := errorCode(t, rec); got != tc.wantCode {
				t.Fatalf("code %q, want %q", got, tc.wantCode)
			}
			// SEC-3 rule 5: the copy never names a private or ineligible agent.
			if tc.leak != "" && strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(tc.leak)) {
				t.Fatalf("error copy names the agent id: %s", rec.Body.String())
			}
			if got := f.stored(); got != "" {
				t.Fatalf("a refused write stored %q", got)
			}
		})
	}
	if events := logs.records(t, compactAgentChangedEvent); len(events) != 0 {
		t.Fatalf("refused writes emitted %d change events", len(events))
	}

	// An unchanged value skips eligibility: archiving the saved agent must not
	// block other settings saves that carry it back.
	good := f.eligibleAgent("compact-agent-later-archived")
	f.seed(good)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, good)
	if rec := f.send(http.MethodPut, map[string]any{"settings": map[string]any{"compact_agent_id": good, "x": 1}}, humanActor); rec.Code != http.StatusOK {
		t.Fatalf("unchanged value after archive: status %d: %s", rec.Code, rec.Body.String())
	}
	// A reset stays possible for an ineligible saved agent.
	if rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": nil}, humanActor); rec.Code != http.StatusOK {
		t.Fatalf("reset of an archived agent: status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCompactAgentIDMachineActorRefusedOnEveryPath(t *testing.T) {
	f := newCompactAgentFixture(t)
	logs := captureLogs(t)
	agentA := f.eligibleAgent("compact-agent-machine-a")
	agentB := f.eligibleAgent("compact-agent-machine-b")

	type write struct {
		name   string
		method string
		body   map[string]any
		stored string // value stored before the write
	}
	writes := []write{
		{"dedicated set", http.MethodPatch, map[string]any{"compact_agent_id": agentA}, ""},
		{"dedicated change", http.MethodPatch, map[string]any{"compact_agent_id": agentB}, agentA},
		{"dedicated reset", http.MethodPatch, map[string]any{"compact_agent_id": nil}, agentA},
		{"PUT nested set", http.MethodPut, map[string]any{"settings": map[string]any{"compact_agent_id": agentA}}, ""},
		{"PATCH nested set", http.MethodPatch, map[string]any{"settings": map[string]any{"compact_agent_id": agentA}}, ""},
		{"PUT nested change", http.MethodPut, map[string]any{"settings": map[string]any{"compact_agent_id": agentB}}, agentA},
		{"PUT key-omitting replacement", http.MethodPut, map[string]any{"settings": map[string]any{"other": 1}}, agentA},
		{"PATCH key-omitting replacement", http.MethodPatch, map[string]any{"settings": map[string]any{"other": 1}}, agentA},
		{"PUT nested null", http.MethodPut, map[string]any{"settings": map[string]any{"compact_agent_id": nil}}, agentA},
		{"PUT non-object settings", http.MethodPut, map[string]any{"settings": "oops"}, agentA},
		{"invalid value", http.MethodPatch, map[string]any{"compact_agent_id": "not-an-id"}, agentA},
	}
	for _, actor := range []struct {
		name string
		kind machineKind
	}{{"task token", taskTokenActor}, {"cloud node", cloudNodeActor}} {
		for _, w := range writes {
			t.Run(actor.name+"/"+w.name, func(t *testing.T) {
				dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'compact_agent_id' WHERE id = $1`, testWorkspaceID)
				if w.stored != "" {
					f.seed(w.stored)
				}
				queued := f.compactionTask(agentA, "queued", "compaction")
				rec := f.send(w.method, w.body, actor.kind)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
				}
				if got := errorCode(t, rec); got != "compact_agent_forbidden" {
					t.Fatalf("code %q, want compact_agent_forbidden", got)
				}
				if got := f.stored(); got != w.stored {
					t.Fatalf("a refused write changed the setting: %q, want %q", got, w.stored)
				}
				if got := f.taskStatus(queued); got != "queued" {
					t.Fatalf("a refused write cancelled a compaction task: %s", got)
				}
			})
		}
	}
	if events := logs.records(t, compactAgentChangedEvent); len(events) != 0 {
		t.Fatalf("refused writes emitted %d change events", len(events))
	}
}

func TestCompactAgentIDMachineWriteWithUnchangedValueAccepted(t *testing.T) {
	f := newCompactAgentFixture(t)
	logs := captureLogs(t)
	agentA := f.eligibleAgent("compact-agent-unchanged")

	// Nothing stored: a machine settings write without the key changes nothing.
	if rec := f.send(http.MethodPut, map[string]any{"settings": map[string]any{"other": 1}}, taskTokenActor); rec.Code != http.StatusOK {
		t.Fatalf("machine write with no compact_agent_id: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": nil}, taskTokenActor); rec.Code != http.StatusOK {
		t.Fatalf("machine null while unset: status %d: %s", rec.Code, rec.Body.String())
	}

	f.seed(agentA)
	queued := f.compactionTask(agentA, "queued", "compaction")
	for _, w := range []struct {
		name   string
		method string
		body   map[string]any
	}{
		{"dedicated same value", http.MethodPatch, map[string]any{"compact_agent_id": agentA}},
		{"dedicated same value, other case", http.MethodPatch, map[string]any{"compact_agent_id": strings.ToUpper(agentA)}},
		{"full settings carrying the value", http.MethodPut, map[string]any{"settings": map[string]any{"compact_agent_id": agentA, "x": 2}}},
		{"unrelated field", http.MethodPatch, map[string]any{"name": "Compact Unchanged Name"}},
		{"pause flip", http.MethodPatch, map[string]any{"wakeups_paused": false}},
		{"compact_prompt only", http.MethodPatch, map[string]any{"compact_prompt": "Keep SHAs."}},
	} {
		for _, kind := range []machineKind{taskTokenActor, cloudNodeActor} {
			if rec := f.send(w.method, w.body, kind); rec.Code != http.StatusOK {
				t.Fatalf("%s (actor %d): status %d: %s", w.name, kind, rec.Code, rec.Body.String())
			}
			if got := f.stored(); got != agentA {
				t.Fatalf("%s (actor %d): stored %q, want %q", w.name, kind, got, agentA)
			}
		}
	}
	if got := f.taskStatus(queued); got != "queued" {
		t.Fatalf("an unchanged-value write cancelled a compaction task: %s", got)
	}
	if events := logs.records(t, compactAgentChangedEvent); len(events) != 0 {
		t.Fatalf("unchanged writes emitted %d change events", len(events))
	}
}

func TestCompactAgentIDMemberRefused(t *testing.T) {
	f := newCompactAgentFixture(t)
	logs := captureLogs(t)
	agentA := f.eligibleAgent("compact-agent-member")
	member := createPlainMember(t, "compact-agent-member@multica.test")

	req := withURLParam(newRequestAs(member, http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"compact_agent_id": agentA}), "id", testWorkspaceID)
	rec := httptest.NewRecorder()
	testHandler.UpdateWorkspace(rec, req)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "compact_agent_forbidden" {
		t.Fatalf("member set: status %d code %q: %s", rec.Code, errorCode(t, rec), rec.Body.String())
	}
	if got := f.stored(); got != "" {
		t.Fatalf("a member write stored %q", got)
	}
	if events := logs.records(t, compactAgentChangedEvent); len(events) != 0 {
		t.Fatalf("a refused member write emitted %d events", len(events))
	}
}

func TestCompactAgentIDChangeCancelsCompactionTasks(t *testing.T) {
	f := newCompactAgentFixture(t)
	agentA := f.eligibleAgent("compact-agent-cancel-a")
	agentB := f.eligibleAgent("compact-agent-cancel-b")
	f.seed(agentA)

	queued := f.compactionTask(agentA, "queued", "compaction")
	deferred := f.compactionTask(agentA, "deferred", "compaction")
	dispatched := f.compactionTask(agentA, "dispatched", "compaction")
	running := f.compactionTask(agentA, "running", "compaction")
	waiting := f.compactionTask(agentA, "waiting_local_directory", "compaction")
	otherAgent := f.compactionTask(agentB, "queued", "compaction")
	ordinary := f.compactionTask(agentA, "queued", "")
	finished := f.compactionTask(agentA, "completed", "compaction")

	if rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": agentB}, humanActor); rec.Code != http.StatusOK {
		t.Fatalf("change: status %d: %s", rec.Code, rec.Body.String())
	}
	for name, id := range map[string]string{"queued": queued, "deferred": deferred, "dispatched": dispatched, "running": running, "waiting_local_directory": waiting, "queued for the new agent": otherAgent} {
		if got := f.taskStatus(id); got != "cancelled" {
			t.Errorf("%s compaction task is %q after the change, want cancelled", name, got)
		}
	}
	if got := f.taskStatus(ordinary); got != "queued" {
		t.Errorf("an ordinary queued task is %q, want queued", got)
	}
	if got := f.taskStatus(finished); got != "completed" {
		t.Errorf("a finished compaction task is %q, want completed", got)
	}
	var cancelledBy string
	dbfx.QueryRow(t, `SELECT cancelled_by_type FROM agent_task_queue WHERE id = $1`, queued).Scan(&cancelledBy)
	if cancelledBy != "system" {
		t.Errorf("cancelled_by_type = %q, want system", cancelledBy)
	}
}

// SEC-1 rule 4: A→B→A must not revive a task queued under A, and a task
// queued under A after the change back is a new task the user chose to queue.
func TestCompactAgentIDRestoreAndClearCancelAndNeverDeliver(t *testing.T) {
	f := newCompactAgentFixture(t)
	agentA := f.eligibleAgent("compact-agent-restore-a")
	agentB := f.eligibleAgent("compact-agent-restore-b")
	f.seed(agentA)
	queuedUnderA := f.compactionTask(agentA, "queued", "compaction")

	for _, next := range []any{agentB, agentA} {
		if rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": next}, humanActor); rec.Code != http.StatusOK {
			t.Fatalf("change to %v: status %d: %s", next, rec.Code, rec.Body.String())
		}
	}
	if got := f.stored(); got != agentA {
		t.Fatalf("after A→B→A stored %q, want %q", got, agentA)
	}
	if got := f.taskStatus(queuedUnderA); got != "cancelled" {
		t.Fatalf("task queued under A is %q after A→B→A, want cancelled", got)
	}
	// The cancelled task is never claimable: no claim for its runtime delivers it.
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(context.Background(), parseUUID(handlerTestRuntimeID(t)))
	if err == nil && claimed != nil && uuidToString(claimed.ID) == queuedUnderA {
		t.Fatalf("a cancelled compaction task was claimed")
	}

	queuedAgain := f.compactionTask(agentA, "queued", "compaction")
	if rec := f.send(http.MethodPatch, map[string]any{"compact_agent_id": nil}, humanActor); rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.taskStatus(queuedAgain); got != "cancelled" {
		t.Fatalf("task queued before a clear is %q, want cancelled", got)
	}
}

func TestCompactAgentIDWriteWaitsForWorkspaceRowLock(t *testing.T) {
	f := newCompactAgentFixture(t)
	agentA := f.eligibleAgent("compact-agent-lock")

	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM workspace WHERE id = $1 FOR UPDATE`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.send(http.MethodPatch, map[string]any{"compact_agent_id": agentA}, humanActor) }()
	select {
	case rec := <-done:
		t.Fatalf("the write finished (%d) while another transaction held the workspace row lock", rec.Code)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write never finished after the lock was released")
	}
}
