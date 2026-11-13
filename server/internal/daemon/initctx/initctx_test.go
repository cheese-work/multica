package initctx

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	for bytes, want := range map[int]int{0: 0, 1: 1, 3: 1, 4: 1, 5: 2} {
		if got := EstimateTokens(strings.Repeat("a", bytes)); got != want {
			t.Errorf("EstimateTokens(%d bytes) = %d, want %d", bytes, got, want)
		}
	}
}

const brief = "preamble\n\n## Available Commands\n\ntools body\n\n## Skills\n\nskills body\n\n" +
	"## Project Context\n\nproject body\n\n## Workspace Context\n\nws body\n\n## Mystery\n\nunknown body\n"

func parts(r Report) map[Part]PartReport {
	m := map[Part]PartReport{}
	for _, p := range r.Parts {
		m[p.Part] = p
	}
	return m
}

func TestBuildMapsHeadingsAndSumsBytes(t *testing.T) {
	prompt, seed := "do the task", "resumed seed"
	r := Build(Input{Brief: brief, Prompt: prompt, Seed: seed})
	if len(r.Parts) != len(AllParts) {
		t.Fatalf("got %d parts, want %d", len(r.Parts), len(AllParts))
	}
	m, sum := parts(r), 0
	for _, p := range r.Parts {
		sum += p.Bytes
	}
	if want := len(brief) + len(prompt) + len(seed); sum != want {
		t.Fatalf("part bytes sum = %d, want %d", sum, want)
	}
	if m[PartTools].Bytes != len("## Available Commands\n\ntools body\n\n") {
		t.Errorf("tools bytes = %d", m[PartTools].Bytes)
	}
	if m[PartSkills].Bytes == 0 || m[PartProject].Bytes == 0 {
		t.Error("skills/project empty")
	}
	// preamble + Workspace Context + unknown heading all land in instructions.
	want := len("preamble\n\n") + len("## Workspace Context\n\nws body\n\n") + len("## Mystery\n\nunknown body\n")
	if m[PartInstructions].Bytes != want {
		t.Errorf("instructions bytes = %d, want %d", m[PartInstructions].Bytes, want)
	}
	if m[PartTask].Bytes != len(prompt) || m[PartSeed].Bytes != len(seed) {
		t.Errorf("task/seed bytes = %d/%d", m[PartTask].Bytes, m[PartSeed].Bytes)
	}
}

func TestBuildEmptySeedAndMemory(t *testing.T) {
	m := parts(Build(Input{Brief: brief, Prompt: "p"}))
	if m[PartSeed].Bytes != 0 || m[PartMemory].Bytes != 0 {
		t.Errorf("seed/memory should be empty: %+v %+v", m[PartSeed], m[PartMemory])
	}
}

func TestBuildIgnoresHeadingsInCodeFences(t *testing.T) {
	b := "## Skills\n\n```\n## Project Context\n```\n"
	m := parts(Build(Input{Brief: b}))
	if m[PartProject].Bytes != 0 || m[PartSkills].Bytes != len(b) {
		t.Errorf("fenced heading split the section: %+v", m)
	}
}

func TestBuildOverBudgetNote(t *testing.T) {
	r := Build(Input{Brief: brief, Prompt: strings.Repeat("x", 40), Budgets: map[Part]int{PartTask: 4, PartSeed: 4}})
	m := parts(r)
	if m[PartTask].Budget != 4 || len(m[PartTask].Notes) != 1 ||
		m[PartTask].Notes[0] != "over budget by 6 est. tokens" {
		t.Errorf("task = %+v", m[PartTask])
	}
	if len(m[PartSeed].Notes) != 0 || len(m[PartTools].Notes) != 0 {
		t.Error("unexpected notes on under-budget or unbudgeted parts")
	}
	if m[PartTask].Bytes != 40 {
		t.Error("report-only: part must not be trimmed")
	}
}

func TestBuildFenceVariants(t *testing.T) {
	for name, fence := range map[string][2]string{
		"tilde":    {"~~~", "~~~"},
		"indented": {"  ```", "  ```"},
		"long":     {"````", "````"},
	} {
		b := "## Skills\n\n" + fence[0] + "\n## Project Context\n```\n" + fence[1] + "\n"
		m := parts(Build(Input{Brief: b}))
		if m[PartProject].Bytes != 0 || m[PartSkills].Bytes != len(b) {
			t.Errorf("%s: fenced heading split the section: %+v", name, m)
		}
	}
}

func TestBuildZeroBudgetIsABudget(t *testing.T) {
	m := parts(Build(Input{Brief: brief, Prompt: "12345", Budgets: map[Part]int{PartTask: 0, PartSeed: 0}}))
	if got := m[PartTask].Notes; len(got) != 1 || got[0] != "over budget by 2 est. tokens" {
		t.Errorf("task notes = %q", got)
	}
	if len(m[PartSeed].Notes) != 0 || len(m[PartTools].Notes) != 0 {
		t.Error("empty part or unbudgeted part got a note")
	}
}
