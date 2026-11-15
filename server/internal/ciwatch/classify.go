package ciwatch

import (
	"regexp"
	"strings"
)

// Class is the System 1 class of a failed run (CHE-4265, Routing order step 6).
type Class string

const (
	ClassRateLimit Class = "infra_rate_limit"
	ClassConfig    Class = "config_or_secret"
	ClassTransient Class = "infra_transient"
	ClassFlaky     Class = "flaky"
	ClassCode      Class = "code_or_test"
	ClassUnknown   Class = "unknown"
)

// Decision is the rule layer's answer. ShadowOnly is true for unknown in phase 1.
type Decision struct {
	Class      Class
	Protocol   string // P1..P4; for unknown, the protocol it would route to
	Route      string // set for unknown: "triage"
	Rule       string // which rule matched, "" for unknown
	Confidence float64
	ShadowOnly bool
}

const (
	minConfidence      = 0.6
	textConfidence     = 0.9 // a log pattern matched
	uninspectedPenalty = 0.1 // per failed job whose log was not read (jobs kept: 2)
	metaConfidence     = 0.9 // job or run metadata decided; no log needed
	flakyThreshold     = 3
)

// Log patterns, in precedence order. They run on redacted excerpts only.
var (
	reRateLimit = regexp.MustCompile(`(?i)api rate limit exceeded|secondary rate limit|(403|429)[^\n]*rate.?limit|rate.?limit[^\n]*(403|429)`)
	reConfig    = regexp.MustCompile(`(?i)input required and not supplied|resource not accessible by integration|http 403|403 forbidden|permission denied|invalid workflow file|secret[s]? [^\n]*(not set|missing|empty)`)
	reTransient = regexp.MustCompile(`(?i)runner has received a shutdown signal|runner lost|lost communication with the server|operation was canceled|connection reset by peer|econnreset|etimedout|tls handshake timeout|\b50[0234] (internal server error|bad gateway|service unavailable|gateway time-?out)|failed to (save|restore) cache|unable to reserve cache|cache service`)
	reCode      = regexp.MustCompile(`(?i)--- FAIL:|^FAIL\b|\bpanic:|assertionerror|\berror TS\d+|error\[E\d+\]|compilation failed|undefined: |cannot find symbol|golangci-lint|eslint|\d+ tests? failed|tests?:\s+\d+ failed`)
)

var protocolOf = map[Class]string{
	ClassRateLimit: "P1", ClassConfig: "P4", ClassTransient: "P1", ClassFlaky: "P3", ClassCode: "P2",
}

// Classify applies the fixed precedence of CHE-4265 step 6. First match wins.
// Partial evidence only reaches the rules that need no log text (startup_failure
// with zero jobs, runner loss, flaky counter); everything else is unknown.
// Unknown is shadow-only in phase 1 and records "would route P2 triage".
func Classify(ev *FailureEvent, h History) Decision {
	var logs []string
	for _, j := range ev.Jobs {
		logs = append(logs, j.Excerpt)
	}
	text := strings.Join(logs, "\n")
	partial := ev.Evidence != EvidenceFull
	conf := textConfidence - uninspectedPenalty*float64(max(0, ev.FailedJobCount-len(ev.Jobs)))

	hit := func(c Class, rule string, confidence float64) (Decision, bool) {
		if confidence < minConfidence {
			return Decision{}, false
		}
		return Decision{Class: c, Protocol: protocolOf[c], Rule: rule, Confidence: confidence}, true
	}
	textRule := func(c Class, re *regexp.Regexp, rule string) (Decision, bool) {
		if partial || !re.MatchString(text) {
			return Decision{}, false
		}
		return hit(c, rule, conf)
	}

	startupNoJobs := ev.Conclusion == "startup_failure" && ev.PartialReason == PartialNoJobs && ev.FailedJobCount == 0
	for _, try := range []func() (Decision, bool){
		func() (Decision, bool) { return textRule(ClassRateLimit, reRateLimit, "rate_limit_text") },
		func() (Decision, bool) {
			if startupNoJobs {
				return hit(ClassConfig, "startup_failure_no_jobs", metaConfidence)
			}
			return textRule(ClassConfig, reConfig, "config_text")
		},
		func() (Decision, bool) {
			if ev.RunnerLost {
				return hit(ClassTransient, "runner_lost_metadata", metaConfidence)
			}
			return textRule(ClassTransient, reTransient, "transient_text")
		},
		func() (Decision, bool) {
			if h.FlakeCount7d >= flakyThreshold {
				return hit(ClassFlaky, "flake_counter", metaConfidence)
			}
			return Decision{}, false
		},
		func() (Decision, bool) { return textRule(ClassCode, reCode, "code_text") }, // textRule skips partial evidence
	} {
		if d, ok := try(); ok {
			return d
		}
	}
	return Decision{Class: ClassUnknown, Protocol: "P2", Route: "triage", ShadowOnly: true}
}
