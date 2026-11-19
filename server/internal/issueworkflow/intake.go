package issueworkflow

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// intake_simple and qa_proof (CHE-7668, CHE-7658 step 3): entry gates of the
// Quick workflow. intake_simple holds when the author's intake block says the
// issue is simple; qa_proof holds when the exiting agent left QA proof and
// said what work remains after close. Both read text an agent wrote, so every
// value that cannot be read fails closed.

const (
	GuardIntakeSimple = "intake_simple"
	GuardQAProof      = "qa_proof"

	// MaxIntakeParamHours bounds the Param of intake_simple: a "simple" cap
	// above a working day is a misconfiguration.
	MaxIntakeParamHours = 24.0
)

// IntakeLines are the lines of the intake block, in the order they are
// reported.
var IntakeLines = []string{"Estimate", "Repos", "Schema change", "Security change", "Human decision"}

var (
	intakeLineRe = regexp.MustCompile(`(?i)^[\s>*_\-]*(Estimate|Repos|Schema change|Security change|Human decision)[\s*_]*:[\s*_]*(.*?)[\s*_]*$`)
	estimateRe   = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(h|hr|hrs|hour|hours|m|min|mins|minute|minutes)?$`)
	reposCountRe = regexp.MustCompile(`^\d+$`)
	httpURLRe    = regexp.MustCompile(`(?i)^https?://\S+$`)
)

// IntakeFacts is what intake_simple reads (CHE-7668).
type IntakeFacts struct {
	// Err is a failed read; the gate fails closed.
	Err string
	// Lines maps a canonical line name to its newest value.
	Lines map[string]string
	// OpenDecision is true while the issue has an open decision request.
	OpenDecision bool
}

// ParseIntakeLines returns the intake lines found in text, keyed by canonical
// name. A later line replaces an earlier one. Markdown bullets, quotes and
// bold around the name are ignored; an empty value does not count as a line.
func ParseIntakeLines(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		m := intakeLineRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if len(m) < 3 || m[2] == "" {
			continue
		}
		for _, name := range IntakeLines {
			if strings.EqualFold(name, m[1]) {
				out[name] = m[2]
			}
		}
	}
	return out
}

// ParamHours reads the Param of an intake_simple gate: a positive number of
// hours, at most MaxIntakeParamHours.
func ParamHours(param string) (float64, bool) {
	h, err := strconv.ParseFloat(strings.TrimSpace(param), 64)
	if err != nil || h <= 0 || h > MaxIntakeParamHours || h != h {
		return 0, false
	}
	return h, true
}

// parseEstimateHours reads "1", "1h", "0.5 hours", "45m" or "30 min".
func parseEstimateHours(v string) (float64, bool) {
	m := estimateRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	if u := strings.ToLower(m[2]); strings.HasPrefix(u, "m") {
		n /= 60
	}
	return n, true
}

// repoCount reads a count ("1") or a comma-separated list of owner/name.
func repoCount(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if reposCountRe.MatchString(v) {
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	n := 0
	for _, name := range strings.Split(v, ",") {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t") || strings.Count(name, "/") != 1 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
			return 0, false
		}
		n++
	}
	return n, true
}

