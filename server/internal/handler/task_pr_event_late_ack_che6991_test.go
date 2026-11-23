package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-6991: an ack that lost to the dispatcher's fallback gets no row and is
// logged as late_ack_after_fallback. Nothing else is logged that way.

func TestLateAckAfterFallbackIsLogged(t *testing.T) {
	for name, delivered := range map[string]bool{"delivered": true, "failed": false} {
		t.Run(name, func(t *testing.T) {
			w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "fallback", "turn_ended", 1, "5 seconds")
			logs := captureLogs(t)
			w.ack(t, delivered, http.StatusConflict)
			if !strings.Contains(logs.String(), "late_ack_after_fallback") || !strings.Contains(logs.String(), w.eventID) {
				t.Fatalf("log = %q, want late_ack_after_fallback naming the item", logs.String())
			}
		})
	}
}

func TestRefusedAckOfOtherItemsIsNotLoggedAsLate(t *testing.T) {
	for _, status := range []string{"failed", "pending"} {
		t.Run(status, func(t *testing.T) {
			w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, status, "turn_ended", 0, "5 seconds")
			logs := captureLogs(t)
			w.ack(t, true, http.StatusConflict)
			if strings.Contains(logs.String(), "late_ack_after_fallback") {
				t.Fatalf("refusal logged late_ack_after_fallback: %q", logs.String())
			}
		})
	}
}

func TestAcceptedAckIsNotLoggedAsLate(t *testing.T) {
	w := newWakeupItemFixture(t, protocol.DaemonCapabilityTaskSupplementPREventV2).seed(t, "delivering", "", 1, "5 seconds")
	logs := captureLogs(t)
	w.ack(t, true, http.StatusOK)
	if strings.Contains(logs.String(), "late_ack_after_fallback") {
		t.Fatalf("accepted ack logged: %q", logs.String())
	}
}
