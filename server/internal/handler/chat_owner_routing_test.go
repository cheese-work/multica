package handler

import (
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// An agent-authored decision request publishes chat events whose actor is the
// agent; the realtime layer must still route them to the DM's creator.
func TestAgentDecisionRequestChatEventsRouteToSessionCreator(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	var mutex sync.Mutex
	var seen []events.Event
	collect := func(event events.Event) {
		if event.ActorID != fixture.agentID {
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		seen = append(seen, event)
	}
	testHandler.Bus.Subscribe(protocol.EventChatSessionCreated, collect)
	testHandler.Bus.Subscribe(protocol.EventChatMessage, collect)

	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "new_dm": true, "for": fixture.humanID, "ask": fixture.structuredAsk(), "escalation_class": "reversible"})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusCreated).JSON(&map[string]any{})

	mutex.Lock()
	defer mutex.Unlock()
	if len(seen) < 2 {
		test.Fatalf("agent chat events = %d, want session-created and message", len(seen))
	}
	for _, event := range seen {
		if event.ChatSessionOwnerID != fixture.humanID {
			test.Fatalf("%s owner = %q, want session creator %q (agent %q)", event.Type, event.ChatSessionOwnerID, fixture.humanID, fixture.agentID)
		}
	}
}
