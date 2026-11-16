package main

import "testing"

// The comment tests build a test-local command, so they never notice a lost
// init() registration. Pin the real command: a v0.8.0 merge once had to keep
// these fork flags at a conflict hunk.
func TestIssueCommentAddRegistersForkFlags(t *testing.T) {
	for name, wantType := range map[string]string{
		"resolve-thread": "bool",
		"supersedes":     "stringSlice",
		"attachment":     "stringArray",
	} {
		f := issueCommentAddCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("issue comment add: --%s is not registered", name)
		}
		if f.Value.Type() != wantType {
			t.Errorf("issue comment add --%s type = %s, want %s", name, f.Value.Type(), wantType)
		}
	}
}
