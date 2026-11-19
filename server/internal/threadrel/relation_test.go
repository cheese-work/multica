package threadrel_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/threadrel"
)

// Fixture ids. The fixture file writes $V, $O, $A, $B; the test expands them
// before decoding so the mention markup stays valid for util.ParseMentions.
const (
	idV = "11111111-1111-1111-1111-111111111111" // viewer member
	idO = "22222222-2222-2222-2222-222222222222" // other member
	idA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" // agent
	idB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" // second agent
)

type fixtureFile struct {
	Cases []fixtureCase `json:"cases"`
}

type fixtureCase struct {
	Name   string `json:"name"`
	Edge   int    `json:"edge"`
	Viewer struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"viewer"`
	FYICountsAsWithMe *bool      `json:"fyi_counts_as_with_me"`
	ResolvedAt        *time.Time `json:"resolved_at"`
	Comments          []struct {
		Author struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"author"`
		Content           string    `json:"content"`
		At                time.Time `json:"at"`
		NotifyOnly        *bool     `json:"notify_only"`
		DecisionRequestID string    `json:"decision_request_id"`
		ResolvesThread    bool      `json:"resolves_thread"`
		Deleted           bool      `json:"deleted"`
	} `json:"comments"`
	Decisions []struct {
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Status     string `json:"status"`
	} `json:"decisions"`
	Expect struct {
		WithMe      bool `json:"with_me"`
		AgentsOnly  bool `json:"agents_only"`
		WaitingOnMe bool `json:"waiting_on_me"`
	} `json:"expect"`
}

func loadCases(t *testing.T) []fixtureCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/relation_cases.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	expanded := strings.NewReplacer("$V", idV, "$O", idO, "$A", idA, "$B", idB).Replace(string(raw))
	var f fixtureFile
	if err := json.Unmarshal([]byte(expanded), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return f.Cases
}

func toInput(c fixtureCase) threadrel.Input {
	in := threadrel.Input{
		Viewer:            threadrel.Actor{Type: c.Viewer.Type, ID: c.Viewer.ID},
		FYICountsAsWithMe: true,
		ResolvedAt:        c.ResolvedAt,
	}
	if c.FYICountsAsWithMe != nil {
		in.FYICountsAsWithMe = *c.FYICountsAsWithMe
	}
	for _, cm := range c.Comments {
		in.Comments = append(in.Comments, threadrel.Comment{
			Author:            threadrel.Actor{Type: cm.Author.Type, ID: cm.Author.ID},
			Content:           cm.Content,
			CreatedAt:         cm.At,
			NotifyOnly:        cm.NotifyOnly,
			DecisionRequestID: cm.DecisionRequestID,
			ResolvesThread:    cm.ResolvesThread,
			Deleted:           cm.Deleted,
		})
	}
	for _, d := range c.Decisions {
		in.Decisions = append(in.Decisions, threadrel.Decision{
			Target: threadrel.Actor{Type: d.TargetType, ID: d.TargetID},
			Status: d.Status,
		})
	}
	return in
}

func TestClassifyFixture(t *testing.T) {
	cases := loadCases(t)
	covered := map[int]bool{}
	for _, c := range cases {
		covered[c.Edge] = true
		t.Run(c.Name, func(t *testing.T) {
			got := threadrel.Classify(toInput(c))
			want := threadrel.Result{
				WithMe:      c.Expect.WithMe,
				AgentsOnly:  c.Expect.AgentsOnly,
				WaitingOnMe: c.Expect.WaitingOnMe,
			}
			if got != want {
				t.Fatalf("edge %d: got %+v, want %+v", c.Edge, got, want)
			}
		})
	}
	for edge := 1; edge <= 15; edge++ {
		if !covered[edge] {
			t.Errorf("fixture has no row for edge case %d", edge)
		}
	}
}

// Input order must not matter: the rule sorts by created_at itself.
func TestClassifyIgnoresInputOrder(t *testing.T) {
	for _, c := range loadCases(t) {
		in := toInput(c)
		want := threadrel.Classify(in)
		for i, j := 0, len(in.Comments)-1; i < j; i, j = i+1, j-1 {
			in.Comments[i], in.Comments[j] = in.Comments[j], in.Comments[i]
		}
		if got := threadrel.Classify(in); got != want {
			t.Errorf("%s: reversed input gives %+v, want %+v", c.Name, got, want)
		}
	}
}
