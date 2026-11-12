// Package initctx reports how the initial context of a run (runtime brief,
// first prompt, resumed-session seed) splits into parts and what each part
// costs in estimated tokens. Pure and report-only: nothing is trimmed.
package initctx

import (
	"fmt"
	"strings"
)

type Part string

const (
	PartInstructions Part = "instructions"
	PartTools        Part = "tools"
	PartSkills       Part = "skills"
	PartMemory       Part = "memory"
	PartProject      Part = "project"
	PartTask         Part = "task"
	PartSeed         Part = "seed"
)

// AllParts is the order of Report.Parts.
var AllParts = []Part{PartInstructions, PartTools, PartSkills, PartMemory, PartProject, PartTask, PartSeed}

// headingParts maps brief H2 titles to parts; unknown titles and text before
// the first H2 fall back to PartInstructions.
var headingParts = map[string]Part{
	"Available Commands": PartTools,
	"Skills":             PartSkills,
	"Memory":             PartMemory,
	"Project Context":    PartProject,
	"Workspace Context":  PartInstructions,
}

type Input struct {
	Brief   string
	Prompt  string
	Seed    string       // resumed-session seed; empty when absent
	Budgets map[Part]int // est. tokens per part; a missing key = no budget, 0 = nothing allowed
}

type PartReport struct {
	Part   Part     `json:"part"`
	Bytes  int      `json:"bytes"`
	Tokens int      `json:"tokens"`
	Budget int      `json:"budget"`
	Notes  []string `json:"notes,omitempty"`
}

type Report struct {
	Parts []PartReport `json:"parts"`
}

// EstimateTokens is the (bytes+3)/4 heuristic.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// Build splits the inputs into parts. Part bytes sum to the input bytes.
func Build(in Input) Report {
	text := map[Part]*strings.Builder{}
	for _, p := range AllParts {
		text[p] = &strings.Builder{}
	}
	cur, open := PartInstructions, fence{}
	for _, line := range strings.SplitAfter(in.Brief, "\n") {
		if f, ok := parseFence(line); ok && open.n == 0 {
			open = f
		} else if ok && f.char == open.char && f.n >= open.n && f.bare {
			open = fence{}
		} else if title, ok := strings.CutPrefix(line, "## "); ok && open.n == 0 {
			cur = PartInstructions
			if p, known := headingParts[strings.TrimSpace(title)]; known {
				cur = p
			}
		}
		text[cur].WriteString(line)
	}
	text[PartTask].WriteString(in.Prompt)
	text[PartSeed].WriteString(in.Seed)

	r := Report{}
	for _, p := range AllParts {
		s := text[p].String()
		pr := PartReport{Part: p, Bytes: len(s), Tokens: EstimateTokens(s), Budget: max(in.Budgets[p], 0)}
		if _, ok := in.Budgets[p]; ok && pr.Tokens > pr.Budget {
			pr.Notes = append(pr.Notes, fmt.Sprintf("over budget by %d est. tokens", pr.Tokens-pr.Budget))
		}
		r.Parts = append(r.Parts, pr)
	}
	return r
}

// fence is a CommonMark code fence line: up to 3 spaces, then 3+ backticks or tildes.
type fence struct {
	char byte
	n    int
	bare bool // nothing but whitespace after the run, so it can close a fence
}

func parseFence(line string) (fence, bool) {
	rest := strings.TrimLeft(line, " ")
	if len(line)-len(rest) > 3 || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return fence{}, false
	}
	after := strings.TrimLeft(rest, rest[:1])
	n := len(rest) - len(after)
	return fence{rest[0], n, strings.TrimSpace(after) == ""}, n >= 3
}
