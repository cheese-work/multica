package incident

import (
	"sort"
	"time"
)

const (
	StepOpen    = "open"
	StepComment = "comment"
	StepClose   = "close"
)

// Open is an incident issue that is still open. LastFailure is the newest
// failure of its class the issue already reports.
type Open struct{ LastFailure time.Time }

// Step is one action for the caller. Incident is set for open and comment.
type Step struct {
	Kind, Class string
	Incident    Incident
	LastFailure time.Time // newest failure of the class: what the issue should report next
}

// Plan decides what to do with the failed runs of the last Window and the
// open incident per class: open one on a first hit, comment on a newer
// failure, close after Window with no failure of the class. Output is sorted
// by class.
func Plan(runs []FailedRun, open map[string]Open, now time.Time) []Step {
	newest := map[string]time.Time{}
	for _, r := range runs {
		if c := Classify(r.Error); c != "" && !r.At.After(now) && r.At.After(newest[c]) {
			newest[c] = r.At
		}
	}
	var out []Step
	for _, in := range Detect(runs, now) {
		o, isOpen := open[in.Class]
		switch {
		case !isOpen:
			out = append(out, Step{Kind: StepOpen, Class: in.Class, Incident: in, LastFailure: newest[in.Class]})
		case newest[in.Class].After(o.LastFailure):
			out = append(out, Step{Kind: StepComment, Class: in.Class, Incident: in, LastFailure: newest[in.Class]})
		}
	}
	for class, o := range open {
		last := o.LastFailure
		if newest[class].After(last) {
			last = newest[class]
		}
		if now.Sub(last) > Window {
			out = append(out, Step{Kind: StepClose, Class: class, LastFailure: last})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}
