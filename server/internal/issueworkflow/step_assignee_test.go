package issueworkflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolveStepAssignee(t *testing.T) {
	handler := StepAssignee{Type: "agent", ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}}
	other := StepAssignee{Type: "agent", ID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true}}
	sticky := StepAssignee{Type: "member", ID: pgtype.UUID{Bytes: [16]byte{3}, Valid: true}}

	cases := []struct {
		name             string
		override, kept   *StepAssignee
		want             StepAssignee
		wantOver, wantKp bool
	}{
		{"no override", nil, nil, handler, false, false},
		{"override", &other, nil, other, true, false},
		{"naming the handler", &handler, nil, handler, false, false},
		{"sticky", nil, &sticky, sticky, true, true},
		{"sticky equal to the handler", nil, &handler, handler, false, false},
		{"override beats sticky", &other, &sticky, other, true, false},
		{"naming the handler beats sticky", &handler, &sticky, handler, false, false},
	}
	for _, c := range cases {
		got, over, kept := ResolveStepAssignee(handler, c.override, c.kept)
		if got != c.want || over != c.wantOver || kept != c.wantKp {
			t.Errorf("%s = (%v, %v, %v), want (%v, %v, %v)", c.name, got, over, kept, c.want, c.wantOver, c.wantKp)
		}
	}
}
