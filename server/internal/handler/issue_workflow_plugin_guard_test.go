package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Plugin guards at step entry (CHE-10256, CHE-9779 step 4).

const pluginGateGuard = "plugin(com.example.guarded, qa-proof)"

// guardStub replaces the guard call. script gets the 1-based call number and the
// invocation, and answers; a nil script allows.
type guardStub struct {
	mu     sync.Mutex
	calls  []service.HookInvocation
	script func(n int, invocation service.HookInvocation) service.GuardResult
}

func (s *guardStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *guardStub) snapshot(n int) guardSnapshotInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[n-1].Input.(guardSnapshotInput)
}

func stubGuard(t *testing.T, script func(n int, invocation service.HookInvocation) service.GuardResult) *guardStub {
	t.Helper()
	stub := &guardStub{script: script}
	previous := testHandler.invokeGuardForTest
	testHandler.invokeGuardForTest = func(_ context.Context, invocation service.HookInvocation) (service.GuardResult, error) {
		stub.mu.Lock()
		stub.calls = append(stub.calls, invocation)
		n := len(stub.calls)
		stub.mu.Unlock()
		if stub.script == nil {
			return service.GuardResult{Decision: service.GuardDecisionAllow}, nil
		}
		return stub.script(n, invocation), nil
	}
	t.Cleanup(func() { testHandler.invokeGuardForTest = previous })
	return stub
}

func allowGuard() service.GuardResult {
	return service.GuardResult{Decision: service.GuardDecisionAllow}
}

func denyGuard(reason string) service.GuardResult {
	return service.GuardResult{Decision: service.GuardDecisionDeny, Reason: reason}
}

type pluginGuardFixture struct {
	edgeCase
	installationID string
}

// newPluginGuardCase installs the guard plugin and puts an issue at in_progress
// on a workflow whose in_review step carries the plugin gate in mode.
func newPluginGuardCase(t *testing.T, installationID int64, manifest, mode string) pluginGuardFixture {
	t.Helper()
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	recorder := installGuardPlugin(t, withLocalPluginSourceIn(t, t.TempDir(), manifest))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var installed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode installation: %v", err)
	}
	c := newEdgeCase(t, installationID, gateSteps(gate(pluginGateGuard, mode)))
	c.enter(t, "in_progress")
	return pluginGuardFixture{edgeCase: c, installationID: installed.ID}
}

func (f pluginGuardFixture) move(t *testing.T, actor ...func(*http.Request) *http.Request) *testutil.Response {
	t.Helper()
	return putIssue(t, f.issueID, map[string]any{"status": "in_review"}, actor...)
}

func (f pluginGuardFixture) declare(t *testing.T, key, value string) {
	t.Helper()
	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, f.issueID, key, map[string]any{"value": value})).Want(http.StatusOK)
}

func failedFact(t *testing.T, res *testutil.Response) string {
	t.Helper()
	raw, _ := res.Map()["failed_guards"].([]any)
	if len(raw) != 1 {
		t.Fatalf("failed_guards = %v, want one; body %s", raw, res.Text())
	}
	return raw[0].(map[string]any)["fact"].(string)
}

func TestPluginGuardAllowCommitsTheMove(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newPluginGuardCase(t, 1025601, guardMetadataTestManifest, "block")
	stub := stubGuard(t, nil)

	f.move(t).Want(http.StatusOK)

	wantIssueStatus(t, f.issueID, "in_review")
	if stub.count() != 1 {
		t.Fatalf("guard calls = %d, want 1", stub.count())
	}
	snap := stub.snapshot(1)
	if snap.From != "in_progress" || snap.To != "in_review" || snap.IssueID != f.issueID || snap.Hook != "qa-proof" || snap.Plugin != "com.example.guarded" {
		t.Fatalf("snapshot = %+v, want the move in_progress -> in_review of the issue", snap)
	}
	if snap.Actor.Type != "member" || snap.Actor.ID != testUserID {
		t.Fatalf("snapshot actor = %+v, want the calling member", snap.Actor)
	}
}

func TestPluginGuardDenyRefusesWithPluginAndReason(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newPluginGuardCase(t, 1025602, guardMetadataTestManifest, "block")
	stubGuard(t, func(int, service.HookInvocation) service.GuardResult { return denyGuard("post_close_work is missing") })

	res := f.move(t)
	res.Want(http.StatusConflict)

	if code, _ := res.Map()["code"].(string); code != "entry_gate_failed" {
		t.Fatalf("code = %q, want entry_gate_failed; body %s", code, res.Text())
	}
	if got := failedGuards(t, res); len(got) != 1 || got[0] != pluginGateGuard {
		t.Fatalf("failed guards = %v, want [%s]", got, pluginGateGuard)
	}
	if fact := failedFact(t, res); fact != "post_close_work is missing" {
		t.Fatalf("fact = %q, want the plugin's reason", fact)
	}
	wantIssueStatus(t, f.issueID, "in_progress")
}

func TestPluginGuardWarnRecordsAWarningOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newPluginGuardCase(t, 1025603, guardMetadataTestManifest, "warn")
	stubGuard(t, func(int, service.HookInvocation) service.GuardResult { return denyGuard("not yet") })

	f.move(t).Want(http.StatusOK)

	wantIssueStatus(t, f.issueID, "in_review")
	if got := activityCount(t, f.issueID, "entry_gate_warning"); got != 1 {
		t.Fatalf("entry_gate_warning rows = %d, want 1", got)
	}
	if got := activityCount(t, f.issueID, "entry_gate_override"); got != 0 {
		t.Fatalf("entry_gate_override rows = %d, want 0", got)
	}
}

func TestPluginGuardOffMakesNoCall(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newPluginGuardCase(t, 1025604, guardMetadataTestManifest, "off")
	stub := stubGuard(t, func(int, service.HookInvocation) service.GuardResult { return denyGuard("never asked") })

	f.move(t).Want(http.StatusOK)

	if stub.count() != 0 {
		t.Fatalf("guard calls = %d, want 0 for a gate that is off", stub.count())
	}
}

// A step that names no plugin gate makes no outbound call, and its write answers
// as it did before the guard existed.
func TestPluginGuardStepWithoutPluginGateMakesNoCall(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	stub := stubGuard(t, func(int, service.HookInvocation) service.GuardResult { return denyGuard("never asked") })
	c := newEdgeCase(t, 1025605, gateSteps(gate("pr.ready", "block")))
	c.enter(t, "in_progress")

	putIssue(t, c.issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	res := putIssue(t, c.issueID, map[string]any{"status": "in_review"})
	res.Want(http.StatusConflict)

	if stub.count() != 0 {
		t.Fatalf("guard calls = %d, want 0", stub.count())
	}
	if got := failedGuards(t, res); len(got) != 1 || got[0] != "pr.ready" {
		t.Fatalf("failed guards = %v, want [pr.ready]: the refusal must read as before", got)
	}
}

// The override flag is the gate's and the hook's together.
func TestPluginGuardMemberOverrideFollowsTheHook(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	reason := map[string]any{"status": "in_review", "entry_gate_override_reason": "the plugin is wrong here"}
	deny := func(int, service.HookInvocation) service.GuardResult { return denyGuard("no proof") }

	t.Run("overridable hook", func(t *testing.T) {
		f := newPluginGuardCase(t, 1025606, guardMetadataTestManifest, "block")
		stubGuard(t, deny)
		putIssue(t, f.issueID, reason).Want(http.StatusOK)
		wantIssueStatus(t, f.issueID, "in_review")
		if got := activityCount(t, f.issueID, "entry_gate_override"); got != 1 {
			t.Fatalf("entry_gate_override rows = %d, want 1", got)
		}
	})
	t.Run("hook that is not overridable", func(t *testing.T) {
		manifest := strings.Replace(guardMetadataTestManifest, `"overridable": true`, `"overridable": false`, 1)
		f := newPluginGuardCase(t, 1025607, manifest, "block")
		stubGuard(t, deny)
		putIssue(t, f.issueID, reason).Want(http.StatusConflict)
		wantIssueStatus(t, f.issueID, "in_progress")
		if got := activityCount(t, f.issueID, "entry_gate_override"); got != 0 {
			t.Fatalf("entry_gate_override rows = %d, want 0", got)
		}
	})
	t.Run("agent never overrides", func(t *testing.T) {
		f := newPluginGuardCase(t, 1025608, guardMetadataTestManifest, "block")
		stubGuard(t, deny)
		f.move(t, func(r *http.Request) *http.Request { return asAgentTask(t, r) }).Want(http.StatusConflict)
	})
}

// Writers that are not a person's explicit write skip the gates, as before.
func TestPluginGuardSystemWriteSkips(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	if rec := installGuardPlugin(t, withLocalPluginSourceIn(t, t.TempDir(), guardMetadataTestManifest)); rec.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", rec.Code, rec.Body.String())
	}
	stub := stubGuard(t, func(int, service.HookInvocation) service.GuardResult { return denyGuard("never asked") })
	seedTestCatalog(t)
	steps := []map[string]any{{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "done", "entry": []map[string]any{gate(pluginGateGuard, "block")}}}
	wf := createTestWorkflowSteps(t, "System plugin "+workflowTestSuffix(), steps)
	projectID := createWorkflowTestProject(t, "System plugin project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "plugin gated merge", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}

	testHandler.advanceIssueToDone(context.Background(), issue, testWorkspaceID)

	wantIssueStatus(t, issueID, "done")
	if stub.count() != 0 {
		t.Fatalf("guard calls = %d, want 0 for a system write", stub.count())
	}
}

// A declares post_close_work and asks to move; the guard allows at revision R.
// B declares the same value again before A's transaction takes its locks. A's
// old allow must not commit: the write restarts, the guard is asked again, sees
// that the newest writer changed, and denies.
func TestPluginGuardInterleavedRedeclarationVoidsTheAllow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name string
		// between runs after A's guard call answered, as B.
		between func(t *testing.T, f pluginGuardFixture)
	}{
		{"revision bump", func(t *testing.T, f pluginGuardFixture) { f.declare(t, "post_close_work", "X") }},
		// A defect that stops the revision from moving still changes the writer row.
		{"revision bump stubbed out", func(t *testing.T, f pluginGuardFixture) {
			var revision int64
			if err := testPool.QueryRow(context.Background(), `SELECT revision FROM issue WHERE id=$1`, f.issueID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			f.declare(t, "post_close_work", "X")
			dbfx.Exec(t, `UPDATE issue SET revision=$2 WHERE id=$1`, f.issueID, revision)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPluginGuardCase(t, 1025610+int64(len(tc.name)), guardMetadataTestManifest, "block")
			f.declare(t, "post_close_work", "X")
			var first string
			stub := stubGuard(t, func(n int, inv service.HookInvocation) service.GuardResult {
				w := inv.Input.(guardSnapshotInput).MetadataWrites["post_close_work"].WriteID
				if n == 1 {
					first = w
					tc.between(t, f)
					return allowGuard()
				}
				if w == first {
					return allowGuard()
				}
				return denyGuard("post_close_work was set by another actor")
			})

			res := f.move(t)
			res.Want(http.StatusConflict)

			if stub.count() != 2 {
				t.Fatalf("guard calls = %d, want 2: the old allow must not commit", stub.count())
			}
			if first == "" || stub.snapshot(2).MetadataWrites["post_close_work"].WriteID == first {
				t.Fatalf("second snapshot must name the newer writer row, first=%q second=%+v", first, stub.snapshot(2).MetadataWrites)
			}
			if fact := failedFact(t, res); fact != "post_close_work was set by another actor" {
				t.Fatalf("fact = %q, want the re-evaluated denial", fact)
			}
			wantIssueStatus(t, f.issueID, "in_progress")
		})
	}
}

