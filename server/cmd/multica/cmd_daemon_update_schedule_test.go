package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newUpdateScheduleTestCmd(t *testing.T, serverURL string) *cobra.Command {
	t.Helper()
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	cmd := &cobra.Command{Use: "update-schedule"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	addDaemonUpdateScheduleFlags(cmd)
	_ = cmd.Flags().Set("server-url", serverURL)
	_ = cmd.Flags().Set("workspace-id", "ws-1")
	return cmd
}

func TestDaemonUpdateScheduleGet(t *testing.T) {
	var gotReq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Method + " " + r.URL.Path
		w.Write([]byte(`{"enabled":true,"cron":"0 2 * * *","timezone":"Asia/Ho_Chi_Minh","window_minutes":30,"next_runs":["2026-11-18T02:00:00+07:00"]}`))
	}))
	defer srv.Close()

	cmd := newUpdateScheduleTestCmd(t, srv.URL)
	_ = cmd.Flags().Set("runtime-id", "rt-1")
	out, err := captureRuntimeStdout(t, func() error { return runDaemonUpdateScheduleGet(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if gotReq != "GET /api/runtimes/rt-1/update-schedule" {
		t.Fatalf("request = %q", gotReq)
	}
	for _, want := range []string{"0 2 * * *", "Asia/Ho_Chi_Minh", "30", "2026-11-18T02:00:00+07:00"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDaemonUpdateScheduleSetSendsTheFlags(t *testing.T) {
	var gotReq string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"enabled":false,"cron":"30 3 * * 1","timezone":"UTC","window_minutes":10,"next_runs":[]}`))
	}))
	defer srv.Close()

	cmd := newUpdateScheduleTestCmd(t, srv.URL)
	for k, v := range map[string]string{"runtime-id": "rt-1", "cron": "30 3 * * 1", "timezone": "UTC", "window-minutes": "10", "enabled": "false"} {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set --%s: %v", k, err)
		}
	}
	if _, err := captureRuntimeStdout(t, func() error { return runDaemonUpdateScheduleSet(cmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if gotReq != "PUT /api/runtimes/rt-1/update-schedule" {
		t.Fatalf("request = %q", gotReq)
	}
	if gotBody["enabled"] != false || gotBody["cron"] != "30 3 * * 1" || gotBody["timezone"] != "UTC" || gotBody["window_minutes"] != float64(10) {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestDaemonUpdateScheduleSetDefaultsToEnabledAndOmitsUnsetFlags(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"enabled":true,"cron":"0 2 * * *","timezone":"Asia/Ho_Chi_Minh","window_minutes":30}`))
	}))
	defer srv.Close()

	cmd := newUpdateScheduleTestCmd(t, srv.URL)
	_ = cmd.Flags().Set("runtime-id", "rt-1")
	_ = cmd.Flags().Set("cron", "0 2 * * *")
	if _, err := captureRuntimeStdout(t, func() error { return runDaemonUpdateScheduleSet(cmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if gotBody["enabled"] != true {
		t.Fatalf("enabled = %v, want true by default", gotBody["enabled"])
	}
	if _, ok := gotBody["timezone"]; ok {
		t.Fatalf("body %v sends timezone although the flag was not set", gotBody)
	}
	if _, ok := gotBody["window_minutes"]; ok {
		t.Fatalf("body %v sends window_minutes although the flag was not set", gotBody)
	}
}

func TestDaemonUpdateScheduleSetNeedsRuntimeAndCron(t *testing.T) {
	cmd := newUpdateScheduleTestCmd(t, "http://127.0.0.1:1")
	if err := runDaemonUpdateScheduleSet(cmd, nil); err == nil || !strings.Contains(err.Error(), "--runtime-id") {
		t.Fatalf("err = %v, want one naming --runtime-id", err)
	}
	_ = cmd.Flags().Set("runtime-id", "rt-1")
	if err := runDaemonUpdateScheduleSet(cmd, nil); err == nil || !strings.Contains(err.Error(), "--cron") {
		t.Fatalf("err = %v, want one naming --cron", err)
	}
}

func TestDaemonUpdateScheduleClear(t *testing.T) {
	var gotReq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cmd := newUpdateScheduleTestCmd(t, srv.URL)
	_ = cmd.Flags().Set("runtime-id", "rt-1")
	out, err := captureRuntimeStdout(t, func() error { return runDaemonUpdateScheduleClear(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if gotReq != "DELETE /api/runtimes/rt-1/update-schedule" {
		t.Fatalf("request = %q", gotReq)
	}
	if !strings.Contains(strings.ToLower(out), "cleared") {
		t.Fatalf("output = %q, want a confirmation", out)
	}
}
