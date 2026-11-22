package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/guard"
)

func TestSetGuardLogPointsAtTaskTempDir(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{}
	path := setGuardLog(env, tmp)
	if env[guard.LogEnv] != path || filepath.Dir(path) != tmp {
		t.Fatalf("log env %q path %q, want a file in %q", env[guard.LogEnv], path, tmp)
	}
}

func TestReadGuardDecisionsKeepsLinePositionAndSkipsBadLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.jsonl")
	body := `{"time":"2026-11-14T10:00:00Z","plugin":"pr-title-tag","action":"fixed","tool":"gh","subcommand":"pr create","line":"tag added","issue":"CHE-1"}
not json
{"time":"2026-11-14T10:01:00Z","plugin":"ci-wait","action":"refused","tool":"gh","subcommand":"pr checks","line":"refused"}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readGuardDecisions(p)
	if len(got) != 2 || got[0].Seq != 0 || got[1].Seq != 2 || got[0].Issue != "CHE-1" || got[1].Plugin != "ci-wait" {
		t.Fatalf("decisions = %+v, want 2 rows at seq 0 and 2", got)
	}
	if readGuardDecisions(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("a missing log must read as no decisions")
	}
}

func TestReadGuardDecisionsCapsAtUploadLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.jsonl")
	line := `{"time":"2026-11-14T10:00:00Z","plugin":"x","action":"pass"}` + "\n"
	var b []byte
	for i := 0; i < guardDecisionUploadCap+50; i++ {
		b = append(b, line...)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if n := len(readGuardDecisions(p)); n != guardDecisionUploadCap {
		t.Fatalf("read %d decisions, want the first %d", n, guardDecisionUploadCap)
	}
}
