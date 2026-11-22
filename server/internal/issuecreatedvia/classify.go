// Package issuecreatedvia decides issue.created_via at create time (CHE-6376
// rule v1). One pure classifier plus a loader that reads only server-trusted
// rows: the acting agent's own task, its trigger comment and the parent issue.
package issuecreatedvia

// The values stored in issue.created_via. Integration-channel issues
// (rule 7) store NULL and never reach the classifier.
const (
	Dialog    = "dialog"
	DM        = "dm"
	Comment   = "comment"
	Agent     = "agent"
	Autopilot = "autopilot"
	CLI       = "cli"
	Form      = "form"
)

// Origin types the classifier reads.
const (
	OriginQuickCreate = "quick_create"
	OriginAutopilot   = "autopilot"
	OriginAgentCreate = "agent_create"
)

// TaskFacts are the origin-task fields the rules read.
type TaskFacts struct {
	HasChatSession bool
	// Autopilot is true when the task has an autopilot_run_id, or its
	// trigger_evidence_kind is autopilot_run or rule_version.
	Autopilot bool
	// EvidenceKind is trigger_evidence_kind; EvidenceKindNull marks a task
	// made before migration 184_agent_task_attribution (rule 5b).
	EvidenceKind     string
	EvidenceKindNull bool
	// TriggerCommentByMember is true when trigger_comment_id names a comment
	// whose author_type is member.
	TriggerCommentByMember bool
}

// Input is everything Classify reads.
type Input struct {
	OriginType   string
	CreatorAgent bool
	// Channel is the request channel of a member creator ("cli" or other).
	Channel string
	// Task is nil when the create has no usable origin task or a lookup failed.
	Task *TaskFacts
	// ParentMadeBySameTask: the new issue's parent has origin agent_create
	// with this task as its origin.
	ParentMadeBySameTask bool
}

// Classify applies rule v1. The first matching row wins.
func Classify(in Input) string {
	switch {
	case in.OriginType == OriginQuickCreate:
		return Dialog
	case in.OriginType == OriginAutopilot:
		return Autopilot
	case !in.CreatorAgent && in.Channel == "cli":
		return CLI
	case !in.CreatorAgent:
		return Form
	}
	t := in.Task
	if t == nil {
		return Agent
	}
	switch {
	case t.Autopilot:
		return Autopilot
	case t.HasChatSession && in.ParentMadeBySameTask:
		return Agent
	case t.HasChatSession:
		return DM
	case t.EvidenceKind == "comment" && t.TriggerCommentByMember:
		return Comment
	case t.EvidenceKindNull && t.TriggerCommentByMember:
		return Comment // rule 5b: legacy task, never a wakeup
	}
	return Agent
}
