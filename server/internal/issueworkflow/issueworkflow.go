// Package issueworkflow owns project workflows (MUL-7420).
//
// A workflow is a whole pipeline: it selects and orders statuses from the
// workspace's shared issue_status catalog, and says who each column hands an
// issue to. Every workflow lists todo, done and cancelled; backlog and
// blocked are optional; only their positions are fixed (backlog, todo first;
// blocked, done, cancelled last). Statuses stay shared and peer-level, so
// issue.status keeps holding a catalog key.
//
// When an issue of a project enters a status through an ordinary issue write,
// it is assigned to the workflow's handler for that status and, for an agent,
// its run starts with the step's instructions. A relative handler (the project
// lead) resolves per project, so projects can share a workflow.
//
// A project without a workflow uses the workspace Default workflow. Default
// is a stored row once created; before that it is the built-in statuses.
// Once stored, Default limits status writes like any workflow, on its
// projects and on issues outside any project alike (MUL-8250). Issues already
// on statuses Default does not list keep them until someone moves them.
package issueworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/agentpreset"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Handler types a project can hand an issue to when it enters a status.
const (
	HandlerNone        = "none"
	HandlerAgent       = "agent"
	HandlerSquad       = "squad"
	HandlerMember      = "member"
	HandlerProjectLead = "project_lead"
	HandlerCreator     = "creator"
	// HandlerHumanByRole hands the issue to a person, by role: the issue's
	// requester, then the project lead, then the workspace owner.
	HandlerHumanByRole = "human_by_role"
	// HandlerHumanInCharge marks a status that waits on the person in charge
	// of a decision request (CHE-2591 L5). Entering it never hands off by
	// itself: the entry check assigns the issue explicitly.
	HandlerHumanInCharge = "human_in_charge"
)

const (
	MaxSteps             = 50
	MaxNameRunes         = 64
	MaxDescriptionRunes  = 256
	MaxInstructionsRunes = 8000
	DefaultName          = "Default"
)

// StartStatus is where a new issue lands unless its creator picks a status.
const StartStatus = issuestatus.Todo

// These statuses sit in fixed places around a workflow's own steps. Backlog
// and blocked are optional; the rest every workflow lists.
var (
	fixedHead = []string{issuestatus.Backlog, issuestatus.Todo}
	fixedTail = []string{issuestatus.Blocked, issuestatus.Done, issuestatus.Cancelled}
)

// IsRequired reports whether every workflow must list the status: new issues
// start on todo, and done and cancelled are how work ends.
func IsRequired(key string) bool {
	switch key {
	case issuestatus.Todo, issuestatus.Done, issuestatus.Cancelled:
		return true
	}
	return false
}

// IsFixed reports whether the status has a fixed place when listed.
func IsFixed(key string) bool {
	for _, k := range fixedHead {
		if k == key {
			return true
		}
	}
	for _, k := range fixedTail {
		if k == key {
			return true
		}
	}
	return false
}

// AcceptsHandoff reports whether entering the status can hand an issue off.
// Parking, finishing and cancelling an issue never reassign it.
func AcceptsHandoff(key string) bool {
	switch key {
	case issuestatus.Backlog, issuestatus.Done, issuestatus.Cancelled:
		return false
	}
	return true
}

// Arrange puts a workflow's steps in board order: the fixed statuses it
// lists in their places around its own steps, with the required ones added
// when missing. A repeated fixed status keeps its first step.
func Arrange(steps []Step) []Step {
	fixed := map[string]Step{}
	middle := make([]Step, 0, len(steps))
	for _, s := range steps {
		if !IsFixed(s.StatusKey) {
			middle = append(middle, s)
			continue
		}
		if _, ok := fixed[s.StatusKey]; !ok {
			fixed[s.StatusKey] = s
		}
	}
	place := func(out []Step, keys []string) []Step {
		for _, key := range keys {
			if s, ok := fixed[key]; ok {
				out = append(out, s)
			} else if IsRequired(key) {
				out = append(out, Step{StatusKey: key, Handler: Handler{Type: HandlerNone}})
			}
		}
		return out
	}
	out := place(make([]Step, 0, len(steps)+len(fixedHead)+len(fixedTail)), fixedHead)
	out = append(out, middle...)
	return place(out, fixedTail)
}

// ArrangeKeys is Arrange for bare status keys.
func ArrangeKeys(keys []string) []string {
	return keysOf(Arrange(stepsFor(keys)))
}

func keysOf(steps []Step) []string {
	keys := make([]string, len(steps))
	for i, s := range steps {
		keys[i] = s.StatusKey
	}
	return keys
}

// MiddleKeys returns the workflow's own steps, without the fixed statuses.
func MiddleKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if !IsFixed(key) {
			out = append(out, key)
		}
	}
	return out
}

// BuiltInDefaultKeys is Default before a workspace stores its own.
func BuiltInDefaultKeys() []string {
	return ArrangeKeys([]string{issuestatus.Backlog, issuestatus.InProgress, issuestatus.InReview, issuestatus.Blocked})
}