func yesNo(v string) (yes, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// Estimate is the raw Estimate value, "" when the line is missing.
func (f *IntakeFacts) Estimate() string {
	if f == nil {
		return ""
	}
	return f.Lines["Estimate"]
}

// Refusal is empty when the issue reads as simple for a cap of maxHours; else
// every fact that rules it out, joined.
func (f *IntakeFacts) Refusal(maxHours float64) string {
	if f == nil {
		return "intake facts were not loaded"
	}
	if f.Err != "" {
		return f.Err
	}
	var facts []string
	for _, name := range IntakeLines {
		if f.Lines[name] == "" {
			facts = append(facts, name+": line is missing")
		}
	}
	if v := f.Lines["Estimate"]; v != "" {
		switch h, ok := parseEstimateHours(v); {
		case !ok:
			facts = append(facts, fmt.Sprintf("Estimate: %q is not a duration like 1h or 45m", v))
		case h > maxHours+1e-9:
			facts = append(facts, fmt.Sprintf("Estimate: %q is over the %s h cap", v, strconv.FormatFloat(maxHours, 'f', -1, 64)))
		}
	}
	if v := f.Lines["Repos"]; v != "" {
		switch n, ok := repoCount(v); {
		case !ok:
			facts = append(facts, fmt.Sprintf("Repos: %q is not a count or a list of owner/name repositories", v))
		case n > 1:
			facts = append(facts, fmt.Sprintf("Repos: %d repos, more than 1 repo", n))
		}
	}
	for _, name := range []string{"Schema change", "Security change", "Human decision"} {
		if v := f.Lines[name]; v != "" {
			switch yes, ok := yesNo(v); {
			case !ok:
				facts = append(facts, fmt.Sprintf("%s: %q is not yes or no", name, v))
			case yes:
				facts = append(facts, name+": yes")
			}
		}
	}
	if f.OpenDecision {
		facts = append(facts, "an open decision request is linked")
	}
	return strings.Join(facts, "; ")
}

func intakeFix() string {
	return "write the lines Estimate:, Repos:, Schema change:, Security change: and Human decision: in a comment with a simple answer for each (Estimate within the cap, one repo, no, no, no) and close any open decision request, or move the issue to the full workflow"
}

// IntakeCall reads the call out of an evaluation: "rejected" when the
// intake_simple gate failed, else "simple", and the raw Estimate value.
func IntakeCall(failed []EntryFailure, f *IntakeFacts) (call, estimate string) {
	call = "simple"
	for _, e := range failed {
		if e.Guard == GuardIntakeSimple {
			call = "rejected"
		}
	}
	return call, f.Estimate()
}

// QAFacts is what qa_proof reads (CHE-7668).
type QAFacts struct {
	// Err is a failed read; the gate fails closed.
	Err string
	// Bodies are the comments the exiting agent wrote since the issue entered
	// the step.
	Bodies []string
	// PostCloseWork is the issue's post_close_work metadata ("" when unset or
	// not a string).
	PostCloseWork string
}

// HasQALine reports whether a comment carries a "QA:" line with proof: an
// http(s) URL as the value, or a closed fenced block with content right after
// the line.
func HasQALine(body string) bool {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		m := qaLineRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		if httpURLRe.MatchString(m[1]) || fencedBlockFollows(lines[i+1:]) {
			return true
		}
	}
	return false
}

var qaLineRe = regexp.MustCompile(`(?i)^[\s>*_\-]*QA[\s*_]*:[\s*_]*(.*?)[\s*_]*$`)

// fencedBlockFollows reports whether the next non-blank line opens a ``` fence
// that closes with at least one non-blank line inside.
func fencedBlockFollows(rest []string) bool {
	i := 0
	for i < len(rest) && strings.TrimSpace(rest[i]) == "" {
		i++
	}
	if i >= len(rest) || !strings.HasPrefix(strings.TrimSpace(rest[i]), "```") {
		return false
	}
	content := false
	for _, l := range rest[i+1:] {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			return content
		}
		if strings.TrimSpace(l) != "" {
			content = true
		}
	}
	return false
}

// Refusal is empty when the gate holds, else the facts that refuse it.
func (f *QAFacts) Refusal() string {
	if f == nil {
		return "QA facts were not loaded"
	}
	if f.Err != "" {
		return f.Err
	}
	var facts []string
	proof := false
	for _, b := range f.Bodies {
		if HasQALine(b) {
			proof = true
			break
		}
	}
	if !proof {
		facts = append(facts, "no comment by the exiting agent since the issue entered the step has a QA: line with a URL or a fenced block")
	}
	if strings.TrimSpace(f.PostCloseWork) == "" {
		facts = append(facts, "post_close_work is not set")
	}
	return strings.Join(facts, "; ")
}

func qaFix() string {
	return "comment a QA: line with a URL or a fenced block of test output, then set the post_close_work metadata (none, or the work left)"
}

// validateGateParam: only intake_simple takes a Param, and it needs one.
func validateGateParam(g Guard, e EntryGate) error {
	if g.Kind != GuardIntakeSimple {
		if e.Param != "" {
			return invalid("entry gate %q takes no param", e.Guard)
		}
		return nil
	}
	if _, ok := ParamHours(e.Param); !ok {
		return invalid("entry gate %q needs param: the estimate cap in hours, above 0 and at most %g, e.g. \"1\"", e.Guard, MaxIntakeParamHours)
	}
	return nil
}
