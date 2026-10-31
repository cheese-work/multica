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
			want := []Problem{}
			for _, pair := range c.Refuse {
				want = append(want, Problem{Line: int(pair[0].(float64)), Message: pair[1].(string)})
			}
			got := append([]Problem{}, Check(c.Text)...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Check(%q)\n got  %v\n want %v", c.Text, got, want)
			}
		})
	}
}
