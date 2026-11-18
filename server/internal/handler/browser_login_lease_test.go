package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/browserlease"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Class H login lease (CHE-5989, W3-3b): member auth, C00 only, the member's own lease only.

type loginLeaseFixture struct {
	h       *Handler
	profile string
	other   string // a second workspace member
}

type loginLeaseOut struct {
	LeaseID string `json:"lease_id"`
	State   string `json:"state"`
	Class   string `json:"class"`
	Aud     string `json:"aud"`
	Token   string `json:"token"`
	Epoch   int64  `json:"epoch"`
}

func newLoginLeaseFixture(t *testing.T) loginLeaseFixture {
	t.Helper()
	ctx := context.Background()
	q := db.New(testPool)
	ws := parseUUID(testWorkspaceID)
	prof, err := q.CreateBrowserProfile(ctx, db.CreateBrowserProfileParams{WorkspaceID: ws, Name: "login", Class: "H"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateBrowserInstance(ctx, db.CreateBrowserInstanceParams{WorkspaceID: ws, Name: "c00-1"}); err != nil {
		t.Fatal(err)
	}
	var other string
	email := "login-lease-" + uuid.NewString() + "@multica.test"
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Login Lease Other', $1) RETURNING id`, email).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"browser_lease", "browser_instance", "browser_profile"} {
			testPool.Exec(ctx, "DELETE FROM "+tbl+" WHERE workspace_id = $1", testWorkspaceID)
		}
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, other)
	})
	h := testHandler
	h.BrowserLeases = &browserlease.Service{
		Pool: testPool, Queries: q, Gate: allowHooks{}, Decisions: allowHooks{},
		Registry: browserlease.NameRegistry{Queries: q}, Signer: stubSigner{},
	}
	t.Cleanup(func() { h.BrowserLeases = nil })
	return loginLeaseFixture{h: h, profile: uuid.UUID(prof.ID.Bytes).String(), other: other}
}

