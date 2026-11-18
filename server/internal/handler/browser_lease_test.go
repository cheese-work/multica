package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type allowHooks struct{}

func (allowHooks) Check(context.Context, browserlease.Subject) error { return nil }
func (allowHooks) Validate(context.Context, browserlease.Subject) (*time.Time, error) {
	return nil, nil
}

type stubSigner struct{}

func (stubSigner) Sign(c browserlease.Claims) (string, error) { return "tok." + c.JTI, nil }

type browserLeaseFixture struct {
	h       *Handler
	taskID  string
	agentID string
	profile string
	daemon  string
}

func newBrowserLeaseFixture(t *testing.T, taskStatus string) browserLeaseFixture {
	t.Helper()
	daemon := "bl-daemon-" + uuid.NewString()
	runtimeID := dbfx.Runtime(t, "browser-lease runtime", testutil.Cols{"daemon_id": daemon})
	agentID := dbfx.Agent(t, "browser-lease agent "+uuid.NewString(), runtimeID)
	issueID := dbfx.Issue(t, "browser lease issue")
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": taskStatus, "accountable_user_id": testUserID,
	})
	q := db.New(testPool)
	ws := parseUUID(testWorkspaceID)
	prof, err := q.CreateBrowserProfile(context.Background(), db.CreateBrowserProfileParams{WorkspaceID: ws, Name: "p", Class: "S0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateBrowserInstance(context.Background(), db.CreateBrowserInstanceParams{WorkspaceID: ws, Name: "x99-1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"browser_lease", "browser_instance", "browser_profile"} {
			testPool.Exec(context.Background(), "DELETE FROM "+tbl+" WHERE workspace_id = $1", testWorkspaceID)
		}
	})
	h := testHandler
	h.BrowserLeases = &browserlease.Service{
		Pool: testPool, Queries: q, Gate: allowHooks{}, Decisions: allowHooks{},
		Registry: browserlease.NameRegistry{Queries: q}, Signer: stubSigner{},
	}
	t.Cleanup(func() { h.BrowserLeases = nil })
	return browserLeaseFixture{h: h, taskID: taskID, agentID: agentID, profile: uuid.UUID(prof.ID.Bytes).String(), daemon: daemon}
}

func (f browserLeaseFixture) call(t *testing.T, fn http.HandlerFunc, path string, body any, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := newDaemonTokenRequest("POST", path, body, testWorkspaceID, f.daemon)
	req = withURLParams(req, append([]string{"id", f.taskID}, kv...)...)
	w := httptest.NewRecorder()
	fn(w, req)
	return w
}

