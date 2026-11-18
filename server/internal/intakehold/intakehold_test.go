package intakehold

import (
	"math/rand"
	"testing"
	"time"
)

var (
	t0       = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	starts   = time.Date(2026, 11, 17, 9, 14, 52, 0, time.UTC)
	endsAt   = time.Date(2026, 11, 22, 17, 0, 0, 0, time.UTC)
	fiveCls  = []string{"spend", "deploy", "publish", "irreversible", "product_direction"}
	adoption = "1dc08d8e-58e4-4ab3-9ebc-3596324a5e4d"
)

func entry() *Entry {
	return &Entry{
		ProjectID: "5e621e12-eeb4-40a5-9571-25657d5b0ec5", Mode: ModeBlock, StartsAt: starts, EndsAt: endsAt,
		ExemptTypes:             []string{"fix", "bug", "review", "incident"},
		ExemptLabels:            []string{"bug", "incident"},
		ExemptEscalationClasses: fiveCls,
		ExemptAutopilotIDs:      []string{adoption},
		ExemptIssueIDs:          []string{"01a1e90c-cb5a-7e37-abba-ead8e1db017f"},
		Source:                  "src",
	}
}

func issue() Issue {
	return Issue{ID: "issue-1", Title: "feat(x): new", Status: "backlog", Priority: "medium", CreatedAt: starts.Add(time.Hour)}
}

func at(d time.Duration) *time.Time { v := t0.Add(d); return &v }

func answered(class string, a Answer) Request {
	return Request{Authority: "human", Class: class, Status: "answered", Answers: []Answer{a}}
}

func person(auth, from, to, release string) StatusRow {
	return StatusRow{ActorType: "member", CauseKind: "ui_or_api", AuthSource: auth, From: from, To: to, Release: release}
}

func TestHeldFalse(t *testing.T) {
	cases := map[string]func(*Issue, *Entry){}
	for _, typ := range []string{"fix", "bug", "review", "incident"} {
		typ := typ
		cases["title type "+typ] = func(i *Issue, _ *Entry) { i.Title = typ + "(x): y" }
	}
	for _, label := range []string{"bug", "incident"} {
		label := label
		cases["label "+label] = func(i *Issue, _ *Entry) { i.Labels = []string{label} }
	}
	for _, class := range fiveCls {
		class := class
		cases["effective answer before now "+class] = func(i *Issue, _ *Entry) {
			i.Requests = []Request{answered(class, Answer{ByPerson: true, WakeAt: at(-time.Second)})}
		}
		cases["effective answer at now "+class] = func(i *Issue, _ *Entry) {
			i.Requests = []Request{answered(class, Answer{ByPerson: true, WakeAt: at(0)})}
		}
	}
	cases["adoption autopilot"] = func(i *Issue, _ *Entry) { i.AutopilotID = adoption }
	cases["parent"] = func(i *Issue, _ *Entry) { i.HasParent = true }
	cases["created before starts_at"] = func(i *Issue, _ *Entry) { i.CreatedAt = starts.Add(-time.Second) }
	cases["exempt issue id"] = func(i *Issue, _ *Entry) { i.ID = "01a1e90c-cb5a-7e37-abba-ead8e1db017f" }
	cases["no entry"] = func(_ *Issue, e *Entry) { *e = Entry{} }
	cases["at ends_at"] = func(_ *Issue, e *Entry) { e.EndsAt = t0 }
	cases["after ends_at"] = func(_ *Issue, e *Entry) { e.EndsAt = t0.Add(-time.Second) }
	cases["before starts_at"] = func(i *Issue, e *Entry) { e.StartsAt = t0.Add(time.Second); i.CreatedAt = t0.Add(2 * time.Second) }
	cases["mode off"] = func(_ *Issue, e *Entry) { e.Mode = ModeOff }
	cases["in_progress, no park row"] = func(i *Issue, _ *Entry) { i.Status = "in_progress" }
	cases["in_progress, retained park row"] = func(i *Issue, _ *Entry) {
		i.Status, i.HasPark, i.ParkPrior, i.ParkTo = "in_progress", true, "backlog", "need_decision"
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			i, e := issue(), entry()
			edit(&i, e)
			use := e
			if name == "no entry" {
				use = nil
			}
			if Held(i, use, t0) {
				t.Fatalf("Held = true, want false")
			}
		})
	}
}

