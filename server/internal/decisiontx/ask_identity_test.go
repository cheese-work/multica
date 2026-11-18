package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func decodeTestAsk(test *testing.T, mutate func(map[string]any)) Ask {
	test.Helper()
	raw := validAskMap()
	mutate(raw)
	body, _ := json.Marshal(raw)
	ask, err := DecodeAsk(body)
	if err != nil {
		test.Fatal(err)
	}
	return ask
}

func TestIdentityWithoutReadTime(test *testing.T) {
	first := decodeTestAsk(test, func(ask map[string]any) {})
	later := decodeTestAsk(test, func(ask map[string]any) {
		ask["probes"] = []string{}
		ask["state_claims"].([]any)[0].(map[string]any)["observed_at"] = time.Now().Add(-30 * time.Second).Format(time.RFC3339)
	})
	if first.StateClaims[0].ObservedAt == later.StateClaims[0].ObservedAt {
		test.Fatal("fixture must differ in observed_at")
	}
	if first.IdentityWithoutReadTime() != later.IdentityWithoutReadTime() {
		test.Fatal("asks that differ only in read time and nil vs empty probes must share an identity")
	}
	if first.Content("") == later.Content("") {
		test.Fatal("the rendered asks must still differ, or the fix proves nothing")
	}
	other := decodeTestAsk(test, func(ask map[string]any) {
		ask["state_claims"].([]any)[0].(map[string]any)["claim"] = "The change is not ready."
	})
	if first.IdentityWithoutReadTime() == other.IdentityWithoutReadTime() {
		test.Fatal("a different claim is a different question")
	}
	if !strings.Contains(first.Content(""), "read at "+first.StateClaims[0].ObservedAt) {
		test.Fatal("computing the identity must leave the ask's read time in place")
	}
}