// Anything the verdict depends on that changes between the call and the lock
// restarts the write once, and the second call decides.
func TestPluginGuardChangedStateBetweenCallAndLockRestarts(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, tc := range []struct {
		name   string
		change func(t *testing.T, f pluginGuardFixture)
	}{
		{"installation disabled", func(t *testing.T, f pluginGuardFixture) {
			dbfx.Exec(t, `UPDATE plugin_installation SET enabled=FALSE, updated_at=now() WHERE id=$1`, f.installationID)
		}},
		{"installation upgraded", func(t *testing.T, f pluginGuardFixture) {
			dbfx.Exec(t, `UPDATE plugin_installation SET version='9.9.9', updated_at=now() WHERE id=$1`, f.installationID)
		}},
		{"guard policy edited", func(t *testing.T, f pluginGuardFixture) {
			// No updated_at bump: the policy fields themselves are fingerprinted.
			dbfx.Exec(t, `UPDATE plugin_installation SET manifest=jsonb_set(manifest, '{contributes,hooks,0,guard,overridable}', 'false') WHERE id=$1`, f.installationID)
		}},
		{"workflow entry gates edited", func(t *testing.T, f pluginGuardFixture) {
			dbfx.Exec(t, `UPDATE issue_workflow SET steps=jsonb_set(steps, '{3,entry,0,mode}', '"warn"'), updated_at=now() WHERE id=$1`, f.workflowID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPluginGuardCase(t, 1025620+int64(i), guardMetadataTestManifest, "block")
			stub := stubGuard(t, func(n int, _ service.HookInvocation) service.GuardResult {
				if n == 1 {
					tc.change(t, f)
					return allowGuard()
				}
				return denyGuard("second look")
			})

			res := f.move(t)

			if stub.count() != 2 {
				t.Fatalf("guard calls = %d, want 2", stub.count())
			}
			if tc.name == "workflow entry gates edited" {
				// The gate is now a warning: the denial of the second call only warns.
				res.Want(http.StatusOK)
				wantIssueStatus(t, f.issueID, "in_review")
				return
			}
			res.Want(http.StatusConflict)
			if fact := failedFact(t, res); fact != "second look" {
				t.Fatalf("fact = %q, want the second call's verdict", fact)
			}
			wantIssueStatus(t, f.issueID, "in_progress")
		})
	}
}

// Two mismatches in a row end in a 409 and two outbound calls, no more.
func TestPluginGuardTwoMismatchesAreEntryGateStale(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newPluginGuardCase(t, 1025630, guardMetadataTestManifest, "block")
	stub := stubGuard(t, func(int, service.HookInvocation) service.GuardResult {
		f.declare(t, "qa_note", "again")
		return allowGuard()
	})

	res := f.move(t)
	res.Want(http.StatusConflict)

	if code, _ := res.Map()["code"].(string); code != "entry_gate_stale" {
		t.Fatalf("code = %q, want entry_gate_stale; body %s", code, res.Text())
	}
	if stub.count() != 2 {
		t.Fatalf("guard calls = %d, want 2", stub.count())
	}
	wantIssueStatus(t, f.issueID, "in_progress")
}