func TestHeldTrue(t *testing.T) {
	cases := map[string]func(*Issue){
		"urgent task":   func(i *Issue) { i.Title = "task(x): y"; i.Priority = "urgent" },
		"no title type": func(i *Issue) { i.Title = "just words" },
		"reversible class": func(i *Issue) {
			i.Requests = []Request{answered("reversible", Answer{ByPerson: true, WakeAt: at(-time.Hour)})}
		},
		"open request": func(i *Issue) {
			i.Requests = []Request{{Authority: "human", Class: "spend", Status: "open"}}
		},
		"cancelled request": func(i *Issue) {
			i.Requests = []Request{{Authority: "human", Class: "spend", Status: "cancelled"}}
		},
		"agent-recorded answer": func(i *Issue) {
			i.Requests = []Request{answered("spend", Answer{ByPerson: false, WakeAt: at(-time.Hour)})}
		},
		"wake_at one second ahead": func(i *Issue) {
			i.Requests = []Request{answered("spend", Answer{ByPerson: true, WakeAt: at(time.Second)})}
		},
		"wake_at null": func(i *Issue) { i.Requests = []Request{answered("spend", Answer{ByPerson: true})} },
		"withdrawn before wake_at": func(i *Issue) {
			i.Requests = []Request{answered("spend", Answer{ByPerson: true, Disclaimed: true})}
		},
		"retracted after effect": func(i *Issue) {
			i.Requests = []Request{answered("spend", Answer{ByPerson: true, WakeAt: at(-time.Hour), Disclaimed: true})}
		},
		"need_decision with park": func(i *Issue) {
			i.Status, i.HasPark, i.ParkPrior, i.ParkTo = "need_decision", true, "backlog", "need_decision"
		},
		"blocked with park": func(i *Issue) {
			i.Status, i.HasPark, i.ParkPrior, i.ParkTo = "blocked", true, "backlog", "blocked"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			i := issue()
			edit(&i)
			if !Held(i, entry(), t0) {
				v, _ := Evaluate(i, entry(), t0)
				t.Fatalf("Held = false, want true (%+v)", v)
			}
		})
	}
}

func TestPersonRelease(t *testing.T) {
	parked := func(rows ...StatusRow) Issue {
		i := issue()
		i.Status, i.HasPark, i.ParkPrior, i.ParkTo = "need_decision", true, "backlog", "need_decision"
		i.Rows = rows
		return i
	}
	backAgain := func(rows ...StatusRow) Issue {
		i := issue()
		i.HasPark, i.ParkPrior, i.ParkTo = true, "backlog", "need_decision"
		i.Rows = rows
		return i
	}
	park := StatusRow{ActorType: "agent", CauseKind: "decision", From: "backlog", To: "need_decision"}
	type tc struct {
		name    string
		issue   Issue
		release bool
	}
	var cases []tc
	for _, auth := range []string{"interactive_jwt", "cli_jwt", "pat"} {
		cases = append(cases,
			tc{"a from backlog " + auth, parked(person(auth, "backlog", "in_progress", "")), true},
			tc{"b from need_decision " + auth, parked(park, person(auth, "need_decision", "in_progress", "src")), true},
			tc{"b from blocked " + auth, parked(person(auth, "blocked", "in_progress", "src")), true},
			tc{"b back in backlog " + auth, backAgain(person(auth, "need_decision", "in_progress", "src")), true},
			tc{"b no order, no park row " + auth, parked(person(auth, "need_decision", "in_progress", "src")), true},
		)
	}
	for _, auth := range []string{"cloud_pat", "task_token"} {
		for _, from := range []string{"backlog", "need_decision"} {
			for _, key := range []string{"", "src"} {
				cases = append(cases, tc{"machine " + auth + " from " + from + " key=" + key, parked(person(auth, from, "in_progress", key)), false})
			}
		}
	}
	noAuth := person("", "need_decision", "in_progress", "src")
	remap := person("interactive_jwt", "need_decision", "in_progress", "src")
	remap.CauseKind = "workflow_remap"
	decision := remap
	decision.CauseKind = "decision"
	cases = append(cases,
		tc{"no auth_source with key", parked(noAuth), false},
		tc{"workflow_remap with key", parked(remap), false},
		tc{"decision cause with key", parked(decision), false},
		tc{"b without key, after a park from backlog", parked(park, person("interactive_jwt", "need_decision", "in_progress", "")), false},
		tc{"to backlog with key", parked(person("interactive_jwt", "need_decision", "backlog", "src")), false},
		tc{"empty key", parked(person("interactive_jwt", "need_decision", "in_progress", "")), false},
		tc{"agent row from backlog", parked(StatusRow{ActorType: "agent", CauseKind: "ui_or_api", AuthSource: "interactive_jwt", From: "backlog", To: "in_progress"}), false},
	)
	rng := rand.New(rand.NewSource(1))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The rows go to the rule in more than one order; the result is the same.
			for round := 0; round < 4; round++ {
				i := c.issue
				i.Rows = append([]StatusRow(nil), c.issue.Rows...)
				rng.Shuffle(len(i.Rows), func(a, b int) { i.Rows[a], i.Rows[b] = i.Rows[b], i.Rows[a] })
				v, applies := Evaluate(i, entry(), t0)
				released := applies && v.Reason == ReasonReleasedByMember
				if released != c.release {
					t.Fatalf("released = %v, want %v (verdict %+v)", released, c.release, v)
				}
				if c.release && Held(i, entry(), t0) {
					t.Fatalf("a released issue is held")
				}
			}
		})
	}
}

