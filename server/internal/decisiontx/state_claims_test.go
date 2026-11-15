package decisiontx

import (
	"encoding/json"
	"testing"
	"time"
)

func claimAt(at time.Time) map[string]any {
	return map[string]any{"claim": "PR is draft", "readback_command": "gh pr view 12 --json isDraft", "observed_at": at.Format(time.RFC3339)}
}

func TestStateClaimsRequired(test *testing.T) {
	fixed := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	askNow = func() time.Time { return fixed }
	test.Cleanup(func() { askNow = time.Now })
	cases := []struct {
		name   string
		claims any
		del    bool
		want   string
	}{
		{name: "valid", claims: []any{claimAt(fixed.Add(-5 * time.Minute))}},
		{name: "missing field", del: true, want: "ask.state_claims requires at least one item"},
		{name: "empty list", claims: []any{}, want: "ask.state_claims requires at least one item"},
		{name: "no claim text", claims: []any{map[string]any{"readback_command": "x", "observed_at": fixed.Format(time.RFC3339)}}, want: "ask.state_claims[0].claim is required"},
		{name: "no readback", claims: []any{map[string]any{"claim": "c", "observed_at": fixed.Format(time.RFC3339)}}, want: "ask.state_claims[0].readback_command is required"},
		{name: "no observed_at", claims: []any{map[string]any{"claim": "c", "readback_command": "x"}}, want: "ask.state_claims[0].observed_at must be an RFC 3339 time with an offset"},
		{name: "bad observed_at", claims: []any{map[string]any{"claim": "c", "readback_command": "x", "observed_at": "yesterday"}}, want: "ask.state_claims[0].observed_at must be an RFC 3339 time with an offset"},
		{name: "future", claims: []any{claimAt(fixed.Add(time.Hour))}, want: "ask.state_claims[0].observed_at is in the future"},
		{name: "stale", claims: []any{claimAt(fixed.Add(-3 * time.Hour))}, want: "ask.state_claims[0].observed_at is older than 2h; read the state again"},
		{name: "second claim bad", claims: []any{claimAt(fixed), map[string]any{"claim": "c", "readback_command": " ", "observed_at": fixed.Format(time.RFC3339)}}, want: "ask.state_claims[1].readback_command is required"},
	}
	for _, kind := range []string{KindDecision, KindAction} {
		for _, tc := range cases {
			test.Run(kind+"/"+tc.name, func(test *testing.T) {
				ask := validAskMap()
				if kind == KindAction {
					ask = validActionAskMap()
				}
				delete(ask, "state_claims")
				if !tc.del {
					ask["state_claims"] = tc.claims
				}
				raw, _ := json.Marshal(ask)
				if _, err := DecodeAskKind(raw, kind); errString(err) != tc.want {
					test.Fatalf("error = %q, want %q", errString(err), tc.want)
				}
			})
		}
	}
}