func TestBrowserLeaseMintRenewRelease(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	w := f.call(t, f.h.MintBrowserLease, "/api/daemon/tasks/"+f.taskID+"/browser-leases", map[string]string{"profile_id": f.profile})
	if w.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		LeaseID string `json:"lease_id"`
		State   string `json:"state"`
		Token   string `json:"token"`
		Aud     string `json:"aud"`
		Epoch   int64  `json:"epoch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "active" || out.Token == "" || out.Aud != "steel-gateway/x99" || out.Epoch != 1 {
		t.Fatalf("mint body = %+v", out)
	}

	w = f.call(t, f.h.RenewBrowserLease, "/x", nil, "leaseId", out.LeaseID)
	if w.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", w.Code, w.Body.String())
	}
	w = f.call(t, f.h.ReleaseBrowserLease, "/x", nil, "leaseId", out.LeaseID)
	if w.Code != http.StatusOK {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	w = f.call(t, f.h.RenewBrowserLease, "/x", nil, "leaseId", out.LeaseID)
	if w.Code != http.StatusConflict {
		t.Fatalf("renew after release: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLeaseMintNeedsARunningTask(t *testing.T) {
	f := newBrowserLeaseFixture(t, "queued")
	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": f.profile})
	if w.Code != http.StatusConflict {
		t.Fatalf("queued task: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLeaseMintRefusesAUserCredential(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	req := newOwnerDaemonRequest("POST", "/x", map[string]string{"profile_id": f.profile})
	req = withURLParam(req, "id", f.taskID)
	w := httptest.NewRecorder()
	f.h.MintBrowserLease(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("user credential: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLeaseMintFailsClosedUntilConfigured(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	f.h.BrowserLeases = nil
	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": f.profile})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLeaseMintDeniedByTheGateIs403(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	f.h.BrowserLeases.Gate = browserlease.DenyAll{}
	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": f.profile})
	if w.Code != http.StatusForbidden {
		t.Fatalf("gate denied: %d %s", w.Code, w.Body.String())
	}
}

// seedActiveBrowserLease inserts an active lease for taskID and returns its id. The grant path
// is W3-3, so the row is written directly.
func seedActiveBrowserLease(t *testing.T, taskID string) string {
	t.Helper()
	id := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO browser_lease (id, workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, granted_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'S0', 'active', 1, now(), now() + interval '15 minutes')`,
		id, testWorkspaceID, uuid.NewString(), uuid.NewString(), uuid.NewString(), taskID, uuid.NewString(), testUserID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM browser_lease WHERE id = $1`, id) })
	return id
}

func browserLeaseState(t *testing.T, id string) (state, reason string) {
	t.Helper()
	var r *string
	if err := testPool.QueryRow(context.Background(), `SELECT state, end_reason FROM browser_lease WHERE id = $1`, id).Scan(&state, &r); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

func TestReleaseBrowserLeaseEndsOnlyTheCallersTaskLease(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4734 release runtime")
	agentID := dbfx.Agent(t, "CHE-4734 release agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4734 release issue")
	mine := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	theirs := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	myLease := seedActiveBrowserLease(t, mine)
	theirLease := seedActiveBrowserLease(t, theirs)

	call := func(taskID, leaseID string) map[string]int {
		req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/browser-leases/"+leaseID+"/release", nil)
		req = withURLParams(req, "id", taskID, "leaseId", leaseID)
		var out map[string]int
		testutil.Call(t, testHandler.ReleaseBrowserLease, req).Want(http.StatusOK).JSON(&out)
		return out
	}

	if got := call(mine, theirLease)["revoked"]; got != 0 {
		t.Fatalf("releasing another task's lease revoked %d", got)
	}
	if state, _ := browserLeaseState(t, theirLease); state != "active" {
		t.Fatalf("another task's lease state = %q, want active", state)
	}
	if got := call(mine, myLease)["revoked"]; got != 1 {
		t.Fatalf("release revoked %d, want 1", got)
	}
	if state, reason := browserLeaseState(t, myLease); state != "ended" || reason != "release" {
		t.Fatalf("lease state=%q reason=%q, want ended release", state, reason)
	}
	if got := call(mine, myLease)["revoked"]; got != 0 {
		t.Fatalf("second release revoked %d, want an idempotent 0", got)
	}
}

func TestRevokeAllBrowserLeasesEndsTheWorkspace(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4734 revoke-all runtime")
	agentID := dbfx.Agent(t, "CHE-4734 revoke-all agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4734 revoke-all issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	lease := seedActiveBrowserLease(t, task)

	req := withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/browser-leases/revoke-all", nil), "id", testWorkspaceID)
	var out map[string]int
	testutil.Call(t, testHandler.RevokeAllBrowserLeases, req).Want(http.StatusOK).JSON(&out)
	if out["revoked"] < 1 {
		t.Fatalf("revoked = %d, want at least 1", out["revoked"])
	}
	if state, reason := browserLeaseState(t, lease); state != "ended" || reason != "revoke_all" {
		t.Fatalf("lease state=%q reason=%q, want ended revoke_all", state, reason)
	}
}

func TestDisablingTheSteelPluginRevokesLeases(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4734 plugin runtime")
	agentID := dbfx.Agent(t, "CHE-4734 plugin agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4734 plugin issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	lease := seedActiveBrowserLease(t, task)
	wsUUID := parseUUID(testWorkspaceID)
	req := newRequest(http.MethodPost, "/x", nil)

	testHandler.revokeLeasesOfDisabledPlugin(req, db.PluginInstallation{PluginKey: "some.other.plugin", WorkspaceID: wsUUID})
	if state, _ := browserLeaseState(t, lease); state != "active" {
		t.Fatalf("an unrelated plugin revoked the lease: state = %q", state)
	}
	testHandler.revokeLeasesOfDisabledPlugin(req, db.PluginInstallation{PluginKey: SteelBrowserPluginKey, WorkspaceID: wsUUID})
	if state, reason := browserLeaseState(t, lease); state != "ended" || reason != "plugin_disabled" {
		t.Fatalf("lease state=%q reason=%q, want ended plugin_disabled", state, reason)
	}
}

func TestBrowserLeaseStatusNeverReturnsATokenAndSeesTheEnd(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": f.profile})
	var minted struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil || minted.LeaseID == "" {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}

	w = f.call(t, f.h.StatusBrowserLease, "/x", nil, "leaseId", minted.LeaseID)
	var st struct {
		State string `json:"state"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != http.StatusOK || st.State != "active" || st.Token != "" {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}

	if w = f.call(t, f.h.StatusBrowserLease, "/x", nil, "leaseId", uuid.NewString()); w.Code != http.StatusNotFound {
		t.Fatalf("status of an unknown lease: %d %s, want 404", w.Code, w.Body.String())
	}

	f.call(t, f.h.ReleaseBrowserLease, "/x", nil, "leaseId", minted.LeaseID)
	if w = f.call(t, f.h.StatusBrowserLease, "/x", nil, "leaseId", minted.LeaseID); w.Code != http.StatusConflict {
		t.Fatalf("status after release: %d %s, want 409", w.Code, w.Body.String())
	}
}

