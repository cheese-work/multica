package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeCodexRollout(t *testing.T, codexHome, threadID string, versions ...string) {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", "2026", "09", "22")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"type":"session_meta","payload":{"id":"` + threadID + `"}}` + "\n")
	for _, v := range versions {
		mav := `null`
		if v != "" {
			mav = `"` + v + `"`
		}
		b.WriteString(`{"type":"turn_context","payload":{"model":"gpt-5.6-terra","multi_agent_version":` + mav + `}}` + "\n")
	}
	path := filepath.Join(dir, "rollout-2026-09-22T18-23-47-"+threadID+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexResumeKeepsMultiAgentV2(t *testing.T) {
	t.Setenv("MULTICA_CODEX_MULTI_AGENT", "")
	home := t.TempDir()
	writeCodexRollout(t, home, "thr-v2", "v2", "v2")
	writeCodexRollout(t, home, "thr-disabled", "disabled")
	writeCodexRollout(t, home, "thr-was-v2", "v2", "disabled")
	writeCodexRollout(t, home, "thr-null", "")

	cases := []struct {
		name, home, thread string
		want               bool
	}{
		{"persisted v2 thread", home, "thr-v2", true},
		{"thread started with multi-agent disabled", home, "thr-disabled", false},
		{"last turn decides", home, "thr-was-v2", false},
		{"no multi_agent_version", home, "thr-null", false},
		{"unknown thread resumes as before", home, "thr-missing", false},
		{"no resume requested", home, "", false},
		{"no CODEX_HOME", "", "thr-v2", false},
	}
	for _, tc := range cases {
		if got := codexResumeKeepsMultiAgentV2(tc.home, tc.thread); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	t.Setenv("MULTICA_CODEX_MULTI_AGENT", "1")
	if codexResumeKeepsMultiAgentV2(home, "thr-v2") {
		t.Error("MULTICA_CODEX_MULTI_AGENT opt-in must keep resuming v2 threads")
	}
}

// A task whose prior thread was persisted under multi-agent v2 must start a
// fresh thread instead of resuming it (CHE-702).
func TestCodexExecuteStartsFreshInsteadOfResumingMultiAgentV2Thread(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	t.Setenv("MULTICA_CODEX_MULTI_AGENT", "")
	home := t.TempDir()
	writeCodexRollout(t, home, "thr-old-v2", "v2")
	requests := filepath.Join(t.TempDir(), "requests.jsonl")

	fakePath := writeFakeCodexAppServer(t, ""+
		`read line; echo "$line" >> "`+requests+`"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read line; echo "$line" >> "`+requests+`"`+"\n"+
		`read line; echo "$line" >> "`+requests+`"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-fresh"}}}'`+"\n"+
		`read line; echo "$line" >> "`+requests+`"`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-fresh","turn":{"id":"turn-1","status":"completed"}}}'`+"\n")

	result, _ := executeFakeCodexCollectingMessagesWithConfig(t, fakePath,
		Config{Logger: slog.Default(), Env: map[string]string{"CODEX_HOME": home}},
		ExecOptions{Timeout: 5 * time.Second, ResumeSessionID: "thr-old-v2", ResumeContinuityNotice: "CONTEXT-LOST"},
		10*time.Second)
	if result.Status != "completed" {
		t.Fatalf("status=%q error=%q", result.Status, result.Error)
	}
	raw, err := os.ReadFile(requests)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, `"thread/resume"`) {
		t.Fatalf("resumed the multi-agent v2 thread:\n%s", got)
	}
	if !strings.Contains(got, `"thread/start"`) {
		t.Fatalf("did not start a fresh thread:\n%s", got)
	}
	if !strings.Contains(got, "CONTEXT-LOST") {
		t.Fatalf("fresh turn lost the continuity notice:\n%s", got)
	}
}
