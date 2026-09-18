// protocol_lint_report is a minimal, read-only CLI for CHE-552: it prints the
// real protocol-omission base rate — total completing turns checked by
// protocollint.Check (CHE-529's runProtocolLint, server/internal/handler/
// daemon.go), how many were completely clean, and a violation-code breakdown
// — over a trailing time window, read from protocol_lint_run.
//
// This is the only server-side surface for the report: cmd/multica is a pure
// HTTP client of the daemon API with no direct database access, and no
// existing "admin report" HTTP endpoint exists to extend without adding new
// server surface area the task explicitly scoped out. Following the
// established direct-DB standalone-binary pattern in this package (see
// cmd/backfill_task_usage_hourly, cmd/backfill_issue_last_activity) keeps
// this read-only and minimal instead.
//
// Usage:
//
//	go run ./cmd/protocol_lint_report --since 720h
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("protocol lint report failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	since := flag.Duration("since", 24*time.Hour, "how far back to report, e.g. 24h, 720h")
	flag.Parse()
	if *since <= 0 {
		return fmt.Errorf("--since must be positive, got %s", since)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	until := time.Now().UTC()
	from := until.Add(-*since)

	report, err := handler.BuildProtocolLintOmissionReport(ctx, db.New(pool), from, until)
	if err != nil {
		return fmt.Errorf("build report: %w", err)
	}

	printReport(report)
	return nil
}

func printReport(r handler.ProtocolLintOmissionReport) {
	fmt.Printf("Protocol lint omission report: %s to %s\n", r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339))
	fmt.Printf("  Turns checked:        %d\n", r.TotalChecked)
	fmt.Printf("  Zero-violation turns: %d (%.2f%%)\n", r.ZeroViolationCount, r.ZeroViolationPercent)
	if len(r.ViolationCodeCounts) == 0 {
		fmt.Println("  Violation codes:      none")
		return
	}
	fmt.Println("  Violation codes:")
	for _, c := range r.ViolationCodeCounts {
		fmt.Printf("    %-32s %d\n", c.Code, c.Occurrences)
	}
}