// The gateway URL comes from the operator's MULTICA_BROWSER_GATEWAY_URLS, keyed by host. The
// token audience is only a label ("steel-gateway/c00") and is never turned into a URL.
func TestBrowserLeaseResponseCarriesTheConfiguredGatewayURL(t *testing.T) {
	t.Setenv("MULTICA_BROWSER_GATEWAY_URLS", `{"c00":"https://c00.example.ts.net:35443","x99":"http://127.0.0.1:4100"}`)
	for host, want := range map[string]string{"c00": "https://c00.example.ts.net:35443", "x99": "http://127.0.0.1:4100"} {
		out := browserLeaseResponseOf(browserlease.Result{Host: host, Token: "tok"})
		if out.GatewayURL != want || out.Aud != "steel-gateway/"+host {
			t.Errorf("host %s: gateway_url=%q aud=%q, want %q", host, out.GatewayURL, out.Aud, want)
		}
	}
	if out := browserLeaseResponseOf(browserlease.Result{Host: "x99"}); out.GatewayURL != "" {
		t.Errorf("no token: gateway_url=%q, want none", out.GatewayURL)
	}
	t.Setenv("MULTICA_BROWSER_GATEWAY_URLS", "")
	if out := browserLeaseResponseOf(browserlease.Result{Host: "c00", Token: "tok"}); out.GatewayURL != "" {
		t.Errorf("unconfigured: gateway_url=%q, want none", out.GatewayURL)
	}
}

func seedDecisionBoundBrowserLease(t *testing.T, taskID, decisionID string) string {
	t.Helper()
	id := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO browser_lease (id, workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, decision_id, class, state, epoch, granted_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'S2', 'active', 1, now(), now() + interval '15 minutes')`,
		id, testWorkspaceID, uuid.NewString(), uuid.NewString(), uuid.NewString(), taskID, uuid.NewString(), testUserID, decisionID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM browser_lease WHERE id = $1`, id) })
	return id
}

