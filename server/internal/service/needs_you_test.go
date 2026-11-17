package service

import (
	"testing"
	"time"
)

func nyDecision(id string) AttentionSubject {
	return AttentionSubject{Key: DecisionKey(id, 1), Kind: AttentionKindDecision, Start: attnT0}
}

func nyChain(root string, subjects ...AttentionSubject) ChatAttentionChain {
	return ChatAttentionChain{RootID: root, CanonicalID: root, Subjects: subjects}
}

// CHE-5670 done-when seed, projected: A 2 decisions; B 1 decision + 1 ask; C unseen; D 2 decisions with
// one snoozed; E one unclassified turn; F archived (decision + ask); G hidden (decision + ask); H unlinked.
func TestProjectNeedsYouDoneWhenSeed(t *testing.T) {
	now := attnT0.Add(time.Hour)
	snoozeEnd := now.Add(2 * time.Hour)
	marks := map[string]AttentionMark{DecisionKey("d2", 1): {SnoozedUntil: &snoozeEnd}}

	f := nyChain("F", nyDecision("f1"), attnAsk("fa", attnT0))
	f.Archived = true
	g := nyChain("G", nyDecision("g1"), attnAsk("ga", attnT0))
	g.Hidden = true
	chains := []ChatAttentionChain{
		nyChain("A", nyDecision("a1"), nyDecision("a2")),
		nyChain("B", nyDecision("b1"), attnAsk("ba", attnT0)),
		nyChain("C", AttentionSubject{Key: ItemKey("cu"), Kind: AttentionKindUnseen, Start: attnT0}),
		nyChain("D", nyDecision("d1"), nyDecision("d2")),
		nyChain("E", AttentionSubject{Key: TurnKey("et"), Kind: AttentionKindTurn}),
		f, g,
	}
	got := ProjectNeedsYou(chains, []AttentionSubject{nyDecision("h1")}, marks, now)

	want := AttentionCounts{Decision: 7, Ask: 1, Unseen: 1}
	if got.ByKind != want {
		t.Errorf("by_kind = %+v, want %+v", got.ByKind, want)
	}
	if got.Total != 9 {
		t.Errorf("total = %d, want 9", got.Total)
	}
	if got.OutsideChats != 3 {
		t.Errorf("outside_chats.decision = %d, want 3 (archived, hidden, unlinked)", got.OutsideChats)
	}
	if got.Snoozed != (AttentionCounts{Decision: 1}) {
		t.Errorf("snoozed = %+v, want 1 decision", got.Snoozed)
	}
	if got.Unclassified != 1 || got.WithAgent != 0 {
		t.Errorf("unclassified=%d with_agent=%d, want 1/0", got.Unclassified, got.WithAgent)
	}
	if !got.AsOf.Equal(now) {
		t.Errorf("as_of = %v, want %v", got.AsOf, now)
	}
	if got.NextChangeAt == nil || !got.NextChangeAt.Equal(snoozeEnd) {
		t.Errorf("next_change_at = %v, want the snooze end %v", got.NextChangeAt, snoozeEnd)
	}
}

// D2: ask, later and unseen of an archived or hidden chain are not counted anywhere.
func TestProjectNeedsYouClosedChainKeepsOnlyDecisions(t *testing.T) {
	for _, closed := range []string{"archived", "hidden"} {
		t.Run(closed, func(t *testing.T) {
			later := AttentionSubject{Key: ItemKey("l"), Kind: AttentionKindLater, Start: attnT0, DueAt: &attnT0}
			unseen := AttentionSubject{Key: ItemKey("u"), Kind: AttentionKindUnseen, Start: attnT0}
			turn := AttentionSubject{Key: TurnKey("t"), Kind: AttentionKindTurn}
			c := nyChain("X", attnAsk("a", attnT0), later, unseen, turn)
			c.Archived, c.Hidden = closed == "archived", closed == "hidden"
			got := ProjectNeedsYou([]ChatAttentionChain{c}, nil, nil, attnT0.Add(time.Hour))
			if got != (NeedsYou{AsOf: got.AsOf}) {
				t.Fatalf("closed chain counted: %+v", got)
			}
		})
	}
}

