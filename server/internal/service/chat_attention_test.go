package service

import (
	"slices"
	"testing"
	"time"
)

var attnT0 = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func attnAsk(id string, start time.Time) AttentionSubject {
	return AttentionSubject{Key: ItemKey(id), Kind: AttentionKindAsk, Start: start}
}

func attnChain(subjects ...AttentionSubject) ChatAttentionChain {
	return ChatAttentionChain{RootID: "r", CanonicalID: "c", Subjects: subjects, Marks: map[string]AttentionMark{}}
}

func TestSubjectKeys(t *testing.T) {
	if got := ItemKey("a1"); got != "item:a1" {
		t.Errorf("ItemKey = %q", got)
	}
	if got := DecisionKey("d1", 2); got != "decision:d1@2" {
		t.Errorf("DecisionKey = %q", got)
	}
	if got := TurnKey("t1"); got != "turn:t1" {
		t.Errorf("TurnKey = %q", got)
	}
}

// Rules 4-6 + A14: fresh -> yellow at 4h -> red at 24h with a test clock, and
// next_change_at names the next crossing.
func TestProjectionStaleLevelsAndNextChange(t *testing.T) {
	c := attnChain(attnAsk("a", attnT0))
	cases := []struct {
		name string
		at   time.Duration
		want string
		next *time.Time
	}{
		{"fresh at open", 0, AttentionFresh, ptr(attnT0.Add(4 * time.Hour))},
		{"fresh just before 4h", 4*time.Hour - time.Second, AttentionFresh, ptr(attnT0.Add(4 * time.Hour))},
		{"yellow at 4h", 4 * time.Hour, AttentionYellow, ptr(attnT0.Add(24 * time.Hour))},
		{"yellow just before 24h", 24*time.Hour - time.Second, AttentionYellow, ptr(attnT0.Add(24 * time.Hour))},
		{"red at 24h", 24 * time.Hour, AttentionRed, nil},
		{"red later", 100 * time.Hour, AttentionRed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectChatAttention(c, attnT0.Add(tc.at))
			if got.StaleLevel != tc.want {
				t.Fatalf("level = %q, want %q", got.StaleLevel, tc.want)
			}
			if (got.NextChangeAt == nil) != (tc.next == nil) || (tc.next != nil && !got.NextChangeAt.Equal(*tc.next)) {
				t.Fatalf("next_change_at = %v, want %v", got.NextChangeAt, tc.next)
			}
			if got.AttentionSource == nil || *got.AttentionSource != AttentionKindAsk || !got.AttentionSince.Equal(attnT0) {
				t.Fatalf("source/since = %v/%v", got.AttentionSource, got.AttentionSince)
			}
		})
	}
}

// Rule 1 + A14: an archived canonical DM is never listed.
func TestProjectionArchivedChainIsHidden(t *testing.T) {
	c := attnChain(attnAsk("a", attnT0))
	c.Archived = true
	got := ProjectChatAttention(c, attnT0.Add(30*time.Hour))
	if got.StaleLevel != AttentionNone || len(got.Keys) != 0 || got.NextChangeAt != nil || got.AttentionSource != nil || got.AttentionSince != nil {
		t.Fatalf("archived chain projected: %+v", got)
	}
}

// Rule 2 + rule 4 (A2): snooze hides one subject, never resets its age.
func TestProjectionSnoozeHidesOnlyThatSubjectAndKeepsAge(t *testing.T) {
	c := attnChain(attnAsk("old", attnT0), attnAsk("new", attnT0.Add(25*time.Hour)))
	c.Marks[ItemKey("old")] = AttentionMark{SnoozedUntil: ptr(attnT0.Add(26 * time.Hour))}
	now := attnT0.Add(25*time.Hour + time.Minute)

	got := ProjectChatAttention(c, now)
	if !slices.Equal(got.Keys, []string{ItemKey("new")}) {
		t.Fatalf("keys = %v, want only the unsnoozed ask", got.Keys)
	}
	if got.StaleLevel != AttentionFresh {
		t.Fatalf("level = %q, want fresh (old ask is hidden)", got.StaleLevel)
	}
	if got.NextChangeAt == nil || !got.NextChangeAt.Equal(attnT0.Add(26*time.Hour)) {
		t.Fatalf("next_change_at = %v, want snooze end", got.NextChangeAt)
	}

	got = ProjectChatAttention(c, attnT0.Add(26*time.Hour))
	if got.StaleLevel != AttentionRed || !got.AttentionSince.Equal(attnT0) {
		t.Fatalf("after snooze: level=%q since=%v, want red from the original start", got.StaleLevel, got.AttentionSince)
	}
}

