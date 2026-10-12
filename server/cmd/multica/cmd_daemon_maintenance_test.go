package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testServerPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	return port
}

// A 409/403 from /shutdown is the daemon enforcing a window: it must surface
// as a refusal (callers then skip the kill fallback). A lost response or dead
// listener is an unknown outcome and fails closed the same way.
func TestRequestDaemonShutdownDistinguishesRefusalFromTransportFailure(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Maintenance-Token")
		if gotToken != "good" {
			http.Error(w, `{"error":"token_mismatch"}`, http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	port := testServerPort(t, srv)

	var refused *daemonShutdownRefusedError
	err := requestDaemonShutdown(port, "")
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "token_mismatch") {
		t.Fatalf("no token: %v, want refusal", err)
	}
	if err := requestDaemonShutdown(port, "good"); err != nil || gotToken != "good" {
		t.Fatalf("good token: err=%v token=%q", err, gotToken)
	}

	srv.Close()
	err = requestDaemonShutdown(port, "good")
	if err == nil || !errors.As(err, &refused) {
		t.Fatalf("closed listener: %v, want fail-closed (no kill) error", err)
	}
}

func TestPostDaemonMaintenanceSurfacesDaemonReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/maintenance/acquire" {
			http.Error(w, `{"error":"busy"}`, http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte(`{"status":"released"}`))
	}))
	defer srv.Close()
	port := testServerPort(t, srv)

	if _, err := postDaemonMaintenance(port, "/maintenance/acquire", map[string]int{"ttl_seconds": 5}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("acquire: %v, want busy refusal", err)
	}
	out, err := postDaemonMaintenance(port, "/maintenance/release", map[string]string{"token": "t"})
	if err != nil || out["status"] != "released" {
		t.Fatalf("release: %v %v", out, err)
	}
}

func TestMaintenanceTokenFromCmdPrefersFlagThenEnv(t *testing.T) {
	cmd := daemonRestartCmd
	t.Setenv(maintenanceTokenEnv, "from-env")
	if got := maintenanceTokenFromCmd(cmd); got != "from-env" {
		t.Fatalf("env fallback = %q", got)
	}
	_ = cmd.Flags().Set("maintenance-token", "from-flag")
	t.Cleanup(func() { _ = cmd.Flags().Set("maintenance-token", "") })
	if got := maintenanceTokenFromCmd(cmd); got != "from-flag" {
		t.Fatalf("flag = %q", got)
	}
}

// Only a daemon predating /shutdown (404/405) stays kill-eligible; lost
// responses, 5xx and timeouts must not, since a window may be held.
func TestRequestDaemonShutdownFailsClosedOnUnknownOutcome(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"lost response": func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		},
		"500": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			var refused *daemonShutdownRefusedError
			for _, tok := range []string{"", "stale"} {
				if err := requestDaemonShutdown(testServerPort(t, srv), tok); !errors.As(err, &refused) {
					t.Fatalf("token %q: %v, want fail-closed error", tok, err)
				}
			}
		})
	}
	legacy := httptest.NewServer(http.NotFoundHandler())
	defer legacy.Close()
	err := requestDaemonShutdown(testServerPort(t, legacy), "")
	var refused *daemonShutdownRefusedError
	if err == nil || errors.As(err, &refused) {
		t.Fatalf("legacy 404: %v, want plain error (kill-eligible)", err)
	}
}
