package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Frames are sanitized from Claude Code 2.1.318's stream-json schema for
// system/model_refusal_fallback, system/model_fallback and
// system/model_refusal_no_fallback. The incident-shaped one is Opus 5.5 -> 4.8
// with api_refusal_category=cyber (CHE-10140). UUIDs are fake.

func modelChanges(t *testing.T, frames string) []Message {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	var out []Message
	for _, m := range collectClaudeMessages(t, frames) {
		if m.Type == MessageModelChange {
			out = append(out, m)
		}
	}
	return out
}

const incidentRefusalFrame = `{"type":"system","subtype":"model_refusal_fallback","uuid":"11111111-1111-4111-8111-111111111111","session_id":"s1","trigger":"refusal","direction":"retry","scope":"session","original_model":"claude-opus-5-5","fallback_model":"claude-opus-4-8","request_id":"req_1","api_refusal_category":"cyber","api_refusal_explanation":null,"content":"x"}`

func TestClaudeModelRefusalFallbackRetryIsAnAttempt(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"init","session_id":"s1"}
`+incidentRefusalFrame+`
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`)
	// Only the attempt: nothing evidences the fallback model serving yet.
	if len(got) != 1 {
		t.Fatalf("got %d model_change messages, want 1: %#v", len(got), got)
	}
	in := got[0].Input
	for k, want := range map[string]any{
		"runtime": "claude", "event": "model_refusal_fallback", "phase": "attempt",
		"source_model": "claude-opus-5-5", "target_model": "claude-opus-4-8",
		"reason": "refusal", "category": "cyber", "direction": "retry", "scope": "session",
		"native_event_id": "11111111-1111-4111-8111-111111111111", "request_id": "req_1",
	} {
		if in[k] != want {
			t.Errorf("input[%q] = %#v, want %#v", k, in[k], want)
		}
	}
}

func TestClaudeModelRefusalFallbackAbsentScopeIsSession(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"model_refusal_fallback","uuid":"u1","trigger":"refusal","direction":"retry","original_model":"a","fallback_model":"b","request_id":null,"api_refusal_category":null}
`)
	if len(got) != 1 || got[0].Input["scope"] != "session" {
		t.Fatalf("absent scope must read as session: %#v", got)
	}
	if _, ok := got[0].Input["category"]; ok {
		t.Fatalf("null category must be omitted, got %#v", got[0].Input)
	}
	if _, ok := got[0].Input["request_id"]; ok {
		t.Fatalf("null request id must be omitted, got %#v", got[0].Input)
	}
}

func TestClaudeFallbackModelConfirmedByMainThreadAssistantFrame(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, incidentRefusalFrame+`
{"type":"assistant","session_id":"s1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"subagent"}]},"parent_tool_use_id":"toolu_sub"}
{"type":"assistant","session_id":"s1","message":{"id":"m2","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}]}}
{"type":"assistant","session_id":"s1","message":{"id":"m3","model":"claude-opus-4-8","content":[{"type":"text","text":"again"}]}}
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`)
	if len(got) != 2 {
		t.Fatalf("got %d, want attempt + one confirmation: %#v", len(got), got)
	}
	c := got[1].Input
	if c["phase"] != "confirmed" || c["target_model"] != "claude-opus-4-8" || c["source_model"] != "claude-opus-5-5" {
		t.Fatalf("confirmation = %#v", c)
	}
	if c["native_event_id"] != "11111111-1111-4111-8111-111111111111:confirmed" {
		t.Fatalf("confirmation identity = %#v", c["native_event_id"])
	}
}

func TestClaudeFallbackThatFailsIsReportedAsFailureNotSwitch(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, incidentRefusalFrame+`
{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"There's an issue with the selected model (claude-opus-4-8)."}
`)
	if len(got) != 2 || got[1].Input["phase"] != "failed" {
		t.Fatalf("want attempt then failed, got %#v", got)
	}
	for _, m := range got {
		if m.Input["phase"] == "confirmed" {
			t.Fatalf("a failed fallback must not be confirmed: %#v", got)
		}
	}
}

