package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
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
// as a refusal (callers then skip the kill fallback). A dead listener stays a
// plain transport error so the existing forced-kill fallback still applies.
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
	if err == nil || errors.As(err, &refused) {
		t.Fatalf("closed listener: %v, want a non-refusal transport error", err)
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
