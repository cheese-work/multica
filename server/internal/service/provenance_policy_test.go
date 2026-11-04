package service

import "testing"

// The export marker changed from [REDACTED CREDENTIAL] to KEY=[REDACTED], so
// consumers must see a new policy id.
func TestProvenanceRedactionPolicyVersion(t *testing.T) {
	if ProvenanceRedactionPolicy != "redact.Text:v2" {
		t.Fatalf("got %q, want redact.Text:v2", ProvenanceRedactionPolicy)
	}
}
