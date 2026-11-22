// Backfill_issue_created_via fills issue.created_via for issues made before the
// server classified it at create (CHE-6376 S2). It runs the same classifier as
// the create path (server/internal/issuecreatedvia) over rows whose value is
// NULL or `agent`, and writes only that column.
//
// Default is a dry run: it prints a JSON report and writes nothing.
//
//	backfill_issue_created_via                              dry run
//	backfill_issue_created_via --apply --receipt FILE       write, one receipt line per changed row
//	backfill_issue_created_via --revert FILE                write `old` back where the value still equals `new`
//
// See README.md for the exact docker exec commands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/issuecreatedvia/backfill"
	"github.com/multica-ai/multica/server/internal/logger"
)

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("backfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		apply     = flag.Bool("apply", false, "write the classified values (needs --receipt)")
		receipt   = flag.String("receipt", "", "receipt file for --apply; must not exist")
		revert    = flag.String("revert", "", "receipt file to revert")
		workspace = flag.String("workspace", "", "limit the run to one workspace id (default: all)")
		batchSize = flag.Int("batch-size", 500, "rows per commit")
	)
	flag.Parse()

	if *apply && *revert != "" {
		return errors.New("--apply and --revert are exclusive")
	}
	if *apply && *receipt == "" {
		return errors.New("--apply needs --receipt <path>")
	}
	if !*apply && *receipt != "" {
		return errors.New("--receipt only goes with --apply")
	}
	opts := backfill.Options{BatchSize: *batchSize}
	if *workspace != "" {
		if err := opts.WorkspaceID.Scan(*workspace); err != nil {
			return fmt.Errorf("--workspace: %w", err)
		}
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	var out any
	switch {
	case *revert != "":
		out, err = backfill.Revert(ctx, pool, opts, *revert)
	case *apply:
		out, err = backfill.Apply(ctx, pool, opts, *receipt)
	default:
		out, err = backfill.DryRun(ctx, pool, opts)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
