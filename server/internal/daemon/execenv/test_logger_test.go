package execenv

import (
	"bytes"
	"log/slog"
	"testing"
)

// The codex sandbox WARN fires once per Prepare/Reuse in dozens of tests.
// Parallel test output can split its timestamp off, and warning-budget then
// counts the stray line as a new CI warning (CHE-4202).
func TestTestLoggerDropsWarnButKeepsError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	testLogger().Warn("codex sandbox: running unsandboxed with danger-full-access")
	if buf.Len() != 0 {
		t.Fatalf("testLogger wrote a WARN: %q", buf.String())
	}
	testLogger().Error("boom")
	if !bytes.Contains(buf.Bytes(), []byte("boom")) {
		t.Fatalf("testLogger dropped an ERROR: %q", buf.String())
	}
}
