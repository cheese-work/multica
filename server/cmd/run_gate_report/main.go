// run_gate_report is a read-only CLI for CHE-1919 (CHE-1917 Slice 0b): it joins
// the run gate shadow ledger (run_gate_event, CHE-1918) to the runs, receipts and
// run outcomes they led to, and prints capture completeness by source with the
// offline 2-, 5- and 10-minute coalescing counterfactuals.
//
// Every figure is a measurement. Nothing here admits, folds, delays or skips a
// run, and a counterfactual is never a delivery receipt. See
// docs/run-gate-ledger.md.
//
// Usage:
//
//	go run ./cmd/run_gate_report --workspace <uuid> --since 168h [--json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("run gate report failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	workspace := flag.String("workspace", "", "workspace id (required)")
	since := flag.Duration("since", 24*time.Hour, "how far back to report, e.g. 24h, 168h")
	asJSON := flag.Bool("json", false, "print the full report, including every joined event, as JSON")
	flag.Parse()
	workspaceID, err := util.ParseUUID(*workspace)
	if err != nil {
		return fmt.Errorf("--workspace must be a workspace id: %w", err)
	}
	if *since <= 0 {
		return fmt.Errorf("--since must be positive, got %s", *since)
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

	now := time.Now().UTC()
	report, err := service.BuildRunGateReport(ctx, db.New(pool), workspaceID, now.Add(-*since), now, now)
	if err != nil {
		return fmt.Errorf("build report: %w", err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printReport(report)
	return nil
}

// ict renders a time for a person: ICT (UTC+7), suffixed.
func ict(t time.Time) string {
	return t.In(time.FixedZone("ICT", 7*3600)).Format("2006-01-02 15:04") + " ICT"
}

func printReport(r service.RunGateReport) {
	fmt.Printf("Run gate report (measurement only): %s to %s\n", ict(r.Since), ict(r.Until))
	if r.LedgerFirstEventAt != nil {
		fmt.Printf("  Ledger first event: %s (earlier windows predate capture)\n", ict(*r.LedgerFirstEventAt))
	} else {
		fmt.Println("  Ledger first event: none")
	}
	for _, s := range r.Sources {
		fmt.Printf("\n%s: %d events (%d observations), %d native-fold eligible\n", s.Source, s.Captured, s.Observations, s.NativeFoldEligible)
		p := s.Producer
		fmt.Printf("  Capture completeness: %d of %d surviving producer rows matched, %d missing", p.Captured, p.Evidenced, p.Missing)
		if !p.DenominatorComplete {
			fmt.Printf(" (denominator INCOMPLETE, a floor not the producer volume: %s)", strings.Join(p.Limits, ", "))
		}
		if p.LedgerRuleGone > 0 {
			fmt.Printf("; %d ledger events whose rule is deleted (their receipts are not counted)", p.LedgerRuleGone)
		}
		if p.LedgerWithoutReceipt > 0 {
			fmt.Printf("; %d ledger events wrote no receipt", p.LedgerWithoutReceipt)
		}
		fmt.Println()
		printCounts("  Admission", s.Admission)
		printCounts("  Disposition", s.Dispositions)
		for _, w := range s.Windows {
			fmt.Printf("  %3d min window: %d open, %d folded (native-eligible %d, producer-coalesced %d, run acted %d, unknown %d), protected %v\n",
				w.WindowSeconds/60, w.Opens, w.Folded, w.FoldedNativeEligible, w.FoldedAdmissionCoalesced, w.FoldedWithMutation, w.FoldedUnknown, w.Protected)
		}
	}
}

func printCounts(label string, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%s:", label)
	for _, k := range keys {
		fmt.Printf(" %s=%d", k, counts[k])
	}
	fmt.Println()
}
