package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	staleResumedConversationID = "67a6d8f2-8523-46fc-8fc9-87e630cbe295"
	staleResumedError          = "agent executor error: NOT_FOUND (code 404): Requested entity was not found."
)

// staleResumedFixture is a sanitized fake of the CHE-1710 reproduction: a
// resumed AGY turn whose current response is DONE and exits 0, while the
// structured result still carries an old conversation error.
type staleResumedFixture struct {
	responseState string // state of the current agent_response step
	response      string // result.response
	resultError   string // result.error; defaults to the stale 404
	toolError     bool   // current turn has a failed tool step
	logLines      string // glog lines for the current run
	exitCode      int
	tail          string // shell run after the result line
}

func (f staleResumedFixture) script() string {
	if f.responseState == "" {
		f.responseState = "DONE"
	}
	if f.resultError == "" {
		f.resultError = staleResumedError
	}
	tool := ""
	if f.toolError {
		tool = fmt.Sprintf(`printf '%%s\n' '{"event":"step_update","step_update":{"conversation_id":"%s","step_index":305,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","error":{"type":"EXIT","message":"exit status 2"}}}}'
`, staleResumedConversationID)
	}
	return fmt.Sprintf(`#!/bin/sh
log=""
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file) log="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -z "$log" ] || printf '%%s' '%s' >> "$log"
%sprintf '%%s\n' '{"event":"step_update","step_update":{"conversation_id":"%s","step_index":306,"state":"%s","step_type":"agent_response","text_delta":"%s"}}'
printf '%%s\n' '{"event":"result","result":{"conversation_id":"%s","status":"ERROR","response":"%s","error":"%s"}}'
%s
exit %d
`, f.logLines, tool, staleResumedConversationID, f.responseState, f.response,
		staleResumedConversationID, f.response, f.resultError, f.tail, f.exitCode)
}

func runStaleResumed(t *testing.T, ctx context.Context, f staleResumedFixture, opts ExecOptions) Result {
	t.Helper()
	fakePath := filepath.Join(t.TempDir(), "agy")
	writeTestExecutable(t, fakePath, []byte(f.script()))

	backend, err := New("antigravity", Config{ExecutablePath: fakePath, Logger: quietAntigravityLogger()})
	if err != nil {
		t.Fatalf("new antigravity backend: %v", err)
	}
	session, err := backend.Execute(ctx, "prompt-ignored", opts)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for range session.Messages {
	}
	result, ok := <-session.Result
	if !ok {
		t.Fatal("result channel closed without a value")
	}
	return result
}

// resumedOpts resumes the conversation with the server-recorded prior failure,
// the positive evidence that the result error is historical.
func resumedOpts() ExecOptions {
	return ExecOptions{ResumeSessionID: staleResumedConversationID, ResumePriorError: staleResumedError}
}

// The stale historical 404 must not fail a resumed turn whose current response
// completed, the process exited 0 and this run's log has no executor error.
func TestAntigravityBackendIgnoresStaleResumedResultError(t *testing.T) {
	t.Parallel()

	result := runStaleResumed(t, context.Background(), staleResumedFixture{response: "RESUMED_HIGH_SECOND_OK"}, resumedOpts())
	// Output is the current response only: no historical text leaks in.
	if result.Status != "completed" || result.Output != "RESUMED_HIGH_SECOND_OK" || result.Error != "" {
		t.Fatalf("result = status %q output %q error %q", result.Status, result.Output, result.Error)
	}
	if result.SessionID != staleResumedConversationID {
		t.Fatalf("session id = %q", result.SessionID)
	}
}

// Every other resumed shape keeps failing: only the proven-stale result error
// is tolerated.
func TestAntigravityBackendKeepsResumedFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fix    staleResumedFixture
		opts   ExecOptions
		status string
		errHas string
	}{
		{
			name:   "current 404 in this run's log",
			fix:    staleResumedFixture{response: "partial", logLines: "E0624 12:34:25.944050 1 log.go:398] " + staleResumedError + "\n"},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "partial latest response",
			fix:    staleResumedFixture{response: "partial", responseState: "ACTIVE"},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "empty response",
			fix:    staleResumedFixture{response: ""},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "current tool failure",
			fix:    staleResumedFixture{response: "answer", toolError: true},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "nonzero exit",
			fix:    staleResumedFixture{response: "answer", exitCode: 1},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "print timeout in this run's log",
			fix:    staleResumedFixture{response: "answer", logLines: "E0623 17:17:59.017212 1 printmode.go:289] Print mode: timed out after 100 polls (printed=3)\n"},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "genuine current 404 without a log entry and without prior-error provenance",
			fix:    staleResumedFixture{response: "answer"},
			opts:   ExecOptions{ResumeSessionID: staleResumedConversationID},
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "prior error recorded for a different error",
			fix:    staleResumedFixture{response: "answer"},
			opts:   ExecOptions{ResumeSessionID: staleResumedConversationID, ResumePriorError: "agent executor error: UNAVAILABLE (code 503)"},
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "result names another conversation than the resumed one",
			fix:    staleResumedFixture{response: "answer"},
			opts:   ExecOptions{ResumeSessionID: "77a6d8f2-8523-46fc-8fc9-87e630cbe295", ResumePriorError: staleResumedError},
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "prior error without a resumed conversation",
			fix:    staleResumedFixture{response: "answer"},
			opts:   ExecOptions{ResumePriorError: staleResumedError},
			status: "failed",
			errHas: "NOT_FOUND",
		},
		{
			name:   "current structured error that differs from the prior error and is absent from the log",
			fix:    staleResumedFixture{response: "answer", resultError: "agent executor error: PERMISSION_DENIED (code 403): quota exhausted"},
			opts:   resumedOpts(),
			status: "failed",
			errHas: "PERMISSION_DENIED",
		},
		{
			name:   "fresh conversation keeps the structured error",
			fix:    staleResumedFixture{response: "answer"},
			opts:   ExecOptions{},
			status: "failed",
			errHas: "NOT_FOUND",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := runStaleResumed(t, context.Background(), tt.fix, tt.opts)
			if result.Status != tt.status || !strings.Contains(result.Error, tt.errHas) {
				t.Fatalf("result = status %q output %q error %q, want status %q error containing %q",
					result.Status, result.Output, result.Error, tt.status, tt.errHas)
			}
		})
	}
}

// Cancellation and timeout outrank the stale-result exemption even when the
// stale-looking result line was already emitted.
func TestAntigravityBackendResumedStaleResultStillHonoursCancelAndTimeout(t *testing.T) {
	t.Parallel()

	slow := staleResumedFixture{response: "answer", tail: "sleep 60"}

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		opts := resumedOpts()
		opts.Timeout = time.Second
		result := runStaleResumed(t, context.Background(), slow, opts)
		if result.Status != "timeout" {
			t.Fatalf("status = %q error %q, want timeout", result.Status, result.Error)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		ctx, stop := context.WithCancel(context.Background())
		time.AfterFunc(500*time.Millisecond, stop)
		result := runStaleResumed(t, ctx, slow, resumedOpts())
		if result.Status != "aborted" {
			t.Fatalf("status = %q error %q, want aborted", result.Status, result.Error)
		}
	})
}
