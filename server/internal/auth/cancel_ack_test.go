package auth

import "testing"

func TestCancelAckProof(t *testing.T) {
	proof := CancelAckProof("task-1", "rt-1")
	if !VerifyCancelAckProof("task-1", "rt-1", proof) {
		t.Fatal("proof does not verify for its own task and runtime")
	}
	for name, c := range map[string][3]string{
		"empty":         {"task-1", "rt-1", ""},
		"other task":    {"task-2", "rt-1", proof},
		"other runtime": {"task-1", "rt-2", proof},
		"garbage":       {"task-1", "rt-1", "nope"},
	} {
		if VerifyCancelAckProof(c[0], c[1], c[2]) {
			t.Errorf("%s: proof verified, want rejected", name)
		}
	}
}
