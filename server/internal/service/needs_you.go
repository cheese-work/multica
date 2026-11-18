package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Homes of a counted decision (CHE-5670 D3).
const (
	HomeChat     = "chat"
	HomeArchived = "archived"
	HomeHidden   = "hidden"
	HomeUnlinked = "unlinked"
)

// NeedsYou is the one summary of what a member owes (CHE-5670 D1-D4).
type NeedsYou struct {
	AsOf         time.Time
	Total        int
	ByKind       AttentionCounts // decision, ask, later, unseen only
	OutsideChats int             // decisions whose home is not a chat row
	Snoozed      AttentionCounts // decision, ask, later, unseen only
	WithAgent    int
	Unclassified int
	NextChangeAt *time.Time
}

// ProjectNeedsYou computes the summary at `now`. Pure: callers load the chains and pass the clock.
//
// A chat row counts through ProjectChatAttention, so the summary and the chat list share one
// definition. A chain the viewer cannot open (archived or hidden) keeps only its decisions (D2, D3);
// they count in OutsideChats. `unlinked` holds decisions with no chat row of the viewer (D3).
func ProjectNeedsYou(chains []ChatAttentionChain, unlinked []AttentionSubject, marks map[string]AttentionMark, now time.Time) NeedsYou {
	out := NeedsYou{AsOf: now}
	var next *time.Time
	add := func(p ChatAttention, outside bool) {
		out.ByKind.Ask += p.Counts.Ask
		out.ByKind.Later += p.Counts.Later
		out.ByKind.Unseen += p.Counts.Unseen
		out.ByKind.Decision += p.Counts.Decision
		out.Snoozed.Ask += p.Snoozed.Ask
		out.Snoozed.Later += p.Snoozed.Later
		out.Snoozed.Unseen += p.Snoozed.Unseen
		out.Snoozed.Decision += p.Snoozed.Decision
		out.WithAgent += p.Counts.Waiting
		out.Unclassified += p.Counts.Unclassified
		if outside {
			out.OutsideChats += p.Counts.Decision
		}
		if p.NextChangeAt != nil && (next == nil || p.NextChangeAt.Before(*next)) {
			next = p.NextChangeAt
		}
	}
	for _, c := range chains {
		c.Marks = marks // one mark set for the whole viewer; marks are keyed by subject
		if c.Archived || c.Hidden {
			c.Subjects = decisionsOnly(c.Subjects)
			c.Archived = false // project the kept decisions; the chain is closed for every other subject
			add(ProjectChatAttention(c, now), true)
			continue
		}
		add(ProjectChatAttention(c, now), false)
	}
	if len(unlinked) > 0 {
		add(ProjectChatAttention(ChatAttentionChain{RootID: HomeUnlinked, Subjects: unlinked, Marks: marks}, now), true)
	}
	out.Total = out.ByKind.Ask + out.ByKind.Later + out.ByKind.Unseen + out.ByKind.Decision
	out.NextChangeAt = next
	return out
}

func decisionsOnly(in []AttentionSubject) []AttentionSubject {
	var out []AttentionSubject
	for _, s := range in {
		if s.Kind == AttentionKindDecision {
			out = append(out, s)
		}
	}
	return out
}

// LoadNeedsYou loads one member's inputs and projects the summary at `now`. `hiddenRoots` holds the
// chain roots whose agent the member cannot open (the caller resolves access; the service has no actor).
func LoadNeedsYou(ctx context.Context, q *db.Queries, workspaceID, userID pgtype.UUID, hiddenRoots map[string]bool, now time.Time) (NeedsYou, error) {
	in, err := loadAttentionInputs(ctx, q, workspaceID, userID)
	if err != nil {
		return NeedsYou{}, err
	}
	for i := range in.chains {
		in.chains[i].Hidden = hiddenRoots[in.chains[i].RootID]
	}
	return ProjectNeedsYou(in.chains, in.unlinked, in.marks, now), nil
}