// Handler names who receives an issue when it enters a step. ID is set only
// for the concrete types (agent, squad, member).
type Handler struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	// Preset names the agent preset a run on this step uses (CHE-2249); "base"
	// forces the agent row. Only an agent or squad handler carries one.
	Preset string `json:"preset,omitempty"`
}

// Step is one status of a workflow with its handoff.
type Step struct {
	StatusKey     string  `json:"status_key"`
	Handler       Handler `json:"handler"`
	Instructions  string  `json:"instructions"`
	NextStatusKey string  `json:"next_status_key,omitempty"`
	BackStatusKey string  `json:"back_status_key,omitempty"`
	// AdvanceOnCIGreen moves an issue sitting on this step to its next status
	// when the checks of a linked PR pass on the current head.
	AdvanceOnCIGreen bool `json:"advance_on_ci_green,omitempty"`
	// Edges are the step's exits, each taken when its guard holds. Without
	// them the exits derive from the fields above (see EdgesOf).
	Edges []Edge `json:"edges,omitempty"`
	// SubWorkflow makes the step a sub-workflow node (CHE-2031): entering it
	// creates a child issue that follows another workflow, and a children_done
	// edge joins when every child of the stage is done.
	SubWorkflow *SubWorkflow `json:"sub_workflow,omitempty"`
	// Entry are the conditions a status write must meet to put an issue into
	// this step (CHE-2295); none means no gate.
	Entry []EntryGate `json:"entry,omitempty"`
	// DynamicAssignment says the step's assignee is chosen at run time, so it
	// has no handler. Lint-only: the dispatcher ignores it.
	DynamicAssignment bool `json:"dynamic_assignment,omitempty"`
	// WaitsOnOpenDecision declares the step a no-op while a human decision
	// request is open on the issue (CHE-4976): the answer wakes the issue, so
	// entering the step starts no run. Only a step whose author set it is
	// skipped; entry work in the instructions is never inferred away.
	WaitsOnOpenDecision bool `json:"waits_on_open_decision,omitempty"`
	// LintWaive silences lint rules on this step, each with its reason. A
	// waived finding is still reported, marked waived.
	LintWaive []LintWaiver `json:"lint_waive,omitempty"`
	// KeepOtherAssignee makes entering the step leave an assignee in place
	// when that assignee is neither this step's handler nor the handler of the
	// step the issue leaves (CHE-5013).
	//
	// TEMPORARY: CHE-5024 (per-issue workflows) replaces it; an issue's own
	// workflow will then say who owns each step. Remove this flag with it.
	KeepOtherAssignee bool `json:"keep_other_assignee,omitempty"`
}

// LintWaiver waives one lint rule on a step.
type LintWaiver struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

// SubWorkflow names the workflow a sub-workflow step starts and the stage its
// child issues join on.
type SubWorkflow struct {
	WorkflowID string `json:"workflow_id"`
	Stage      int    `json:"stage,omitempty"`
}

// Definition is a decoded workflow.
type Definition struct {
	ID        pgtype.UUID
	Name      string
	IsDefault bool
	Steps     []Step
	// MaxRunsPerIssue is the workflow's run budget per issue; 0 means unset
	// (see EffectiveMaxRuns).
	MaxRunsPerIssue int
	// IncludeChildPRs makes the PRs of an issue's direct children count as
	// its own for edge guards.
	IncludeChildPRs bool
	// RunBudgetSince is when the budget starts counting: runs before it are
	// free, so issues in flight when the budget shipped start fresh.
	RunBudgetSince time.Time
	// StartStatusKey is the status where new issues of the workflow start
	// (CHE-3113); empty means StartStatus.
	StartStatusKey string
}

// Start is the status a new issue lands on when its creator picks none. A key
// the workflow no longer lists falls back to StartStatus.
func (d Definition) Start() string {
	if d.StartStatusKey != "" && d.Has(d.StartStatusKey) {
		return d.StartStatusKey
	}
	return StartStatus
}

// ValidateStartStatus checks a workflow's start status: empty keeps todo;
// otherwise the key must be one of the steps, in an unstarted or started
// category, so a new issue never begins finished or cancelled.
func ValidateStartStatus(key string, steps []Step, catalog map[string]CatalogEntry) error {
	if key == "" {
		return nil
	}
	if !(Definition{Steps: steps}).Has(key) {
		return invalid("start status %q is not a status of this workflow", key)
	}
	if category := catalog[key].Category; category != issuestatus.CategoryUnstarted && category != issuestatus.CategoryStarted {
		return invalid("start status %q must be an unstarted or started status", key)
	}
	return nil
}

// HandsOff reports whether entering the step reassigns the issue.
func (s Step) HandsOff() bool {
	switch s.Handler.Type {
	case HandlerAgent, HandlerSquad, HandlerMember, HandlerProjectLead, HandlerCreator, HandlerHumanByRole:
		return true
	}
	return false
}

