package designguard

import (
	"context"
	"errors"
	"testing"
)

// Tasks the fake Multica knows. Labs come from the registry below, never from the view.
var tasks = []Task{
	{ID: "ta", AgentID: "A", Provider: "claude", Model: "sonnet"},  // anthropic
	{ID: "ta2", AgentID: "A2", Provider: "claude", Model: "opus"},  // anthropic
	{ID: "tb", AgentID: "B", Provider: "codex", Model: "gpt"},      // openai
	{ID: "ts", AgentID: "S", Provider: "codex", Model: "gpt"},      // openai
	{ID: "tg", AgentID: "G", Provider: "gemini", Model: "pro"},     // google
	{ID: "tx", AgentID: "X", Provider: "other", Model: "unlisted"}, // lab unknown
}

var labs = map[string]string{"claude/sonnet": "anthropic", "claude/opus": "anthropic", "codex/gpt": "openai", "gemini/pro": "google"}

func fakeDeps() Deps {
	return Deps{
		Tasks: func(_ context.Context, ids []string) ([]Task, error) {
			var out []Task
			for _, id := range ids {
				for _, t := range tasks {
					if t.ID == id {
						out = append(out, t)
					}
				}
			}
			return out, nil
		},
		Lab: func(p, m string) string { return labs[p+"/"+m] },
	}
}

func agent(id, task string) Author { return Author{Kind: "agent", AgentID: id, TaskID: task} }
func complete(a ...Author) View    { return View{Authors: a, ProvenanceComplete: true} }

func TestCheckIndependence(t *testing.T) {
	cases := []struct {
		name     string
		view     View
		reviewer Reviewer
		asker    string
		want     string
	}{
		// D1.5 case table.
		{"A designs, B edits, A reviews, B sends", complete(agent("A", "ta"), agent("B", "tb")), Reviewer{"A", "ta"}, "B", CodeSelfReview},
		{"copied page: another anthropic agent reviews", complete(agent("A", "ta")), Reviewer{"A2", "ta2"}, "B", CodeSameLab},
		{"restore keeps A's rows, A reviews", complete(agent("A", "ta")), Reviewer{"A", "ta"}, "B", CodeSelfReview},
		{"author task id Multica does not know (borrowed id)", complete(agent("A", "nope")), Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"human write only: no agent invented", complete(Author{Kind: "user"}), Reviewer{"S", "ts"}, "B", ""},
		{"unknown row, provenance incomplete", View{Authors: []Author{{Kind: "unknown"}}}, Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"A finished, S (openai) reviews later", complete(agent("A", "ta")), Reviewer{"S", "ts"}, "B", ""},
		{"reviewer edited a frame in its own run", complete(agent("A", "ta"), agent("S", "ts")), Reviewer{"S", "ts"}, "B", CodeSelfReview},
		// A1 to A6 edges.
		{"A1 empty author set", View{ProvenanceComplete: true}, Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"A1 provenance flag false", View{Authors: []Author{agent("A", "ta")}}, Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"A1 agent mismatch: task belongs to another agent", complete(agent("A", "tb")), Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"A1 agent row without task id", complete(agent("A", "")), Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"A1 system row", complete(Author{Kind: "system"}), Reviewer{"S", "ts"}, "B", CodeAuthorUnknown},
		{"A2 author lab not in registry", complete(agent("X", "tx")), Reviewer{"S", "ts"}, "B", CodeAuthorLabUnknown},
		{"A4 reviewer task unknown", complete(agent("A", "ta")), Reviewer{"S", "gone"}, "B", CodeReviewerLabUnknown},
		{"A4 reviewer task of another agent", complete(agent("A", "ta")), Reviewer{"S", "tb"}, "B", CodeReviewerLabUnknown},
		{"A4 reviewer lab not in registry (unlisted pair)", complete(agent("A", "ta")), Reviewer{"X", "tx"}, "B", CodeReviewerLabUnknown},
		{"A4 reviewer without task id", complete(agent("A", "ta")), Reviewer{"S", ""}, "B", CodeReviewerLabUnknown},
		{"A5 one of two author labs matches", complete(agent("A", "ta"), agent("B", "tb")), Reviewer{"S", "ts"}, "G", CodeSameLab},
		{"A6 reviewer is the asker", complete(agent("A", "ta")), Reviewer{"S", "ts"}, "S", CodeSameAgentAsAsker},
		{"A6 skipped at verdict time (no asker)", complete(agent("A", "ta")), Reviewer{"S", "ts"}, "", ""},
		{"user rows ignored beside an independent agent", complete(Author{Kind: "user"}, agent("A", "ta")), Reviewer{"G", "tg"}, "B", ""},
		{"third lab reviews two-lab authors", complete(agent("A", "ta"), agent("B", "tb")), Reviewer{"G", "tg"}, "B", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CheckIndependence(context.Background(), c.view, c.reviewer, c.asker, fakeDeps())
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestCheckIndependenceLookupError(t *testing.T) {
	d := fakeDeps()
	d.Tasks = func(context.Context, []string) ([]Task, error) { return nil, errors.New("db down") }
	if got, err := CheckIndependence(context.Background(), complete(agent("A", "ta")), Reviewer{"S", "ts"}, "B", d); err == nil {
		t.Fatalf("lookup error swallowed, got code %q", got)
	}
}
