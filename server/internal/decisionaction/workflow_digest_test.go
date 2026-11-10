package decisionaction

import (
	"encoding/json"
	"testing"
)

// The digest binds the workflow id and the exact steps, canonicalised: key order
// and whitespace do not matter, a value change or another workflow does.
func TestWorkflowDigestBindsIDAndStepsCanonically(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	steps := json.RawMessage(`[{"status_key":"todo","instructions":"a<b"}]`)
	got, err := WorkflowDigest(id, steps)
	if err != nil {
		t.Fatal(err)
	}
	// sha256 of {"steps":[{"instructions":"a<b","status_key":"todo"}],"workflow_id":"<id>"}
	const pinned = "3f488f2ab0844b2f01a9759c2c290540f529b3f08d98a9c9c420e9e6e734f38c"
	if got != pinned {
		t.Fatalf("digest = %s, want %s", got, pinned)
	}
	reordered := json.RawMessage(` [ {"instructions":"a<b", "status_key":"todo"} ] `)
	if again, _ := WorkflowDigest(id, reordered); again != got {
		t.Errorf("key order or whitespace changed the digest: %s", again)
	}
	if other, _ := WorkflowDigest("22222222-2222-4222-8222-222222222222", steps); other == got {
		t.Error("digest does not bind the workflow id")
	}
	if changed, _ := WorkflowDigest(id, json.RawMessage(`[{"status_key":"todo","instructions":"b"}]`)); changed == got {
		t.Error("digest ignores a value change")
	}
	if _, err := WorkflowDigest(id, json.RawMessage(`[`)); err == nil {
		t.Error("invalid JSON hashed")
	}
}
