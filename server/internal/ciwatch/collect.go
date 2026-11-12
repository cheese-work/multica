package ciwatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"
)

// Plan limits (CHE-4265). Tests lower them through Collector fields.
const (
	maxJobPages    = 3
	maxJobsKept    = 2
	maxExcerptLine = 200
	maxExcerptByte = 8 << 10
	defLogBytes    = 16 << 20
	defLogTimeout  = 30 * time.Second
)

// Collector applies routing steps 1-4 and collects redacted evidence.
type Collector struct {
	GH     GitHub
	Budget Budget
	Runs   Runs
	Log    *slog.Logger // nil = slog.Default()

	// Zero values mean the plan's limits.
	LogTimeout  time.Duration
	MaxLogBytes int64
	// Redact is RedactTail unless a test replaces it.
	Redact func(io.Reader, int) ([]string, error)
}

var errLogTooBig = errors.New("ciwatch: log over byte limit")

// capReader fails once more than max bytes were read, so a 50 MB log is never drained.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errLogTooBig
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errLogTooBig
	}
	return n, err
}

// route runs steps 1-3 (and the cancelled half of 4). done=false means collect evidence.
func (c *Collector) route(ctx context.Context, in Input) (res Result, done bool, err error) {
	run, cfg := in.Run, in.Config
	// Step 1: gate.
	branches := cfg.Branches
	if len(branches) == 0 {
		branches = []string{in.DefaultBranch}
	}
	if !in.FlagOn || cfg.Mode == "" || cfg.Mode == "off" || strings.HasPrefix(run.Event, "pull_request") ||
		!slices.ContainsFunc(branches, func(g string) bool { ok, _ := path.Match(g, run.HeadBranch); return ok }) {
		return Result{Kind: KindDropped}, true, nil
	}
	// Step 2: owned elsewhere. Other owners (deploy, freshness watch, merge queue) alert themselves.
	base := strings.TrimSuffix(path.Base(run.WorkflowPath), path.Ext(run.WorkflowPath))
	listed := func(l []string) bool {
		return slices.ContainsFunc(l, func(s string) bool {
			return s == run.WorkflowName || s == run.WorkflowPath || s == path.Base(run.WorkflowPath) || s == base
		})
	}
	if run.Event == "merge_group" || listed(cfg.WorkflowsExclude) || strings.HasPrefix(base, "cd-deploy") || strings.HasPrefix(base, "cd-freshness-watch") {
		return Result{Kind: KindP0, Reason: ReasonOwnedElsewhere}, true, nil
	}
	if !slices.Contains(cfg.Events, run.Event) || (len(cfg.WorkflowsInclude) > 0 && !listed(cfg.WorkflowsInclude)) {
		return Result{Kind: KindDropped}, true, nil
	}
	// Step 3: conclusion.
	switch run.Conclusion {
	case "success":
		return Result{Kind: KindLabelOnly}, true, nil
	case "cancelled":
		exists, _, err := c.Runs.Newer(ctx, run)
		if err != nil {
			return Result{}, true, err
		}
		if exists {
			return Result{Kind: KindP0, Reason: ReasonSuperseded}, true, nil
		}
		return Result{Kind: KindP0, Reason: ReasonCancelled}, true, nil
	case "failure", "timed_out", "startup_failure":
	default:
		return Result{Kind: KindDropped}, true, nil
	}
	// Step 4: a newer green run already healed it.
	exists, ok, err := c.Runs.Newer(ctx, run)
	if err != nil {
		return Result{}, true, err
	}
	if exists && ok {
		return Result{Kind: KindP0, Reason: ReasonSuperseded, Label: LabelSelfHealed}, true, nil
	}
	return Result{}, false, nil
}

