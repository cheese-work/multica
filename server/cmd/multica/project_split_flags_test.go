package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func splitTestCmd(t *testing.T, kv ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "update"}
	addProjectSplitFlags(c)
	for i := 0; i < len(kv); i += 2 {
		if err := c.Flags().Set(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestProjectSplitRequest(t *testing.T) {
	body, clear, err := projectSplitRequest(splitTestCmd(t))
	if body != nil || clear || err != nil {
		t.Fatalf("no flags: got %v %v %v, want nothing", body, clear, err)
	}

	body, clear, err = projectSplitRequest(splitTestCmd(t,
		"split-arm-b", "wf-1", "split-start", "2026-11-14T00:00:00Z", "split-end", "2026-11-28T00:00:00Z",
		"split-exclude-assignee", "a1", "split-exclude-assignee", "a2", "split-exclude-squad", "s1"))
	want := map[string]any{
		"arm_b_workflow_id":     "wf-1",
		"starts_at":             "2026-11-14T00:00:00Z",
		"ends_at":               "2026-11-28T00:00:00Z",
		"excluded_assignee_ids": []string{"a1", "a2"},
		"excluded_squad_ids":    []string{"s1"},
	}
	if err != nil || clear || !reflect.DeepEqual(body, want) {
		t.Fatalf("set: got %v %v %v, want %v", body, clear, err, want)
	}

	if _, clear, err = projectSplitRequest(splitTestCmd(t, "clear-split", "true")); err != nil || !clear {
		t.Fatalf("clear: got clear=%v err=%v", clear, err)
	}
	// arm B, start and end go together; clear excludes the rest.
	if _, _, err = projectSplitRequest(splitTestCmd(t, "split-arm-b", "wf-1")); err == nil {
		t.Fatal("arm B without start and end must fail")
	}
	if _, _, err = projectSplitRequest(splitTestCmd(t, "clear-split", "true", "split-arm-b", "wf-1")); err == nil {
		t.Fatal("clear with set flags must fail")
	}
}
