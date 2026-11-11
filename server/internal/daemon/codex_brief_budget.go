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
	own, parent := len(brief), 0
	if fi, err := os.Stat(filepath.Join(workDir, "AGENTS.md")); err == nil {
		own = int(fi.Size()) // includes any user-authored content already in the file
	}
	parent = codexParentInstructionBytes(workDir)
	limit := execenv.CodexProjectDocLimit(own + parent)
	receipt := execenv.NewBriefReceipt(brief, limit)
	receipt.ParentBytes = parent
	receipt.Truncated = parent+own > limit
	if receipt.Truncated {
		log.Error("codex instruction files exceed project_doc_max_bytes; the tail would be lost",
			"brief_bytes", receipt.TotalBytes, "parent_bytes", parent, "limit_bytes", limit)
	}
	log.Info("codex brief budget", "brief_bytes", receipt.TotalBytes, "parent_bytes", parent, "limit_bytes", limit, "sections", receipt.Sections)
	if data, err := json.MarshalIndent(receipt, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(rootDir, briefReceiptFile), data, 0o644); err != nil {
			log.Warn("write brief receipt failed", "error", err)
		}
	}
	return []string{"-c", fmt.Sprintf("project_doc_max_bytes=%d", limit)}
}

// codexParentInstructionBytes sums the instruction files Codex concatenates
// with workDir's AGENTS.md under one project_doc_max_bytes: one per directory
// from the project root (the nearest ancestor-or-self holding .git) down to,
// but not including, workDir. AGENTS.override.md wins over AGENTS.md in a
// directory. Without a .git ancestor Codex reads workDir alone.
func codexParentInstructionBytes(workDir string) int {
	var parents []string // nearest first
	for dir := workDir; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break // dir is the project root
		}
		next := filepath.Dir(dir)
		if next == dir {
			return 0 // no project root
		}
		parents = append(parents, next)
	}
	total := 0
	for _, dir := range parents {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
			if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
				total += int(fi.Size())
				break
			}
		}
	}
	return total
}
