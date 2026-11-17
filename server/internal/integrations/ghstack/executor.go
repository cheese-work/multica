package ghstack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// API is the GitHub surface the executor reads and writes. *ghsnapshot.Client
// satisfies it.
type API interface {
	DefaultBranch(ctx context.Context, installationID int64, owner, repo string) (string, error)
	PullByNumber(ctx context.Context, installationID int64, owner, repo string, n int) (ghsnapshot.PullRef, error)
	PullsByHead(ctx context.Context, installationID int64, owner, repo, head string) ([]ghsnapshot.PullRef, error)
	StacksForPR(ctx context.Context, installationID int64, owner, repo string, pr int) ([]ghsnapshot.Stack, error)
	CreateStack(ctx context.Context, installationID int64, owner, repo string, prs []int) (ghsnapshot.Stack, error)
	AddToStack(ctx context.Context, installationID int64, owner, repo string, n int, prs []int) (ghsnapshot.Stack, error)
}

var _ API = (*ghsnapshot.Client)(nil)

// WorkspaceMode is one bound workspace's effective mode for the repo.
type WorkspaceMode struct {
	WorkspaceID string
	Mode        Mode
}

// Resolution is a fresh read of the mode: Final is rules 2-4 (lowest across
// bound workspaces, env kill), Workspaces feeds report targeting (rule 5).
type Resolution struct {
	Final      Mode
	Workspaces []WorkspaceMode
}

// ModeSource reads the settings anew on every call; the executor never caches.
type ModeSource interface {
	Resolve(ctx context.Context, installationID int64, owner, repo string) (Resolution, error)
}

// Outcome codes beyond the Decide report codes.
const (
	OutcomeCreated       Code = "created"
	OutcomeExtended      Code = "extended"
	OutcomeWouldCreate   Code = "would_create"
	OutcomeWouldExtend   Code = "would_extend"
	OutcomeConflict      Code = "conflict"
	OutcomeUnsupported   Code = "unsupported"
	OutcomeNoPermission  Code = "no_permission"
	OutcomeSchema        Code = "schema"
	OutcomeReadbackFails Code = "readback_failed"
)

// Job is one link job: PR N of owner/repo under an installation.
type Job struct {
	InstallationID int64
	Owner, Repo    string
	PR             int
}

// Outcome is what the Reporter records. Workspaces holds only workspaces whose
// own effective mode is report or link. BaseRef is N's base branch.
type Outcome struct {
	Job        Job
	BaseRef    string
	Code       Code
	Workspaces []WorkspaceMode
}

// Reporter records an outcome (activity entry on each issue linked to N).
// Implementations dedupe identical (PR, base ref, code) reports.
type Reporter interface {
	Report(ctx context.Context, o Outcome) error
}

// Executor runs link jobs, one per repo at a time.
type Executor struct {
	API      API
	Modes    ModeSource
	Reporter Reporter
	Metric   func(code Code) // optional
	Log      *slog.Logger    // optional

	flights flights
}

// flights is the per-repo single-flight: one job per (installation, owner,
// repo) runs at a time; a second waits and then reads fresh facts, so two
// layers opened together cannot create two stacks.
type flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	sem  chan struct{}
	refs int
}

