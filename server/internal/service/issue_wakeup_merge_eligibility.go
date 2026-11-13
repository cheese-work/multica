package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MergeFacts mirrors the facts file of scripts/agent_merge_eligibility.py in
// cheese-work/multica-dotfiles. A pointer or nil slice means the fact is absent.
type MergeFacts struct {
	Tier              *int      `json:"tier"`
	ClassifierTrusted *bool     `json:"classifier_trusted"`
	CoverageComplete  *bool     `json:"coverage_complete"`
	PRBound           *bool     `json:"pr_bound"`
	ChecksGreen       *bool     `json:"checks_green"`
	SecurityGate      string    `json:"security_gate"`
	CrossLabPass      *bool     `json:"cross_lab_pass"`
	Paused            *bool     `json:"paused"`
	MainRedPaused     *bool     `json:"main_red_paused"`
	MergerIsAuthor    *bool     `json:"merger_is_author"`
	Paths             []string  `json:"paths"`
	Labels            *[]string `json:"labels"`
	ScopeClassified   *bool     `json:"scope_classified"`
	GateVetoElapsed   *bool     `json:"gate_veto_elapsed"`
	GateVetoed        *bool     `json:"gate_vetoed"`
	// LastPassAt is the newest signed PASS. When set, the veto deadline is
	// derived from it (persisted by the evaluator) and overrides GateVetoElapsed.
	LastPassAt *time.Time `json:"last_pass_at"`
	// Evidence identifies the receipt the facts were read from; ObservedAt is when.
	Evidence   string    `json:"evidence"`
	ObservedAt time.Time `json:"observed_at"`
}

// MergeCandidate is the exact PR state the facts must describe.
type MergeCandidate struct {
	WorkspaceID   pgtype.UUID
	Owner, Repo   string
	Number        int32
	HeadSHA       string
	BaseRef       string
	MergerAgentID pgtype.UUID
}

// MergeFactsSource returns stored facts for one candidate. It must read a
// cache, not call a network, because it runs inside the poll transaction.
type MergeFactsSource interface {
	MergeFacts(ctx context.Context, c MergeCandidate) (MergeFacts, error)
}

// MergePolicy is the workspace setting "merge_eligibility". The scheduler holds
// no policy of its own: every path rule, label and accepted security verdict
// comes from here, so a workspace tunes it without a code change.
type MergePolicy struct {
	// Revision names the policy version; it is part of the candidate identity.
	Revision string `json:"revision"`
	// MaxFactAgeSeconds bounds how old facts may be; zero or missing is invalid.
	MaxFactAgeSeconds int `json:"max_fact_age_seconds"`
	// GateVetoSeconds is the veto window after the last PASS; zero means 24 h.
	GateVetoSeconds int `json:"gate_veto_seconds"`
	// SecurityGates lists the accepted security verdicts, e.g. "success".
	SecurityGates          []string `json:"security_gates"`
	EscalationPathPatterns []struct {
		Pattern string `json:"pattern"`
		// Except exempts a path the pattern matched (for example tests and docs).
		Except string `json:"except"`
	} `json:"escalation_path_patterns"`
	EscalationLabels []string `json:"escalation_labels"`
	GatePathPatterns []string `json:"gate_path_patterns"`
}

type compiledMergePolicy struct {
	MergePolicy
	escalation []struct{ match, except *regexp.Regexp }
	gate       []*regexp.Regexp
}

func loadMergePolicy(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) (*compiledMergePolicy, error) {
	ws, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	var settings struct {
		Policy *MergePolicy `json:"merge_eligibility"`
	}
	if json.Unmarshal(ws.Settings, &settings) != nil || settings.Policy == nil ||
		settings.Policy.Revision == "" || settings.Policy.MaxFactAgeSeconds <= 0 || len(settings.Policy.SecurityGates) == 0 {
		return nil, errMergePolicyMissing
	}
	out := &compiledMergePolicy{MergePolicy: *settings.Policy}
	for _, e := range settings.Policy.EscalationPathPatterns {
		match, err := regexp.Compile(e.Pattern)
		if err != nil {
			return nil, errMergePolicyMissing
		}
		var except *regexp.Regexp
		if e.Except != "" {
			if except, err = regexp.Compile(e.Except); err != nil {
				return nil, errMergePolicyMissing
			}
		}
		out.escalation = append(out.escalation, struct{ match, except *regexp.Regexp }{match, except})
	}
	for _, g := range settings.Policy.GatePathPatterns {
		re, err := regexp.Compile(g)
		if err != nil {
			return nil, errMergePolicyMissing
		}
		out.gate = append(out.gate, re)
	}
	return out, nil
}

