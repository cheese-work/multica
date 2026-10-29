package migrations

import (
	"strings"
	"testing"
)

func TestF1bQueueProbeAIsNoOp(test *testing.T) {
	for _, direction := range []string{"up", "down"} {
		filename := "784_f1b_queue_probe_a." + direction + ".sql"
		statement := strings.TrimSpace(readMigrationFile(test, filename))
		if statement != "SELECT 1;" {
			test.Fatalf("migration %s must be the no-op SELECT 1; got %q", filename, statement)
		}
	}
}
