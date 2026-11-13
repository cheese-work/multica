package ciwatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Evidence levels and the reasons a failure event is partial.
const (
	EvidenceFull    = "full"
	EvidencePartial = "partial"

	PartialBudget         = "budget"
	PartialBackoff        = "backoff"
	PartialRedactionError = "redaction_error"
	PartialLogTruncated   = "log_truncated"
	PartialNoJobs         = "no_jobs"
	PartialJobsTruncated  = "jobs_truncated" // more job pages than we read
	PartialFetchError     = "fetch_error"    // not in the plan's list: a non-limit GitHub failure
)

// Result kinds from Handle.
type Kind string

const (
	KindDropped   Kind = "dropped"    // step 1: nothing stored
	KindP0        Kind = "p0"         // metadata row only; Reason says why
	KindLabelOnly Kind = "label_only" // success: outcome labelling, no evidence
	KindEvidence  Kind = "evidence"   // Event holds the collected evidence
)

// P0 reasons and outcome labels.
const (
	ReasonOwnedElsewhere = "owned_elsewhere"
	ReasonSuperseded     = "superseded"
	ReasonCancelled      = "cancelled"
	LabelSelfHealed      = "self_healed"
)

// Config is the part of the per-repo watch config that routing steps 1 and 2 read.
type Config struct {
	Mode             string   // off, shadow, act
	Branches         []string // globs; empty = DefaultBranch only
	Events           []string
	WorkflowsInclude []string // empty = all
	WorkflowsExclude []string
}

// Run is a completed workflow_run as mirrored in github_workflow_run.
type Run struct {
	WorkspaceID    string
	InstallationID int64
	RepoOwner      string
	RepoName       string
	RunID          int64
	RunAttempt     int32
	WorkflowPath   string
	WorkflowName   string
	Event          string
	HeadBranch     string
	HeadSHA        string
	Conclusion     string
}

// Input is everything Handle needs; the caller reads it, Handle makes no DB call of its own.
type Input struct {
	FlagOn        bool // server flag ci_watcher
	DefaultBranch string
	Config        Config
	Run           Run
}

// Result is the routing decision for one completed run.
type Result struct {
	Kind   Kind
	Reason string        // P0 reason
	Label  string        // outcome label for earlier events
	Event  *FailureEvent // set when Kind == KindEvidence
}

// JobEvidence is one failed job. Excerpt is redacted text, empty when none was kept.
type JobEvidence struct {
	JobID   int64  `json:"job_id"`
	Name    string `json:"name"`
	Excerpt string `json:"excerpt"`
}

// FailureEvent is one stored failure event. Idempotency key: repo, run id, attempt.
type FailureEvent struct {
	WorkspaceID    string
	RepoOwner      string
	RepoName       string
	RunID          int64
	RunAttempt     int32
	WorkflowName   string
	Event          string
	HeadBranch     string
	HeadSHA        string
	Conclusion     string
	SignatureHash  string
	Evidence       string // EvidenceFull or EvidencePartial
	PartialReason  string
	FailedJobCount int
	Jobs           []JobEvidence
	RunnerLost     bool // a failed job's metadata shows runner loss; not stored, classified in memory
}

// Job is one entry of the jobs list.
type Job struct {
	ID          int64
	Name        string
	Conclusion  string
	CompletedAt time.Time
	RunnerLost  bool // job metadata shows the runner vanished mid-job
}

// RateLimitError is what GitHub answered when it asked us to back off. Header
// holds the throttling response's headers as sent (the reset is not clamped);
// RetryAfter is the client's clamped wait, used when Header is nil.
type RateLimitError struct {
	RetryAfter time.Duration
	Header     http.Header
}

func (e *RateLimitError) Error() string { return "github rate limited" }

// GitHub is the installation-scoped API surface the collector uses. The
// implementation must send no Range header and follow the log redirect.
type GitHub interface {
	// ListJobs returns one page (100 per page, filter=latest) of the run attempt's
	// jobs and the response headers (nil on error).
	ListJobs(ctx context.Context, run Run, page int) (jobs []Job, hasMore bool, h http.Header, err error)
	// JobLog streams the job log from byte 0, with GitHub's API response headers.
	JobLog(ctx context.Context, run Run, jobID int64) (io.ReadCloser, http.Header, error)
}

// Release settles one reserved call. Pass the response headers so the shared
// ledger sees what other consumers spent; nil when the call failed. An error
// means the settlement was not stored, so the ledger balance is stale: the
// caller must make no further call.
type Release func(h http.Header) error

// Budget is the shared installation ledger. CHE-4275 (S3) supplies the real one.
type Budget interface {
	// Reserve holds cost calls of budget; call release once the call returned.
	// ErrNoBudget means below the floor or unknown; ErrBackoff means a backoff
	// is active.
	Reserve(ctx context.Context, installationID int64, cost int) (release Release, err error)
	// Limited records a 403/429 answer so every repo on the installation backs
	// off. An error means the backoff was not stored: the caller must stop.
	Limited(ctx context.Context, installationID int64, e *RateLimitError) error
}

var (
	ErrNoBudget = errors.New("ciwatch: no budget")
	ErrBackoff  = errors.New("ciwatch: backing off")
)

// Runs answers the supersession questions from the run mirror, with no API call.
type Runs interface {
	// Newer reports whether a later run of the same workflow and branch exists,
	// and whether one of them already succeeded.
	Newer(ctx context.Context, run Run) (exists, succeeded bool, err error)
}
