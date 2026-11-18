package daemon

import (
	"errors"
	"strings"
	"testing"
)

// CHE-6971 step 3: a kind "wakeup" item (PR-event v2) is a platform notice for
// a live run. These tests drive the real loop against a fake server.

const wakeupInstructionText = "Re-check CI on PR 12 and merge if green."

func wakeupItem() map[string]string {
	return map[string]string{"event_id": "firing-1", "kind": "wakeup", "wakeup_id": "wakeup-1", "text": wakeupInstructionText}
}

func TestWakeupItemIsInjectedAsPlatformBlockAndAckedDelivered(t *testing.T) {
	s := &prEventServer{prEvent: wakeupItem()}
	injected := runPREventLoopWith(t, s, true, true, nil)
	if len(injected) != 1 {
		t.Fatalf("injections = %q, want 1", injected)
	}
	got := injected[0]
	if !strings.HasPrefix(got, "[wakeup] ") || !strings.Contains(got, wakeupInstructionText) {
		t.Fatalf("instruction = %q", got)
	}
	if strings.Contains(got, "[PR EVENT]") {
		t.Fatalf("wakeup rendered as a PR event: %q", got)
	}
	if len(s.acks) != 1 || !strings.HasSuffix(s.acks[0], "/pr-events/firing-1/ack") || !s.ackBodies[0].Delivered {
		t.Fatalf("acks = %v %+v", s.acks, s.ackBodies)
	}
}

func TestWakeupItemIsNeverFormattedAsHumanGuidance(t *testing.T) {
	got := formatWakeupInstruction(&TaskPREvent{Kind: "wakeup", WakeupID: "wakeup-1", Text: wakeupInstructionText})
	for _, forbidden := range []string{"Human", "human", "ADDITIONAL GUIDANCE", "ADDITIONAL INPUT", "author"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("instruction carries %q: %q", forbidden, got)
		}
	}
	for _, want := range []string{"[wakeup]", "platform", "not from a person", "current state"} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction lacks %q: %q", want, got)
		}
	}
}

func TestWakeupItemInjectionErrorAcksFailedWithReason(t *testing.T) {
	s := &prEventServer{prEvent: wakeupItem(), allowFailed: true}
	runPREventLoopWith(t, s, true, true, errors.New("provider said no"))
	if len(s.ackBodies) != 1 || s.ackBodies[0].Delivered || s.ackBodies[0].Error != "provider_rejected" {
		t.Fatalf("ack bodies = %+v, want one failed provider_rejected", s.ackBodies)
	}
}

func TestWakeupItemTurnNotStartedAcksFailedTurnNotStarted(t *testing.T) {
	s := &prEventServer{prEvent: wakeupItem(), allowFailed: true}
	runPREventLoopWith(t, s, true, true, errors.New("turn not started"))
	if len(s.ackBodies) != 1 || s.ackBodies[0].Delivered || s.ackBodies[0].Error != "turn_not_started" {
		t.Fatalf("ack bodies = %+v, want one failed turn_not_started", s.ackBodies)
	}
}

// With a v1 echo the server must not send a wakeup item; if one arrives anyway
// the daemon injects nothing and fails the item so the wakeup falls back.
func TestWakeupItemOnV1RunIsNotHandled(t *testing.T) {
	s := &prEventServer{prEvent: wakeupItem(), allowFailed: true}
	injected := runPREventLoopWith(t, s, true, false, nil)
	if len(injected) != 0 {
		t.Fatalf("v1 run injected a wakeup item: %q", injected)
	}
	if len(s.ackBodies) != 1 || s.ackBodies[0].Delivered || s.ackBodies[0].Error != "provider_rejected" {
		t.Fatalf("ack bodies = %+v, want one failed provider_rejected", s.ackBodies)
	}
}

func TestPREventDeliveryUnchangedOnV2Run(t *testing.T) {
	s := &prEventServer{prEvent: map[string]string{"event_id": "event-1", "kind": "checks_failed", "head_sha": "newsha", "check_name": "unit tests"}}
	injected := runPREventLoopWith(t, s, true, true, nil)
	if len(injected) != 1 || !strings.HasPrefix(injected[0], "[PR EVENT] System:") || strings.Contains(injected[0], "[wakeup]") {
		t.Fatalf("injections = %q", injected)
	}
	if len(s.acks) != 1 || !strings.HasSuffix(s.acks[0], "/pr-events/event-1/ack") {
		t.Fatalf("acks = %v", s.acks)
	}
}