// A withdrawn or expired decision revokes the leases bound to it, whichever path ended it
// (CHE-4742, W3-13). A lease bound to another decision stays live.
func TestEndedDecisionRevokesItsBrowserLeases(t *testing.T) {
	ws := testWorkspaceID
	cases := []struct {
		name       string
		cancel     string
		wantReason string
		publish    func(request db.DecisionRequest)
	}{
		{"agent withdraws the ask", "agent_withdrawn", browserlease.ReasonDecisionWithdrawn, func(r db.DecisionRequest) {
			testHandler.publishDecisionWithdrawal(ws, decisionRequestResult{Request: r})
		}},
		{"expiry or lifecycle cancel", "expired", browserlease.ReasonDecisionExpired, func(r db.DecisionRequest) {
			testHandler.publishWithdrawal(decisiontx.Withdrawal{Request: r})
		}},
		{"human withdraws the answer", "", browserlease.ReasonDecisionWithdrawn, func(r db.DecisionRequest) {
			// The request reopens, so the withdrawal carries the answer only.
			testHandler.publishWithdrawalEvents(ws, "member", testUserID, decisiontx.Withdrawal{Answer: db.DecisionAnswer{DecisionRequestID: r.ID, DisclaimedAt: pgtype.Timestamptz{Valid: true}}}, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtimeID := dbfx.Runtime(t, "CHE-4742 runtime")
			agentID := dbfx.Agent(t, "CHE-4742 agent "+uuid.NewString(), runtimeID)
			issueID := dbfx.Issue(t, "CHE-4742 issue")
			task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
			decision, other := uuid.NewString(), uuid.NewString()
			bound := seedDecisionBoundBrowserLease(t, task, decision)
			elsewhere := seedDecisionBoundBrowserLease(t, task, other)

			request := db.DecisionRequest{ID: parseUUID(decision), WorkspaceID: parseUUID(ws), Status: "cancelled", CancelReason: pgtype.Text{String: tc.cancel, Valid: tc.cancel != ""}}
			tc.publish(request)

			if state, reason := browserLeaseState(t, bound); state != "ended" || reason != tc.wantReason {
				t.Fatalf("bound lease state=%q reason=%q, want ended %s", state, reason, tc.wantReason)
			}
			if state, _ := browserLeaseState(t, elsewhere); state != "active" {
				t.Fatalf("a lease of another decision was revoked: state = %q", state)
			}
		})
	}
}

// An approved (answered) decision that reaches its expires_at revokes its leases through the
// decision worker's own pass; another decision's lease stays live (CHE-4742 review P1).
func TestAnsweredDecisionPastExpiryRevokesItsBrowserLeases(t *testing.T) {
	fixture := newAskedRecordFixture(t)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		t.Fatal(err)
	}
	if record.Request.Status != "answered" {
		t.Fatalf("request status = %q, want answered", record.Request.Status)
	}
	decision := uuidToString(record.Request.ID)
	runtimeID := dbfx.Runtime(t, "CHE-4742 expiry runtime")
	agentID := dbfx.Agent(t, "CHE-4742 expiry agent "+uuid.NewString(), runtimeID)
	issueID := dbfx.Issue(t, "CHE-4742 expiry issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	bound := seedDecisionBoundBrowserLease(t, task, decision)
	elsewhere := seedDecisionBoundBrowserLease(t, task, uuid.NewString())

	dbfx.Exec(t, `DELETE FROM decision_worker_lease`)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_worker_lease`) })
	// One worker for both passes: its consequences lease is held for 30 s after the first.
	worker := NewDecisionWorker(testHandler)
	pass := func() {
		t.Helper()
		if ran, err := worker.ProcessPass(context.Background()); err != nil || !ran {
			t.Fatalf("decision worker pass ran=%v err=%v", ran, err)
		}
	}
	dbfx.Exec(t, `UPDATE decision_request SET expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, decision)
	pass()
	if state, _ := browserLeaseState(t, bound); state != "active" {
		t.Fatalf("lease revoked before its decision expired: state = %q", state)
	}

	dbfx.Exec(t, `UPDATE decision_request SET expires_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, decision)
	pass()
	if state, reason := browserLeaseState(t, bound); state != "ended" || reason != browserlease.ReasonDecisionExpired {
		t.Fatalf("bound lease state=%q reason=%q, want ended %s", state, reason, browserlease.ReasonDecisionExpired)
	}
	if state, _ := browserLeaseState(t, elsewhere); state != "active" {
		t.Fatalf("a lease of another decision was revoked: state = %q", state)
	}
}
