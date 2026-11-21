package ciwatch

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/mergequeue"
)

const (
	maxQueueJobs     = 5    // failed jobs whose log a queue build reads
	maxQueueLogLines = 5000 // redacted lines kept per log: ponytail: the tail only, a longer log loses its earliest failures
)

var (
	logStamp   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z `)
	failedTest = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	failedPkg  = regexp.MustCompile(`^FAIL\s+(\S+)`)
)

// ParseFailingTests reads the failing tests, with their Go packages, from the
// redacted lines of a `go test -v` job log. `go test` prints a package's
// output whole, so a package's `FAIL <package>` line closes the `--- FAIL`
// lines before it. A failing test with no package line after it is dropped.
func ParseFailingTests(lines []string) []mergequeue.TestID {
	var out, pending []mergequeue.TestID
	for _, line := range lines {
		line = logStamp.ReplaceAllString(line, "")
		if m := failedTest.FindStringSubmatch(line); m != nil {
			pending = append(pending, mergequeue.TestID{Name: m[1]})
		} else if m := failedPkg.FindStringSubmatch(line); m != nil {
			for _, t := range pending {
				t.Package = m[1]
				out = append(out, t)
			}
			pending = nil
		}
	}
	return out
}

// QueueBuild collects the evidence of a failed merge_group run: its failed
// checks, the failing tests by package and name, and the log signature. It
// spends the installation's GitHub budget through c.Budget and returns the
// budget or GitHub error when it cannot finish, so the caller retries.
func (c *Collector) QueueBuild(ctx context.Context, run Run) (mergequeue.Collected, error) {
	var out mergequeue.Collected
	var persistErr, firstErr error
	end := c.stopper(ctx, run, func(string) {}, &persistErr)
	stop := func(err error) bool {
		if firstErr == nil {
			firstErr = err
		}
		end(err)
		return true
	}
	// ponytail: jobs past the third page of the job list are not read.
	failed, _, _ := c.failedJobs(ctx, run, func(string) {}, stop)
	if persistErr != nil {
		return out, persistErr
	}
	if firstErr != nil {
		return out, firstErr
	}
	var text, names []string
	names = append(names, run.WorkflowName)
	seen := map[mergequeue.TestID]bool{}
	for i, j := range failed {
		out.Checks = append(out.Checks, mergequeue.FailedCheck{Name: j.Name, Conclusion: j.Conclusion})
		if i >= maxQueueJobs {
			continue
		}
		lines, err := c.tail(ctx, run, j.ID, maxQueueLogLines)
		if errors.Is(err, errLogTooBig) || errors.Is(err, errRedact) || errors.Is(err, context.DeadlineExceeded) {
			continue // ponytail: a log too large or unredactable adds no test names; the check is still listed
		}
		if err != nil {
			end(err)
			if persistErr != nil {
				return out, persistErr
			}
			return out, err
		}
		for _, t := range ParseFailingTests(lines) {
			if !seen[t] {
				seen[t] = true
				out.Failing = append(out.Failing, t)
			}
		}
		if i < maxJobsKept {
			text = append(text, LimitTail(lines, maxExcerptByte))
			names = append(names, j.Name)
		}
	}
	out.Signature = Signature(strings.Join(text, "\n"), names...)
	return out, nil
}
