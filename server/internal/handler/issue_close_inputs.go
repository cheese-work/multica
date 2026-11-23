package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The close predicates in two halves (CHE-9266). collectCloseInputs reads the
// rows each predicate needs into closeInputs, one section per predicate. The
// evaluation (evaluateClose) is a pure function of closeInputs, so the same
// predicates run over the rows recorded in an auto_close_skipped activity and
// give the answer the live check gave at that moment.

type closeIssueInput struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	HasParent     bool   `json:"has_parent"`
	TopLevel      bool   `json:"top_level"`
	PostCloseWork string `json:"post_close_work"`
	CheeseStop    bool   `json:"cheese_stop"`
}

type closeDoneEdgeInput struct {
	AgentDone bool   `json:"agent_done"`
	OnMerge   bool   `json:"pr_merged"`
	Error     string `json:"error,omitempty"`
}

type closePRInput struct {
	State         string `json:"state"`
	HeadSHA       string `json:"head_sha"`
	BaseRef       string `json:"base_ref"`
	DefaultBranch string `json:"default_branch"`
}

type closePRsInput struct {
	Linked []closePRInput `json:"linked"`
	Error  string         `json:"error,omitempty"`
}

type closeVerdictInput struct {
	Found            bool   `json:"found"`
	ID               string `json:"id,omitempty"`
	Action           string `json:"action,omitempty"`
	ActorID          string `json:"actor_id,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
	Findings         string `json:"findings,omitempty"`
	VoidedSince      int64  `json:"voided_since"`
	TextChangesSince int64  `json:"text_changes_since"`
	Error            string `json:"error,omitempty"`
}

type closeFlagInput struct {
	Open  bool   `json:"open"`
	Error string `json:"error,omitempty"`
}

type closeWakeupInput struct {
	Step  string `json:"step"`
	Other int64  `json:"other"`
	Own   int64  `json:"own"`
	Error string `json:"error,omitempty"`
}

type closeParentInput struct {
	Required     bool   `json:"required"`
	ID           string `json:"id,omitempty"`
	DeliveryStop string `json:"delivery_stop,omitempty"`
	Error        string `json:"error,omitempty"`
}

// closeInputs holds what the predicates read. A nil section was not read: the
// live check stops at the first predicate that does not hold.
type closeInputs struct {
	Issue        closeIssueInput     `json:"issue"`
	DoneEdge     *closeDoneEdgeInput `json:"done_edge,omitempty"`
	PRs          *closePRsInput      `json:"prs,omitempty"`
	Verdict      *closeVerdictInput  `json:"verdict,omitempty"`
	OpenChildren *closeFlagInput     `json:"open_children,omitempty"`
	OpenDecision *closeFlagInput     `json:"open_decision,omitempty"`
	Wakeups      *closeWakeupInput   `json:"wakeups,omitempty"`
	Parent       *closeParentInput   `json:"parent,omitempty"`
}

// closePredicateResult is one predicate on one set of inputs. Read is false when
// its inputs were not read; Error is set when they could not be read, which
// counts as not held.
type closePredicateResult struct {
	Name  string `json:"name"`
	Read  bool   `json:"read"`
	Held  bool   `json:"held"`
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type closeEvaluation struct {
	Predicates   []closePredicateResult
	FirstFailure string
	// Read names the predicates that held before the first failure, the verdict
	// predicate left out when it only awaits the merge.
	Read  []string
	State closeState
}

const (
	closeFailMergeWatcher = "merge_watcher_missing"
	closeAwaitingVerdict  = "verdict_pass_current"
)

type closePredicate struct {
	name    string
	collect func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs)
	eval    func(in *closeInputs) closePredicateResult
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func closeFlagResult(name string, f *closeFlagInput, what string) closePredicateResult {
	r := closePredicateResult{Name: name}
	if f == nil {
		return r
	}
	r.Read = true
	if f.Error != "" {
		r.Error = f.Error
		return r
	}
	r.Held = !f.Open
	r.Value = fmt.Sprintf("%s=%t", what, f.Open)
	return r
}

// closePredicates is the ordered predicate table; the order is the order of
// the first failure.
var closePredicates = []closePredicate{
	{"done_edge", func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs) {
		agentDone, onMerge, err := h.doneEdges(ctx, issue)
		in.DoneEdge = &closeDoneEdgeInput{AgentDone: agentDone, OnMerge: onMerge, Error: errText(err)}
	}, func(in *closeInputs) closePredicateResult {
		r := closePredicateResult{Name: "done_edge"}
		if in.DoneEdge == nil {
			return r
		}
		r.Read, r.Error = true, in.DoneEdge.Error
		r.Held = r.Error == "" && (in.DoneEdge.AgentDone || in.DoneEdge.OnMerge)
		r.Value = fmt.Sprintf("agent_done=%t pr_merged=%t", in.DoneEdge.AgentDone, in.DoneEdge.OnMerge)
		return r
	}},
	{"has_parent", func(context.Context, *Handler, db.Issue, *closeInputs) {}, func(in *closeInputs) closePredicateResult {
		return closePredicateResult{Name: "has_parent", Read: true, Held: in.Issue.HasParent || in.Issue.TopLevel,
			Value: fmt.Sprintf("has_parent=%t top_level=%t", in.Issue.HasParent, in.Issue.TopLevel)}
	}},
	{closeAwaitingVerdict, func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs) {
		h.collectCloseVerdict(ctx, issue, in)
	}, func(in *closeInputs) closePredicateResult {
		r := closePredicateResult{Name: closeAwaitingVerdict}
		if in.Verdict == nil || in.PRs == nil {
			return r
		}
		holds, awaiting, value, errMsg := evalCloseVerdict(in)
		r.Read, r.Held, r.Value, r.Error = true, holds || awaiting, value, errMsg
		return r
	}},
	{"prs_merged_into_default", func(context.Context, *Handler, db.Issue, *closeInputs) {}, func(in *closeInputs) closePredicateResult {
		r := closePredicateResult{Name: "prs_merged_into_default"}
		if in.PRs == nil {
			return r
		}
		r.Read = true
		if in.Issue.Status != statusMerged {
			r.Held, r.Value = true, "not required before merged"
			return r
		}
		if in.PRs.Error != "" {
			r.Error = in.PRs.Error
			return r
		}
		r.Held = true
		for _, pr := range in.PRs.Linked {
			if pr.State != "merged" || pr.BaseRef == "" || pr.BaseRef != pr.DefaultBranch {
				r.Held = false
			}
		}
		r.Value = fmt.Sprintf("linked_prs=%d", len(in.PRs.Linked))
		return r
	}},
	{"no_open_child", func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs) {
		open, err := h.hasOpenChildren(ctx, issue)
		in.OpenChildren = &closeFlagInput{Open: open, Error: errText(err)}
	}, func(in *closeInputs) closePredicateResult {
		return closeFlagResult("no_open_child", in.OpenChildren, "open_child")
	}},
	{"no_open_decision", func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs) {
		open, err := h.Queries.HasOpenDecisionRequestForIssue(ctx, db.HasOpenDecisionRequestForIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
		in.OpenDecision = &closeFlagInput{Open: open, Error: errText(err)}
	}, func(in *closeInputs) closePredicateResult {
		return closeFlagResult("no_open_decision", in.OpenDecision, "open_decision")
	}},
	{"no_live_wakeup", func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs) {
		n, err := h.Queries.CountIssueWakeupsLiveAtEntry(ctx, db.CountIssueWakeupsLiveAtEntryParams{IssueID: issue.ID, Step: issue.Status})
		in.Wakeups = &closeWakeupInput{Step: issue.Status, Other: n.Other, Own: n.Own, Error: errText(err)}
	}, func(in *closeInputs) closePredicateResult {
		r := closePredicateResult{Name: "no_live_wakeup"}
		if in.Wakeups == nil {
			return r
		}
		r.Read, r.Error = true, in.Wakeups.Error
		r.Held = r.Error == "" && in.Wakeups.Other == 0
		r.Value = fmt.Sprintf("other=%d own=%d", in.Wakeups.Other, in.Wakeups.Own)
		return r
	}},
	{"no_cheese_stop", func(context.Context, *Handler, db.Issue, *closeInputs) {}, func(in *closeInputs) closePredicateResult {
		return closePredicateResult{Name: "no_cheese_stop", Read: true, Held: !in.Issue.CheeseStop, Value: fmt.Sprintf("cheese_stop=%t", in.Issue.CheeseStop)}
	}},
	{"parent_not_stopped", func(ctx context.Context, h *Handler, issue db.Issue, in *closeInputs) {
		p := &closeParentInput{Required: issue.ParentIssueID.Valid || !in.Issue.TopLevel}
		in.Parent = p
		if !p.Required {
			return
		}
		p.ID = uuidToString(issue.ParentIssueID)
		parent, err := h.Queries.GetIssue(ctx, issue.ParentIssueID)
		if err != nil {
			p.Error = err.Error()
			return
		}
		p.DeliveryStop = metadataString(parent.Metadata, deliveryStopKey)
	}, func(in *closeInputs) closePredicateResult {
		r := closePredicateResult{Name: "parent_not_stopped"}
		if in.Parent == nil {
			return r
		}
		r.Read = true
		switch {
		case !in.Parent.Required:
			r.Held, r.Value = true, "top-level issue"
		case in.Parent.Error != "":
			r.Error = in.Parent.Error
		default:
			r.Held = in.Parent.DeliveryStop != "stopped"
			r.Value = fmt.Sprintf("delivery_stop=%q", in.Parent.DeliveryStop)
		}
		return r
	}},
	{"post_close_work_none", func(context.Context, *Handler, db.Issue, *closeInputs) {}, func(in *closeInputs) closePredicateResult {
		return closePredicateResult{Name: "post_close_work_none", Read: true, Held: in.Issue.PostCloseWork == "none", Value: fmt.Sprintf("post_close_work=%q", in.Issue.PostCloseWork)}
	}},
}

// collectCloseVerdict reads the linked PRs and the verdict of record with what
// voids it.
func (h *Handler) collectCloseVerdict(ctx context.Context, issue db.Issue, in *closeInputs) {
	q := h.Queries
	prs, err := q.ListIssuePullRequestHeads(ctx, issue.ID)
	in.PRs = &closePRsInput{Error: errText(err)}
	for _, pr := range prs {
		in.PRs.Linked = append(in.PRs.Linked, closePRInput{State: pr.State, HeadSHA: pr.HeadSha, BaseRef: pr.BaseRef, DefaultBranch: pr.DefaultBranch})
	}
	v := &closeVerdictInput{}
	in.Verdict = v
	verdict, err := q.GetLatestIssueVerdict(ctx, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		v.Error = err.Error()
		return
	}
	v.Found = true
	v.ID, v.Action, v.ActorID = uuidToString(verdict.ID), verdict.Action, uuidToString(verdict.ActorID)
	v.CreatedAt = timestampToString(verdict.CreatedAt)
	if verdict.Action != activityPassed {
		return
	}
	var details struct {
		Findings string `json:"findings"`
		HeadSHA  string `json:"head_sha"`
	}
	if err := json.Unmarshal(verdict.Details, &details); err != nil {
		v.Error = err.Error()
		return
	}
	v.Findings, v.HeadSHA = details.Findings, details.HeadSHA
	voided, err := q.CountIssueVerdictBreaksSince(ctx, db.CountIssueVerdictBreaksSinceParams{IssueID: issue.ID, Since: verdict.CreatedAt})
	if err != nil {
		v.Error = err.Error()
		return
	}
	v.VoidedSince = voided
	if in.PRs.Error == "" && len(in.PRs.Linked) == 0 {
		newer, err := q.CountIssueTextCandidateChangesSince(ctx, db.CountIssueTextCandidateChangesSinceParams{IssueID: issue.ID, Since: verdict.CreatedAt, ReviewerID: verdict.ActorID})
		if err != nil {
			v.Error = err.Error()
			return
		}
		v.TextChangesSince = newer
	}
}

// evalCloseVerdict: the newest verdict row is a PASS without a P0/P1 finding (a
// PASS write may carry findings, so the action name is not enough), nothing
// voided it since, and the candidate is the one it reviewed: every linked PR
// merged at the head the PASS names, or, with no PR, no attachment, description
// revision or comment (other than the reviewer's) newer than the PASS.
// awaiting: the same, except that a linked PR is still open or draft at the
// PASS's head. Anything unreadable keeps the handler run.
func evalCloseVerdict(in *closeInputs) (holds, awaiting bool, value, errMsg string) {
	v := in.Verdict
	if v.Error != "" {
		return false, false, "", v.Error
	}
	if !v.Found || v.Action != activityPassed {
		return false, false, "no PASS is the newest verdict", ""
	}
	var findings []ReviewFinding
	if err := json.Unmarshal([]byte(v.Findings), &findings); err != nil {
		return false, false, "", err.Error()
	}
	for _, f := range findings {
		if f.Severity == "P0" || f.Severity == "P1" {
			return false, false, "PASS carries a " + f.Severity + " finding", ""
		}
	}
	if v.VoidedSince > 0 {
		return false, false, "a later row voids the PASS", ""
	}
	if in.PRs.Error != "" {
		return false, false, "", in.PRs.Error
	}
	if len(in.PRs.Linked) == 0 {
		if v.TextChangesSince > 0 {
			return false, false, "the candidate text changed after the PASS", ""
		}
		return true, false, "PASS covers the current text", ""
	}
	for _, pr := range in.PRs.Linked {
		if v.HeadSHA == "" || pr.HeadSHA != v.HeadSHA {
			return false, false, "a linked PR head differs from the PASS head", ""
		}
		switch pr.State {
		case "merged":
		case "open", "draft":
			awaiting = true
		default:
			return false, false, "a linked PR is " + pr.State, ""
		}
	}
	if awaiting {
		return false, true, "PASS head awaits its merge", ""
	}
	return true, false, "PASS head merged", ""
}

// evaluateClose runs the first limit predicates on in; predicates beyond it
// stay unread. It is the one evaluation, live and replayed.
func evaluateClose(in *closeInputs, limit int) closeEvaluation {
	var ev closeEvaluation
	awaiting := false
	for i, p := range closePredicates {
		if i >= limit {
			ev.Predicates = append(ev.Predicates, closePredicateResult{Name: p.name})
			continue
		}
		r := p.eval(in)
		ev.Predicates = append(ev.Predicates, r)
		if !r.Read || ev.FirstFailure != "" {
			continue
		}
		if !r.Held {
			ev.FirstFailure = p.name
			continue
		}
		if p.name == closeAwaitingVerdict {
			_, awaiting, _, _ = evalCloseVerdict(in)
			if awaiting {
				continue
			}
		}
		ev.Read = append(ev.Read, p.name)
	}
	switch {
	case ev.FirstFailure != "":
		ev.State = closeStateRejected
	case limit < len(closePredicates):
		ev.State = closeStateRejected
	case awaiting && in.DoneEdge != nil && in.DoneEdge.OnMerge && in.Wakeups != nil && in.Wakeups.Own > 0:
		ev.State = closeStateAwaitingMerge
	case awaiting:
		ev.State, ev.FirstFailure = closeStateRejected, closeFailMergeWatcher
	default:
		ev.State = closeStateHolds
	}
	return ev
}

// collectCloseInputs reads the predicates in order. all false stops at the
// first that does not hold, as the live check does; all true reads every one.
func (h *Handler) collectCloseInputs(ctx context.Context, issue db.Issue, topLevel, all bool) (closeInputs, closeEvaluation) {
	in := closeInputs{Issue: closeIssueInput{
		ID: uuidToString(issue.ID), Status: issue.Status, HasParent: issue.ParentIssueID.Valid, TopLevel: topLevel,
		PostCloseWork: metadataString(issue.Metadata, postCloseWorkKey), CheeseStop: hasCheeseStopLine(issue.Description.String),
	}}
	n := len(closePredicates)
	for i, p := range closePredicates {
		p.collect(ctx, h, issue, &in)
		if all {
			continue
		}
		if ev := evaluateClose(&in, i+1); ev.FirstFailure != "" {
			n = i + 1
			break
		}
	}
	ev := evaluateClose(&in, n)
	for _, r := range ev.Predicates {
		if r.Error != "" && r.Name == ev.FirstFailure {
			slog.Warn("auto close: predicate unreadable; the step's run starts", "predicate", r.Name, "err", r.Error, "issue_id", uuidToString(issue.ID))
		}
	}
	return in, ev
}