func (f *flights) acquire(ctx context.Context, key string) (release func(), err error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*flight{}
	}
	fl := f.m[key]
	if fl == nil {
		fl = &flight{sem: make(chan struct{}, 1)}
		f.m[key] = fl
	}
	fl.refs++
	f.mu.Unlock()
	drop := func() {
		f.mu.Lock()
		if fl.refs--; fl.refs == 0 {
			delete(f.m, key)
		}
		f.mu.Unlock()
	}
	select {
	case fl.sem <- struct{}{}:
		return func() { <-fl.sem; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func (e *Executor) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// run state shared by the steps of one job.
type runState struct {
	j       Job
	baseRef string
	res     Resolution
}

// Run executes one job. Read, decide and write failures that are reports never
// return an error; only transport and rate-limit failures do, so the caller
// may retry.
func (e *Executor) Run(ctx context.Context, j Job) error {
	key := fmt.Sprintf("%d/%s/%s", j.InstallationID, strings.ToLower(j.Owner), strings.ToLower(j.Repo))
	release, err := e.flights.acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()

	st := &runState{j: j}
	if st.res, err = e.Modes.Resolve(ctx, j.InstallationID, j.Owner, j.Repo); err != nil {
		return err
	}
	if st.res.Final == ModeOff {
		return nil
	}
	for attempt := 0; ; attempt++ {
		f, err := e.readFacts(ctx, st)
		if err != nil {
			if code := readCode(err); code != "" {
				e.report(ctx, st, code)
				return nil
			}
			var se *ghsnapshot.StatusError
			if errors.As(err, &se) && se.Status == http.StatusNotFound && st.baseRef == "" {
				return nil // the PR is gone: nothing to link
			}
			return err
		}
		act := Decide(f)
		switch act.Kind {
		case None:
			return nil
		case Report:
			e.report(ctx, st, act.Code)
			return nil
		}
		// Re-read the settings immediately before the write (rule 6).
		if st.res, err = e.Modes.Resolve(ctx, j.InstallationID, j.Owner, j.Repo); err != nil {
			return err
		}
		if st.res.Final == ModeOff {
			return nil
		}
		if st.res.Final != ModeLink {
			e.report(ctx, st, map[Kind]Code{Create: OutcomeWouldCreate, Add: OutcomeWouldExtend}[act.Kind])
			return nil
		}
		werr := e.write(ctx, j, act)
		if errors.Is(werr, ghsnapshot.ErrStacksConflict) {
			if attempt == 0 {
				continue // re-read every fact and decide once more
			}
			e.report(ctx, st, OutcomeConflict)
			return nil
		}
		if werr != nil {
			if code := readCode(werr); code != "" {
				e.report(ctx, st, code)
				return nil
			}
			return werr
		}
		done := OutcomeCreated
		if act.Kind == Add {
			done = OutcomeExtended
		}
		ok, rerr := e.readBack(ctx, j, act.PRs)
		switch {
		case rerr != nil && readCode(rerr) == "":
			return rerr
		case rerr != nil || !ok:
			done = OutcomeReadbackFails
		}
		e.report(ctx, st, done)
		return nil
	}
}

func readCode(err error) Code {
	switch {
	case errors.Is(err, ghsnapshot.ErrStacksUnsupported):
		return OutcomeUnsupported
	case errors.Is(err, ghsnapshot.ErrStacksNoPermission):
		return OutcomeNoPermission
	case errors.Is(err, ghsnapshot.ErrStacksSchema):
		return OutcomeSchema
	}
	return ""
}

func (e *Executor) write(ctx context.Context, j Job, a Action) error {
	var err error
	if a.Kind == Create {
		_, err = e.API.CreateStack(ctx, j.InstallationID, j.Owner, j.Repo, a.PRs)
	} else {
		_, err = e.API.AddToStack(ctx, j.InstallationID, j.Owner, j.Repo, a.StackID, a.PRs)
	}
	return err
}

// readBack reports whether a stack now holds want, in order and adjacent.
func (e *Executor) readBack(ctx context.Context, j Job, want []int) (bool, error) {
	stacks, err := e.API.StacksForPR(ctx, j.InstallationID, j.Owner, j.Repo, j.PR)
	if err != nil {
		return false, err
	}
	for _, s := range stacks {
		var nums []int
		for _, p := range s.PullRequests {
			nums = append(nums, p.Number)
		}
		for i := 0; i+len(want) <= len(nums); i++ {
			match := true
			for k, w := range want {
				if nums[i+k] != w {
					match = false
					break
				}
			}
			if match {
				return true, nil
			}
		}
	}
	return false, nil
}

// report records an outcome: metric and log always, the Reporter only when a
// workspace's own effective mode is report or link (rule 5).
func (e *Executor) report(ctx context.Context, st *runState, code Code) {
	if e.Metric != nil {
		e.Metric(code)
	}
	e.log().Info("ghstack: outcome", "installation_id", st.j.InstallationID, "repo", st.j.Owner+"/"+st.j.Repo, "pr", st.j.PR, "base", st.baseRef, "code", string(code), "mode", string(st.res.Final))
	var eligible []WorkspaceMode
	for _, w := range st.res.Workspaces {
		if w.Mode == ModeReport || w.Mode == ModeLink {
			eligible = append(eligible, w)
		}
	}
	if len(eligible) == 0 || e.Reporter == nil {
		return
	}
	if err := e.Reporter.Report(ctx, Outcome{Job: st.j, BaseRef: st.baseRef, Code: code, Workspaces: eligible}); err != nil {
		e.log().Warn("ghstack: report failed", "pr", st.j.PR, "code", string(code), "err", err.Error())
	}
}

// facts collects what Decide reads, walking down from N one base at a time.
type facts struct {
	prs    map[int]PR
	full   map[int]bool // PR came from a pull read, not from a stack member stub
	stacks map[int]Stack
}

func (c *facts) addPR(p ghsnapshot.PullRef) {
	state := Open
	switch {
	case p.Merged:
		state = Merged
	case p.State != "open":
		state = Closed
	}
	c.prs[p.Number] = PR{Number: p.Number, Head: p.Head, Base: p.Base, State: state, Fork: p.Fork}
	c.full[p.Number] = true
}

func (e *Executor) addStacks(ctx context.Context, j Job, c *facts, pr int) (stacked bool, err error) {
	stacks, err := e.API.StacksForPR(ctx, j.InstallationID, j.Owner, j.Repo, pr)
	if err != nil {
		return false, err
	}
	for _, s := range stacks {
		var nums []int
		for _, m := range s.PullRequests {
			nums = append(nums, m.Number)
			if _, ok := c.prs[m.Number]; ok {
				continue
			}
			state := Open
			if m.MergedAt != "" {
				state = Merged
			} else if m.State != "open" {
				state = Closed
			}
			c.prs[m.Number] = PR{Number: m.Number, Head: m.HeadRef, State: state}
		}
		c.stacks[s.Number] = Stack{ID: s.Number, PRs: nums}
		stacked = true
	}
	return stacked, nil
}

func (e *Executor) readFacts(ctx context.Context, st *runState) (Facts, error) {
	j := st.j
	def, err := e.API.DefaultBranch(ctx, j.InstallationID, j.Owner, j.Repo)
	if err != nil {
		return Facts{}, err
	}
	n, err := e.API.PullByNumber(ctx, j.InstallationID, j.Owner, j.Repo, j.PR)
	if err != nil {
		return Facts{}, err
	}
	st.baseRef = n.Base
	c := &facts{prs: map[int]PR{}, full: map[int]bool{}, stacks: map[int]Stack{}}
	c.addPR(n)
	out := func() Facts {
		f := Facts{N: n.Number, Default: def}
		for _, p := range c.prs {
			f.PRs = append(f.PRs, p)
		}
		sort.Slice(f.PRs, func(a, b int) bool { return f.PRs[a].Number < f.PRs[b].Number })
		for _, s := range c.stacks {
			f.Stacks = append(f.Stacks, s)
		}
		sort.Slice(f.Stacks, func(a, b int) bool { return f.Stacks[a].ID < f.Stacks[b].ID })
		return f
	}
	if n.State != "open" || n.Merged || n.Fork || n.Base == def {
		return out(), nil
	}
	if _, err := e.addStacks(ctx, j, c, n.Number); err != nil {
		return Facts{}, err
	}
	seen := map[string]bool{n.Head: true}
	cur := n
	for depth := 0; depth <= maxAncestors+1 && cur.Base != def; depth++ {
		pulls, err := e.API.PullsByHead(ctx, j.InstallationID, j.Owner, j.Repo, cur.Base)
		if err != nil {
			return Facts{}, err
		}
		var open []ghsnapshot.PullRef
		for _, p := range pulls {
			c.addPR(p)
			if p.State == "open" && !p.Merged && !p.Fork {
				open = append(open, p)
			}
		}
		if len(open) != 1 || seen[open[0].Head] {
			break
		}
		a := open[0]
		seen[a.Head] = true
		if stacked, err := e.addStacks(ctx, j, c, a.Number); err != nil {
			return Facts{}, err
		} else if stacked {
			break
		}
		cur = a
	}
	return out(), nil
}
