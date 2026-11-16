package ciwatch

import "testing"

func TestSignatureStableAcrossNoise(t *testing.T) {
	a := "2026-11-06T04:50:01.123Z step\n2026-11-06T04:50:02Z ##[error]--- FAIL: TestX (0.31s)\n\tfoo_test.go:42: got 7 want 8 in /home/runner/work/multica/multica/server\n"
	b := "2026-11-07T09:10:11.999Z step\n2026-11-07T09:10:12Z ##[error]--- FAIL: TestX (1.92s)\n\tfoo_test.go:42: got 9 want 8 in /home/runner/work/other/other/server\n"
	if sa, sb := Signature(a), Signature(b); sa != sb || len(sa) != 64 {
		t.Errorf("signatures differ or malformed: %q %q", sa, sb)
	}
	if Signature(a) == Signature("##[error]Process completed with exit code 1.\n") {
		t.Error("different failures share a signature")
	}
}

func TestSignatureFallbackUsesNames(t *testing.T) {
	if Signature("", "CI", "unit") == Signature("", "CI", "lint") || Signature("", "CI", "unit") == "" {
		t.Error("fallback must be deterministic and name-specific")
	}
}
