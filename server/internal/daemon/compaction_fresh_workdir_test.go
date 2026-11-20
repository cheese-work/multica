package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
)

func chatReuseTestTask(id string) Task {
	task := leaderReuseTestTask(id)
	task.IssueID, task.IsLeaderTask, task.ChatSessionID = "", false, "chat-dm"
	return task
}

// CHE-2915 (SEC-2): the server sends a compaction task with no prior session
// and no prior workdir. The daemon must then prepare a new workdir and start a
// fresh provider session instead of reusing the DM's. The DM follow-up in the
// same test is the control: with the pointer it reuses the workdir and resumes.
func TestRunTaskCompactionGetsFreshWorkdirAndSession(t *testing.T) {
	t.Parallel()

	d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()

	dm, err := d.runTask(context.Background(), chatReuseTestTask("task-dm-1"), "claude", 0, d.logger)
	if err != nil || dm.WorkDir == "" || dm.SessionID == "" {
		t.Fatalf("DM turn: %+v, %v", dm, err)
	}

	followUp := chatReuseTestTask("task-dm-2")
	followUp.PriorSessionID, followUp.PriorWorkDir = dm.SessionID, dm.WorkDir
	reused, err := d.runTask(context.Background(), followUp, "claude", 0, d.logger)
	if err != nil || !sameDir(t, reused.WorkDir, dm.WorkDir) {
		t.Fatalf("control: DM follow-up WorkDir = %q (%v), want reused %q", reused.WorkDir, err, dm.WorkDir)
	}

	compaction := chatReuseTestTask("task-compaction") // claim leaves both pointers empty
	got, err := d.runTask(context.Background(), compaction, "claude", 0, d.logger)
	if err != nil {
		t.Fatalf("compaction runTask: %v", err)
	}
	if got.WorkDir == "" || sameDir(t, got.WorkDir, dm.WorkDir) {
		t.Fatalf("compaction WorkDir = %q, want a new directory, not the DM's %q", got.WorkDir, dm.WorkDir)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	invocations := strings.Split(string(args), "--invocation-end--\n")
	if last := invocations[len(invocations)-2]; strings.Contains(last, "--resume") {
		t.Fatalf("compaction invocation resumed a provider session:\n%s", last)
	}
}
