package ciwatch

import "context"

import "github.com/jackc/pgx/v5"

// The three transactional destinations. Every method runs inside the caller's
// transaction; the Ledger commits or rolls back all of it together. A nil
// destination keeps its protocol Shadow-only: the action ends failed with
// reason non_transactional_destination (CHE-4265 Destination gate).

type IssueSpec struct {
	WorkspaceID, Title, Body, Priority string
	AssigneeType, AssigneeID           string
	CreatorID                          string // member who is the issue's creator
	Wake                               bool   // false: assign without starting a run
	Metadata                           map[string]any
	OriginID                           string // action id, for traceability
}

type IssueUpdate struct {
	WorkspaceID, IssueID string
	TitlePrefix          string
	Priority             string
	PrincipalID          string // the member whose invocation permissions the wake is checked against
	Wake                 bool   // start the assignee's run (or the squad leader's)
}

type CommentSpec struct {
	WorkspaceID, IssueID, AuthorID, Body string
}

type NotifySpec struct {
	WorkspaceID, MemberID, IssueID, Title, Body string
}

// CheckClaimer takes the shared red-main claim for one check, the GitHub ref
// refs/heads/red-main/<key> that red-main workers also take. holder is the
// signature that wants the issue; claiming again with the same holder must
// succeed, so a retry after a rolled-back creation is not locked out. ok=false
// means another holder owns the check.
type CheckClaimer interface {
	ClaimCheck(ctx context.Context, tx pgx.Tx, repoOwner, repoName, checkKey, holder string) (ok bool, err error)
}

type IssueDestination interface {
	CreateIssue(ctx context.Context, tx pgx.Tx, s IssueSpec) (issueID string, err error)
	UpdateIssue(ctx context.Context, tx pgx.Tx, u IssueUpdate) error
}

// CommentDestination must create the comment with suppress_agent_ids set to the
// full trigger set the server computes for it, so it wakes no agent.
type CommentDestination interface {
	AddComment(ctx context.Context, tx pgx.Tx, s CommentSpec) (commentID string, err error)
}

type InboxDestination interface {
	Notify(ctx context.Context, tx pgx.Tx, s NotifySpec) (itemID string, err error)
}

type Destinations struct {
	Issue   IssueDestination
	Comment CommentDestination
	Inbox   InboxDestination
	// RedMain is the shared check claim. Without it no issue is created: the
	// per-check advisory lock only orders this watcher's own transactions.
	RedMain CheckClaimer
}
