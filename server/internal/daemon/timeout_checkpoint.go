package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/redact"
)

// Near-limit warning and timeout checkpoint (CHE-9762).
//
// A run that hits its wall-clock cap is killed with nothing durable written.
// Two daemon-side pieces close that gap without touching the task `error`
// text (the failure classifier and resume guards read it):
//
//  1. About MULTICA_AGENT_NEAR_LIMIT_WARNING before the cap, the daemon steers
//     one message into the live session asking the agent to push WIP and post
//     a checkpoint comment, then continue.
//  2. When the cap fires anyway, the daemon probes git state in the workdir and
//     sends it, with the last tool calls and the warning outcome, as the
//     optional `timeout_checkpoint` field on the fail callback.

const (
	// timeoutCheckpointActions is the ring buffer size.
	timeoutCheckpointActions = 8
	// timeoutCheckpointActionChars caps one recorded tool input.
	timeoutCheckpointActionChars = 160
	// timeoutCheckpointProbeBudget bounds the whole git probe.
	timeoutCheckpointProbeBudget = 10 * time.Second
	// timeoutCheckpointMaxRepos bounds the repositories probed.
	timeoutCheckpointMaxRepos = 5
	// nearLimitRetryInterval is how often an unready steer is retried.
	nearLimitRetryInterval = 15 * time.Second
	// nearLimitRetryMargin is the retry deadline before the cap (T-2 min).
	nearLimitRetryMargin = 2 * time.Minute

	warningStateDelivered    = "delivered"
	warningStateNotDelivered = "not_delivered"
	warningStateDisabled     = "disabled"
)

// nearLimitWarningText is the steer sent to the agent. It must not tell the
// agent to stop: the run continues until it finishes or the cap fires.
const nearLimitWarningText = `[RUN LIMIT WARNING] This run will be stopped by the platform's time cap in about %s.

Do this now, then CONTINUE the task:
1. Commit your work in progress and push it to your branch.
2. Post ONE checkpoint comment on the issue: what is done, what is not done, the branch, the head commit SHA, and the next step.
3. Go back to the task. Do not stop or hand off because of this message.`

// timeoutCheckpointAction is one recent tool call. Input is the value already
// redacted for the transcript, cut to timeoutCheckpointActionChars.
type timeoutCheckpointAction struct {
	Tool  string    `json:"tool"`
	Input string    `json:"input,omitempty"`
	At    time.Time `json:"at"`
}

// timeoutCheckpointRepo is the git state of one checkout. Remote URLs are
// never read: they can carry tokens.
type timeoutCheckpointRepo struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	// Ahead counts commits not yet on the upstream, or on any remote when the
	// branch has no upstream.
	Ahead int `json:"ahead"`
	Dirty int `json:"dirty"`
}

type timeoutCheckpointWarning struct {
	State  string    `json:"state"`
	At     time.Time `json:"at,omitzero"`
	Reason string    `json:"reason,omitempty"`
}

// timeoutCheckpoint is the optional `timeout_checkpoint` field of the fail
// callback. An old server ignores it; an old daemon never sends it.
type timeoutCheckpoint struct {
	Repos   []timeoutCheckpointRepo   `json:"repos"`
	Actions []timeoutCheckpointAction `json:"last_actions"`
	// ToolCalls is the total number of tool calls the run made, so the reader
	// can say "last 8 of N".
	ToolCalls int                      `json:"tool_calls"`
	Warning   timeoutCheckpointWarning `json:"warning"`
	// Probe is empty on success, else "failed <reason>".
	Probe string `json:"probe,omitempty"`
}

// runCheckpoint holds the per-run facts the timeout branch reports. All
// methods are safe on a nil receiver, so callers that never attached one
// (background jobs, unit tests) record nothing.
type runCheckpoint struct {
	mu      sync.Mutex
	actions []timeoutCheckpointAction
	total   int
	warning timeoutCheckpointWarning
	// warned latches once per run: a same-run fresh-session retry must not
	// warn a second time.
	warned bool
}

type runCheckpointKey struct{}

func newRunCheckpoint() *runCheckpoint {
	return &runCheckpoint{warning: timeoutCheckpointWarning{State: warningStateDisabled}}
}