// Rule 3: a waiting ask is shown without age.
func TestProjectionWaitingAskHasNoAge(t *testing.T) {
	w := attnAsk("w", attnT0)
	w.Waiting = true
	now := attnT0.Add(48 * time.Hour)

	got := ProjectChatAttention(attnChain(w), now)
	if got.StaleLevel != AttentionNone || got.AttentionSource != nil || got.AttentionSince != nil {
		t.Fatalf("waiting-only chain = %+v, want none", got)
	}
	if !slices.Equal(got.Keys, []string{ItemKey("w")}) || got.Counts.Waiting != 1 {
		t.Fatalf("keys=%v waiting=%d, want the waiting ask displayed", got.Keys, got.Counts.Waiting)
	}

	got = ProjectChatAttention(attnChain(w, attnAsk("a", now.Add(-time.Hour))), now)
	if got.StaleLevel != AttentionFresh || *got.AttentionSince != now.Add(-time.Hour) {
		t.Fatalf("level=%q since=%v, want the aged ask to decide", got.StaleLevel, got.AttentionSince)
	}
}

// Rule 6: source is the oldest visible obligation's kind; counts per kind; keys sorted.
func TestProjectionSourceCountsAndKeys(t *testing.T) {
	dec := AttentionSubject{Key: DecisionKey("d", 1), Kind: AttentionKindDecision, Start: attnT0.Add(-5 * time.Hour)}
	unseen := AttentionSubject{Key: ItemKey("u"), Kind: AttentionKindUnseen, Start: attnT0.Add(-time.Hour)}
	a := attnAsk("a", attnT0.Add(-2*time.Hour))
	got := ProjectChatAttention(attnChain(unseen, a, dec), attnT0)

	if *got.AttentionSource != AttentionKindDecision || !got.AttentionSince.Equal(dec.Start) {
		t.Fatalf("source/since = %s/%v, want the decision", *got.AttentionSource, got.AttentionSince)
	}
	if got.StaleLevel != AttentionYellow {
		t.Fatalf("level = %q, want yellow", got.StaleLevel)
	}
	want := AttentionCounts{Ask: 1, Unseen: 1, Decision: 1}
	if got.Counts != want {
		t.Fatalf("counts = %+v, want %+v", got.Counts, want)
	}
	if !slices.IsSorted(got.Keys) || len(got.Keys) != 3 {
		t.Fatalf("keys = %v, want 3 sorted", got.Keys)
	}
}

