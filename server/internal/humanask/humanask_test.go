package humanask

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type fixtureCase struct {
	Name   string   `json:"name"`
	Text   string   `json:"text"`
	Refuse [][2]any `json:"refuse"`
	Warn   [][2]any `json:"warn"`
}

func problems(pairs [][2]any) []Problem {
	out := []Problem{}
	for _, pair := range pairs {
		out = append(out, Problem{Line: int(pair[0].(float64)), Message: pair[1].(string)})
	}
	return out
}

func TestCheckMatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []fixtureCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if got, want := append([]Problem{}, Check(c.Text)...), problems(c.Refuse); !reflect.DeepEqual(got, want) {
				t.Fatalf("Check(%q)\n got  %v\n want %v", c.Text, got, want)
			}
			if got, want := append([]Problem{}, Warnings(c.Text)...), problems(c.Warn); !reflect.DeepEqual(got, want) {
				t.Fatalf("Warnings(%q)\n got  %v\n want %v", c.Text, got, want)
			}
		})
	}
}
