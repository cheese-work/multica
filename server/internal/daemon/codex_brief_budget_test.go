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