// Home order: archived beats chat; hidden beats archived (first match wins: unlinked, hidden, archived).
func TestProjectNeedsYouHomePrecedence(t *testing.T) {
	c := nyChain("X", nyDecision("d"))
	c.Archived, c.Hidden = true, true
	got := ProjectNeedsYou([]ChatAttentionChain{c}, nil, nil, attnT0)
	if got.OutsideChats != 1 || got.ByKind.Decision != 1 || got.Total != 1 {
		t.Fatalf("got %+v", got)
	}
}

// D4: a snooze mark of an older revision does not hide the reopened decision; a due snooze returns it.
func TestProjectNeedsYouSnoozeIsKeyedByRevision(t *testing.T) {
	future := attnT0.Add(5 * time.Hour)
	d := AttentionSubject{Key: DecisionKey("d", 2), Kind: AttentionKindDecision, Start: attnT0}
	marks := map[string]AttentionMark{DecisionKey("d", 1): {SnoozedUntil: &future}}
	c := nyChain("X", d)
	got := ProjectNeedsYou([]ChatAttentionChain{c}, nil, marks, attnT0.Add(time.Hour))
	if got.ByKind.Decision != 1 || got.Snoozed.Decision != 0 {
		t.Fatalf("older-revision snooze hid the decision: %+v", got)
	}

	marks = map[string]AttentionMark{DecisionKey("d", 2): {SnoozedUntil: &future}}
	got = ProjectNeedsYou([]ChatAttentionChain{c}, nil, marks, attnT0.Add(time.Hour))
	if got.ByKind.Decision != 0 || got.Snoozed.Decision != 1 || got.Total != 0 {
		t.Fatalf("snoozed decision counted: %+v", got)
	}
	got = ProjectNeedsYou([]ChatAttentionChain{c}, nil, marks, future)
	if got.ByKind.Decision != 1 || got.Snoozed.Decision != 0 {
		t.Fatalf("due snooze did not return: %+v", got)
	}

	// A snoozed unlinked decision leaves the counts and counts as snoozed too.
	u := ProjectNeedsYou(nil, []AttentionSubject{d}, marks, attnT0.Add(time.Hour))
	if u.OutsideChats != 0 || u.Snoozed.Decision != 1 {
		t.Fatalf("snoozed unlinked: %+v", u)
	}
}

// D2: with_agent and a not-yet-due later are not items; a due later is.
func TestProjectNeedsYouWaitingAndLater(t *testing.T) {
	w := attnAsk("w", attnT0)
	w.Waiting = true
	dueLater := attnT0.Add(2 * time.Hour)
	l := AttentionSubject{Key: ItemKey("l"), Kind: AttentionKindLater, Start: dueLater, DueAt: &dueLater}
	c := nyChain("X", w, l)

	got := ProjectNeedsYou([]ChatAttentionChain{c}, nil, nil, attnT0.Add(time.Hour))
	if got.Total != 0 || got.WithAgent != 1 || got.ByKind.Later != 0 {
		t.Fatalf("before due: %+v", got)
	}
	if got.NextChangeAt == nil || !got.NextChangeAt.Equal(dueLater) {
		t.Fatalf("next_change_at = %v, want later due", got.NextChangeAt)
	}
	got = ProjectNeedsYou([]ChatAttentionChain{c}, nil, nil, dueLater)
	if got.Total != 1 || got.ByKind.Later != 1 {
		t.Fatalf("after due: %+v", got)
	}
}

// Invariant 1 over a mixed input.
func TestProjectNeedsYouTotalIsSumOfKinds(t *testing.T) {
	c := nyChain("X", nyDecision("d"), attnAsk("a", attnT0),
		AttentionSubject{Key: ItemKey("u"), Kind: AttentionKindUnseen, Start: attnT0})
	got := ProjectNeedsYou([]ChatAttentionChain{c}, nil, nil, attnT0)
	k := got.ByKind
	if got.Total != k.Decision+k.Ask+k.Later+k.Unseen || got.Total != 3 {
		t.Fatalf("total %d vs kinds %+v", got.Total, k)
	}
}
