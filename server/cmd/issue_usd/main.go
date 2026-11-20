// issue_usd prices agent runs and rolls them up to root issues (CHE-6535, S5a
// of CHE-6498). It has no database access: it reads the same JSON a person
// sees from `multica agent tasks <agent-id> --output json` (every agent, paged
// with --before) and `multica issue get` (the parent chain), prices each run
// with metrics.PriceForModelAlias, and prints
//
//   - USD per run (p50, p90),
//   - USD per root issue that reached a done-category status (children and
//     reruns rolled up),
//   - uncosted tokens (model has no price row),
//   - "TTL unknown" tokens: cache writes with no 1-hour/5-minute split, priced
//     at the 5-minute rate; over 5% in a week prints a FINDING line.
//
// Usage (on X99, with a logged-in multica CLI):
//
//	go run ./cmd/issue_usd --days 7 [--json]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

var beforeRe = regexp.MustCompile(`--before "([^"]+)"`)

type cli struct{ path string }

func (c cli) run(args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command(c.path, args...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		return nil, e.Bytes(), fmt.Errorf("%s %v: %w: %s", c.path, args, err, bytes.TrimSpace(e.Bytes()))
	}
	return o.Bytes(), e.Bytes(), nil
}

func (c cli) agentIDs() ([]string, error) {
	out, _, err := c.run("agent", "list", "--include-archived", "--output", "json")
	if err != nil {
		return nil, err
	}
	var agents []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.ID)
	}
	return ids, nil
}

func (c cli) runsPage(agentID, before string) ([]Run, string, error) {
	args := []string{"agent", "tasks", agentID, "--output", "json"}
	if before != "" {
		args = append(args, "--before", before)
	}
	out, errOut, err := c.run(args...)
	if err != nil {
		return nil, "", err
	}
	var runs []Run
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, "", fmt.Errorf("agent tasks %s: %w", agentID, err)
	}
	next := ""
	if m := beforeRe.FindSubmatch(errOut); m != nil {
		next = string(m[1])
	} else if m := beforeRe.FindSubmatch(out); m != nil {
		next = string(m[1])
	}
	return runs, next, nil
}

func (c cli) issue(id string) (Issue, error) {
	out, _, err := c.run("issue", "get", id, "--output", "json")
	if err != nil {
		return Issue{}, err
	}
	var is Issue
	if err := json.Unmarshal(out, &is); err != nil {
		return Issue{}, fmt.Errorf("issue get %s: %w", id, err)
	}
	return is, nil
}

// parallel runs fn over items with at most n workers.
func parallel[T any](items []T, n int, fn func(T)) {
	var wg sync.WaitGroup
	ch := make(chan T)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				fn(it)
			}
		}()
	}
	for _, it := range items {
		ch <- it
	}
	close(ch)
	wg.Wait()
}

func main() {
	days := flag.Int("days", 7, "window length in days, counted back from now")
	asJSON := flag.Bool("json", false, "print the report as JSON")
	workers := flag.Int("workers", 6, "parallel CLI calls")
	flag.Parse()
	if err := mainErr(*days, *asJSON, *workers); err != nil {
		fmt.Fprintln(os.Stderr, "issue_usd:", err)
		os.Exit(1)
	}
}

func mainErr(days int, asJSON bool, workers int) error {
	c := cli{path: os.Getenv("MULTICA_CLI_PATH")}
	if c.path == "" {
		c.path = "multica"
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	agents, err := c.agentIDs()
	if err != nil {
		return err
	}
	var (
		mu       sync.Mutex
		runs     []Run
		failures []error
	)
	parallel(agents, workers, func(id string) {
		got, err := fetchRuns(func(before string) ([]Run, string, error) { return c.runsPage(id, before) }, cutoff)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures = append(failures, err)
			return
		}
		runs = append(runs, got...)
	})

	// Walk the parent chain breadth-first; a failed lookup leaves the issue out
	// of the map, so its runs land in "unknown root" rather than a wrong root.
	issues := map[string]Issue{}
	tried := map[string]bool{}
	var frontier []string
	for _, r := range runs {
		if r.IssueID != "" && !tried[r.IssueID] {
			tried[r.IssueID] = true
			frontier = append(frontier, r.IssueID)
		}
	}
	for len(frontier) > 0 {
		var next []string
		parallel(frontier, workers, func(id string) {
			is, err := c.issue(id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			is.ID = id
			issues[id] = is
			if is.ParentID != "" && !tried[is.ParentID] {
				tried[is.ParentID] = true
				next = append(next, is.ParentID)
			}
		})
		frontier = next
	}

	rep := Aggregate(runs, issues)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Printf("Window: last %d days (since %s), %d agents, %d runs\n", days, cutoff.UTC().Format(time.RFC3339), len(agents), len(runs))
		fmt.Print(Render(rep))
	}
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "issue_usd: %d CLI calls failed (their runs are in unknown root or missing); first: %v\n", len(failures), failures[0])
	}
	return nil
}
