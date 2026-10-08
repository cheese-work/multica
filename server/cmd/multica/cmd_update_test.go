package main

import (
	"strings"
	"testing"
)

func TestRunUpdateRejectsNonPositiveDownloadTimeout(t *testing.T) {
	orig := updateDownloadTimeout
	updateDownloadTimeout = 0
	t.Cleanup(func() { updateDownloadTimeout = orig })

	err := runUpdate(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "download timeout must be greater than zero") {
		t.Fatalf("runUpdate error = %v, want download timeout validation", err)
	}
}

func TestReleaseVersionsMatch(test *testing.T) {
	for _, testcase := range []struct {
		current string
		latest  string
		want    bool
	}{
		{"0.6.0-20261008-1331", "v0.6.0", true},
		{"v0.6.0-20261008-1331", "0.6.0+metadata", true},
		{"0.6.0", "v0.6.0-20261009-0900", true},
		{"0.5.0-20261008-1331", "v0.6.0", false},
		{"0.6.0-rc1-20261008-1331", "v0.6.0", false},
		{"dev", "v0.6.0", false},
	} {
		if got := releaseVersionsMatch(testcase.current, testcase.latest); got != testcase.want {
			test.Errorf("releaseVersionsMatch(%q, %q) = %v, want %v", testcase.current, testcase.latest, got, testcase.want)
		}
	}
}