func TestClaudeSubagentOnlyFallbackIsLocalScopeAndNeverConfirmed(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"model_refusal_fallback","uuid":"u-local","trigger":"refusal","direction":"retry","scope":"local","original_model":"claude-opus-5-5","fallback_model":"claude-opus-4-8","request_id":"r","api_refusal_category":"cyber"}
{"type":"assistant","session_id":"s1","message":{"id":"m2","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}]}}
{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"boom"}
`)
	if len(got) != 1 || got[0].Input["scope"] != "local" {
		t.Fatalf("subagent fallback must stay one local-scope attempt: %#v", got)
	}
}

func TestClaudeRevertAndStickyDirections(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"model_refusal_fallback","uuid":"u-sticky","trigger":"refusal","direction":"sticky","scope":"session","original_model":"claude-opus-5-5","fallback_model":"claude-opus-4-8","request_id":null}
{"type":"system","subtype":"model_refusal_fallback","uuid":"u-revert","trigger":"refusal","direction":"revert","scope":"session","original_model":"claude-opus-4-8","fallback_model":"claude-opus-5-5","request_id":null}
`)
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Input["phase"] != "sticky" || got[1].Input["phase"] != "return" {
		t.Fatalf("phases = %v, %v", got[0].Input["phase"], got[1].Input["phase"])
	}
	// A revert is the return to the primary: the source is the model it left.
	if got[1].Input["source_model"] != "claude-opus-4-8" || got[1].Input["target_model"] != "claude-opus-5-5" {
		t.Fatalf("revert models = %#v", got[1].Input)
	}
}

func TestClaudeAvailabilityFallbackKeepsProviderReason(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"model_fallback","uuid":"u-ov","trigger":"overloaded","original_model":"claude-opus-5-5","fallback_model":"claude-opus-4-8","content":"overloaded"}
`)
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	in := got[0].Input
	if in["event"] != "model_fallback" || in["reason"] != "overloaded" || in["phase"] != "attempt" || in["scope"] != "session" {
		t.Fatalf("input = %#v", in)
	}
	if _, ok := in["category"]; ok {
		t.Fatalf("availability fallback has no refusal category: %#v", in)
	}
}

func TestClaudeRefusalWithoutFallbackIsATargetFailure(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"model_refusal_no_fallback","uuid":"u-nf","original_model":"claude-opus-5-5","request_id":"r","api_refusal_category":"cyber","content":"x"}
`)
	if len(got) != 1 || got[0].Input["phase"] != "failed" || got[0].Input["source_model"] != "claude-opus-5-5" {
		t.Fatalf("got %#v", got)
	}
	if _, ok := got[0].Input["target_model"]; ok {
		t.Fatalf("no fallback exists, target must be absent: %#v", got[0].Input)
	}
}

func TestClaudeModelChangeWithoutModelsIsNotInvented(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"model_refusal_fallback","uuid":"u","trigger":"refusal","direction":"retry"}
{"type":"system","subtype":"init","session_id":"s1"}
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`)
	if len(got) != 0 {
		t.Fatalf("a frame naming no model pair is unreadable evidence, got %#v", got)
	}
}

func TestClaudeNoModelTelemetryEmitsNothing(t *testing.T) {
	t.Parallel()
	got := modelChanges(t, `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","session_id":"s1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"hi"}]}}
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`)
	if len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

// A result frame with no modelUsage and no model must not book the run's
// tokens to the requested model once the runtime announced a main-thread
// fallback: the requested model is not evidence of what served (CHE-10140).
func TestClaudeUsageIsNotBookedToRequestedModelAfterMainThreadFallback(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	frames := incidentRefusalFrame + `
{"type":"assistant","session_id":"s1","message":{"id":"m2","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"result","subtype":"success","session_id":"s1","result":"ok","usage":{"input_tokens":10,"output_tokens":5}}
`
	usage := runClaudeFixtureWithModel(t, frames, "claude-opus-5-5").Usage
	if _, booked := usage["claude-opus-5-5"]; booked {
		t.Fatalf("tokens booked to the requested model after a fallback: %#v", usage)
	}
	if usage["claude-opus-4-8"].OutputTokens != 5 {
		t.Fatalf("served model usage lost: %#v", usage)
	}
}

func TestClaudeUsageFallsBackToRequestedModelWithoutAFallbackEvent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	frames := `{"type":"result","subtype":"success","session_id":"s1","result":"ok","usage":{"input_tokens":10,"output_tokens":5}}
`
	usage := runClaudeFixtureWithModel(t, frames, "claude-opus-5-5").Usage
	if usage["claude-opus-5-5"].OutputTokens != 5 {
		t.Fatalf("existing behaviour changed: %#v", usage)
	}
}

func runClaudeFixtureWithModel(t *testing.T, frames, model string) Result {
	t.Helper()
	fixturePath := filepath.Join(t.TempDir(), "frames.jsonl")
	if err := os.WriteFile(fixturePath, []byte(frames), 0o600); err != nil {
		t.Fatal(err)
	}
	fakePath := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nIFS= read -r _\ncat "+fixturePath+"\n"))
	backend, err := New("claude", Config{ExecutablePath: fakePath, Env: map[string]string{"IS_SANDBOX": "1"}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Model: model, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	select {
	case r := <-session.Result:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
		return Result{}
	}
}
