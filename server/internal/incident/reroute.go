package incident

import "sort"

type Role string

const (
	RoleAuthor Role = "author"
	RoleReview Role = "review"
)

// Work is one queued or failed run on an affected agent.
type Work struct {
	RunID, AgentID, IssueID string
	Role                    Role
	Skills                  []string // skills the work needs
	// AuthorLab is the lab of whoever authored the head under review (review work).
	AuthorLab string
	// ReviewerLab is the lab of the reviewer the issue is bound to (author work).
	// RGS-1: the reviewer's lab must differ from the author's.
	ReviewerLab string
}

// Agent is a candidate target.
type Agent struct {
	ID, Lab   string
	Skills    []string
	FreeSlots int
	Affected  bool // runs of the incident class fail on this agent; never a target
}

type Verdict string

const (
	Reroute    Verdict = "reroute"
	KeepQueued Verdict = "keep_queued"
)

// Decision is what happens to one piece of work.
type Decision struct {
	Work    Work
	Verdict Verdict
	To      string // agent id for Reroute
	Note    string // visible note for KeepQueued
}

// PlanReroute moves author work to a healthy agent with a free slot and the
// needed skills, never one in the reviewer's lab (RGS-1). Review work moves only
// to a healthy agent outside the author's lab; when none exists it stays queued
// with a note. Review work is never dropped and never goes to the author's lab.
// Slots are spent as work is placed, so one agent is not overfilled.
func PlanReroute(work []Work, agents []Agent) []Decision {
	slots := map[string]int{}
	for _, a := range agents {
		slots[a.ID] = a.FreeSlots
	}
	sorted := append([]Agent(nil), agents...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make([]Decision, 0, len(work))
	for _, w := range work {
		d := Decision{Work: w, Verdict: KeepQueued}
		forbiddenLab := w.ReviewerLab
		if w.Role == RoleReview {
			forbiddenLab = w.AuthorLab
		}
		best := -1
		for i, a := range sorted {
			if a.Affected || slots[a.ID] <= 0 || a.ID == w.AgentID || !hasAll(a.Skills, w.Skills) {
				continue
			}
			if forbiddenLab != "" && a.Lab == forbiddenLab {
				continue
			}
			if best < 0 || slots[a.ID] > slots[sorted[best].ID] {
				best = i
			}
		}
		if best >= 0 {
			d.Verdict, d.To = Reroute, sorted[best].ID
			slots[d.To]--
		} else if w.Role == RoleReview {
			d.Note = "Review waits: provider incident, and no healthy reviewer outside the author's lab. It stays queued and is not self-reviewed."
		} else {
			d.Note = "Author work waits: no healthy agent with free slots and the needed skills."
		}
		out = append(out, d)
	}
	return out
}

func hasAll(have, need []string) bool {
	set := map[string]bool{}
	for _, s := range have {
		set[s] = true
	}
	for _, s := range need {
		if !set[s] {
			return false
		}
	}
	return true
}
