package workflowlint

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

func agentH(id string) issueworkflow.Handler {
	return issueworkflow.Handler{Type: issueworkflow.HandlerAgent, ID: id}
}

// good is a healthy agent on the one recorded qualified runtime (CHE-735).
var good = Agent{Provider: "codex", RuntimeID: x99Codex, RuntimeVersion: "0.155.1", RuntimeOnline: true}

// withHandler returns clean() with the handler of step key replaced.
func withHandler(key string, h issueworkflow.Handler) issueworkflow.Definition {
	def := clean()
	for i := range def.Steps {
		if def.Steps[i].StatusKey == key {
			def.Steps[i].Handler = h
		}
	}
	return def
}

func TestCleanWorkflowWithEnvHasNoFindings(t *testing.T) {
	env := Env{Agents: map[string]Agent{"a": good}, Live: true, Usage: &Usage{Projects: 1}}
	if fs := LintWith(clean(), env); len(fs) != 0 {
		t.Fatalf("want no findings, got %v", rules(fs))
	}
}

func TestWF004UnusableAgent(t *testing.T) {
	cases := map[string]map[string]Agent{
		"archived": {"a": {Provider: "claude", RuntimeOnline: true, Archived: true}},
		"paused":   {"a": {Provider: "claude", RuntimeOnline: true, Paused: true}},
		"deleted":  {},
	}
	for name, agents := range cases {
		t.Run(name, func(t *testing.T) {
			fs := LintWith(clean(), Env{Agents: agents})
			if !has(fs, "WF004", "todo") {
				t.Fatalf("want WF004@todo, got %v", rules(fs))
			}
			for _, f := range fs {
				if f.Rule == "WF004" && f.Level != LevelError {
					t.Fatalf("WF004 must be an error, got %s", f.Level)
				}
			}
		})
	}
	if fs := LintWith(clean(), Env{Agents: map[string]Agent{"a": good}}); countRule(fs, "WF004") != 0 {
		t.Fatalf("healthy agent: want no WF004, got %v", rules(fs))
	}
	if fs := LintWith(clean(), Env{}); countRule(fs, "WF004") != 0 {
		t.Fatalf("no agent data: want no WF004, got %v", rules(fs))
	}
}

func TestWF005RuntimeNotQualified(t *testing.T) {
	codex := func(version string) Env {
		return Env{Agents: map[string]Agent{"a": {Provider: "codex", RuntimeID: x99Codex, RuntimeVersion: version, RuntimeOnline: true}}}
	}
	if fs := LintWith(clean(), codex("0.166.0")); !has(fs, "WF005", "todo") {
		t.Fatalf("codex 0.166.0: want WF005@todo, got %v", rules(fs))
	}
	if fs := LintWith(clean(), codex("")); !has(fs, "WF005", "todo") {
		t.Fatalf("codex, unknown version: want WF005, got %v", rules(fs))
	}
	for _, v := range []string{"0.155.1", "codex-cli 0.155.1"} {
		if fs := LintWith(clean(), codex(v)); countRule(fs, "WF005") != 0 {
			t.Fatalf("codex %q is qualified, got %v", v, rules(fs))
		}
	}
	// No qualification is recorded for Claude, so any Claude version fails WF005.
	for _, v := range []string{"2.1.0", ""} {
		env := Env{Agents: map[string]Agent{"a": {Provider: "claude", RuntimeVersion: v, RuntimeOnline: true}}}
		if fs := LintWith(clean(), env); !has(fs, "WF005", "todo") {
			t.Fatalf("claude %q has no recorded qualification: want WF005, got %v", v, rules(fs))
		}
	}
}

const x99Codex = "bc0835c7-a25d-4a16-a3bc-e2921d092ed9"

func TestWF005SeedBindsCodexToX99(t *testing.T) {
	at := func(rt string) Env {
		return Env{Agents: map[string]Agent{"a": {Provider: "codex", RuntimeID: rt, RuntimeVersion: "0.155.1", RuntimeOnline: true}}}
	}
	if fs := LintWith(clean(), at(x99Codex)); countRule(fs, "WF005") != 0 {
		t.Fatalf("X99 codex 0.155.1: want no WF005, got %v", rules(fs))
	}
	for name, rt := range map[string]string{"C00": "a596f781-3a60-414c-b346-ebc11d72451c", "missing": ""} {
		if fs := LintWith(clean(), at(rt)); !has(fs, "WF005", "todo") {
			t.Fatalf("codex 0.155.1 on %s runtime: want WF005, got %v", name, rules(fs))
		}
	}
}