func isFalse(b *bool) bool { return b != nil && !*b }
func isTrue(b *bool) bool  { return b != nil && *b }

// refusals ports refusals() of scripts/agent_merge_eligibility.py. A missing or
// malformed fact refuses; paused, main_red_paused and merger_is_author must be
// exactly false. The same-lab break-glass is Cheese-specific and expires on
// 13 Nov 2026, so it has no counterpart here.
func (p *compiledMergePolicy) refusals(f MergeFacts, now time.Time) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if f.Tier == nil || *f.Tier < 0 || *f.Tier > 2 {
		add("tier missing or out of range")
	}
	for name, v := range map[string]*bool{"classifier_trusted": f.ClassifierTrusted, "coverage_complete": f.CoverageComplete, "pr_bound": f.PRBound, "checks_green": f.ChecksGreen, "cross_lab_pass": f.CrossLabPass, "scope_classified": f.ScopeClassified} {
		if !isTrue(v) {
			add("%s is not true", name)
		}
	}
	for name, v := range map[string]*bool{"paused": f.Paused, "main_red_paused": f.MainRedPaused, "merger_is_author": f.MergerIsAuthor} {
		if !isFalse(v) {
			add("%s is not false", name)
		}
	}
	if !slices.Contains(p.SecurityGates, f.SecurityGate) {
		add("security gate %q is not accepted", f.SecurityGate)
	}
	if f.Evidence == "" {
		add("facts carry no evidence reference")
	}
	if f.ObservedAt.IsZero() || f.ObservedAt.After(now) || now.Sub(f.ObservedAt) > time.Duration(p.MaxFactAgeSeconds)*time.Second {
		add("facts are stale or undated")
	}
	if len(f.Paths) == 0 || f.Labels == nil {
		return append(out, "paths or labels missing")
	}
	onGatePath := false
	for _, path := range f.Paths {
		for _, e := range p.escalation {
			if e.match.MatchString(path) && (e.except == nil || !e.except.MatchString(path)) {
				add("escalation path: %s", path)
				break
			}
		}
		for _, g := range p.gate {
			if g.MatchString(path) {
				onGatePath = true
			}
		}
	}
	// ponytail: the Python script waives the veto window while its gate is "disabled";
	// here that is a policy choice, expressed by listing only the gates that need it.
	if onGatePath && f.SecurityGate != "disabled" && !isTrue(f.GateVetoElapsed) {
		add("gate-path PR: veto window has not elapsed")
	}
	if onGatePath && !isFalse(f.GateVetoed) {
		add("gate-path PR: veto set or not checked")
	}
	for _, l := range *f.Labels {
		if slices.ContainsFunc(p.EscalationLabels, func(e string) bool { return strings.EqualFold(e, l) }) {
			add("escalation label: %s", l)
		}
	}
	return out
}

