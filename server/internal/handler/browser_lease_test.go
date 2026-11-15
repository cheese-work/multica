package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/browserlease"
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
	req = withURLParams(req, append([]string{"taskId", f.taskID}, kv...)...)
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
	if w.Code != http.StatusNoContent {
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
	req = withURLParam(req, "taskId", f.taskID)
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
