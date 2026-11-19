package service

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Reasons logged when an issue the quick_first window covers is not drawn.
const (
	quickReasonNoFullWorkflow = "no_full_workflow"
	quickReasonCallerOverride = "caller_override"
	quickReasonTypeNotListed  = "type_not_listed"
	quickReasonStatus         = "status"
	quickReasonAssignee       = "member_or_squad_assignee"
	quickReasonProjectSplit   = "project_split"
	quickReasonParentNotQuick = "parent_not_quick"
	quickReasonQuickMissing   = "quick_workflow_missing"
	quickReasonSource         = "source"
)

// quickFirstFacts are what the draw reads of a create. Assignee is the one the
// caller named, not a step handler.
type quickFirstFacts struct {
	Title         string
	Source        string
	CallerFlow    bool // the create carries a workflow override before the draw
	Status        string
	StatusOmitted bool
	AssigneeType  string
	AssigneeSet   bool
	ProjectSplit  bool // the project's A/B split window is open
	Routed        bool // a full workflow binds the issue: the arm-D route
	HasParent     bool
	ParentFlow    pgtype.UUID
	Number        int32
}

// quickChoice is the draw: Arm is true for arm Q; Reason says why an issue in
// the window is not drawn ("" when it is, or when the arm is off for it).
type quickChoice struct {
	Arm    bool
	Drawn  bool // the issue was in the draw (arm Q or arm D)
	Reason string
}

// chooseQuickFirst decides one create. Off or outside the window it leaves the
// issue alone and logs nothing. Then, in order: the type must be listed; the
// workflow must not be the caller's (a child of a Quick parent skips the type check); the status todo or backlog (or omitted);
// no member or squad assignee; an open project split wins; a child follows its
// parent's Quick and never draws; a root needs a bound route and draws by
// number (even = arm Q).
func chooseQuickFirst(qf *issueworkflow.QuickFirst, now time.Time, f quickFirstFacts) quickChoice {
	if qf == nil || !qf.Open(now) {
		return quickChoice{}
	}
	// The types list gates the root draw. A child of a Quick parent follows it
	// whatever its own title says.
	if typ, ok := issueworkflow.TitleType(f.Title); !(f.HasParent && f.ParentFlow == qf.WorkflowID) && (!ok || !qf.Lists(typ)) {
		return quickChoice{Reason: quickReasonTypeNotListed}
	}
	switch {
	case f.Source == issueworkflow.WorkflowSourceCaller || (f.CallerFlow && f.Source == ""):
		return quickChoice{Reason: quickReasonCallerOverride}
	case f.Source != "" && f.Source != issueworkflow.WorkflowSourceTitleType:
		return quickChoice{Reason: quickReasonSource}
	}
	if !f.StatusOmitted && f.Status != "todo" && f.Status != "backlog" {
		return quickChoice{Reason: quickReasonStatus}
	}
	if f.AssigneeSet && f.AssigneeType != "agent" {
		return quickChoice{Reason: quickReasonAssignee}
	}
	if f.ProjectSplit {
		return quickChoice{Reason: quickReasonProjectSplit}
	}
	if f.HasParent {
		if f.ParentFlow == qf.WorkflowID {
			return quickChoice{Arm: true, Drawn: true}
		}
		return quickChoice{Reason: quickReasonParentNotQuick}
	}
	if !f.Routed {
		return quickChoice{Reason: quickReasonNoFullWorkflow}
	}
	return quickChoice{Arm: f.Number%2 == 0, Drawn: true}
}

// active reports whether the split's window is open at now.
func (s *WorkflowSplit) active(now time.Time) bool {
	return s != nil && !now.Before(s.StartsAt) && now.Before(s.EndsAt)
}
