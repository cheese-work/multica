package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// codexResumeKeepsMultiAgentV2 reports whether resuming threadID would bring
// back Codex's multi-agent v2 lifecycle. The daemon's model-catalog override
// (CHE-778) only reaches NEW threads: thread/resume replays the thread's own
// persisted turn context, so a thread started before the override keeps
// multi_agent_version=v2 and its hidden shell tool on every resume. That is
// how long-lived Codex issue sessions kept answering "no terminal / no
// Multica CLI" with zero tool calls after the override shipped (CHE-702).
//
// It reads the last turn_context in the thread's rollout under codexHome.
// Unknown state (no home, no rollout, unreadable file) returns false, so the
// resume proceeds exactly as before.
func codexResumeKeepsMultiAgentV2(codexHome, threadID string) bool {
	if codexHome == "" || threadID == "" || codexMultiAgentOptIn() {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return false
	}
	for _, path := range matches {
		if v, ok := lastCodexMultiAgentVersion(path); ok && v == "v2" {
			return true
		}
	}
	return false
}

// codexMultiAgentOptIn mirrors execenv's MULTICA_CODEX_MULTI_AGENT escape
// hatch: an operator who opted into native multi-agent keeps v2 threads.
func codexMultiAgentOptIn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MULTICA_CODEX_MULTI_AGENT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func lastCodexMultiAgentVersion(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	marker := []byte(`"turn_context"`)
	var version string
	found := false
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, marker) {
			var entry struct {
				Type    string `json:"type"`
				Payload struct {
					MultiAgentVersion *string `json:"multi_agent_version"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &entry) == nil && entry.Type == "turn_context" {
				version, found = "", true
				if entry.Payload.MultiAgentVersion != nil {
					version = *entry.Payload.MultiAgentVersion
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", false
			}
			return version, found
		}
	}
}
