// Package threadrel decides, per viewer and per thread, whether a comment
// thread is "with me", "agents only" or "waiting on me". It is a pure rule:
// no database, no HTTP. Callers load the thread and pass it in.
package threadrel

import (
	"sort"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// Actor and status values the rule reads.
const (
	ActorMember = "member"
	ActorAgent  = "agent"
	ActorSystem = "system"

	DecisionOpen = "open"
)

// Actor is a member, an agent or the system.
type Actor struct {
	Type string // "member", "agent" or "system"
	ID   string
}

// Comment is one comment of the thread.
type Comment struct {
	Author            Actor
	Content           string
	CreatedAt         time.Time
	NotifyOnly        *bool // nil for rows written before the column existed
	DecisionRequestID string
	ResolvesThread    bool
	Deleted           bool
}

// Decision is a decision request whose ask comment is in the thread.
type Decision struct {
	ID     string // matches Comment.DecisionRequestID of its ask comment
	Target Actor
	Status string
}

// Input is everything the rule reads.
type Input struct {
	Viewer            Actor
	FYICountsAsWithMe bool
	ResolvedAt        *time.Time
	Comments          []Comment
	Decisions         []Decision
}

// Result is the relation of one thread to the viewer.
type Result struct {
	WithMe      bool
	AgentsOnly  bool
	WaitingOnMe bool
}

// Classify applies the rule.
//
//	with_me:      V wrote a comment, or someone else mentions V directly, or a
//	              decision request in the thread targets V (a member viewer).
//	agents_only:  not with_me, and no member wrote a comment (system does not count).
//	waiting_on_me: an open decision targets V, or V's newest direct, non-FYI
//	              mention has no later comment by V and no resolution after it.
//
// Deleted comments are ignored. System comments never count as a mention.
// A mention in a comment that carries a decision request aimed at the viewer
// is left to the request's status, not to the "no later reply" test. A mention in a comment
// that resolves the thread, or with notify_only unset, never makes the thread
// waiting; the first is treated as FYI, the second as a legacy tag.
func Classify(in Input) Result {
	comments := make([]Comment, 0, len(in.Comments))
	for _, c := range in.Comments {
		if !c.Deleted {
			comments = append(comments, c)
		}
	}
	sort.SliceStable(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

	var (
		withMe       bool
		memberWrote  bool
		lastViewerAt = -1
		askAt        = -1
	)
	for i, c := range comments {
		if c.Author.Type == ActorMember {
			memberWrote = true
		}
		if c.Author == in.Viewer {
			withMe = true // rule 1
			lastViewerAt = i
			continue
		}
		if c.Author.Type == ActorSystem || !mentionsDirectly(c.Content, in.Viewer) {
			continue
		}
		fyi := c.ResolvesThread || (c.NotifyOnly != nil && *c.NotifyOnly)
		legacy := c.NotifyOnly == nil && !c.ResolvesThread
		if !fyi || legacy || in.FYICountsAsWithMe { // rule 2
			withMe = true
		}
		if !fyi && !legacy && !leftToRequest(c, in) {
			askAt = i
		}
	}

	waiting := false
	if in.Viewer.Type == ActorMember {
		for _, d := range in.Decisions {
			if d.Target != in.Viewer {
				continue
			}
			withMe = true // rule 3, any status
			if d.Status == DecisionOpen {
				waiting = true // (a)
			}
		}
	}
	if askAt >= 0 && lastViewerAt < askAt { // (b)
		resolved := in.ResolvedAt != nil && !in.ResolvedAt.Before(comments[askAt].CreatedAt)
		if !resolved {
			waiting = true
		}
	}

	return Result{
		WithMe:      withMe,
		AgentsOnly:  !withMe && !memberWrote,
		WaitingOnMe: waiting,
	}
}

// leftToRequest reports whether a mention in c is settled by the decision
// request c carries (rule (a) and the request's status) instead of rule (b):
// only when that request targets the viewer. A direct mention of someone else
// in the ask comment is an ordinary mention. A request missing from the input
// keeps the exemption, so a partial load never invents a waiting thread.
func leftToRequest(c Comment, in Input) bool {
	if c.DecisionRequestID == "" {
		return false
	}
	for _, d := range in.Decisions {
		if d.ID == c.DecisionRequestID {
			return d.Target == in.Viewer
		}
	}
	return true
}

// mentionsDirectly reports whether content mentions the viewer by id and type.
// @all and @squad mentions never match.
func mentionsDirectly(content string, viewer Actor) bool {
	for _, m := range util.ParseMentions(content) {
		if m.Type == viewer.Type && m.ID == viewer.ID {
			return true
		}
	}
	return false
}