func (f loginLeaseFixture) req(userID, leaseID string, body any) *http.Request {
	req := newRequestAs(userID, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/browser-login-leases", body)
	kv := []string{"id", testWorkspaceID}
	if leaseID != "" {
		kv = append(kv, "leaseId", leaseID)
	}
	return withURLParams(req, kv...)
}

func (f loginLeaseFixture) mint(t *testing.T, userID string) loginLeaseOut {
	t.Helper()
	w := httptest.NewRecorder()
	f.h.MintBrowserLoginLease(w, f.req(userID, "", map[string]string{"profile_id": f.profile}))
	if w.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var out loginLeaseOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f loginLeaseFixture) status(fn http.HandlerFunc, req *http.Request) int {
	w := httptest.NewRecorder()
	fn(w, req)
	return w.Code
}

func TestBrowserLoginLeaseMintRenewRelease(t *testing.T) {
	f := newLoginLeaseFixture(t)
	out := f.mint(t, testUserID)
	if out.State != "active" || out.Class != "H" || out.Token == "" || out.Aud != "steel-gateway/c00" || out.Epoch != 1 {
		t.Fatalf("mint body = %+v", out)
	}
	var userID string
	var agent, task, issue *string
	if err := testPool.QueryRow(context.Background(), `SELECT user_id::text, agent_id::text, task_id::text, issue_id::text FROM browser_lease WHERE id = $1`, out.LeaseID).
		Scan(&userID, &agent, &task, &issue); err != nil {
		t.Fatal(err)
	}
	if userID != testUserID || agent != nil || task != nil || issue != nil {
		t.Fatalf("row user=%s agent=%v task=%v issue=%v, want the member and no agent, task or issue", userID, agent, task, issue)
	}

	if code := f.status(f.h.RenewBrowserLoginLease, f.req(testUserID, out.LeaseID, nil)); code != http.StatusOK {
		t.Fatalf("renew: %d", code)
	}
	w := httptest.NewRecorder()
	f.h.ReleaseBrowserLoginLease(w, f.req(testUserID, out.LeaseID, nil))
	var rel map[string]int
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rel) != nil || rel["revoked"] != 1 {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	if state, reason := browserLeaseState(t, out.LeaseID); state != "ended" || reason != "release" {
		t.Fatalf("state=%q reason=%q, want ended release", state, reason)
	}
	if code := f.status(f.h.RenewBrowserLoginLease, f.req(testUserID, out.LeaseID, nil)); code != http.StatusConflict {
		t.Fatalf("renew after release: %d", code)
	}
}

func TestBrowserLoginLeaseOfAnotherMemberIs403(t *testing.T) {
	f := newLoginLeaseFixture(t)
	out := f.mint(t, testUserID)
	if code := f.status(f.h.RenewBrowserLoginLease, f.req(f.other, out.LeaseID, nil)); code != http.StatusForbidden {
		t.Fatalf("renew by another member: %d", code)
	}
	if code := f.status(f.h.ReleaseBrowserLoginLease, f.req(f.other, out.LeaseID, nil)); code != http.StatusForbidden {
		t.Fatalf("release by another member: %d", code)
	}
	if state, _ := browserLeaseState(t, out.LeaseID); state != "active" {
		t.Fatalf("another member ended the lease: state = %q", state)
	}
}

func TestBrowserLoginLeaseRefusesANonMember(t *testing.T) {
	f := newLoginLeaseFixture(t)
	w := httptest.NewRecorder()
	f.h.MintBrowserLoginLease(w, f.req(uuid.NewString(), "", map[string]string{"profile_id": f.profile}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-member mint: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLoginLeaseRefusesADaemonCredential(t *testing.T) {
	f := newLoginLeaseFixture(t)
	out := f.mint(t, testUserID)
	daemon := func(leaseID string, body any) *http.Request {
		req := newDaemonTokenRequest(http.MethodPost, "/x", body, testWorkspaceID, "bl-daemon-"+uuid.NewString())
		kv := []string{"id", testWorkspaceID}
		if leaseID != "" {
			kv = append(kv, "leaseId", leaseID)
		}
		return withURLParams(req, kv...)
	}
	if code := f.status(f.h.MintBrowserLoginLease, daemon("", map[string]string{"profile_id": f.profile})); code != http.StatusForbidden {
		t.Fatalf("daemon mint: %d", code)
	}
	if code := f.status(f.h.RenewBrowserLoginLease, daemon(out.LeaseID, nil)); code != http.StatusForbidden {
		t.Fatalf("daemon renew: %d", code)
	}
	if code := f.status(f.h.ReleaseBrowserLoginLease, daemon(out.LeaseID, nil)); code != http.StatusForbidden {
		t.Fatalf("daemon release: %d", code)
	}
}

func TestBrowserLoginLeaseRefusesAnAgentTaskToken(t *testing.T) {
	f := newLoginLeaseFixture(t)
	req := f.req(testUserID, "", map[string]string{"profile_id": f.profile})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", uuid.NewString())
	if code := f.status(f.h.MintBrowserLoginLease, req); code != http.StatusForbidden {
		t.Fatalf("agent task token: %d", code)
	}
}

func TestBrowserLoginLeaseFailsClosed(t *testing.T) {
	f := newLoginLeaseFixture(t)
	mint := func() int {
		return f.status(f.h.MintBrowserLoginLease, f.req(testUserID, "", map[string]string{"profile_id": f.profile}))
	}
	f.h.BrowserLeases.Gate = browserlease.DenyAll{}
	if code := mint(); code != http.StatusForbidden {
		t.Fatalf("gate denied: %d", code)
	}
	f.h.BrowserLeases.Gate = allowHooks{}
	f.h.BrowserLeases.Decisions = browserlease.DenyAll{}
	if code := mint(); code != http.StatusConflict {
		t.Fatalf("decision required: %d", code)
	}
	f.h.BrowserLeases.Decisions = allowHooks{}
	f.h.BrowserLeases.Signer = nil
	if code := mint(); code != http.StatusServiceUnavailable {
		t.Fatalf("no signer: %d", code)
	}
	f.h.BrowserLeases = nil
	if code := mint(); code != http.StatusServiceUnavailable {
		t.Fatalf("not configured: %d", code)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE workspace_id = $1`, testWorkspaceID).Scan(&n)
	if n != 0 {
		t.Fatalf("a refused mint left %d lease rows", n)
	}
}

func TestBrowserLoginLeaseMintsOnlyClassH(t *testing.T) {
	f := newLoginLeaseFixture(t)
	prof, err := db.New(testPool).CreateBrowserProfile(context.Background(), db.CreateBrowserProfileParams{WorkspaceID: parseUUID(testWorkspaceID), Name: "s2", Class: "S2"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.h.MintBrowserLoginLease(w, f.req(testUserID, "", map[string]string{"profile_id": uuid.UUID(prof.ID.Bytes).String()}))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("S2 profile on the login route: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLoginLeaseSecondMintIs409(t *testing.T) {
	f := newLoginLeaseFixture(t)
	f.mint(t, testUserID)
	w := httptest.NewRecorder()
	f.h.MintBrowserLoginLease(w, f.req(testUserID, "", map[string]string{"profile_id": f.profile}))
	if w.Code != http.StatusConflict {
		t.Fatalf("second mint: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserLoginLeaseRouteDoesNotTouchAnAgentLease(t *testing.T) {
	f := newLoginLeaseFixture(t)
	runtimeID := dbfx.Runtime(t, "login-lease agent-lease runtime")
	agentID := dbfx.Agent(t, "login-lease agent", runtimeID)
	issueID := dbfx.Issue(t, "login-lease agent lease issue")
	task := dbfx.Task(t, agentID, map[string]any{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})
	lease := seedActiveBrowserLease(t, task) // user_id = testUserID, class S0
	if code := f.status(f.h.RenewBrowserLoginLease, f.req(testUserID, lease, nil)); code != http.StatusForbidden {
		t.Fatalf("renew of an agent lease: %d", code)
	}
	if code := f.status(f.h.ReleaseBrowserLoginLease, f.req(testUserID, lease, nil)); code != http.StatusForbidden {
		t.Fatalf("release of an agent lease: %d", code)
	}
	if state, _ := browserLeaseState(t, lease); state != "active" {
		t.Fatalf("the login route ended an agent lease: state = %q", state)
	}
}
