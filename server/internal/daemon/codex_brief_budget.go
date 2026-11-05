package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

const briefReceiptFile = "brief_receipt.json"

// codexBriefBudget raises Codex's project_doc_max_bytes to cover the AGENTS.md
// just written, so Codex does not silently drop the brief tail (CHE-3687). It
// returns the `-c` args to append to the launch and writes the section-size
// receipt to rootDir. A brief the limit still cannot hold is logged as an error.
func codexBriefBudget(rootDir, workDir, brief string, log *slog.Logger) []string {
	size := len(brief)
	if fi, err := os.Stat(filepath.Join(workDir, "AGENTS.md")); err == nil {
		size = int(fi.Size()) // includes any user-authored content already in the file
	}
	limit := execenv.CodexProjectDocLimit(size)
	receipt := execenv.NewBriefReceipt(brief, limit)
	if receipt.Truncated {
		log.Error("codex brief exceeds project_doc_max_bytes; the tail would be lost",
			"brief_bytes", receipt.TotalBytes, "limit_bytes", limit)
	}
	log.Info("codex brief budget", "brief_bytes", receipt.TotalBytes, "limit_bytes", limit, "sections", receipt.Sections)
	if data, err := json.MarshalIndent(receipt, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(rootDir, briefReceiptFile), data, 0o644); err != nil {
			log.Warn("write brief receipt failed", "error", err)
		}
	}
	return []string{"-c", fmt.Sprintf("project_doc_max_bytes=%d", limit)}
}
