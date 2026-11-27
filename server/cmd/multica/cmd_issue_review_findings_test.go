package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
)

func TestFindingFlags(t *testing.T) {
	parse := func(vals ...string) ([]map[string]any, error) {
		c := &cobra.Command{}
		c.Flags().StringArray("finding", nil, "")
		for _, v := range vals {
			_ = c.Flags().Set("finding", v)
		}
		return findingFlags(c)
	}
	got, err := parse("P1@missing_rollback:no rollback: step", "P0@scope_creep@revision:new endpoint")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got[0]["category"] != "missing_rollback" || got[0]["summary"] != "no rollback: step" || got[0]["introduced_by_revision"] != nil {
		t.Fatalf("first = %v", got[0])
	}
	if got[1]["introduced_by_revision"] != true {
		t.Fatalf("second = %v", got[1])
	}
	legacy, err := parse("P1:no category yet")
	if err != nil || len(legacy) != 1 || legacy[0]["summary"] != "no category yet" {
		t.Fatalf("legacy form: %v, %v", legacy, err)
	}
	if _, has := legacy[0]["category"]; has {
		t.Fatalf("legacy form sent a category: %v", legacy[0])
	}
	for _, bad := range []string{"P1@x@later:s", "P1@x:", "@x:s"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The review-gate handler tests send testdata/finding_revision.json as the
// review_findings of a round-2 verdict (CHE-9942 W2). This test pins that file
// to what the CLI emits for the same --finding value, so a change to the wire
// format of findingFlags fails here before it silently refuses a verdict.
func TestFindingFlagsMatchHandlerFixture(t *testing.T) {
	c := &cobra.Command{}
	c.Flags().StringArray("finding", nil, "")
	_ = c.Flags().Set("finding", "P1@missing_test_first@revision:B")
	got, err := findingFlags(c)
	if err != nil {
		t.Fatal(err)
	}
	// TestMain moves the working directory, so resolve from this file.
	_, self, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "..", "internal", "handler", "testdata", "finding_revision.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want []map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	// Round-trip the CLI output through JSON, as the request body does.
	sent, _ := json.Marshal(got)
	var onWire []map[string]any
	_ = json.Unmarshal(sent, &onWire)
	if !reflect.DeepEqual(onWire, want) {
		t.Fatalf("findingFlags emits %s, fixture holds %s", sent, raw)
	}
}
