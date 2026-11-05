package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// Opt-in: runs the real `codex app-server` through agent.New("codex") against
// a local mock Responses endpoint (no model account, no cost) and checks the
// first model request for the last brief line. MULTICA_CODEX_APPSERVER_MOCK=1.
func TestCodexAppServerReceivesFullBrief(t *testing.T) {
	if os.Getenv("MULTICA_CODEX_APPSERVER_MOCK") != "1" {
		t.Skip("set MULTICA_CODEX_APPSERVER_MOCK=1 to run against the local codex binary")
	}
	for _, tc := range []struct {
		name     string
		withHint bool
		wantTail bool
	}{{"control-no-daemon-limit", false, false}, {"daemon-limit", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var first string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if strings.HasSuffix(r.URL.Path, "/responses") {
					mu.Lock()
					if first == "" {
						first = string(b)
					}
					mu.Unlock()
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n")
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			home := t.TempDir()
			cfg := fmt.Sprintf("model = \"mock\"\nmodel_provider = \"mock\"\n[model_providers.mock]\nname = \"mock\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", srv.URL+"/v1")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			work := filepath.Join(root, "workdir")
			if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			filler := strings.Repeat("Rule line with enough words to look like prose.\n", 400)
			brief, err := execenv.InjectRuntimeConfig(work, "codex", execenv.TaskContextForEnv{
				IssueID: "issue-1", AgentID: "a-1", AgentName: "Eve", WorkspaceContext: filler, AgentInstructions: filler,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(brief) <= execenv.CodexProjectDocDefaultBytes {
				t.Fatalf("brief %d bytes does not exceed the 32 KiB default", len(brief))
			}
			lines := strings.Split(strings.TrimRight(brief, "\n"), "\n")
			tail := lines[len(lines)-1]
			if strings.ContainsAny(tail, "\"\\<>&") {
				t.Fatalf("last brief line is not JSON-plain: %q", tail)
			}

			var extra []string
			if tc.withHint {
				extra = codexBriefBudget(root, work, brief, slog.Default())
			}
			be, err := agent.New("codex", agent.Config{ExecutablePath: "codex", Env: map[string]string{"CODEX_HOME": home}, Logger: slog.Default()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			s, err := be.Execute(ctx, "say ok", agent.ExecOptions{Cwd: work, ExtraArgs: extra, Timeout: 80 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				for range s.Messages {
				}
			}()
			res := <-s.Result
			mu.Lock()
			body := first
			mu.Unlock()
			if body == "" {
				t.Fatalf("mock saw no model request; result status=%q err=%q", res.Status, res.Error)
			}
			got := strings.Contains(body, tail)
			t.Logf("extra=%q brief=%d bytes request=%d bytes last-line-present=%v last-line=%q", extra, len(brief), len(body), got, tail)
			if got != tc.wantTail {
				t.Fatalf("last brief line present = %v, want %v", got, tc.wantTail)
			}
		})
	}
}
