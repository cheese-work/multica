package main

import (
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
