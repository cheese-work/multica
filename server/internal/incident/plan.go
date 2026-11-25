package incident

import (
	"sort"
	"time"
)

// Open is the stored state of one open incident. The caller keeps it on the
// incident issue (metadata), so no table is needed.
type Open struct {
	IssueID       string
	Class         Class
	LastFailureAt time.Time // newest failure the incident has recorded
	Notified      bool      // the one DM went out
	RunIDs        []string  // failed runs recorded on the incident, retried once at close
}

type ActionKind string

const (
	ActOpen    ActionKind = "open"    // create the incident issue for Cluster
	ActComment ActionKind = "comment" // add NewRuns to the open incident
	ActNotify  ActionKind = "notify"  // send the one notice; never repeated
	ActClose   ActionKind = "close"   // close the incident, retry RetryRunIDs once
)

type Action struct {
	Kind        ActionKind
	Class       Class
	IssueID     string // empty for ActOpen
	Cluster     *Cluster
	NewRuns     []Run
	RetryRunIDs []string
}

// Plan decides what to do this tick. recent is every classified failure the
// caller read, which may include runs older than the window.
//   - A qualifying cluster with no open incident opens one (and notifies).
//   - An open incident adds the runs newer than its last record as one comment.
//     No second issue, no second notice.
//   - An open incident whose class has had no failure for CloseAfter closes
//     and names the runs to retry once.
func Plan(open []Open, recent []Run, now time.Time, cfg Config) []Action {
	openByClass := map[Class]Open{}
	for _, o := range open {
		openByClass[o.Class] = o
	}
	var acts []Action
	for _, c := range Detect(recent, now, cfg) {
		c := c
		if _, ok := openByClass[c.Class]; !ok {
			acts = append(acts, Action{Kind: ActOpen, Class: c.Class, Cluster: &c}, Action{Kind: ActNotify, Class: c.Class})
		}
	}
	for _, o := range open {
		// An open incident records every new failure of its class, even once
		// the burst is too thin to qualify: those runs still get the one retry.
		var classRuns []Run
		for _, r := range recent {
			if r.Class == o.Class {
				classRuns = append(classRuns, r)
			}
		}
		sort.SliceStable(classRuns, func(i, j int) bool { return classRuns[i].FailedAt.Before(classRuns[j].FailedAt) })
		if fresh := newerThan(classRuns, o.LastFailureAt); len(fresh) > 0 {
			acts = append(acts, Action{Kind: ActComment, Class: o.Class, IssueID: o.IssueID, NewRuns: fresh})
		}
		if !o.Notified {
			acts = append(acts, Action{Kind: ActNotify, Class: o.Class, IssueID: o.IssueID})
		}
		last := o.LastFailureAt
		for _, r := range recent {
			if r.Class == o.Class && r.FailedAt.After(last) {
				last = r.FailedAt
			}
		}
		if now.Sub(last) >= cfg.CloseAfter {
			acts = append(acts, Action{Kind: ActClose, Class: o.Class, IssueID: o.IssueID, RetryRunIDs: o.RunIDs})
		}
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Class < acts[j].Class })
	return acts
}

func newerThan(rs []Run, t time.Time) []Run {
	var out []Run
	for _, r := range rs {
		if r.FailedAt.After(t) {
			out = append(out, r)
		}
	}
	return out
}
