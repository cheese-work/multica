package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Stale levels of a DM's attention (plan r4, rule 5).
const (
	AttentionNone   = "none"
	AttentionFresh  = "fresh"
	AttentionYellow = "yellow"
	AttentionRed    = "red"

	AttentionYellowAfter = 4 * time.Hour
	AttentionRedAfter    = 24 * time.Hour
)

// Subject kinds. ask, later and unseen are chat_attention_item kinds.
const (
	AttentionKindAsk      = "ask"
	AttentionKindLater    = "later"
	AttentionKindUnseen   = "unseen"
	AttentionKindDecision = "decision"
	AttentionKindTurn     = "turn"
)

// AttentionSubject is one displayable thing in a chain.
type AttentionSubject struct {
	Key     string
	Kind    string
	Start   time.Time  // opened_at; decision created_at; later: due_at
	DueAt   *time.Time // later only
	Waiting bool       // ask whose targeted task is still running
}

type AttentionMark struct {
	SnoozedUntil *time.Time
	Acked        bool
}

type ChatAttentionChain struct {
	RootID      string
	CanonicalID string
	Archived    bool
	Subjects    []AttentionSubject
	Marks       map[string]AttentionMark
}

type AttentionCounts struct {
	Ask, Later, Unseen, Decision, Waiting, Unclassified int
}

type ChatAttention struct {
	RootID          string
	CanonicalID     string
	StaleLevel      string
	AttentionSource *string
	AttentionSince  *time.Time
	Counts          AttentionCounts
	Keys            []string // every displayed subject key: the snapshot a client sends back
	Unclassified    []string
	NextChangeAt    *time.Time
}

func ItemKey(id string) string { return "item:" + id }
func DecisionKey(id string, revision int32) string {
	return fmt.Sprintf("decision:%s@%d", id, revision)
}
func TurnKey(taskID string) string { return "turn:" + taskID }

// ProjectChatAttention applies plan r4 rules 1-6 to one chain at `now`. Pure: callers load the
// chain and pass the clock. Marks are keyed by subject key, so the result is the same from any
// DM in the chain.
func ProjectChatAttention(c ChatAttentionChain, now time.Time) ChatAttention {
	out := ChatAttention{RootID: c.RootID, CanonicalID: c.CanonicalID, StaleLevel: AttentionNone}
	if c.Archived { // rule 1
		return out
	}
	var oldest *AttentionSubject
	var changes []time.Time // future instants at which the projection can differ
	for i := range c.Subjects {
		s := c.Subjects[i]
		m := c.Marks[s.Key]
		if m.Acked {
			continue
		}
		if m.SnoozedUntil != nil && m.SnoozedUntil.After(now) { // rule 2: hide this subject only
			changes = append(changes, *m.SnoozedUntil)
			continue
		}
		if s.Kind == AttentionKindLater && s.DueAt != nil && s.DueAt.After(now) { // C1
			changes = append(changes, *s.DueAt)
			continue
		}
		out.Keys = append(out.Keys, s.Key)
		switch {
		case s.Kind == AttentionKindTurn: // C2: listed, never aged or coloured
			out.Counts.Unclassified++
			out.Unclassified = append(out.Unclassified, s.Key)
			continue
		case s.Waiting: // rule 3
			out.Counts.Waiting++
			continue
		}
		switch s.Kind {
		case AttentionKindAsk:
			out.Counts.Ask++
		case AttentionKindLater:
			out.Counts.Later++
		case AttentionKindUnseen:
			out.Counts.Unseen++
		case AttentionKindDecision:
			out.Counts.Decision++
		}
		if oldest == nil || s.Start.Before(oldest.Start) {
			oldest = &c.Subjects[i]
		}
	}
	if oldest != nil { // rules 4-6: the oldest visible obligation sets colour and source
		kind, since := oldest.Kind, oldest.Start
		out.AttentionSource, out.AttentionSince = &kind, &since
		age := now.Sub(since)
		switch {
		case age >= AttentionRedAfter:
			out.StaleLevel = AttentionRed
		case age >= AttentionYellowAfter:
			out.StaleLevel = AttentionYellow
			changes = append(changes, since.Add(AttentionRedAfter))
		default:
			out.StaleLevel = AttentionFresh
			changes = append(changes, since.Add(AttentionYellowAfter))
		}
	}
	for _, t := range changes {
		if out.NextChangeAt == nil || t.Before(*out.NextChangeAt) {
			t := t
			out.NextChangeAt = &t
		}
	}
	slices.Sort(out.Keys)
	slices.Sort(out.Unclassified)
	return out
}

