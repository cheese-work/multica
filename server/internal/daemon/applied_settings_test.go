package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
)

// appliedCapture is a client whose server records applied-settings posts.
func appliedCapture(t *testing.T, status int) (*Daemon, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	client := NewClient("https://daemon.test")
	client.client.Transport = startTaskTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/daemon/tasks/task-1/applied-settings" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(http.NoBody)}, nil
	})
	return &Daemon{client: client}, &bodies
}

func supportedClaim() Task {
	claim := startTestClaim()
	claim.AppliedSettingsSupported = true
	return claim
}

// R3: the report carries the claim generation and what the daemon resolved,
// including a model the claim payload left empty.
func TestReportAppliedSettings_R3_ReportsResolvedSelection(t *testing.T) {
	d, bodies := appliedCapture(t, http.StatusOK)
	d.reportAppliedSettings(context.Background(), supportedClaim(), "claude",
		AppliedSettings{Model: "env-model", ThinkingLevel: "high", ContextArgs: []string{"--autocompact", "200k"}}, slog.Default())

	want := map[string]any{
		"runtime_id": "runtime-1", "dispatched_at": "2026-09-17T09:00:00.123456Z",
		"applied_model": "env-model", "applied_thinking_level": "high", "applied_service_tier": "",
		"applied_context_args": []any{"--autocompact", "200k"},
	}
	if len(*bodies) != 1 || !reflect.DeepEqual((*bodies)[0], want) {
		t.Fatalf("bodies = %v, want [%v]", *bodies, want)
	}
}

// R4: a level or tier the daemon dropped is reported empty, never as requested.
func TestReportAppliedSettings_R4_DroppedValuesReportedEmpty(t *testing.T) {
	d, bodies := appliedCapture(t, http.StatusOK)
	d.reportAppliedSettings(context.Background(), supportedClaim(), "codex", AppliedSettings{Model: "m"}, slog.Default())

	if len(*bodies) != 1 {
		t.Fatalf("posts = %d, want 1", len(*bodies))
	}
	got := (*bodies)[0]
	if got["applied_thinking_level"] != "" || got["applied_service_tier"] != "" || !reflect.DeepEqual(got["applied_context_args"], []any{}) {
		t.Fatalf("body = %v", got)
	}
}

// R5 daemon half: a claim that does not say supported is never reported.
func TestReportAppliedSettings_R5_UnsupportedClaimSendsNothing(t *testing.T) {
	d, bodies := appliedCapture(t, http.StatusOK)
	d.reportAppliedSettings(context.Background(), startTestClaim(), "claude", AppliedSettings{Model: "m"}, slog.Default())

	if len(*bodies) != 0 {
		t.Fatalf("posted %v to a server that did not offer the endpoint", *bodies)
	}
}

// R6: a refused report is swallowed; the caller carries on to launch.
func TestReportAppliedSettings_R6_FailureDoesNotStopTheTask(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusInternalServerError} {
		d, bodies := appliedCapture(t, status)
		d.reportAppliedSettings(context.Background(), supportedClaim(), "claude", AppliedSettings{Model: "m"}, slog.Default())
		if len(*bodies) != 1 {
			t.Fatalf("status %d: posts = %d, want exactly one attempt", status, len(*bodies))
		}
	}
}

func TestCapabilities_AdvertiseAppliedSettings(t *testing.T) {
	for _, capability := range daemonCommonCapabilities() {
		if capability == "applied-settings-v1" {
			return
		}
	}
	t.Fatal("daemon does not advertise applied-settings-v1")
}