// storedStep is a step as the workflow row stores it; a step without a
// handoff is just its status key.
type storedStep struct {
	StatusKey     string       `json:"status_key"`
	Handler       *Handler     `json:"handler,omitempty"`
	Instructions  string       `json:"instructions,omitempty"`
	NextStatusKey string       `json:"next_status_key,omitempty"`
	BackStatusKey string       `json:"back_status_key,omitempty"`
	AdvanceOnCI   bool         `json:"advance_on_ci_green,omitempty"`
	Edges         []Edge       `json:"edges,omitempty"`
	SubWorkflow   *SubWorkflow `json:"sub_workflow,omitempty"`
	Entry         []EntryGate  `json:"entry,omitempty"`

	DynamicAssignment   bool         `json:"dynamic_assignment,omitempty"`
	WaitsOnOpenDecision bool         `json:"waits_on_open_decision,omitempty"`
	LintWaive           []LintWaiver `json:"lint_waive,omitempty"`

	// KeepOtherAssignee: see Step. TEMPORARY, CHE-5024.
	KeepOtherAssignee bool `json:"keep_other_assignee,omitempty"`
}

// Decode parses a stored workflow row.
func Decode(row db.IssueWorkflow) (Definition, error) {
	steps, err := DecodeSteps(row.Steps)
	if err != nil {
		return Definition{}, err
	}
	def := Definition{
		ID: row.ID, Name: row.Name, IsDefault: row.IsDefault, Steps: steps,
		MaxRunsPerIssue: int(row.MaxRunsPerIssue.Int32), IncludeChildPRs: row.IncludeChildPrs,
		StartStatusKey: row.StartStatusKey.String,
	}
	if row.RunBudgetSince.Valid {
		def.RunBudgetSince = row.RunBudgetSince.Time
	}
	return def, nil
}

// DecodeSteps parses a stored steps array, normalizing a missing handler to
// "none".
func DecodeSteps(raw []byte) ([]Step, error) {
	if len(raw) == 0 {
		return []Step{}, nil
	}
	var stored []storedStep
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode workflow steps: %w", err)
	}
	steps := make([]Step, len(stored))
	for i, s := range stored {
		steps[i] = Step{
			StatusKey:     s.StatusKey,
			Handler:       Handler{Type: HandlerNone},
			Instructions:  s.Instructions,
			NextStatusKey: s.NextStatusKey,
			BackStatusKey: s.BackStatusKey,

			AdvanceOnCIGreen: s.AdvanceOnCI,
			Edges:            s.Edges,
			SubWorkflow:      s.SubWorkflow,
			Entry:            s.Entry,

			DynamicAssignment:   s.DynamicAssignment,
			WaitsOnOpenDecision: s.WaitsOnOpenDecision,
			LintWaive:           s.LintWaive,
			KeepOtherAssignee:   s.KeepOtherAssignee,
		}
		if s.Handler != nil && s.Handler.Type != "" {
			steps[i].Handler = *s.Handler
		}
	}
	return steps, nil
}

// DecodeKeys parses a stored steps array into its status keys, in order.
func DecodeKeys(raw []byte) ([]string, error) {
	steps, err := DecodeSteps(raw)
	if err != nil {
		return nil, err
	}
	return keysOf(steps), nil
}

// EncodeSteps serializes a workflow's steps for storage.
func EncodeSteps(steps []Step) ([]byte, error) {
	stored := make([]storedStep, len(steps))
	for i, s := range steps {
		stored[i] = storedStep{StatusKey: s.StatusKey, NextStatusKey: s.NextStatusKey, BackStatusKey: s.BackStatusKey, AdvanceOnCI: s.AdvanceOnCIGreen, Edges: s.Edges, SubWorkflow: s.SubWorkflow, Entry: s.Entry, DynamicAssignment: s.DynamicAssignment, WaitsOnOpenDecision: s.WaitsOnOpenDecision, LintWaive: s.LintWaive, KeepOtherAssignee: s.KeepOtherAssignee}
		// A human_in_charge step hands nothing off, but its handler is the entry
		// rule of a decision episode and must survive storage.
		if s.HandsOff() || s.Handler.Type == HandlerHumanInCharge {
			h := s.Handler
			stored[i].Handler = &h
			stored[i].Instructions = s.Instructions
		}
	}
	return json.Marshal(stored)
}

// EncodeKeys serializes statuses without handoffs for storage.
func EncodeKeys(keys []string) ([]byte, error) {
	return EncodeSteps(stepsFor(keys))
}

func stepsFor(keys []string) []Step {
	steps := make([]Step, len(keys))
	for i, key := range keys {
		steps[i] = Step{StatusKey: key, Handler: Handler{Type: HandlerNone}}
	}
	return steps
}

// Limits reports whether the workflow restricts the statuses its issues may
// enter. Only a Default the workspace has not stored yet does not: it stands
// for "the built-in statuses" before the workspace ever used workflows, and
// limiting by it would strand custom statuses that workspace already uses.
func (d Definition) Limits() bool {
	return !d.IsDefault || d.ID.Valid
}

