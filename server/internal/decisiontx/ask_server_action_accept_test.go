package decisiontx

import "testing"

// after_choice server_action is a known value of the typed-option contract (CHE-7358).
func TestAfterChoiceServerActionIsAKnownValue(test *testing.T) {
	_, err := serverActionAsk(test, statusAction("done"))
	if err != nil {
		test.Fatalf("decode refused a server_action option: %v", err)
	}
}
