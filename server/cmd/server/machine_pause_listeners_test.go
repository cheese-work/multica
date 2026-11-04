package main

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A subscriber that is never registered fails silently, so guard the wiring.
func TestRegisterMachinePauseListenersSubscribesTaskLifecycle(t *testing.T) {
	bus := events.New()
	registerMachinePauseListeners(bus, db.New(testPool))
	for _, eventType := range []string{
		protocol.EventTaskDispatch, protocol.EventTaskCompleted,
		protocol.EventTaskFailed, protocol.EventTaskCancelled,
	} {
		if bus.SubscriberCount(eventType) == 0 {
			t.Errorf("no machine pause listener on %s", eventType)
		}
	}
}