// Has reports whether the workflow lists the status key.
func (d Definition) Has(key string) bool {
	_, ok := d.Step(key)
	return ok
}

// Step returns the step for a status key.
func (d Definition) Step(key string) (Step, bool) {
	for _, s := range d.Steps {
		if s.StatusKey == key {
			return s, true
		}
	}
	return Step{}, false
}

// StatusKeys lists the workflow's statuses in board order.
func (d Definition) StatusKeys() []string {
	return keysOf(d.Steps)
}

// HandsOffAnywhere reports whether any step hands an issue off.
func (d Definition) HandsOffAnywhere() bool {
	for _, s := range d.Steps {
		if s.HandsOff() {
			return true
		}
	}
	return false
}

// NextStatus is where a step's handler moves the issue when done: the target
// of the step's first agent_done exit, else the step's own choice, else the
// next status on the board that is not blocked.
func (d Definition) NextStatus(key string) string {
	step, ok := d.Step(key)
	if !ok {
		return ""
	}
	for _, e := range step.Edges {
		if g, err := ParseGuard(e.Guard); err == nil && g.Kind == GuardAgentDone {
			return e.To
		}
	}
	if step.NextStatusKey != "" {
		return step.NextStatusKey
	}
	for i, s := range d.Steps {
		if s.StatusKey != key {
			continue
		}
		for _, next := range d.Steps[i+1:] {
			if next.StatusKey != issuestatus.Blocked {
				return next.StatusKey
			}
		}
	}
	return ""
}

// CatalogEntry is the slice of an issue_status row validation needs.
type CatalogEntry struct {
	Key      string
	Name     string
	Category string
	Archived bool
}

// Normalize trims free text and handoffs and puts the steps in board order.
func Normalize(name, description string, steps []Step) (string, string, []Step) {
	normalized := make([]Step, len(steps))
	for i, s := range steps {
		normalized[i] = NormalizeHandoff(s)
	}
	return strings.TrimSpace(name), strings.TrimSpace(description), Arrange(normalized)
}

// ValidationError is a user-correctable problem with a workflow definition.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// Validate checks a normalized workflow and its handoffs against the status
// catalog. The Default workflow keeps its reserved name. Handler ids are
// checked by the caller, which owns the workspace lookups.
func Validate(name, description string, steps []Step, isDefault bool, catalog map[string]CatalogEntry) error {
	if !isDefault {
		if name == "" || utf8.RuneCountInString(name) > MaxNameRunes {
			return invalid("name must be 1-%d characters", MaxNameRunes)
		}
		if strings.EqualFold(name, DefaultName) {
			return invalid("%q is reserved for the workspace default workflow", DefaultName)
		}
	}
	if utf8.RuneCountInString(description) > MaxDescriptionRunes {
		return invalid("description must be at most %d characters", MaxDescriptionRunes)
	}
	if len(steps) > MaxSteps {
		return invalid("a workflow can have at most %d statuses", MaxSteps)
	}
	seen := make(map[string]bool, len(steps))
	for _, key := range keysOf(steps) {
		entry, ok := catalog[key]
		if !ok && !issuestatus.IsBuiltIn(key) {
			return invalid("status %q does not exist", key)
		}
		if ok && entry.Archived {
			return invalid("status %q is archived", key)
		}
		if seen[key] {
			return invalid("status %q appears more than once", key)
		}
		seen[key] = true
	}
	def := Definition{Steps: steps}
	for _, s := range steps {
		if err := ValidateHandoff(s, def); err != nil {
			return err
		}
	}
	return nil
}

// NormalizeHandoff trims a handoff and drops fields its handler type does
// not use.
func NormalizeHandoff(s Step) Step {
	s.StatusKey = strings.TrimSpace(s.StatusKey)
	s.Handler.Type = strings.TrimSpace(s.Handler.Type)
	s.Handler.ID = strings.TrimSpace(s.Handler.ID)
	if s.Handler.Type == "" {
		s.Handler.Type = HandlerNone
	}
	switch s.Handler.Type {
	case HandlerAgent, HandlerSquad, HandlerMember:
	default:
		// Only the concrete handler types carry an id; a stale one left
		// by a type switch in the form would otherwise be stored.
		s.Handler.ID = ""
	}
	s.Handler.Preset = strings.TrimSpace(s.Handler.Preset)
	if s.Handler.Type != HandlerAgent && s.Handler.Type != HandlerSquad {
		s.Handler.Preset = ""
	}
	s.Instructions = strings.TrimSpace(s.Instructions)
	if s.Handler.Type == HandlerNone {
		s.Instructions = ""
	}
	s.NextStatusKey = strings.TrimSpace(s.NextStatusKey)
	s.BackStatusKey = strings.TrimSpace(s.BackStatusKey)
	s.Edges = normalizeEdges(s.Edges)
	s.Entry = normalizeEntry(s.Entry)
	if s.SubWorkflow != nil {
		s.SubWorkflow = &SubWorkflow{WorkflowID: strings.TrimSpace(s.SubWorkflow.WorkflowID), Stage: s.SubWorkflow.Stage}
	}
	return s
}

