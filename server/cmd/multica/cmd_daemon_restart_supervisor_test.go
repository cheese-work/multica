package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

type restartSupervisorFunc func(context.Context, string, int, int) error

func (f restartSupervisorFunc) wait(ctx context.Context, profile string, port, pid int) error {
	return f(ctx, profile, port, pid)
}

// Real native HTTP admission and command routing, with a fake service boundary.
// Even legacy /shutdown responses must never kill the supervised process or
// fall through to the portable child launcher.
func TestDaemonRestartSupervisedNativeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		status                                     int
		lost, noProfile, discoveryError, waitError bool
		wantErr                                    string
	}{
		{name: "success", status: 200},
		{name: "forbidden", status: 403, wantErr: "shutdown not confirmed"},
		{name: "busy", status: 409, wantErr: "shutdown not confirmed"},
		{name: "legacy404", status: 404, wantErr: "shutdown not confirmed"},
		{name: "legacy405", status: 405, wantErr: "shutdown not confirmed"},
		{name: "unknown500", status: 500, wantErr: "shutdown not confirmed"},
		{name: "lost", lost: true, wantErr: "shutdown not confirmed"},
		{name: "identity", noProfile: true, wantErr: "profile before shutdown"},
		{name: "discovery", discoveryError: true, wantErr: "ownership mismatch"},
		{name: "readiness", status: 200, waitError: true, wantErr: "readiness failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearDaemonTaskEnv(t)
			t.Chdir(t.TempDir())
			redirectTestHome(t, t.TempDir())
			t.Setenv("MULTICA_SERVER_URL", "")
			profile := "supervised-" + tc.name
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
			defer api.Close()
			if err := cli.SaveCLIConfigForProfile(cli.CLIConfig{Token: "mul_fake", ServerURL: api.URL}, profile); err != nil {
				t.Fatal(err)
			}
			pid := startBlockingChild(t)
			port := healthPortForProfile(profile)
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				t.Fatal(err)
			}
			var shutdowns atomic.Int32
			var verified atomic.Bool
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					health := map[string]any{"status": "running", "pid": pid}
					if !tc.noProfile {
						health["profile"] = profile
					}
					_ = json.NewEncoder(w).Encode(health)
					return
				}
				if r.URL.Path != "/shutdown" || r.Method != "POST" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				shutdowns.Add(1)
				if !verified.Load() {
					t.Error("shutdown preceded supervisor verification")
				}
				if r.Header.Get("X-Maintenance-Token") != "test-hold" {
					t.Error("missing maintenance token")
				}
				if tc.lost {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				w.WriteHeader(tc.status)
			}))
			srv.Listener.Close()
			srv.Listener = ln
			srv.Start()
			defer srv.Close()
			waited := false
			previous := findDaemonRestartSupervisor
			findDaemonRestartSupervisor = func(gotPID int) (daemonRestartSupervisor, error) {
				if gotPID != pid {
					t.Fatalf("discovered PID = %d, want %d", gotPID, pid)
				}
				if tc.discoveryError {
					return nil, errors.New("ownership mismatch")
				}
				verified.Store(true)
				return restartSupervisorFunc(func(ctx context.Context, gotProfile string, gotPort, oldPID int) error {
					waited = true
					if gotProfile != profile || gotPort != port || oldPID != pid || shutdowns.Load() != 1 {
						t.Fatal("invalid supervisor handoff")
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("supervised restart has no deadline")
					}
					if tc.waitError {
						return errors.New("readiness failed")
					}
					return nil
				}), nil
			}
			t.Cleanup(func() { findDaemonRestartSupervisor = previous })
			cmd := newRestartTestCmd(t, profile)
			cmd.Flags().String("maintenance-token", "test-hold", "")
			err = runDaemonRestart(cmd, nil)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want %s", err, tc.wantErr)
			}
			wantShutdown := int32(1)
			if tc.noProfile || tc.discoveryError {
				wantShutdown = 0
			}
			if shutdowns.Load() != wantShutdown {
				t.Fatalf("shutdowns = %d, want %d", shutdowns.Load(), wantShutdown)
			}
			if waited != (tc.status == 200) {
				t.Fatalf("waited = %v", waited)
			}
		})
	}
}
