package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/initctx"
)

func sampleInitContext() *initctx.Report {
	return &initctx.Report{Parts: []initctx.PartReport{
		{Part: initctx.PartTask, Bytes: 10, Tokens: 3, Budget: 0, Notes: []string{"n"}},
	}}
}

// The part bytes must equal the bytes the agent actually receives: the brief
// InjectRuntimeConfig returns (and writes to the workdir) plus the prompt.
func TestBuildInitContextPartBytesEqualBriefPlusPrompt(t *testing.T) {
	brief, err := execenv.InjectRuntimeConfig(t.TempDir(), "claude", execenv.TaskContextForEnv{IssueID: "issue-1"})
	if err != nil || brief == "" {
		t.Fatalf("inject runtime config: brief=%d bytes err=%v", len(brief), err)
	}
	prompt := BuildPrompt(Task{ID: "t1", IssueID: "issue-1"}, "claude")

	report := buildInitContext(brief, prompt)
	if report == nil {
		t.Fatal("buildInitContext returned nil")
	}
	var sum int
	for _, p := range report.Parts {
		sum += p.Bytes
	}
	if want := len(brief) + len(prompt); sum != want {
		t.Fatalf("part bytes = %d, want len(brief)+len(prompt) = %d", sum, want)
	}
}

func TestCompleteTaskBodyCarriesInitContext(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	c := NewClient(srv.URL)

	if err := c.completeTaskWithRetrySchedule(context.Background(), "t1", "out", "", "", "", false, "", "", 0, 0, sampleInitContext(), nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var report initctx.Report
	if err := json.Unmarshal(got["init_context"], &report); err != nil || !reflect.DeepEqual(&report, sampleInitContext()) {
		t.Fatalf("init_context = %s (err %v), want %+v", got["init_context"], err, sampleInitContext())
	}

	got = nil
	if err := c.completeTaskWithRetrySchedule(context.Background(), "t1", "out", "", "", "", false, "", "", 0, 0, nil, nil); err != nil {
		t.Fatalf("complete without report: %v", err)
	}
	if _, ok := got["init_context"]; ok {
		t.Fatalf("init_context sent without a report: %s", got["init_context"])
	}
}

func TestReportTaskResultForwardsInitContext(t *testing.T) {
	d := New(Config{
		ServerBaseURL:  "https://api.example.test",
		WorkspacesRoot: t.TempDir(),
		DaemonID:       "daemon-initctx",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var sent terminalTaskReport
	d.terminalReportSend = func(_ context.Context, got terminalTaskReport, _ []time.Duration) error {
		sent = got
		return nil
	}
	d.reportTaskResult(context.Background(), "t1", TaskResult{Status: "completed", Comment: "ok", InitContext: sampleInitContext()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !reflect.DeepEqual(sent.initContext, sampleInitContext()) {
		t.Fatalf("forwarded initContext = %+v, want %+v", sent.initContext, sampleInitContext())
	}
}

// A report queued before a restart is replayed with its init context intact.
func TestTerminalReportStorePersistsInitContext(t *testing.T) {
	store := newTerminalReportStore(Config{WorkspacesRoot: t.TempDir(), ServerBaseURL: "https://api.example.test", DaemonID: "daemon-1"})
	report := terminalTaskReport{kind: terminalTaskReportComplete, taskID: "t1", output: "ok", initContext: sampleInitContext()}
	if err := store.enqueue(report); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	items, err := store.list()
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v (%d items)", err, len(items))
	}
	if !reflect.DeepEqual(items[0].report.initContext, sampleInitContext()) {
		t.Fatalf("replayed initContext = %+v, want %+v", items[0].report.initContext, sampleInitContext())
	}
}
