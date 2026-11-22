package guard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Plugin 2 (CHE-5194 T4): a run never waits on CI inside a tool call. The call
// holds a run slot while it does no work; the wakeup line says what to do
// instead. Same rule as the Claude hook (CHE-1965) and the Codex hook
// (CHE-5176), but for every runtime that resolves gh and sleep to the shims.

// sleepLimitSeconds is the shortest sleep the guard refuses, as in CHE-1965.
const sleepLimitSeconds = 20

// ciSpec lists the flags of `gh pr checks` and `gh run watch` that take a
// value. -w is --web on `pr checks`, not --watch, so it is not mapped.
var ciSpec = flagSpec{
	valued: set("repo", "interval", "json", "jq", "template", "hostname"),
	short:  map[byte]string{'R': "repo", 'i': "interval", 'q': "jq", 't': "template"},
}

const (
	waitPRLine  = `gh pr checks --watch is refused in runs; run "multica issue wakeup create <issue> --until-pr checks --expires-in 2h --on-timeout wake" and end the run`
	waitRunLine = `gh run watch is refused in runs; run "multica issue wakeup create <issue> --until-gh-run owner/repo:RUN_ID --until-gh-run-state completed --expires-in 2h --on-timeout wake" and end the run`
	sleepLine   = `sleep of %ds or more is refused in runs; run "multica issue wakeup create <issue> --after <duration>" or a condition (--until-pr checks) and end the run`
)

// ciWait reports whether the call is a CI wait and the guard line for it. It
// reads the positional words after flag parsing, so -R and its value may sit
// anywhere. Aliases are expanded before this runs.
func ciWait(tool string, args []string) (line string, hit bool) {
	switch tool {
	case "gh":
		flags, pos := ciSpec.scan(args)
		if len(pos) < 2 {
			return "", false
		}
		switch {
		case pos[0] == "run" && pos[1] == "watch":
			return waitRunLine, true
		case pos[0] == "pr" && pos[1] == "checks" && isOn(flags, "watch"):
			return waitPRLine, true
		}
	case "sleep":
		if secs, ok := sleepSeconds(args); ok && secs >= sleepLimitSeconds {
			return fmt.Sprintf(sleepLine, sleepLimitSeconds), true
		}
	}
	return "", false
}

var sleepArg = regexp.MustCompile(`^(\d+\.?\d*|\.\d+)([smhd]?)$`)

var sleepUnit = map[string]float64{"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}

// sleepSeconds totals the durations of a sleep call like GNU sleep. ok is
// false when an argument is not a duration; the real sleep then reports it.
func sleepSeconds(args []string) (secs float64, ok bool) {
	for _, a := range args {
		if a == "--" {
			continue
		}
		if a == "infinity" {
			return sleepLimitSeconds, true
		}
		m := sleepArg.FindStringSubmatch(a)
		if m == nil {
			return 0, false
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, false
		}
		secs += n * sleepUnit[m[2]]
	}
	return secs, true
}

// graphqlWrite reports whether a `gh api graphql` call may change data: a
// query that is a mutation, a query read from a file, or a body from --input.
// The path-based rule on repos/* does not see these.
func graphqlWrite(flags []flagTok) bool {
	if _, ok := find(flags, "input"); ok {
		return true
	}
	for _, f := range flags {
		if f.name != "field" && f.name != "raw-field" {
			continue
		}
		k, v, _ := strings.Cut(f.value, "=")
		if k != "query" {
			continue
		}
		if strings.HasPrefix(v, "@") || mutationRe.MatchString(v) {
			return true
		}
	}
	return false
}

var mutationRe = regexp.MustCompile(`(?i)\bmutation\b`)
