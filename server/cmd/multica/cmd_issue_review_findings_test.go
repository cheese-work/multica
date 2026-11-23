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
	for _, bad := range []string{"P1:no category", "P1@x@later:s", "P1@x:", "@x:s"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
