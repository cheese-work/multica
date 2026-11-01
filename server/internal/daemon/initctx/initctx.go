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
	Budgets map[Part]int // est. tokens per part; missing or <=0 = no budget
}

type PartReport struct {
	Part   Part
	Bytes  int
	Tokens int
	Budget int
	Notes  []string
}

type Report struct{ Parts []PartReport }

// EstimateTokens is the (bytes+3)/4 heuristic.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// Build splits the inputs into parts. Part bytes sum to the input bytes.
func Build(in Input) Report {
	text := map[Part]*strings.Builder{}
	for _, p := range AllParts {
		text[p] = &strings.Builder{}
	}
	cur, fenced := PartInstructions, false
	for _, line := range strings.SplitAfter(in.Brief, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		} else if title, ok := strings.CutPrefix(line, "## "); ok && !fenced {
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
		if pr.Budget > 0 && pr.Tokens > pr.Budget {
			pr.Notes = append(pr.Notes, fmt.Sprintf("over budget by %d est. tokens", pr.Tokens-pr.Budget))
		}
		r.Parts = append(r.Parts, pr)
	}
	return r
}