func attentionRank(level string) int {
	switch level {
	case AttentionRed:
		return 0
	case AttentionYellow:
		return 1
	case AttentionFresh:
		return 2
	}
	return 3
}

// SortChatAttention orders the DM list (rule 7): red, yellow, fresh, none; oldest start first;
// canonical id as the tie-break.
func SortChatAttention(rows []ChatAttention) {
	slices.SortStableFunc(rows, func(a, b ChatAttention) int {
		if c := cmp.Compare(attentionRank(a.StaleLevel), attentionRank(b.StaleLevel)); c != 0 {
			return c
		}
		if a.AttentionSince != nil && b.AttentionSince != nil {
			if c := a.AttentionSince.Compare(*b.AttentionSince); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})
}

// LoadChatAttention projects every chain of one human in a workspace at `now`, in list order.
// Chains with no subject at all are left out.
func LoadChatAttention(ctx context.Context, q *db.Queries, workspaceID, userID pgtype.UUID, now time.Time) ([]ChatAttention, error) {
	chains, err := q.ListChatAttentionChains(ctx, db.ListChatAttentionChainsParams{WorkspaceID: workspaceID, CreatorID: userID})
	if err != nil {
		return nil, fmt.Errorf("list chains: %w", err)
	}
	items, err := q.ListChatAttentionItems(ctx, db.ListChatAttentionItemsParams{UserID: userID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	decisions, err := q.ListChatAttentionDecisions(ctx, db.ListChatAttentionDecisionsParams{HumanUserID: userID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	turns, err := q.ListChatAttentionLatestTurns(ctx, db.ListChatAttentionLatestTurnsParams{WorkspaceID: workspaceID, UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	markRows, err := q.ListChatAttentionMarks(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list marks: %w", err)
	}

	marks := make(map[string]AttentionMark, len(markRows))
	for _, m := range markRows {
		mark := AttentionMark{Acked: m.AckedAt.Valid}
		if m.SnoozedUntil.Valid {
			mark.SnoozedUntil = &m.SnoozedUntil.Time
		}
		marks[m.SubjectKey] = mark
	}
	subjects := map[string][]AttentionSubject{} // by chain root
	for _, i := range items {
		root := util.UUIDToString(i.ChainRootSessionID)
		s := AttentionSubject{Key: ItemKey(util.UUIDToString(i.ID)), Kind: i.Kind, Start: i.OpenedAt.Time, Waiting: i.Waiting}
		if i.Kind == AttentionKindLater && i.DueAt.Valid {
			s.DueAt, s.Start = &i.DueAt.Time, i.DueAt.Time // a later item counts from its due time (C1)
		}
		subjects[root] = append(subjects[root], s)
	}
	for _, d := range decisions {
		root := util.UUIDToString(d.ChainRootSessionID)
		subjects[root] = append(subjects[root], AttentionSubject{
			Key: DecisionKey(util.UUIDToString(d.ID), d.AnswerRevision), Kind: AttentionKindDecision, Start: d.CreatedAt.Time,
		})
	}
	for _, t := range turns {
		root := util.UUIDToString(t.ChainRootSessionID)
		subjects[root] = append(subjects[root], AttentionSubject{Key: TurnKey(util.UUIDToString(t.TaskID)), Kind: AttentionKindTurn})
	}

	out := make([]ChatAttention, 0, len(chains))
	for _, c := range chains {
		root := util.UUIDToString(c.RootID)
		if c.Archived || len(subjects[root]) == 0 { // A14: an archived chain is not listed
			continue
		}
		out = append(out, ProjectChatAttention(ChatAttentionChain{
			RootID: root, CanonicalID: util.UUIDToString(c.CanonicalID), Archived: c.Archived,
			Subjects: subjects[root], Marks: marks,
		}, now))
	}
	SortChatAttention(out)
	return out, nil
}