func TestVerdictReasons(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*Issue, *Entry)
		held   bool
		owned  bool
		reason string
	}{
		{"held", func(*Issue, *Entry) {}, true, true, ReasonHeld},
		{"ended", func(_ *Issue, e *Entry) { e.EndsAt = t0 }, false, true, ReasonEnded},
		{"not started", func(_ *Issue, e *Entry) { e.StartsAt = t0.Add(time.Hour) }, false, false, ReasonCreatedBeforeStart},
		{"mode off", func(_ *Issue, e *Entry) { e.Mode = ModeOff }, false, true, ReasonModeOff},
		{"created before start", func(i *Issue, _ *Entry) { i.CreatedAt = starts.Add(-time.Hour) }, false, false, ReasonCreatedBeforeStart},
		{"left backlog", func(i *Issue, _ *Entry) { i.Status = "in_progress" }, false, false, ReasonLeftBacklog},
		{"type", func(i *Issue, _ *Entry) { i.Title = "fix(x): y" }, false, false, ReasonExemptType},
		{"label", func(i *Issue, _ *Entry) { i.Labels = []string{"incident"} }, false, false, ReasonExemptLabel},
		{"autopilot", func(i *Issue, _ *Entry) { i.AutopilotID = adoption }, false, false, ReasonExemptAutopilot},
		{"issue id", func(i *Issue, _ *Entry) { i.ID = "01a1e90c-cb5a-7e37-abba-ead8e1db017f" }, false, false, ReasonExemptIssueID},
		{"escalation", func(i *Issue, _ *Entry) {
			i.Requests = []Request{answered("deploy", Answer{ByPerson: true, WakeAt: at(-time.Second)})}
		}, false, false, ReasonExemptEscalation},
		{"member release", func(i *Issue, _ *Entry) { i.Rows = []StatusRow{person("pat", "backlog", "todo", "")} }, false, false, ReasonReleasedByMember},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i, e := issue(), entry()
			c.edit(&i, e)
			v, applies := Evaluate(i, e, t0)
			if !applies || v.Held != c.held || v.Owned != c.owned || v.Reason != c.reason {
				t.Fatalf("verdict %+v applies=%v, want held=%v owned=%v reason=%s", v, applies, c.held, c.owned, c.reason)
			}
			if Owned(i, e, t0) != c.owned || Held(i, e, t0) != c.held {
				t.Fatalf("Owned/Held disagree with Evaluate")
			}
		})
	}
	t.Run("answer not yet effective", func(t *testing.T) {
		i := issue()
		i.Requests = []Request{answered("spend", Answer{ByPerson: true, WakeAt: at(10 * time.Minute)})}
		v, _ := Evaluate(i, entry(), t0)
		if !v.Held || v.Reason != ReasonHeld || v.ExemptAfter == nil || !v.ExemptAfter.Equal(t0.Add(10*time.Minute)) {
			t.Fatalf("verdict %+v", v)
		}
		v, _ = Evaluate(i, entry(), t0.Add(10*time.Minute))
		if v.Held || v.Reason != ReasonExemptEscalation || v.ExemptAfter != nil {
			t.Fatalf("after wake_at: %+v", v)
		}
	})
	t.Run("parent and no entry have no say", func(t *testing.T) {
		i := issue()
		i.HasParent = true
		if _, applies := Evaluate(i, entry(), t0); applies {
			t.Fatal("a child applies")
		}
		if _, applies := Evaluate(issue(), nil, t0); applies {
			t.Fatal("no entry applies")
		}
	})
}

func TestInScope(t *testing.T) {
	// Conditions 1 to 4 only: an exemption or an earlier release does not change what a move is.
	i := issue()
	i.Title = "fix(x): y"
	i.Rows = []StatusRow{person("pat", "backlog", "todo", "src")}
	if !InScope(i, entry(), t0) {
		t.Fatal("exempt and released issue in backlog is still in scope")
	}
	i.Status, i.HasPark, i.ParkPrior, i.ParkTo = "need_decision", true, "backlog", "need_decision"
	if !InScope(i, entry(), t0) {
		t.Fatal("parked from backlog is in scope")
	}
	i.Status = "in_progress"
	if InScope(i, entry(), t0) {
		t.Fatal("left backlog is out of scope")
	}
	i = issue()
	e := entry()
	e.EndsAt = t0
	if InScope(i, e, t0) {
		t.Fatal("after ends_at is out of scope")
	}
	e = entry()
	e.Mode = ModeOff
	if InScope(i, e, t0) {
		t.Fatal("mode off is out of scope")
	}
	i.HasParent = true
	if InScope(i, entry(), t0) {
		t.Fatal("a child is out of scope")
	}
}