func withRunCheckpoint(ctx context.Context, rc *runCheckpoint) context.Context {
	return context.WithValue(ctx, runCheckpointKey{}, rc)
}

func runCheckpointFromContext(ctx context.Context) *runCheckpoint {
	rc, _ := ctx.Value(runCheckpointKey{}).(*runCheckpoint)
	return rc
}

// recordToolCall keeps the last timeoutCheckpointActions calls. input must
// already be redacted (the value sent to the transcript).
func (rc *runCheckpoint) recordToolCall(tool string, input map[string]any, at time.Time) {
	if rc == nil {
		return
	}
	text := ""
	if len(input) > 0 {
		if raw, err := json.Marshal(input); err == nil {
			text = string(raw)
		}
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.total++
	rc.actions = append(rc.actions, timeoutCheckpointAction{Tool: tool, Input: cutRunes(text, timeoutCheckpointActionChars), At: at})
	if over := len(rc.actions) - timeoutCheckpointActions; over > 0 {
		rc.actions = append([]timeoutCheckpointAction(nil), rc.actions[over:]...)
	}
}

func (rc *runCheckpoint) setWarning(w timeoutCheckpointWarning) {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	rc.warning = w
	rc.mu.Unlock()
}

// claimWarning reports whether this call is the first to arm the warning.
func (rc *runCheckpoint) claimWarning() bool {
	if rc == nil {
		return false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.warned {
		return false
	}
	rc.warned = true
	return true
}

func (rc *runCheckpoint) snapshot() ([]timeoutCheckpointAction, int, timeoutCheckpointWarning) {
	if rc == nil {
		return nil, 0, timeoutCheckpointWarning{State: warningStateDisabled}
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]timeoutCheckpointAction(nil), rc.actions...), rc.total, rc.warning
}

func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// nearLimitWarningDue reports whether a run with this cap gets a warning:
// the knob is on and the cap is more than twice the warning, so the warning
// never lands in the first half of a run.
func nearLimitWarningDue(warning, runTimeout time.Duration) bool {
	return warning > 0 && runTimeout > 2*warning
}

// startNearLimitWarning arms the once-per-run warning for one backend
// session. It returns immediately; the goroutine stops with ctx.
func (d *Daemon) startNearLimitWarning(ctx context.Context, rc *runCheckpoint, session *agent.Session, runTimeout time.Duration, taskLog *slog.Logger) {
	warning := d.cfg.AgentNearLimitWarning
	if rc == nil || !nearLimitWarningDue(warning, runTimeout) {
		return
	}
	if session.Supplement == nil || session.SupplementReady == nil {
		rc.setWarning(timeoutCheckpointWarning{State: warningStateNotDelivered, Reason: "provider does not accept messages mid-run"})
		return
	}
	start := time.Now()
	fireAt := start.Add(runTimeout - warning)
	margin := min(nearLimitRetryMargin, warning/4)
	giveUpAt := start.Add(runTimeout - margin)
	interval := d.nearLimitRetryInterval
	if interval <= 0 {
		interval = nearLimitRetryInterval
	}
	go func() {
		if !sleepUntil(ctx, fireAt) || !rc.claimWarning() {
			return
		}
		text := fmt.Sprintf(nearLimitWarningText, warning.Round(time.Second))
		reason := "supplement not ready"
		for {
			if session.SupplementReady() {
				sendCtx, cancel := context.WithDeadline(ctx, giveUpAt)
				err := session.Supplement(sendCtx, text)
				cancel()
				if err == nil {
					rc.setWarning(timeoutCheckpointWarning{State: warningStateDelivered, At: time.Now().UTC()})
					taskLog.Info("near-limit warning delivered", "warning", warning.String(), "run_timeout", runTimeout.String())
					return
				}
				reason = redact.Text(cutRunes(err.Error(), 120))
				taskLog.Warn("near-limit warning steer refused", "reason", reason)
				// A refusal is not retried: the provider answered.
				rc.setWarning(timeoutCheckpointWarning{State: warningStateNotDelivered, Reason: reason})
				return
			}
			if !time.Now().Add(interval).Before(giveUpAt) {
				break
			}
			if !sleepUntil(ctx, time.Now().Add(interval)) {
				return
			}
		}
		rc.setWarning(timeoutCheckpointWarning{State: warningStateNotDelivered, Reason: reason})
		taskLog.Warn("near-limit warning not delivered", "reason", reason)
	}()
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// gitProbeFunc reads the git state of dir and its depth-1 git checkouts.
type gitProbeFunc func(ctx context.Context, dir string) ([]timeoutCheckpointRepo, error)

// buildTimeoutCheckpoint assembles the checkpoint for a timed-out run. The
// probe is bounded; a failed or timed-out probe still yields a checkpoint with
// the actions and the warning record, flagged `probe: failed <reason>`.
func (d *Daemon) buildTimeoutCheckpoint(ctx context.Context, rc *runCheckpoint, workDir string) *timeoutCheckpoint {
	actions, total, warning := rc.snapshot()
	cp := &timeoutCheckpoint{Repos: []timeoutCheckpointRepo{}, Actions: actions, ToolCalls: total, Warning: warning}
	if cp.Actions == nil {
		cp.Actions = []timeoutCheckpointAction{}
	}
	budget := d.timeoutProbeBudget
	if budget <= 0 {
		budget = timeoutCheckpointProbeBudget
	}
	probe := d.gitProbe
	if probe == nil {
		probe = probeGitRepos
	}
	// The run context is already done when the cap fired; the probe must not
	// inherit that.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	repos, err := probe(probeCtx, workDir)
	switch {
	case err == nil:
		cp.Repos = repos
	case errors.Is(err, context.DeadlineExceeded) || probeCtx.Err() != nil:
		cp.Probe = "failed timed out after " + budget.String()
	default:
		cp.Probe = "failed " + redact.Text(cutRunes(err.Error(), 120))
	}
	return cp
}

// probeGitRepos reports dir itself (when it is a checkout) and each depth-1
// subdirectory that is one, at most timeoutCheckpointMaxRepos.
func probeGitRepos(ctx context.Context, dir string) ([]timeoutCheckpointRepo, error) {
	if dir == "" {
		return nil, errors.New("no workdir")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	candidates := []string{}
	if isGitCheckout(dir) {
		candidates = append(candidates, dir)
	}
	for _, e := range entries {
		if len(candidates) >= timeoutCheckpointMaxRepos {
			break
		}
		if e.IsDir() && isGitCheckout(filepath.Join(dir, e.Name())) {
			candidates = append(candidates, filepath.Join(dir, e.Name()))
		}
	}
	repos := make([]timeoutCheckpointRepo, 0, len(candidates))
	for _, c := range candidates {
		repo, err := probeGitRepo(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(c), err)
		}
		rel := "."
		if c != dir {
			rel = filepath.Base(c)
		}
		repo.Path = cutRunes(rel, 120)
		repos = append(repos, repo)
	}
	return repos, nil
}

func isGitCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func probeGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(cutRunes(stderr.String(), 100)))
	}
	return strings.TrimSpace(out.String()), nil
}

func probeGitRepo(ctx context.Context, dir string) (timeoutCheckpointRepo, error) {
	var repo timeoutCheckpointRepo
	head, err := probeGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return repo, err
	}
	repo.Head = head
	branch, err := probeGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return repo, err
	}
	repo.Branch = branch
	status, err := probeGit(ctx, dir, "status", "--porcelain")
	if err != nil {
		return repo, err
	}
	if status != "" {
		repo.Dirty = strings.Count(status, "\n") + 1
	}
	// Ahead of the upstream when there is one; else ahead of every remote ref.
	rev := []string{"rev-list", "--count", "@{upstream}..HEAD"}
	if _, upErr := probeGit(ctx, dir, "rev-parse", "--verify", "--quiet", "@{upstream}"); upErr != nil {
		if ctx.Err() != nil {
			return repo, ctx.Err()
		}
		rev = []string{"rev-list", "--count", "HEAD", "--not", "--remotes"}
	}
	count, err := probeGit(ctx, dir, rev...)
	if err != nil {
		return repo, err
	}
	repo.Ahead, _ = strconv.Atoi(count)
	return repo, nil
}
