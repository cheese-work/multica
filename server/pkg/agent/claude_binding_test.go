//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/runbinding"
)

// CHE-9773: Claude-runtime runs must get the same launcher-published binding
// as Codex-runtime runs.
func TestClaudeManagedBindingHandoff(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	observed := filepath.Join(t.TempDir(), "observed.json")
	fake := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		"IFS= read -r _\n" +
		`python3 -c 'import json,os; p=os.environ["MULTICA_RUN_BINDING_ID"]; r=json.load(open(os.environ["REGISTRY"]+"/"+p+".json")); assert r["pid"]==os.getppid(); assert r["task_id"]=="task-a"; assert r["principal"]=="agent-a"; json.dump(r,open(os.environ["OBSERVED"],"w"))'` + "\n" +
		`echo '{"type":"result","subtype":"success","result":"ok","session_id":"s"}'` + "\n"
	writeTestExecutable(t, fake, []byte(script))
	backend, err := New("claude", Config{
		ExecutablePath: fake,
		Logger:         slog.Default(),
		Env:            map[string]string{"IS_SANDBOX": "1", "REGISTRY": dir, "OBSERVED": observed, "MULTICA_RUN_BINDING_ID": "stale-value"},
		RunBinding:     &runbinding.Spec{Dir: dir, AgentID: "agent-a", TaskID: "task-a", IssueID: "CHE-9773"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "p", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result
	data, err := os.ReadFile(observed)
	if err != nil {
		t.Fatalf("claude child saw no usable binding: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record["binding_id"] == "stale-value" {
		t.Fatal("caller-supplied binding id reached the child")
	}
	if _, err := os.Stat(filepath.Join(dir, record["binding_id"].(string)+".json")); !os.IsNotExist(err) {
		t.Fatal("binding survived process lifecycle")
	}
}
