package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// CHE-4100: an output_only background task prompt holds only the bounded input
// from the task context. No issue id, no multica CLI instruction.
func TestBuildPromptBackgroundHoldsOnlyBoundedInput(t *testing.T) {
	task := Task{
		ID:              "t1",
		WorkspaceID:     "ws1",
		BackgroundJob:   "dm_title",
		BackgroundInput: "hello world",
		// A hostile or stale field must not reach the prompt.
		HandoffNote: "SECRET-HANDOFF",
		IssueID:     "issue-should-not-appear",
	}
	got := BuildPrompt(task, "claude")
	if !strings.Contains(got, "hello world") {
		t.Fatalf("prompt lacks the input:\n%s", got)
	}
	for _, banned := range []string{"multica ", "SECRET-HANDOFF", "issue-should-not-appear", "Your assigned issue"} {
		if strings.Contains(got, banned) {
			t.Fatalf("prompt must not contain %q:\n%s", banned, got)
		}
	}
	if !strings.Contains(strings.ToLower(got), "read-only") {
		t.Fatalf("prompt must carry the read-only brief:\n%s", got)
	}
}

func TestGCMetaForBackgroundTask(t *testing.T) {
	meta, ok := gcMetaForTask(Task{ID: "t1", WorkspaceID: "ws1", BackgroundJob: "dm_title"})
	if !ok || meta.Kind != execenv.GCKindBackground || meta.TaskID != "t1" {
		t.Fatalf("got %+v ok=%v", meta, ok)
	}
}

func TestDaemonAdvertisesBackgroundOutputOnly(t *testing.T) {
	for _, c := range daemonCommonCapabilities() {
		if c == "background_output_only_v1" {
			return
		}
	}
	t.Fatal("daemon must advertise background_output_only_v1")
}

func TestTaskRunTimeoutTightensBackgroundOnly(t *testing.T) {
	const def = 2 * time.Hour
	cases := []struct {
		name string
		task Task
		want time.Duration
	}{
		{"background tightens", Task{BackgroundJob: "dm_title", BackgroundRunSeconds: 30}, 30 * time.Second},
		{"background never loosens", Task{BackgroundJob: "dm_title", BackgroundRunSeconds: 99999}, def},
		{"ordinary task keeps default", Task{BackgroundRunSeconds: 30}, def},
	}
	for _, tc := range cases {
		if got := taskRunTimeout(def, tc.task); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	if got := taskRunTimeout(0, Task{BackgroundJob: "dm_title", BackgroundRunSeconds: 30}); got != 30*time.Second {
		t.Errorf("zero default: got %v", got)
	}
}

// CHE-4100 review P1: a member-attributed background task carries the
// originator's name and email on the claim. The generic per-turn blocks must
// not append them (or any other run context) to a background prompt.
func TestBuildPromptBackgroundOmitsPerTurnContext(t *testing.T) {
	task := Task{
		ID:              "t1",
		WorkspaceID:     "ws1",
		BackgroundJob:   "dm_title",
		BackgroundInput: "hello world",
		InitiatorName:   "Ada Lovelace",
		InitiatorEmail:  "ada@example.com",
		CheckpointBlock: "CHECKPOINT-BLOCK",
		WakeupJoined:    "JOINED-WAKEUP",
	}
	got := BuildPrompt(task, "claude")
	if !strings.Contains(got, "hello world") {
		t.Fatalf("prompt lacks the input:\n%s", got)
	}
	for _, banned := range []string{"Ada Lovelace", "ada@example.com", "CHECKPOINT-BLOCK", "JOINED-WAKEUP"} {
		if strings.Contains(got, banned) {
			t.Fatalf("background prompt must not contain %q:\n%s", banned, got)
		}
	}
}