func TestWF005FailsClosed(t *testing.T) {
	for _, provider := range []string{"typo-provider", ""} {
		env := Env{Agents: map[string]Agent{"a": {Provider: provider, RuntimeVersion: "1.0.0", RuntimeOnline: true}}}
		if fs := LintWith(clean(), env); !has(fs, "WF005", "todo") {
			t.Fatalf("provider %q is not listed: want WF005, got %v", provider, rules(fs))
		}
	}
	q := qualifiedRuntimes{"empty": {}}
	if q.qualifies(Agent{Provider: "empty", RuntimeVersion: "1.0.0"}) {
		t.Fatal("an empty list qualifies nothing")
	}
	// "*" is not a wildcard: a version that was never qualified never passes.
	q = qualifiedRuntimes{"any": {{Version: "*"}}}
	if q.qualifies(Agent{Provider: "any", RuntimeVersion: "9.9"}) || q.qualifies(Agent{Provider: "any"}) {
		t.Fatal(`version "*" must not qualify an unlisted version`)
	}
}

func TestQualifiedRuntimeID(t *testing.T) {
	q := qualifiedRuntimes{"codex": {{Version: "0.155.1", RuntimeID: "x99"}}}
	if !q.qualifies(Agent{Provider: "codex", RuntimeVersion: "0.155.1", RuntimeID: "x99"}) {
		t.Fatal("matching runtime id and version must qualify")
	}
	if q.qualifies(Agent{Provider: "codex", RuntimeVersion: "0.155.1", RuntimeID: "c00"}) {
		t.Fatal("another runtime must not qualify")
	}
}

// authorReviewer is clean() with an agent_review step handled by "r".
func authorReviewer() issueworkflow.Definition {
	def := withHandler("in_progress", agentH("a"))
	def.Steps = append(def.Steps[:3], append([]step{{StatusKey: "agent_review", Handler: agentH("r"),
		Edges: []edge{{To: "in_review", Guard: "agent_verdict(PASS)"}, {To: "in_progress", Guard: "agent_verdict(CHANGES)"}}}}, def.Steps[3:]...)...)
	def.Steps[2].Edges[0].To = "agent_review"
	return def
}

func TestWF006SameLab(t *testing.T) {
	claude := Agent{Provider: "claude", RuntimeOnline: true}
	codex := Agent{Provider: "codex", RuntimeVersion: "0.155.1", RuntimeOnline: true}
	same := Env{Agents: map[string]Agent{"a": claude, "r": claude}}
	fs := LintWith(authorReviewer(), same)
	if !has(fs, "WF006", "agent_review") {
		t.Fatalf("same lab: want WF006@agent_review, got %v", rules(fs))
	}
	for _, f := range fs {
		if f.Rule == "WF006" && f.Level != LevelError {
			t.Fatalf("WF006 must be an error, got %s", f.Level)
		}
	}
	// A Claude author with a Codex reviewer is cross-lab.
	cross := Env{Agents: map[string]Agent{"a": claude, "r": codex}}
	if fs := LintWith(authorReviewer(), cross); countRule(fs, "WF006") != 0 {
		t.Fatalf("cross-lab: want no WF006, got %v", rules(fs))
	}
	// Model beats provider: an opencode agent on a Claude model is Anthropic.
	oc := Agent{Provider: "opencode", Model: "claude-sonnet-5-5", RuntimeOnline: true}
	if fs := LintWith(authorReviewer(), Env{Agents: map[string]Agent{"a": claude, "r": oc}}); !has(fs, "WF006", "agent_review") {
		t.Fatalf("model maps to Anthropic: want WF006, got %v", rules(fs))
	}
}

func TestWF006UnknownLabWarns(t *testing.T) {
	mystery := Agent{Provider: "mystery", RuntimeOnline: true}
	fs := LintWith(authorReviewer(), Env{Agents: map[string]Agent{"a": good, "r": mystery}})
	for _, f := range fs {
		if f.Rule == "WF006" {
			if f.Level != LevelWarning || !strings.Contains(f.Message, "lab unknown") {
				t.Fatalf("want warning containing 'lab unknown', got %+v", f)
			}
			return
		}
	}
	t.Fatalf("unknown lab must warn, got %v", rules(fs))
}

