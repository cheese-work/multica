package incident

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

var t0 = time.Date(2026, 11, 25, 6, 30, 0, 0, time.UTC) // 13:30 ICT

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func TestClassOf(t *testing.T) {
	cases := []struct {
		reason, text string
		want         Class
		ok           bool
	}{
		// The 25/11 error classifies as agent_error.unknown, so text must decide.
		{string(taskfailure.Classify("Concurrency limit exceeded for account")), "Concurrency limit exceeded for account", ClassConcurrencyLimit, true},
		{string(taskfailure.ReasonAgentProviderCapacityOrRateLimit), "429", ClassRateLimit, true},
		{string(taskfailure.ReasonAgentProviderNetwork), "stream disconnected", ClassStreamDisconnect, true},
		{string(taskfailure.ReasonAgentProviderAuthOrAccess), "401", ClassAuth, true},
		{string(taskfailure.ReasonAgentProviderQuotaLimit), "402", ClassQuota, true},
		{string(taskfailure.ReasonAgentProcessFailure), "exit 1", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		got, ok := ClassOf(c.reason, c.text)
		if got != c.want || ok != c.ok {
			t.Errorf("ClassOf(%q,%q) = %q,%v want %q,%v", c.reason, c.text, got, ok, c.want, c.ok)
		}
	}
}

func run(id, agent, issue string, min int, c Class) Run {
	return Run{ID: id, AgentID: agent, RuntimeID: "x99-codex", IssueID: issue, Class: c, FailedAt: at(min)}
}

func TestDetectThresholds(t *testing.T) {
	cfg := DefaultConfig()
	cc := ClassConcurrencyLimit
	tests := []struct {
		name string
		runs []Run
		want int
	}{
		{"three runs on two issues opens", []Run{run("1", "a", "I1", 0, cc), run("2", "b", "I1", 1, cc), run("3", "a", "I2", 2, cc)}, 1},
		{"two runs is noise", []Run{run("1", "a", "I1", 0, cc), run("2", "b", "I2", 1, cc)}, 0},
		{"one issue retrying is not an incident", []Run{run("1", "a", "I1", 0, cc), run("2", "a", "I1", 1, cc), run("3", "a", "I1", 2, cc)}, 0},
		{"outside the 30 minute window", []Run{run("1", "a", "I1", -40, cc), run("2", "b", "I2", -35, cc), run("3", "a", "I3", 0, cc)}, 0},
		{"mixed classes do not add up", []Run{run("1", "a", "I1", 0, cc), run("2", "b", "I2", 1, ClassRateLimit), run("3", "a", "I3", 2, ClassAuth)}, 0},
	}
	for _, tc := range tests {
		if got := len(Detect(tc.runs, at(5), cfg)); got != tc.want {
			t.Errorf("%s: %d clusters, want %d", tc.name, got, tc.want)
		}
	}
}

// replayRuns reconstructs the 25/11 history from CHE-9892: from 13:30 ICT every
// X99 Codex agent fails with "Concurrency limit exceeded for account"; CHE-9825
// failed 3 times on Luna-2, CHE-9775 and CHE-9755 3 times each. Failure times are
// spread over 3 hours because the exact run timestamps were not exported.
func replayRuns() []Run {
	cc := ClassConcurrencyLimit
	return []Run{
		run("r01", "sol-200k", "CHE-9765", 0, cc),
		run("r02", "luna-2", "CHE-9766", 3, cc),
		run("r03", "terra", "CHE-9775", 8, cc),
		run("r04", "luna-2", "CHE-9825", 15, cc),
		run("r05", "luna", "CHE-9755", 22, cc),
		run("r06", "terra", "CHE-9775", 40, cc),
		run("r07", "luna-2", "CHE-9825", 55, cc),
		run("r08", "luna", "CHE-9755", 70, cc),
		run("r09", "terra", "CHE-9775", 100, cc),
		run("r10", "luna-2", "CHE-9825", 125, cc),
		run("r11", "luna", "CHE-9755", 150, cc),
		run("r12", "sol", "CHE-9766", 165, cc),
	}
}

// simulate runs the monitor every 10 minutes against the history and applies
// each action to the stored state, the way an executor would.
func simulate(runs []Run, until int) (acts []Action, open []Open, closedAt int) {
	closedAt = -1
	cfg := DefaultConfig()
	for m := 0; m <= until; m += 10 {
		now := at(m)
		var seen []Run
		for _, r := range runs {
			if !r.FailedAt.After(now) {
				seen = append(seen, r)
			}
		}
		for _, a := range Plan(open, seen, now, cfg) {
			acts = append(acts, a)
			switch a.Kind {
			case ActOpen:
				o := Open{IssueID: "INC-1", Class: a.Class, LastFailureAt: a.Cluster.Last}
				for _, r := range a.Cluster.Runs {
					o.RunIDs = append(o.RunIDs, r.ID)
				}
				open = append(open, o)
			case ActComment:
				for i := range open {
					if open[i].IssueID == a.IssueID {
						for _, r := range a.NewRuns {
							open[i].RunIDs = append(open[i].RunIDs, r.ID)
							open[i].LastFailureAt = r.FailedAt
						}
					}
				}
			case ActNotify:
				for i := range open {
					if open[i].Class == a.Class {
						open[i].Notified = true
					}
				}
			case ActClose:
				closedAt = m
				var keep []Open
				for _, o := range open {
					if o.IssueID != a.IssueID {
						keep = append(keep, o)
					}
				}
				open = keep
			}
		}
	}
	return acts, open, closedAt
}

func count(acts []Action, k ActionKind) int {
	n := 0
	for _, a := range acts {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func TestReplay25Nov_OneIncidentOneNoticeThenClose(t *testing.T) {
	acts, open, closedAt := simulate(replayRuns(), 240)
	if got := count(acts, ActOpen); got != 1 {
		t.Fatalf("incidents opened = %d, want exactly 1", got)
	}
	if got := count(acts, ActNotify); got != 1 {
		t.Fatalf("notices = %d, want exactly 1 (the DM goes out once)", got)
	}
	if got := count(acts, ActComment); got == 0 {
		t.Fatal("later failures must add comments to the open incident")
	}
	if got := count(acts, ActClose); got != 1 || len(open) != 0 {
		t.Fatalf("closes = %d, open left = %d; want 1 and 0", got, len(open))
	}
	for _, a := range acts {
		if a.Kind == ActClose && len(a.RetryRunIDs) != 12 {
			t.Errorf("retry set = %d runs, want all 12 failed runs", len(a.RetryRunIDs))
		}
	}
	// last failure at +165; the first 10 minute tick 30 quiet minutes later is +200.
	if closedAt != 200 {
		t.Errorf("closed at +%d, want +200", closedAt)
	}
}

func TestNoCloseWhileClassStillFails(t *testing.T) {
	_, open, _ := simulate(replayRuns(), 190) // last failure +165; 25 quiet minutes at +190
	if len(open) != 1 {
		t.Fatalf("incident closed after only 25 quiet minutes")
	}
}

func TestSecondClassOpensItsOwnIncident(t *testing.T) {
	runs := append(replayRuns(),
		run("a1", "x", "I1", 20, ClassAuth), run("a2", "y", "I2", 21, ClassAuth), run("a3", "z", "I3", 22, ClassAuth))
	acts, _, _ := simulate(runs, 60)
	if got := count(acts, ActOpen); got != 2 {
		t.Fatalf("opened %d incidents for 2 classes, want 2", got)
	}
}

func TestReplay25Nov_RerouteAuthorKeepReviewsQueued(t *testing.T) {
	agents := []Agent{
		{ID: "sol-200k", Lab: "openai", Skills: []string{"go", "review"}, FreeSlots: 2, Affected: true},
		{ID: "luna-2", Lab: "openai", Skills: []string{"go", "review"}, FreeSlots: 2, Affected: true},
		{ID: "terra", Lab: "openai", Skills: []string{"go", "review"}, FreeSlots: 2, Affected: true},
		{ID: "claude-sonnet", Lab: "anthropic", Skills: []string{"go", "review"}, FreeSlots: 2},
		{ID: "claude-opus", Lab: "anthropic", Skills: []string{"go", "review"}, FreeSlots: 1},
	}
	work := []Work{
		// Author fixes stalled on Codex; the reviewer is Codex too, so Anthropic may author.
		{RunID: "r03", AgentID: "terra", IssueID: "CHE-9775", Role: RoleAuthor, Skills: []string{"go"}, ReviewerLab: "openai"},
		{RunID: "r05", AgentID: "luna", IssueID: "CHE-9755", Role: RoleAuthor, Skills: []string{"go"}, ReviewerLab: "openai"},
		// Reviews of Anthropic-authored heads: the only other lab is down.
		{RunID: "r04", AgentID: "luna-2", IssueID: "CHE-9825", Role: RoleReview, Skills: []string{"review"}, AuthorLab: "anthropic"},
		// An author step whose reviewer is Anthropic cannot go to Anthropic (RGS-1).
		{RunID: "r09", AgentID: "terra", IssueID: "CHE-9766", Role: RoleAuthor, Skills: []string{"go"}, ReviewerLab: "anthropic"},
		// A skill nobody healthy has.
		{RunID: "r10", AgentID: "terra", IssueID: "CHE-9765", Role: RoleAuthor, Skills: []string{"android"}, ReviewerLab: "openai"},
	}
	got := map[string]Decision{}
	for _, d := range PlanReroute(work, agents) {
		got[d.Work.RunID] = d
	}
	for _, id := range []string{"r03", "r05"} {
		if d := got[id]; d.Verdict != Reroute || (d.To != "claude-sonnet" && d.To != "claude-opus") {
			t.Errorf("%s: %+v, want reroute to a healthy Anthropic agent", id, d)
		}
	}
	if d := got["r04"]; d.Verdict != KeepQueued || d.Note == "" {
		t.Errorf("review r04: %+v, want keep_queued with a visible note", d)
	}
	for _, id := range []string{"r09", "r10"} {
		if d := got[id]; d.Verdict != KeepQueued || d.Note == "" {
			t.Errorf("%s: %+v, want keep_queued with a note", id, d)
		}
	}
}

func TestRerouteSpendsSlotsAndNeverOverfills(t *testing.T) {
	agents := []Agent{{ID: "h", Lab: "anthropic", Skills: []string{"go"}, FreeSlots: 1}}
	work := []Work{
		{RunID: "a", AgentID: "x", Role: RoleAuthor, Skills: []string{"go"}},
		{RunID: "b", AgentID: "x", Role: RoleAuthor, Skills: []string{"go"}},
	}
	ds := PlanReroute(work, agents)
	if ds[0].Verdict != Reroute || ds[1].Verdict != KeepQueued {
		t.Fatalf("decisions = %+v, want first rerouted, second queued", ds)
	}
}

func TestReviewMovesToHealthyOtherLab(t *testing.T) {
	agents := []Agent{{ID: "codex", Lab: "openai", Skills: []string{"review"}, FreeSlots: 1}}
	w := []Work{{RunID: "r", AgentID: "claude", Role: RoleReview, Skills: []string{"review"}, AuthorLab: "anthropic"}}
	if d := PlanReroute(w, agents)[0]; d.Verdict != Reroute || d.To != "codex" {
		t.Fatalf("got %+v, want review rerouted to the healthy other-lab agent", d)
	}
}

func TestRenderNamesAndTitle(t *testing.T) {
	cs := Detect(replayRuns(), at(45), DefaultConfig())
	if len(cs) != 1 {
		t.Fatalf("clusters = %d", len(cs))
	}
	names := Names(func(kind, id string) string {
		if kind == "agent" {
			return "@" + id
		}
		return ""
	})
	if got := Title(cs[0]); got != "incident(provider): concurrency_limit, 3 failed runs on 3 issues" {
		t.Errorf("title = %q", got)
	}
	body := Body(cs[0], names)
	for _, want := range []string{"@luna-2", "CHE-9825", "30 minutes"} {
		if !strings.Contains(body, want) {
			t.Errorf("body misses %q:\n%s", want, body)
		}
	}
}
