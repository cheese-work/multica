package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestCodexBriefBudgetCoversBriefAndWritesReceipt(t *testing.T) {
	root, work := t.TempDir(), t.TempDir()
	brief := "intro\n## A\n" + strings.Repeat("x", 40*1024)
	if err := os.WriteFile(filepath.Join(work, "AGENTS.md"), []byte(brief), 0o644); err != nil {
		t.Fatal(err)
	}
	args := codexBriefBudget(root, work, brief, slog.Default())
	want := fmt.Sprintf("project_doc_max_bytes=%d", execenv.CodexProjectDocLimit(len(brief)))
	if len(args) != 2 || args[0] != "-c" || args[1] != want {
		t.Fatalf("args = %q, want -c %s", args, want)
	}
	data, err := os.ReadFile(filepath.Join(root, briefReceiptFile))
	if err != nil || !strings.Contains(string(data), `"truncated": false`) || !strings.Contains(string(data), `"name": "A"`) {
		t.Fatalf("receipt = %q, err = %v", data, err)
	}
}

// Codex concatenates AGENTS.md from the project root (the .git directory) down
// to the working directory under one cap. A parent file larger than any fixed
// allowance must still leave the whole brief inside the limit.
func TestCodexBriefBudgetCountsParentInstructionFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	const parentBytes = 40 * 1024
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(strings.Repeat("p", parentBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(repo, "task")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	brief := "intro\n## A\n" + strings.Repeat("x", 40*1024)
	if err := os.WriteFile(filepath.Join(work, "AGENTS.md"), []byte(brief), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	args := codexBriefBudget(root, work, brief, slog.Default())
	var limit int
	if _, err := fmt.Sscanf(args[1], "project_doc_max_bytes=%d", &limit); err != nil {
		t.Fatalf("args = %q: %v", args, err)
	}
	if limit < parentBytes+len(brief) {
		t.Fatalf("limit %d cannot hold parent %d + brief %d bytes", limit, parentBytes, len(brief))
	}
	data, err := os.ReadFile(filepath.Join(root, briefReceiptFile))
	if err != nil || !strings.Contains(string(data), fmt.Sprintf(`"parent_bytes": %d`, parentBytes)) {
		t.Fatalf("receipt = %q, err = %v", data, err)
	}
}