// CanAdvanceOnCIGreen reports whether CI green may move an issue out of a
// status: never out of a closed, parked or blocked one.
func CanAdvanceOnCIGreen(key string) bool {
	return AcceptsHandoff(key) && key != issuestatus.Blocked
}

// ValidateHandoff checks one normalized handoff against its workflow.
// Handler ids are checked by the caller, which owns the workspace lookups.
func ValidateHandoff(s Step, def Definition) error {
	if !def.Has(s.StatusKey) {
		return invalid("status %q is not part of this workflow", s.StatusKey)
	}
	switch s.Handler.Type {
	case HandlerNone, HandlerProjectLead, HandlerHumanByRole, HandlerHumanInCharge:
	case HandlerAgent, HandlerSquad, HandlerMember:
		if s.Handler.ID == "" {
			return invalid("choose a %s to hand off to", s.Handler.Type)
		}
	case HandlerCreator:
		return invalid("hand off to an agent, a squad, a member or the project lead")
	default:
		return invalid("unknown handler type %q", s.Handler.Type)
	}
	if s.HandsOff() && !AcceptsHandoff(s.StatusKey) {
		return invalid("issues entering %q cannot be handed off", s.StatusKey)
	}
	if p := s.Handler.Preset; p != "" && p != agentpreset.BaseName {
		if err := agentpreset.ValidateName(p); err != nil {
			return invalid("%s", err.Error())
		}
	}
	if utf8.RuneCountInString(s.Instructions) > MaxInstructionsRunes {
		return invalid("instructions must be at most %d characters", MaxInstructionsRunes)
	}
	for _, ref := range []string{s.NextStatusKey, s.BackStatusKey} {
		if ref != "" && (ref == s.StatusKey || !def.Has(ref)) {
			return invalid("%q is not another status of this workflow", ref)
		}
	}
	if s.AdvanceOnCIGreen && !CanAdvanceOnCIGreen(s.StatusKey) {
		return invalid("issues in %q cannot advance on CI green", s.StatusKey)
	}
	if err := validateSubWorkflow(s); err != nil {
		return err
	}
	if err := validateEntry(s); err != nil {
		return err
	}
	return validateEdges(s, def)
}

// Querier is the read surface workflow resolution needs.
type Querier interface {
	GetIssueWorkflow(ctx context.Context, arg db.GetIssueWorkflowParams) (db.IssueWorkflow, error)
	GetDefaultIssueWorkflow(ctx context.Context, workspaceID pgtype.UUID) (db.IssueWorkflow, error)
	GetProjectInWorkspace(ctx context.Context, arg db.GetProjectInWorkspaceParams) (db.Project, error)
}

// DefaultFor loads the workspace Default workflow, or the built-in one when
// the workspace has not stored its own yet.
func DefaultFor(ctx context.Context, q Querier, workspaceID pgtype.UUID) (Definition, error) {
	row, err := q.GetDefaultIssueWorkflow(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Definition{Name: DefaultName, IsDefault: true, Steps: stepsFor(BuiltInDefaultKeys()), IncludeChildPRs: true}, nil
		}
		return Definition{}, err
	}
	return Decode(row)
}

// ErrOverrideMissing means an issue's workflow override names a workflow that
// no longer exists. Deletion is blocked while an issue references a workflow,
// so this is a data fault; the issue never falls back to a looser workflow.
var ErrOverrideMissing = errors.New("the issue's workflow override no longer exists")

// ForProject loads the workflow a project uses. def is nil only for an issue
// outside any project.
func ForProject(ctx context.Context, q Querier, workspaceID, projectID pgtype.UUID) (*Definition, db.Project, error) {
	return ForIssue(ctx, q, workspaceID, pgtype.UUID{}, projectID)
}

// ForIssue loads the workflow an issue follows: its own override when set,
// otherwise its project's. def is nil only for an issue with neither. project
// is the zero value outside a project, so handlers relative to the project
// resolve to nobody. Every consumer resolves through here.
func ForIssue(ctx context.Context, q Querier, workspaceID, workflowID, projectID pgtype.UUID) (*Definition, db.Project, error) {
	var project db.Project
	if projectID.Valid {
		var err error
		project, err = q.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: workspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, db.Project{}, err
		}
	}
	if workflowID.Valid {
		row, err := q.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: workflowID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, project, ErrOverrideMissing
		}
		if err != nil {
			return nil, project, err
		}
		def, err := Decode(row)
		return &def, project, err
	}
	if !project.ID.Valid {
		return nil, db.Project{}, nil
	}
	def, err := ForProjectRow(ctx, q, project)
	return def, project, err
}

