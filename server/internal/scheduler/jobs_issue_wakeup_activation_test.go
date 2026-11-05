package scheduler

import (
	"context"
	"errors"
	"testing"
)

type fakeActivator struct {
	calls int
	err   error
}

func (f *fakeActivator) RunWakeupActivation(context.Context) error { f.calls++; return f.err }

func TestWakeupDefinitionActivationJobRunsAndSurfacesFailure(t *testing.T) {
	boom := errors.New("backfill page failed")
	a := &fakeActivator{err: boom}
	spec := WakeupDefinitionActivationJob(a)
	if spec.Name != "wakeup_definition_activation" || spec.MaxAttempts != 1 {
		t.Fatalf("spec = %+v", spec)
	}
	if _, err := spec.Handler(context.Background(), HandlerInput{}); !errors.Is(err, boom) || a.calls != 1 {
		t.Fatalf("err = %v calls = %d, want the failure surfaced after one call", err, a.calls)
	}
}
