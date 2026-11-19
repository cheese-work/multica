package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

func quickFirstFixture() (*issueworkflow.QuickFirst, time.Time) {
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	return &issueworkflow.QuickFirst{
		WorkflowID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		Types:      []string{"feat", "fix"},
		StartsAt:   start, EndsAt: start.Add(24 * time.Hour),
	}, start.Add(time.Hour)
}

func TestChooseQuickFirst(t *testing.T) {
	qf, now := quickFirstFixture()
	root := quickFirstFacts{Title: "feat(x): a", StatusOmitted: true, Routed: true, Number: 2}
	with := func(edit func(*quickFirstFacts)) quickFirstFacts { f := root; edit(&f); return f }
	cases := []struct {
		name   string
		qf     *issueworkflow.QuickFirst
		now    time.Time
		facts  quickFirstFacts
		arm    bool
		drawn  bool
		reason string
	}{
		{"setting off", nil, now, root, false, false, ""},
		{"even root draws Q", qf, now, root, true, true, ""},
		{"odd root draws D", qf, now, with(func(f *quickFirstFacts) { f.Number = 3 }), false, true, ""},
		{"before the window", qf, qf.StartsAt.Add(-time.Nanosecond), root, false, false, ""},
		{"window start is in", qf, qf.StartsAt, root, true, true, ""},
		{"window end is out", qf, qf.EndsAt, root, false, false, ""},
		{"type not listed", qf, now, with(func(f *quickFirstFacts) { f.Title = "docs(x): a" }), false, false, quickReasonTypeNotListed},
		{"title without a type", qf, now, with(func(f *quickFirstFacts) { f.Title = "plain" }), false, false, quickReasonTypeNotListed},
		{"title-mapped route", qf, now, with(func(f *quickFirstFacts) { f.Source = issueworkflow.WorkflowSourceTitleType; f.CallerFlow = true }), true, true, ""},
		{"caller override", qf, now, with(func(f *quickFirstFacts) { f.Source = issueworkflow.WorkflowSourceCaller; f.CallerFlow = true }), false, false, quickReasonCallerOverride},
		{"override with no source", qf, now, with(func(f *quickFirstFacts) { f.CallerFlow = true }), false, false, quickReasonCallerOverride},
		{"unknown source", qf, now, with(func(f *quickFirstFacts) { f.Source = "bot" }), false, false, quickReasonSource},
		{"in_progress", qf, now, with(func(f *quickFirstFacts) { f.StatusOmitted, f.Status = false, "in_progress" }), false, false, quickReasonStatus},
		{"named todo", qf, now, with(func(f *quickFirstFacts) { f.StatusOmitted, f.Status = false, "todo" }), true, true, ""},
		{"named backlog", qf, now, with(func(f *quickFirstFacts) { f.StatusOmitted, f.Status = false, "backlog" }), true, true, ""},
		{"named agent", qf, now, with(func(f *quickFirstFacts) { f.AssigneeSet, f.AssigneeType = true, "agent" }), true, true, ""},
		{"member assignee", qf, now, with(func(f *quickFirstFacts) { f.AssigneeSet, f.AssigneeType = true, "member" }), false, false, quickReasonAssignee},
		{"squad assignee", qf, now, with(func(f *quickFirstFacts) { f.AssigneeSet, f.AssigneeType = true, "squad" }), false, false, quickReasonAssignee},
		{"open project split wins", qf, now, with(func(f *quickFirstFacts) { f.ProjectSplit = true }), false, false, quickReasonProjectSplit},
		{"no bound route", qf, now, with(func(f *quickFirstFacts) { f.Routed = false }), false, false, quickReasonNoFullWorkflow},
		{"child of Quick, odd", qf, now, with(func(f *quickFirstFacts) {
			f.HasParent, f.ParentFlow, f.Number, f.Routed = true, qf.WorkflowID, 3, false
		}), true, true, ""},
		{"child of Quick, type not listed", qf, now, with(func(f *quickFirstFacts) {
			f.Title, f.HasParent, f.ParentFlow, f.Routed = "docs(x): a", true, qf.WorkflowID, false
		}), true, true, ""},
		{"child of Quick, title without a type", qf, now, with(func(f *quickFirstFacts) {
			f.Title, f.HasParent, f.ParentFlow, f.Routed = "plain", true, qf.WorkflowID, false
		}), true, true, ""},
		{"child of a non-Quick parent, type not listed", qf, now, with(func(f *quickFirstFacts) {
			f.Title, f.HasParent = "docs(x): a", true
		}), false, false, quickReasonTypeNotListed},
		{"child of a non-Quick parent, even", qf, now, with(func(f *quickFirstFacts) { f.HasParent = true }), false, false, quickReasonParentNotQuick},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseQuickFirst(tc.qf, tc.now, tc.facts)
			if got.Arm != tc.arm || got.Drawn != tc.drawn || got.Reason != tc.reason {
				t.Fatalf("got %+v, want arm=%v drawn=%v reason=%q", got, tc.arm, tc.drawn, tc.reason)
			}
		})
	}
}

func TestWorkflowSplitActive(t *testing.T) {
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	split := &WorkflowSplit{StartsAt: start, EndsAt: start.Add(time.Hour)}
	for name, tc := range map[string]struct {
		split *WorkflowSplit
		now   time.Time
		want  bool
	}{
		"nil":         {nil, start, false},
		"before":      {split, start.Add(-time.Second), false},
		"start is in": {split, start, true},
		"end is out":  {split, start.Add(time.Hour), false},
	} {
		if got := tc.split.active(tc.now); got != tc.want {
			t.Errorf("%s: active = %v, want %v", name, got, tc.want)
		}
	}
}