func TestWF006OutOfScope(t *testing.T) {
	claude := Agent{Provider: "claude", RuntimeOnline: true}
	env := Env{Agents: map[string]Agent{"a": claude, "r": claude}}
	t.Run("no agent_review (Gate and Report shapes)", func(t *testing.T) {
		def := withHandler("in_progress", agentH("a"))
		if fs := LintWith(def, env); countRule(fs, "WF006") != 0 {
			t.Fatalf("want no WF006, got %v", rules(fs))
		}
	})
	t.Run("agent_review handler is not concrete", func(t *testing.T) {
		def := authorReviewer()
		def.Steps[3].Handler = issueworkflow.Handler{Type: issueworkflow.HandlerHumanByRole}
		if fs := LintWith(def, env); countRule(fs, "WF006") != 0 {
			t.Fatalf("want no WF006, got %v", rules(fs))
		}
	})
	t.Run("in_progress is dynamic", func(t *testing.T) {
		def := authorReviewer()
		def.Steps[2].Handler = none
		def.Steps[2].DynamicAssignment = true
		if fs := LintWith(def, env); countRule(fs, "WF006") != 0 {
			t.Fatalf("want no WF006, got %v", rules(fs))
		}
	})
}

func TestWF012RuntimeOffline(t *testing.T) {
	off := Env{Agents: map[string]Agent{"a": {Provider: "claude", RuntimeOnline: false}}}
	if fs := LintWith(clean(), off); countRule(fs, "WF012") != 0 {
		t.Fatalf("save time (Live=false): want no WF012, got %v", rules(fs))
	}
	off.Live = true
	fs := LintWith(clean(), off)
	if !has(fs, "WF012", "todo") {
		t.Fatalf("live: want WF012@todo, got %v", rules(fs))
	}
	for _, f := range fs {
		if f.Rule == "WF012" && f.Level != LevelWarning {
			t.Fatalf("WF012 must be a warning, got %s", f.Level)
		}
	}
	on := Env{Agents: map[string]Agent{"a": good}, Live: true}
	if fs := LintWith(clean(), on); countRule(fs, "WF012") != 0 {
		t.Fatalf("online: want no WF012, got %v", rules(fs))
	}
}

func TestWF013Unused(t *testing.T) {
	fs := LintWith(clean(), Env{Usage: &Usage{}})
	if !has(fs, "WF013", "") {
		t.Fatalf("unused: want WF013, got %v", rules(fs))
	}
	for _, f := range fs {
		if f.Rule == "WF013" && f.Level != LevelInfo {
			t.Fatalf("WF013 must be info, got %s", f.Level)
		}
	}
	for name, u := range map[string]Usage{"project": {Projects: 1}, "referenced": {ReferencedByWorkflow: true}} {
		if fs := LintWith(clean(), Env{Usage: &u}); countRule(fs, "WF013") != 0 {
			t.Fatalf("%s: want no WF013, got %v", name, rules(fs))
		}
	}
	def := clean()
	def.IsDefault = true
	if fs := LintWith(def, Env{Usage: &Usage{}}); countRule(fs, "WF013") != 0 {
		t.Fatalf("default workflow: want no WF013, got %v", rules(fs))
	}
	if fs := LintWith(clean(), Env{}); countRule(fs, "WF013") != 0 {
		t.Fatalf("no usage data: want no WF013, got %v", rules(fs))
	}
}

func TestEmbeddedConfigsParse(t *testing.T) {
	if len(qualified) != 1 || len(qualified["codex"]) == 0 || qualified["codex"][0].Version != "0.155.1" {
		t.Fatalf("seed: want codex 0.155.1, got %v", qualified)
	}
	if lab, ok := labs.of(Agent{Provider: "codex"}); !ok || lab != "OpenAI" {
		t.Fatalf("codex lab: got %q %v", lab, ok)
	}
}

// A hidden agent is one the caller may not see. No finding may disclose its
// provider, runtime version, state or lab.
func TestHiddenAgentLeaksNothing(t *testing.T) {
	hidden := Agent{Provider: "codex", RuntimeVersion: "0.1.0", Archived: true, Paused: true, Hidden: true}
	def := clean()
	for i := range def.Steps {
		if def.Steps[i].StatusKey == "in_progress" || def.Steps[i].StatusKey == "agent_review" {
			def.Steps[i].Handler = agentH("a")
		}
	}
	fs := LintWith(def, Env{Agents: map[string]Agent{"a": hidden}, Live: true})
	for _, f := range fs {
		switch f.Rule {
		case "WF004", "WF005", "WF006", "WF012":
			t.Fatalf("hidden agent produced %s: %s", f.Rule, f.Message)
		}
		if strings.Contains(f.Message, "codex") || strings.Contains(f.Message, "0.1.0") {
			t.Fatalf("finding leaks agent detail: %s", f.Message)
		}
	}
}