// Handle routes one completed run and, for a failure that is ours, collects evidence.
// Only redacted text leaves it; the server log gets ids and the signature hash.
func (c *Collector) Handle(ctx context.Context, in Input) (Result, error) {
	if res, done, err := c.route(ctx, in); done {
		return res, err
	}
	run := in.Run
	ev := &FailureEvent{
		WorkspaceID: run.WorkspaceID, RepoOwner: run.RepoOwner, RepoName: run.RepoName,
		RunID: run.RunID, RunAttempt: run.RunAttempt, WorkflowName: run.WorkflowName,
		Event: run.Event, HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA,
		Conclusion: run.Conclusion, Evidence: EvidenceFull,
	}
	partial := func(reason string) {
		ev.Evidence = EvidencePartial
		if ev.PartialReason == "" {
			ev.PartialReason = reason
		}
	}
	// stop reports whether err ends all further API calls, recording why.
	stop := func(err error) bool {
		var rl *RateLimitError
		switch {
		case errors.Is(err, ErrNoBudget):
			partial(PartialBudget)
		case errors.Is(err, ErrBackoff):
			partial(PartialBackoff)
		case errors.As(err, &rl):
			c.Budget.Limited(ctx, run.InstallationID, rl)
			partial(PartialBackoff)
		default:
			return false
		}
		return true
	}

	jobs, ok, truncated := c.failedJobs(ctx, run, partial, stop)
	ev.FailedJobCount = len(jobs)
	if ok && len(jobs) == 0 {
		partial(PartialNoJobs)
	}
	for _, j := range jobs[:min(len(jobs), maxJobsKept)] {
		ev.Jobs = append(ev.Jobs, JobEvidence{JobID: j.ID, Name: j.Name})
	}
	for i := range ev.Jobs {
		if ev.Evidence == EvidencePartial && ev.PartialReason != PartialLogTruncated && ev.PartialReason != PartialRedactionError {
			break // budget/backoff/fetch ended collection
		}
		lines, err := c.tail(ctx, run, ev.Jobs[i].JobID)
		switch {
		case err == nil:
			ev.Jobs[i].Excerpt = LimitTail(lines, maxExcerptByte)
		case stop(err):
		case errors.Is(err, errLogTooBig), errors.Is(err, context.DeadlineExceeded):
			partial(PartialLogTruncated)
		case errors.Is(err, errRedact):
			partial(PartialRedactionError)
		default:
			partial(PartialFetchError)
		}
	}

	if truncated {
		// Set after the log loop: it must not end log collection.
		partial(PartialJobsTruncated)
	}

	var text []string
	names := []string{run.WorkflowName}
	for _, j := range ev.Jobs {
		text = append(text, j.Excerpt)
		names = append(names, j.Name)
	}
	ev.SignatureHash = Signature(strings.Join(text, "\n"), names...)

	if c.Log != nil {
		c.Log.Info("ci evidence collected", "run_id", run.RunID, "attempt", run.RunAttempt,
			"signature", ev.SignatureHash, "evidence", ev.Evidence, "partial_reason", ev.PartialReason)
	}
	return Result{Kind: KindEvidence, Event: ev}, nil
}

// failedJobs pages the jobs list (3 pages max) and returns failed jobs, oldest completion first.
// ok is false when the listing itself failed; truncated is true when GitHub
// still had more pages after the last one read, so the count may be low.
func (c *Collector) failedJobs(ctx context.Context, run Run, partial func(string), stop func(error) bool) (failed []Job, ok, truncated bool) {
	ok = true
	for page := 1; page <= maxJobPages; page++ {
		release, err := c.Budget.Reserve(ctx, run.InstallationID, 1)
		more := false
		if err == nil {
			var js []Job
			js, more, err = c.GH.ListJobs(ctx, run, page)
			release()
			for _, j := range js {
				if j.Conclusion == "failure" || j.Conclusion == "timed_out" {
					failed = append(failed, j)
				}
			}
		}
		if err != nil {
			if !stop(err) {
				partial(PartialFetchError)
			}
			ok = false
			break
		}
		if !more {
			break
		}
		truncated = page == maxJobPages
	}
	slices.SortFunc(failed, func(a, b Job) int {
		if d := a.CompletedAt.Compare(b.CompletedAt); d != 0 {
			return d
		}
		return int(a.ID - b.ID)
	})
	return failed, ok, truncated
}

var errRedact = errors.New("ciwatch: redaction failed")

// tail downloads one job log under the byte and time limits and returns its redacted tail.
func (c *Collector) tail(ctx context.Context, run Run, jobID int64) ([]string, error) {
	timeout, limit, redact := c.LogTimeout, c.MaxLogBytes, c.Redact
	if timeout == 0 {
		timeout = defLogTimeout
	}
	if limit == 0 {
		limit = defLogBytes
	}
	if redact == nil {
		redact = RedactTail
	}
	release, err := c.Budget.Reserve(ctx, run.InstallationID, 1)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := c.GH.JobLog(ctx, run, jobID)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	lines, err := redact(&capReader{r: body, left: limit}, maxExcerptLine)
	switch {
	case err == nil:
		return lines, nil
	case errors.Is(err, errLogTooBig), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return nil, err
	default:
		return nil, errRedact // fail closed: the raw error text could carry log content
	}
}
