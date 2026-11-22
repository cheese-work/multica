package daemon

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// browserLeaseTestDaemon fronts a fake server. The bound task holds the daemon credential
// "mdt_task"; the agent only ever holds "mat_agent".
func browserLeaseTestDaemon(t *testing.T, serverHandler http.HandlerFunc, daemonToken string) *Daemon {
	t.Helper()
	srv := httptest.NewServer(serverHandler)
	t.Cleanup(srv.Close)
	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	d.registerActiveRepoCheckoutTask("mat_agent", activeRepoCheckoutTask{
		WorkspaceID: "ws-1", TaskID: "task-1", DaemonToken: daemonToken,
	})
	return d
}

func callBrowserLease(d *Daemon, agentToken, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/browser/lease", strings.NewReader(body))
	if agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+agentToken)
	}
	w := httptest.NewRecorder()
	d.browserLeaseHandler()(w, req)
	return w
}

func TestBrowserLeaseHandlerForwardsEachOpWithTheTaskDaemonCredential(t *testing.T) {
	cases := []struct {
		body, method, path, wantBody string
	}{
		{`{"op":"lease","profile_id":"p1","decision_id":"d1"}`, "POST", "/api/daemon/tasks/task-1/browser-leases", `{"decision_id":"d1","profile_id":"p1"}`},
		{`{"op":"renew","lease_id":"l1"}`, "POST", "/api/daemon/tasks/task-1/browser-leases/l1/renew", ""},
		{`{"op":"release","lease_id":"l1"}`, "POST", "/api/daemon/tasks/task-1/browser-leases/l1/release", ""},
		{`{"op":"status","lease_id":"l1"}`, "GET", "/api/daemon/tasks/task-1/browser-leases/l1", ""},
	}
	for _, c := range cases {
		var gotAuth, gotMethod, gotPath string
		var gotBody bytes.Buffer
		d := browserLeaseTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
			gotBody.ReadFrom(r.Body)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"state":"queued"}`))
		}, "mdt_task")
		w := callBrowserLease(d, "mat_agent", c.body)
		if gotAuth != "Bearer mdt_task" || gotMethod != c.method || gotPath != c.path {
			t.Errorf("%s: forwarded %s %s auth=%q, want %s %s auth=Bearer mdt_task", c.body, gotMethod, gotPath, gotAuth, c.method, c.path)
		}
		if c.wantBody != "" && strings.TrimSpace(gotBody.String()) != c.wantBody {
			t.Errorf("%s: forwarded body %q, want %q", c.body, gotBody.String(), c.wantBody)
		}
		if w.Code != http.StatusAccepted || strings.TrimSpace(w.Body.String()) != `{"state":"queued"}` {
			t.Errorf("%s: reply %d %q, want the server's 202 body verbatim", c.body, w.Code, w.Body.String())
		}
	}
}

func TestBrowserLeaseHandlerRefusals(t *testing.T) {
	called := false
	d := browserLeaseTestDaemon(t, func(http.ResponseWriter, *http.Request) { called = true }, "mdt_task")
	for _, c := range []struct {
		name, token, body string
		want              int
	}{
		{"no credential", "", `{"op":"lease","profile_id":"p1"}`, http.StatusUnauthorized},
		{"unknown credential", "mat_other", `{"op":"lease","profile_id":"p1"}`, http.StatusUnauthorized},
		{"unknown op", "mat_agent", `{"op":"revoke-all"}`, http.StatusBadRequest},
		{"lease needs a profile", "mat_agent", `{"op":"lease"}`, http.StatusBadRequest},
		{"renew needs a lease", "mat_agent", `{"op":"renew"}`, http.StatusBadRequest},
		{"lease id cannot climb the path", "mat_agent", `{"op":"status","lease_id":"../x"}`, http.StatusBadRequest},
	} {
		if w := callBrowserLease(d, c.token, c.body); w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
	if called {
		t.Error("a refused call reached the server")
	}

	noToken := browserLeaseTestDaemon(t, func(http.ResponseWriter, *http.Request) { called = true }, "")
	if w := callBrowserLease(noToken, "mat_agent", `{"op":"lease","profile_id":"p1"}`); w.Code != http.StatusServiceUnavailable || called {
		t.Errorf("task without a daemon credential: got %d called=%v, want 503 and no server call", w.Code, called)
	}
}