func (s *IssueWakeupService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// evaluateMergeEligibility reports whether a linked GitHub PR is, on its exact
// current head, one the policy lets an agent merge. It only reads stored
// state: PR rows the snapshot refresh keeps, the workspace policy, and facts
// from the MergeFactsSource. It never executes PR code and never merges;
// dispatching a run is not authorization to merge. A fact that is missing or
// stale makes the PR not eligible, never eligible.
func (s *IssueWakeupService) evaluateMergeEligibility(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup) (bool, string, map[string]any, error) {
	policy, err := loadMergePolicy(ctx, q, w.WorkspaceID)
	if err != nil {
		return false, "", nil, err
	}
	// The PR rows are only as current as the snapshot refresh that wrote them.
	if s.Tasks == nil || !s.Tasks.PRSnapshotsEnabled {
		return false, "", nil, errPRSnapshotsUnavailable
	}
	// Held to the end of the transaction, like the pull_request conditions (CHE-2223).
	if err := q.LockGitHubDispatchHaltShared(ctx, w.WorkspaceID); err != nil {
		return false, "", nil, err
	}
	rows, err := tx.Query(ctx, `SELECT pr.repo_owner,pr.repo_name,pr.pr_number,pr.state,pr.head_sha,COALESCE(pr.snapshot_head_sha,''),
			COALESCE(pr.snapshot_base_ref,''),COALESCE(pr.api_mergeable,''),COALESCE(pr.checks_rollup_state,''),pr.snapshot_fetched_at
		FROM github_pull_request pr JOIN issue_pull_request ipr ON ipr.pull_request_id=pr.id WHERE ipr.issue_id=$1`, w.IssueID)
	if err != nil {
		return false, "", nil, err
	}
	type linked struct {
		owner, repo, state, head, snapshotHead, base, mergeable, checks string
		number                                                          int32
		fetchedAt                                                       pgtype.Timestamptz
	}
	var prs []linked
	for rows.Next() {
		var l linked
		if err = rows.Scan(&l.owner, &l.repo, &l.number, &l.state, &l.head, &l.snapshotHead, &l.base, &l.mergeable, &l.checks, &l.fetchedAt); err != nil {
			rows.Close()
			return false, "", nil, err
		}
		prs = append(prs, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, "", nil, err
	}
	if len(prs) == 0 {
		return false, "", nil, errPRNotLinked
	}
	now := s.now()
	var keys []string
	var observed []map[string]any
	var pending error
	for _, l := range prs {
		if l.state != "open" || l.mergeable != "MERGEABLE" || l.checks != "SUCCESS" || l.base == "" || l.head == "" || l.snapshotHead != l.head {
			continue
		}
		// The refresher keeps the old row when GitHub is unreachable, so a matching
		// head does not prove the verdict is current: the snapshot needs a fetch
		// time inside the policy's freshness bound.
		if age := now.Sub(l.fetchedAt.Time); !l.fetchedAt.Valid || age < 0 || age > time.Duration(policy.MaxFactAgeSeconds)*time.Second {
			pending = errPRSnapshotPending
			continue
		}
		if halted, err := q.GitHubDispatchHalted(ctx, db.GitHubDispatchHaltedParams{WorkspaceID: w.WorkspaceID, RepoOwner: l.owner, RepoName: l.repo}); err != nil {
			return false, "", nil, err
		} else if halted {
			continue
		}
		// A set pause variable, or one that is unknown, holds the PR (CHE-5121).
		maxAge := time.Duration(policy.MaxFactAgeSeconds) * time.Second
		if paused, err := mergePaused(ctx, tx, l.owner, l.repo, now, maxAge); err != nil {
			if !errors.Is(err, errMergePauseUnknown) {
				return false, "", nil, err
			}
			pending = err
			continue
		} else if paused {
			continue
		}
		if s.MergeFacts == nil {
			pending = errMergeFactsUnavailable
			continue
		}
		facts, err := s.MergeFacts.MergeFacts(ctx, MergeCandidate{WorkspaceID: w.WorkspaceID, Owner: l.owner, Repo: l.repo, Number: l.number, HeadSHA: l.head, BaseRef: l.base, MergerAgentID: w.AgentID})
		if err != nil {
			pending = errMergeFactsUnavailable
			continue
		}
		// The persisted deadline, not a fact, says whether the veto window ran out.
		if facts.LastPassAt != nil {
			window := defaultGateVeto
			if policy.GateVetoSeconds > 0 {
				window = time.Duration(policy.GateVetoSeconds) * time.Second
			}
			elapsed, err := mergeVetoElapsed(ctx, tx, w.WorkspaceID, l.owner, l.repo, l.number, *facts.LastPassAt, now, window)
			if err != nil {
				return false, "", nil, err
			}
			facts.GateVetoElapsed = &elapsed
		}
		if len(policy.refusals(facts, now)) > 0 {
			continue
		}
		// One key per candidate, comma-free because conditionKeys splits on commas.
		keys = append(keys, strings.ReplaceAll(fmt.Sprintf("%s/%s#%d@%s>%s|%s|%s", l.owner, l.repo, l.number, l.head, l.base, policy.Revision, facts.Evidence), ",", "%2C"))
		observed = append(observed, map[string]any{"repo": l.owner + "/" + l.repo, "number": l.number, "head_sha": l.head, "base_ref": l.base, "policy_revision": policy.Revision, "evidence": facts.Evidence})
	}
	if len(keys) == 0 {
		return false, "", nil, pending
	}
	sort.Strings(keys)
	return true, "pr:" + strings.Join(keys, ","), map[string]any{"pull_requests": observed}, nil
}

var (
	errMergeFactsUnavailable = errors.New("merge facts are unavailable: no facts source is configured or it returned an error")
	errMergePolicyMissing    = errors.New("merge eligibility policy is missing or invalid in the workspace settings")
)
