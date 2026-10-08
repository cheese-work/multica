package main

import (
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
)

func TestVersionJSONSeparatesReleaseStamp(test *testing.T) {
	original := version
	test.Cleanup(func() { version = original })
	for _, testcase := range []struct {
		display string
		plain   string
		build   string
	}{
		{"0.6.0-20261008-1331", "0.6.0", "20261008-1331"},
		{"0.6.0", "0.6.0", ""},
		{"dev", "dev", ""},
	} {
		version = testcase.display
		command := &cobra.Command{}
		command.Flags().String("output", "json", "")
		output, err := captureStdout(test, func() error { return runVersion(command, nil) })
		if err != nil {
			test.Fatal(err)
		}
		var info map[string]string
		if err := json.Unmarshal([]byte(output), &info); err != nil {
			test.Fatal(err)
		}
		if info["version"] != testcase.plain || info["build"] != testcase.build || info["display_version"] != testcase.display {
			test.Fatalf("version JSON = %#v, want version %q, build %q, display %q", info, testcase.plain, testcase.build, testcase.display)
		}
	}
}
