package testutil

import (
	"fmt"
	"os"
)

// ExitWithoutDatabase ends a DB-backed TestMain whose database is unreachable.
// In CI (CI=true or 1) it fails the binary so a no-DB run cannot report PASS
// while testing nothing; locally it skips with one loud line.
func ExitWithoutDatabase(err error) {
	if ci := os.Getenv("CI"); ci == "true" || ci == "1" {
		fmt.Fprintf(os.Stderr, "ERROR: integration suite did not run: database is required in CI: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "WARNING: integration suite did not run: database unavailable: %v\n", err)
	os.Exit(0)
}
