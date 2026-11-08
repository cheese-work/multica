package issueworkflow

import "testing"

func TestDefinitionStart(t *testing.T) {
	steps := []Step{{StatusKey: "todo"}, {StatusKey: "implement"}, {StatusKey: "done"}}
	tests := []struct {
		name string
		def  Definition
		want string
	}{
		{"unset keeps todo", Definition{Steps: steps}, StartStatus},
		{"configured step wins", Definition{Steps: steps, StartStatusKey: "implement"}, "implement"},
		{"key the workflow no longer lists falls back to todo", Definition{Steps: steps, StartStatusKey: "gone"}, StartStatus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.def.Start(); got != tc.want {
				t.Fatalf("Start() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateStartStatus(t *testing.T) {
	steps := []Step{{StatusKey: "todo"}, {StatusKey: "implement"}, {StatusKey: "done"}, {StatusKey: "cancelled"}}
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty means today's behavior", "", false},
		{"unstarted step", "todo", false},
		{"started step", "implement", false},
		{"missing step", "code_review", true},
		{"done category", "done", true},
		{"closed category", "cancelled", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStartStatus(tc.key, steps, testCatalog())
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateStartStatus(%q) error = %v, wantErr %v", tc.key, err, tc.wantErr)
			}
		})
	}
}
