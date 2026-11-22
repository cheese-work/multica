package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/multica-ai/multica/server/internal/guard"
)

// The guard appends one JSON line per decision to a file in the task temp dir
// (guard.LogEnv). When the run ends the daemon uploads the first
// guardDecisionUploadCap lines, so the Guard settings page and `multica guard
// log|stats` can count them (CHE-5207). A failed upload only costs the counters.

const guardDecisionUploadCap = 500

// guardDecisionUpload is one decision as the server stores it. Seq is the line
// position in the log, so a retried upload cannot count a decision twice.
type guardDecisionUpload struct {
	Seq        int    `json:"seq"`
	Time       string `json:"time"`
	Plugin     string `json:"plugin"`
	Action     string `json:"action"`
	Tool       string `json:"tool"`
	Subcommand string `json:"subcommand"`
	Line       string `json:"line"`
	Issue      string `json:"issue"`
	Note       string `json:"note"`
}

// setGuardLog points the guard's decision log at a file in dir and returns it.
func setGuardLog(env map[string]string, dir string) string {
	path := filepath.Join(dir, "guard-decisions.jsonl")
	env[guard.LogEnv] = path
	return path
}

// readGuardDecisions reads the decision log; a missing or unreadable file and
// a malformed line read as no decision.
func readGuardDecisions(path string) []guardDecisionUpload {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []guardDecisionUpload
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for seq := 0; seq < guardDecisionUploadCap && sc.Scan(); seq++ {
		var d guardDecisionUpload
		if json.Unmarshal(sc.Bytes(), &d) != nil || d.Plugin == "" {
			continue
		}
		d.Seq = seq
		out = append(out, d)
	}
	return out
}

func (c *Client) ReportGuardDecisions(ctx context.Context, taskID string, decisions []guardDecisionUpload) error {
	if len(decisions) == 0 {
		return nil
	}
	return c.postJSON(ctx, "/api/daemon/tasks/"+taskID+"/guard-decisions", map[string]any{"decisions": decisions}, nil)
}

func (d *Daemon) uploadGuardDecisions(ctx context.Context, taskID, path string, log *slog.Logger) {
	decisions := readGuardDecisions(path)
	if len(decisions) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := d.client.ReportGuardDecisions(ctx, taskID, decisions); err != nil {
		log.Warn("guard decisions not uploaded", "count", len(decisions), "error", err)
	}
}