// OutsideProject loads the workflow that limits the statuses of an issue
// outside any project: the workspace's stored Default, or nil before the
// workspace stores one. Nothing hands off outside a project, so it is only
// for checking statuses; ForProject stays nil there. (MUL-8250)
func OutsideProject(ctx context.Context, q Querier, workspaceID pgtype.UUID) (*Definition, error) {
	row, err := q.GetDefaultIssueWorkflow(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	def, err := Decode(row)
	if err != nil {
		return nil, err
	}
	return &def, nil
}

// ForProjectRow loads the workflow of an already-loaded project. A dangling
// workflow reference falls back to Default.
func ForProjectRow(ctx context.Context, q Querier, project db.Project) (*Definition, error) {
	var def Definition
	var err error
	if project.WorkflowID.Valid {
		var row db.IssueWorkflow
		row, err = q.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: project.WorkflowID, WorkspaceID: project.WorkspaceID})
		if err == nil {
			def, err = Decode(row)
			return &def, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	def, err = DefaultFor(ctx, q, project.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return &def, nil
}

// Humans are the people a human-by-role handler can fall back to, looked up by
// the caller: the member an agent created the issue for, and the workspace
// owner. An invalid id means nobody holds that role. The caller sets Requester
// only for a current member, and LeadNotMember when the project's member lead
// has left the workspace.
type Humans struct {
	Requester      pgtype.UUID
	LeadNotMember  bool
	WorkspaceOwner pgtype.UUID
	// RoutingAgent, when valid, replaces an agent handler (CHE-3627).
	RoutingAgent pgtype.UUID
}

// ResolveHandler turns a step's handler into a concrete assignee for one
// issue. ok is false when the step does not hand off, or when a relative
// handler (project lead, creator, human by role) has nobody to resolve to.
func ResolveHandler(step Step, project db.Project, issueCreatorType string, issueCreatorID pgtype.UUID, humans Humans) (assigneeType string, assigneeID pgtype.UUID, ok bool) {
	switch step.Handler.Type {
	case HandlerAgent, HandlerSquad, HandlerMember:
		if step.Handler.Type == HandlerAgent && humans.RoutingAgent.Valid {
			return HandlerAgent, humans.RoutingAgent, true
		}
		var id pgtype.UUID
		if err := id.Scan(step.Handler.ID); err != nil || !id.Valid {
			return "", pgtype.UUID{}, false
		}
		return step.Handler.Type, id, true
	case HandlerProjectLead:
		if !project.LeadType.Valid || !project.LeadID.Valid {
			return "", pgtype.UUID{}, false
		}
		return project.LeadType.String, project.LeadID, true
	case HandlerCreator:
		if issueCreatorType == "" || !issueCreatorID.Valid {
			return "", pgtype.UUID{}, false
		}
		return issueCreatorType, issueCreatorID, true
	case HandlerHumanByRole:
		// Always a member: an agent project lead is not a human, so the chain
		// moves on to the workspace owner.
		switch {
		case humans.Requester.Valid:
			return "member", humans.Requester, true
		case project.LeadType.String == "member" && project.LeadID.Valid && !humans.LeadNotMember:
			return "member", project.LeadID, true
		case humans.WorkspaceOwner.Valid:
			return "member", humans.WorkspaceOwner, true
		}
	}
	return "", pgtype.UUID{}, false
}

// MoveTarget picks the status an issue keeps when it moves into a project
// using def: its current status when the workflow lists it, otherwise the
// first step of the same lifecycle category, otherwise the start status.
func MoveTarget(def Definition, currentKey string, categoryOf func(string) string) string {
	if def.Has(currentKey) {
		return currentKey
	}
	category := categoryOf(currentKey)
	for _, s := range def.Steps {
		if category != "" && categoryOf(s.StatusKey) == category {
			return s.StatusKey
		}
	}
	return StartStatus
}

// NotInWorkflowError explains a status write the project's workflow rejects.
// Its message lists the allowed keys because agents read it verbatim.
type NotInWorkflowError struct {
	StatusKey    string
	WorkflowName string
	Allowed      []string
	// OutsideProject marks an issue outside any project, which takes the
	// statuses of the workspace Default.
	OutsideProject bool
}

func (e *NotInWorkflowError) Error() string {
	if e.OutsideProject {
		return fmt.Sprintf("status %q is not part of the workflow %q that issues outside a project use; use one of: %s",
			e.StatusKey, e.WorkflowName, strings.Join(e.Allowed, ", "))
	}
	return fmt.Sprintf("status %q is not part of this project's workflow %q; use one of: %s",
		e.StatusKey, e.WorkflowName, strings.Join(e.Allowed, ", "))
}

// StepMovedError refuses an agent's status change on an issue that has left
// the step the agent's run works on: someone else moved it while the run
// worked, so the run's decision no longer applies, and applying it would undo
// that move.
type StepMovedError struct {
	IssueIdentifier string
	// Step is the status the run works on; Current is where the issue is now.
	Step, StepName       string
	Current, CurrentName string
	// MovedBy names whoever last changed the status; empty when unknown.
	MovedBy string
}

func (e *StepMovedError) Error() string {
	mover := e.MovedBy
	if mover == "" {
		mover = "someone else"
	}
	return fmt.Sprintf("%s is no longer at %s: %s moved it to %s while this run was working, so this status change was not applied. "+
		"Do not change the status again in this run; leave a comment if something still needs attention.",
		e.IssueIdentifier, statusLabel(e.StepName, e.Step), mover, statusLabel(e.CurrentName, e.Current))
}

// StaleVerdictError refuses a status change from a review run that was
// superseded by a newer head: its verdict is for a commit that is no longer the
// head of the pull request.
type StaleVerdictError struct {
	IssueIdentifier string
	// ReviewedSHA is the head the superseded run reviewed.
	ReviewedSHA string
}

func (e *StaleVerdictError) Error() string {
	return fmt.Sprintf("%s: verdict for %s is stale: a newer head superseded this review, so this status change was not applied. "+
		"Do not change the status again in this run; the replacement review covers the new head.",
		e.IssueIdentifier, e.ReviewedSHA)
}

// statusLabel names a status for an agent: its display name with the key the
// CLI takes, or the bare key when the two are the same.
func statusLabel(name, key string) string {
	if name == "" || name == key {
		return fmt.Sprintf("%q", key)
	}
	return fmt.Sprintf("%q (%s)", name, key)
}

// CheckStatus rejects a status the workflow does not list. Callers skip it
// when a write keeps the issue's current status, so an issue already on a
// status its workflow no longer lists keeps working.
func CheckStatus(def *Definition, key string) error {
	if def == nil || !def.Limits() || def.Has(key) {
		return nil
	}
	return &NotInWorkflowError{StatusKey: key, WorkflowName: def.Name, Allowed: def.StatusKeys()}
}

// CheckStatusOutsideProject is CheckStatus for an issue outside any project,
// against OutsideProject's workflow.
func CheckStatusOutsideProject(def *Definition, key string) error {
	err := CheckStatus(def, key)
	var notIn *NotInWorkflowError
	if errors.As(err, &notIn) {
		notIn.OutsideProject = true
	}
	return err
}

// BriefInput carries what the handoff brief names.
type BriefInput struct {
	Workflow        Definition
	ProjectName     string
	StatusKey       string
	IssueIdentifier string
	// BoundStep is the task's effective step when its binding must be respected.
	// A different StatusKey renders a continuation brief without status moves.
	// Empty preserves the normal handoff, preview and unbound-run brief.
	BoundStep string
	// MovedBy names whoever last changed the status, when known.
	MovedBy string
	// StatusName resolves a catalog key to its display name.
	StatusName func(key string) string
	// HandlerName describes who a step hands off to, e.g. "Sentinel" or
	// "Lin (project lead)". Empty means the step does not hand off.
	HandlerName func(step Step) string
	// Observer renders the brief for an agent working on the issue without
	// being its assignee — mentioned in a comment, say: the statuses, and to
	// leave the status to AssigneeName. The zero value is the assignee's
	// brief, with the step's instructions and the commands that finish it.
	Observer bool
	// AssigneeName names the issue's assignee in an observer's brief; empty
	// means unassigned.
	AssigneeName string
	// Routing marks a step the workspace's routing agent handles (CHE-4439 S5):
	// its commands are `multica issue route` lines, which record why, instead
	// of `multica issue status` lines.
	Routing bool
}

// Brief renders what an agent reads about the issue's project workflow: the
// workflow's statuses, who each step hands off to, and — for the assignee —
// the current step's instructions and the commands for the next move. The
// runtime brief's built-in status rules (for example in_review on delivery)
// name statuses this project may not use, so this text replaces them.
//
// It is the one rendering of a workflow for agents: the claim carries it into
// every run's prompt, and the editor and timeline show the same text. Names
// and instructions are user-authored, so control characters are dropped and
// names are kept to one line.
func Brief(in BriefInput) string {
	step, _ := in.Workflow.Step(in.StatusKey)
	stepMoved := in.BoundStep != "" && in.BoundStep != in.StatusKey
	name := func(key string) string {
		if in.StatusName == nil {
			return key
		}
		if n := briefLine(in.StatusName(key)); n != "" {
			return n
		}
		return key
	}
	var b strings.Builder
	title := briefLine(in.Workflow.Name)
	if project := briefLine(in.ProjectName); project != "" {
		title = fmt.Sprintf("%s (%s)", title, project)
	}
	fmt.Fprintf(&b, "This project's workflow — %s\n", title)
	if !stepMoved {
		b.WriteString("This workflow replaces the status rules in your runtime instructions: use only the status keys below")
		if !in.Workflow.Has(issuestatus.InReview) {
			fmt.Fprintf(&b, ", never `%s`", issuestatus.InReview)
		}
		b.WriteString(".\n")
	}
	if stepMoved {
		mover := briefLine(in.MovedBy)
		if mover == "" {
			mover = "someone else"
		}
		fmt.Fprintf(&b, "This run remains bound to %s, but %s is now at %s (last moved by %s).\n",
			statusLabel(name(in.BoundStep), in.BoundStep), in.IssueIdentifier, statusLabel(name(in.StatusKey), in.StatusKey), mover)
		b.WriteString("Do not change the issue's status in this run, including when starting or finishing work; this overrides the status rules in your runtime instructions. Do not retry a refused status change. Leave a comment if something needs attention. A member can review the current state and explicitly rerun the work.\n")
	} else if in.Observer {
		assignee := "is unassigned"
		if who := briefLine(in.AssigneeName); who != "" {
			assignee = "is assigned to " + who
		}
		fmt.Fprintf(&b, "%s is at the %q step and %s. You are not its assignee: leave the status to them unless you are explicitly asked to change it.\n",
			in.IssueIdentifier, name(in.StatusKey), assignee)
	} else {
		fmt.Fprintf(&b, "You are handling the %q step of %s.\n", name(in.StatusKey), in.IssueIdentifier)
		if instructions := briefText(step.Instructions); instructions != "" {
			b.WriteString("\nStep instructions:\n")
			for _, line := range strings.Split(instructions, "\n") {
				b.WriteString("> ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\nStatuses in this workflow:\n")
	for _, s := range in.Workflow.Steps {
		// Built-in statuses have no stored name; their key reads on its own.
		fmt.Fprintf(&b, "- `%s`", s.StatusKey)
		if n := name(s.StatusKey); n != s.StatusKey {
			fmt.Fprintf(&b, " %s", n)
		}
		if in.HandlerName != nil {
			if who := briefLine(in.HandlerName(s)); who != "" {
				fmt.Fprintf(&b, " — hands off to %s", who)
			}
		}
		if s.StatusKey == in.StatusKey {
			b.WriteString("   ← current")
		}
		b.WriteString("\n")
	}
	if stepMoved {
		return b.String()
	}
	if in.Observer {
		b.WriteString("\nMoving the issue into a step that hands off reassigns it to that step's handler and starts their run.\n")
		return b.String()
	}
	b.WriteString("\nHand off by changing the status — moving the issue into a step that hands off reassigns it to that step's handler and starts their run:\n")
	// move renders one command: a routing step records why through `issue route`.
	move := func(status string) string {
		if in.Routing {
			return fmt.Sprintf("multica issue route %s --status %s --reason \"<why>\" --confidence <0-1>", in.IssueIdentifier, status)
		}
		return fmt.Sprintf("multica issue status %s %s", in.IssueIdentifier, status)
	}
	if next := in.Workflow.NextStatus(in.StatusKey); next != "" {
		fmt.Fprintf(&b, "Step done      → %s\n", move(next))
	}
	if step.BackStatusKey != "" {
		fmt.Fprintf(&b, "Needs changes  → %s\n", move(step.BackStatusKey))
	}
	switch {
	case in.Workflow.Has(issuestatus.Blocked):
		fmt.Fprintf(&b, "Can't proceed  → %s, then comment why\n", move(issuestatus.Blocked))
	case step.BackStatusKey != "":
		b.WriteString("Can't proceed  → this workflow has no blocked status: comment why, then use the \"Needs changes\" command above\n")
	default:
		b.WriteString("Can't proceed  → this workflow has no blocked status: comment why and leave the status as it is\n")
	}
	b.WriteString("The system does not advance the status when your run ends.\n")
	b.WriteString("When the issue enters a step you handle, you get a run automatically — do not add a wakeup for that. Wakeups for anything else (a time, a PR, another issue, a step someone else handles) are fine.\n")
	return b.String()
}

// briefLine keeps a user-authored name on one line of a brief: line breaks
// and tabs become single spaces and other control characters are dropped.
func briefLine(s string) string {
	return strings.Join(strings.Fields(briefText(s)), " ")
}

// briefText drops the control characters of user-authored text, keeping its
// line breaks, so it cannot reshape the brief around it.
func briefText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// MaxSubWorkflowStage bounds the stage a sub-workflow step joins on.
const MaxSubWorkflowStage = 100

// validateSubWorkflow checks a sub-workflow step. The join is the step's
// children_done edge: without one the parent would wait for ever. That the
// target workflow exists in the workspace is the caller's check.
func validateSubWorkflow(s Step) error {
	sub := s.SubWorkflow
	if sub == nil {
		return nil
	}
	if _, err := util.ParseUUID(sub.WorkflowID); err != nil {
		return invalid("sub-workflow needs a valid workflow id")
	}
	if sub.Stage < 0 || sub.Stage > MaxSubWorkflowStage {
		return invalid("sub-workflow stage must be between 0 and %d", MaxSubWorkflowStage)
	}
	if !hasGuard(s.Edges, GuardChildrenDone) {
		return invalid("a sub-workflow step needs a children_done edge to join on")
	}
	return nil
}
