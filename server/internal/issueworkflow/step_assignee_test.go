package issueworkflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolveStepAssignee(t *testing.T) {
	handler := StepAssignee{Type: "agent", ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}}
	other := StepAssignee{Type: "agent", ID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true}}

	if got, over := ResolveStepAssignee(handler, nil); got != handler || over {
		t.Errorf("no override = (%v, %v), want the step handler", got, over)
	}
	if got, over := ResolveStepAssignee(handler, &other); got != other || !over {
		t.Errorf("override = (%v, %v), want the override", got, over)
	}
	if got, over := ResolveStepAssignee(handler, &handler); got != handler || over {
		t.Errorf("naming the handler = (%v, %v), want no override", got, over)
	}
}