// Rule 7: red, yellow, fresh, none; then oldest start; then canonical id.
func TestSortChatAttention(t *testing.T) {
	row := func(id, level string, since time.Time) ChatAttention {
		r := ChatAttention{CanonicalID: id, StaleLevel: level}
		if level != AttentionNone {
			r.AttentionSince = &since
		}
		return r
	}
	rows := []ChatAttention{
		row("n", AttentionNone, time.Time{}),
		row("f", AttentionFresh, attnT0),
		row("y2", AttentionYellow, attnT0.Add(time.Hour)),
		row("y1", AttentionYellow, attnT0),
		row("r-b", AttentionRed, attnT0),
		row("r-a", AttentionRed, attnT0),
	}
	SortChatAttention(rows)
	var got []string
	for _, r := range rows {
		got = append(got, r.CanonicalID)
	}
	if want := []string{"r-a", "r-b", "y1", "y2", "f", "n"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A19 / B1: a snooze is keyed by identity; a new decision and a reopened one are new subjects.
func TestProjectionSnoozeByDecisionIdentity(t *testing.T) {
	d := func(id string, rev int32) AttentionSubject {
		return AttentionSubject{Key: DecisionKey(id, rev), Kind: AttentionKindDecision, Start: attnT0}
	}
	c := attnChain(d("d1", 1))
	c.Marks[DecisionKey("d1", 1)] = AttentionMark{SnoozedUntil: ptr(attnT0.Add(2 * time.Hour))}
	now := attnT0.Add(time.Hour)

	if got := ProjectChatAttention(c, now); len(got.Keys) != 0 {
		t.Fatalf("snoozed D1 visible: %v", got.Keys)
	}
	c.Subjects = []AttentionSubject{d("d1", 1), d("d2", 1)}
	if got := ProjectChatAttention(c, now); !slices.Equal(got.Keys, []string{DecisionKey("d2", 1)}) {
		t.Fatalf("keys = %v, want only the new D2", got.Keys)
	}
	c.Subjects = []AttentionSubject{d("d1", 2), d("d2", 1)}
	if got := ProjectChatAttention(c, now); !slices.Equal(got.Keys, []string{DecisionKey("d1", 2), DecisionKey("d2", 1)}) {
		t.Fatalf("keys = %v, want reopened D1@2 and D2", got.Keys)
	}
}

// C1: a `later` before due is out of keys and counts; due_at drives next_change_at
// with no mark; at due it is fresh; an older snoozed ask resumes with its original age.
func TestProjectionLaterBeforeDue(t *testing.T) {
	due := attnT0.Add(2 * time.Hour)
	later := AttentionSubject{Key: ItemKey("l"), Kind: AttentionKindLater, Start: due, DueAt: &due}
	c := attnChain(later)

	got := ProjectChatAttention(c, attnT0)
	if len(got.Keys) != 0 || got.Counts != (AttentionCounts{}) || got.StaleLevel != AttentionNone || got.AttentionSource != nil {
		t.Fatalf("later before due leaked: %+v", got)
	}
	if got.NextChangeAt == nil || !got.NextChangeAt.Equal(due) {
		t.Fatalf("next_change_at = %v, want due_at", got.NextChangeAt)
	}

	got = ProjectChatAttention(c, due)
	if got.StaleLevel != AttentionFresh || got.Counts.Later != 1 || !slices.Equal(got.Keys, []string{ItemKey("l")}) || *got.AttentionSource != AttentionKindLater {
		t.Fatalf("later at due = %+v, want fresh", got)
	}
	if got.NextChangeAt == nil || !got.NextChangeAt.Equal(due.Add(4*time.Hour)) {
		t.Fatalf("next_change_at = %v, want due+4h", got.NextChangeAt)
	}

	// Snoozed older ask + later: ask resumes with its original age at snooze end.
	oldAsk := attnAsk("old", attnT0.Add(-30*time.Hour))
	c = attnChain(oldAsk, later)
	c.Marks[ItemKey("old")] = AttentionMark{SnoozedUntil: ptr(due)}
	got = ProjectChatAttention(c, due)
	if got.StaleLevel != AttentionRed || !got.AttentionSince.Equal(oldAsk.Start) {
		t.Fatalf("after snooze: level=%q since=%v, want red from the original start", got.StaleLevel, got.AttentionSince)
	}
}

// C2 / A20: unclassified turns are listed (collapsed group), never coloured, aged or counted
// as stale; acknowledgement is by turn key, so the next turn reappears.
func TestProjectionUnclassifiedTurns(t *testing.T) {
	turn := func(id string) AttentionSubject { return AttentionSubject{Key: TurnKey(id), Kind: AttentionKindTurn} }
	c := attnChain(turn("t1"))
	now := attnT0.Add(72 * time.Hour)

	got := ProjectChatAttention(c, now)
	if got.StaleLevel != AttentionNone || got.AttentionSource != nil || got.NextChangeAt != nil {
		t.Fatalf("unclassified turn is coloured or aged: %+v", got)
	}
	if !slices.Equal(got.Unclassified, []string{TurnKey("t1")}) || !slices.Equal(got.Keys, []string{TurnKey("t1")}) || got.Counts.Unclassified != 1 {
		t.Fatalf("unclassified=%v keys=%v counts=%+v", got.Unclassified, got.Keys, got.Counts)
	}

	c.Marks[TurnKey("t1")] = AttentionMark{Acked: true}
	if got = ProjectChatAttention(c, now); len(got.Keys) != 0 || len(got.Unclassified) != 0 {
		t.Fatalf("acknowledged turn still listed: %+v", got)
	}

	c.Subjects = []AttentionSubject{turn("t1"), turn("t2")}
	if got = ProjectChatAttention(c, now); !slices.Equal(got.Unclassified, []string{TurnKey("t2")}) {
		t.Fatalf("unclassified = %v, want the new turn t2", got.Unclassified)
	}
}
