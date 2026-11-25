package decisiontx

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// CHE-4029: an ask may name the issues whose wait it covers, so the decision
// wait guard can tell a grouped card from an ordinary link.
func TestDecodeAskCoversIssueIDs(test *testing.T) {
	const other = "33333333-3333-4333-8333-333333333333"
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"absent", nil, ""},
		{"one issue", []any{other}, ""},
		{"not a list", other, "ask.covers_issue_ids must be a list of strings"},
		{"not a uuid", []any{"CHE-1"}, "ask.covers_issue_ids[0] must be an issue UUID"},
		{"repeat", []any{other, strings.ToUpper(other)}, "ask.covers_issue_ids[1] repeats an earlier issue"},
		{"too many", func() any {
			ids := make([]any, 26)
			for i := range ids {
				ids[i] = fmt.Sprintf("%08d-3333-4333-8333-333333333333", i)
			}
			return ids
		}(), "ask.covers_issue_ids takes at most 25 issues"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			if tc.value != nil {
				ask["covers_issue_ids"] = tc.value
			}
			raw, _ := json.Marshal(ask)
			decoded, err := DecodeAsk(raw)
			if tc.want == "" {
				if err != nil {
					test.Fatalf("decode: %v", err)
				}
				if tc.value != nil && len(decoded.CoversIssueIDs) != 1 {
					test.Fatalf("covers = %v", decoded.CoversIssueIDs)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				test.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
