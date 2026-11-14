//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/runbinding"
)

func TestCodexManagedBindingHandoff(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	observed := filepath.Join(t.TempDir(), "observed.json")
	fake := writeFakeCodexAppServer(t, fmt.Sprintf(`
read line
python3 -c 'import json,os; p=os.environ["MULTICA_RUN_BINDING_ID"]; r=json.load(open(os.environ["REGISTRY"]+"/"+p+".json")); assert r["pid"]==os.getppid(); assert r["task_id"]=="task-a"; assert r["principal"]=="agent-a"; json.dump(r,open(%q,"w"))'
echo '{"id":1,"error":{"code":-1,"message":"fixture done"}}'
`, observed))
	b := &codexBackend{cfg: Config{ExecutablePath: fake, Logger: slog.Default(), Env: map[string]string{"REGISTRY": dir, "MULTICA_RUN_BINDING_ID": ""}, RunBinding: &runbinding.Spec{Dir: dir, AgentID: "agent-a", TaskID: "task-a", IssueID: "CHE-5868"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := b.executeOnce(ctx, "test", ExecOptions{}, 1)
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
		t.Fatalf("launcher handoff was not usable: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	// Result closes after lifecycle cleanup has revoked the reference.
	if _, err := os.Stat(filepath.Join(dir, record["binding_id"].(string)+".json")); !os.IsNotExist(err) {
		t.Fatal("binding survived process lifecycle")
	}
}
