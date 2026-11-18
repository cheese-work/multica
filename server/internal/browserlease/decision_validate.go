package browserlease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ApproveLabel is the one option label that approves a browser lease. Only an answer that
// picks this option counts; any other option, or a note typed with it, does not.
const ApproveLabel = "Approve browser lease"

const (
	scopeHeading  = "Lease a browser for this task?"
	roleOwner     = "owner"
	keyAgent      = "agent"
	keyIssue      = "issue"
	keyProfile    = "profile"
	keyClass      = "class"
	keyDurationS  = "duration_seconds"
	scopeKeyCount = 5
)

// Scope is what a lease decision approves (plan CHE-4695 W1, "Decision validation (F4)").
type Scope struct {
	AgentID   pgtype.UUID
	IssueID   pgtype.UUID
	ProfileID pgtype.UUID
	Class     string
	Duration  time.Duration
}

// ScopeDecision renders the decision text of the ask (ask_json "decision"). The validator
// reads the scope back from these lines, so the request that W3-10 creates must use it.
func ScopeDecision(s Scope) string { return renderScope(scopeHeading, s) }

func renderScope(heading string, s Scope) string {
	return fmt.Sprintf("%s\n%s: %s\n%s: %s\n%s: %s\n%s: %s\n%s: %d",
		heading, keyAgent, uuid.UUID(s.AgentID.Bytes), keyIssue, uuid.UUID(s.IssueID.Bytes),
		keyProfile, uuid.UUID(s.ProfileID.Bytes), keyClass, s.Class, keyDurationS, int64(s.Duration/time.Second))
}

// parseScope reads the lines ScopeDecision writes. Any other shape, a repeated or an extra
// line, is not a scope.
func parseScope(decision string) (Scope, bool) { return parseScopeUnder(scopeHeading, decision) }

func parseScopeUnder(heading, decision string) (Scope, bool) {
	lines := strings.Split(decision, "\n")
	if len(lines) != scopeKeyCount+1 || lines[0] != heading {
		return Scope{}, false
	}
	got := map[string]string{}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ": ")
		if _, repeated := got[key]; !ok || repeated {
			return Scope{}, false
		}
		got[key] = value
	}
	var s Scope
	for key, dst := range map[string]*pgtype.UUID{keyAgent: &s.AgentID, keyIssue: &s.IssueID, keyProfile: &s.ProfileID} {
		id, err := uuid.Parse(got[key])
		if err != nil {
			return Scope{}, false
		}
		*dst = pgtype.UUID{Bytes: id, Valid: true}
	}
	seconds, err := strconv.ParseInt(got[keyDurationS], 10, 64)
	if err != nil {
		return Scope{}, false
	}
	s.Class, s.Duration = got[keyClass], time.Duration(seconds)*time.Second
	return s, s.Class != ""
}

// DecisionValidator is the Decisions hook (W3-11). Service.check calls it at mint, at renew
// and when a queued lease reaches the head of its queue, so one rule set covers all three.
type DecisionValidator struct {
	Queries *db.Queries
	Now     func() time.Time
}

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrDecisionInvalid, reason) }

// Validate accepts only an approving answer, from an allowed answerer, whose request names this
// agent, issue, profile and class, once its effective time has arrived. It returns when the
// approval ends: the effective time plus the approved duration.
func (v *DecisionValidator) Validate(ctx context.Context, s Subject) (*time.Time, error) {
	if !s.DecisionID.Valid {
		return nil, ErrDecisionRequired
	}
	d, err := v.Queries.GetBrowserLeaseDecision(ctx, db.GetBrowserLeaseDecisionParams{WorkspaceID: s.WorkspaceID, ID: s.DecisionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalid("decision not found")
	}
	if err != nil {
		return nil, err
	}
	now := v.now()
	if err := checkApproved(d, s, now); err != nil {
		return nil, err
	}
	var ask struct {
		Decision string `json:"decision"`
	}
	scope, ok := Scope{}, json.Unmarshal(d.AskJson, &ask) == nil
	if ok {
		scope, ok = parseScope(ask.Decision)
	}
	switch {
	case !ok:
		return nil, invalid("request carries no lease scope")
	case scope.AgentID != s.AgentID || scope.IssueID != s.IssueID || scope.ProfileID != s.Profile.ID || scope.Class != s.Profile.Class:
		return nil, invalid("request scope differs from the lease")
	case scope.Duration <= 0 || scope.Duration > MaxLeaseDuration:
		return nil, invalid("request duration outside the lease limit")
	case d.AnswerEffectiveAt.Time.After(now):
		return nil, invalid("decision not yet effective")
	}
	end := d.AnswerEffectiveAt.Time.Add(scope.Duration)
	if !end.After(now) {
		return nil, invalid("approved duration has passed")
	}
	return &end, nil
}

// isApproval is true when the answer picked the approve option and added no note: a note makes
// the approval conditional. A chosen option index decides; a free-text answer must be exactly the label.
func isApproval(d db.GetBrowserLeaseDecisionRow) bool {
	if d.AnswerReplyText.Valid && strings.TrimSpace(d.AnswerReplyText.String) != "" {
		return false
	}
	var offered []struct {
		Label string `json:"label"`
	}
	if json.Unmarshal(d.Options, &offered) != nil {
		return false
	}
	if d.AnswerChosenOptionIndex.Valid {
		i := int(d.AnswerChosenOptionIndex.Int32)
		return i >= 0 && i < len(offered) && offered[i].Label == ApproveLabel
	}
	if strings.TrimSpace(d.AnswerText.String) != ApproveLabel {
		return false
	}
	for _, o := range offered {
		if o.Label == ApproveLabel {
			return true
		}
	}
	return false
}

// mayAnswer: a workspace owner (Cheese) decides any class; the accountable user decides an S2
// lease of their own task. A non-member never decides.
func mayAnswer(d db.GetBrowserLeaseDecisionRow, s Subject) bool {
	switch {
	case !d.AnswererRole.Valid:
		return false
	case d.AnswererRole.String == roleOwner:
		return true
	}
	return s.Profile.Class == ClassS2 && d.AnsweredByUserID == s.UserID
}

func (v *DecisionValidator) now() time.Time {
	if v.Now != nil {
		return v.Now().UTC()
	}
	return time.Now().UTC()
}

// checkApproved holds the rules every lease decision shares, per-lease or once per issue: a
// human decision for this agent and issue, still standing, answered with the approving option by
// an allowed answerer.
func checkApproved(d db.GetBrowserLeaseDecisionRow, s Subject, now time.Time) error {
	switch {
	case d.Authority != "human":
		return invalid("decision needs human authority")
	case d.IssueID != s.IssueID || d.AskingAgentID != s.AgentID:
		return invalid("decision belongs to another agent or issue")
	case d.Status == "cancelled" || d.WithdrawnAt.Valid:
		return invalid("decision withdrawn or cancelled")
	case d.Status == "disclaimed":
		return invalid("decision answer disclaimed")
	case d.ExpiresAt.Valid && !d.ExpiresAt.Time.After(now):
		return invalid("decision expired")
	case d.Status == "open":
		return ErrDecisionRequired
	case !d.AnswerText.Valid:
		return invalid("decision has no live answer")
	case !isApproval(d):
		return invalid("answer is not the approving option")
	case !mayAnswer(d, s):
		return invalid("answerer may not decide this class")
	}
	return nil
}
