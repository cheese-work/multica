package service

import "testing"

func TestModelChangeNoticeText(t *testing.T) {
	t.Parallel()
	const opus55, opus48 = "claude-opus-5-5", "claude-opus-4-8"
	base := func(extra map[string]any) map[string]any {
		in := map[string]any{"source_model": opus55, "target_model": opus48, "scope": "session"}
		for k, v := range extra {
			in[k] = v
		}
		return in
	}
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"incident attempt", base(map[string]any{"phase": "attempt", "reason": "refusal", "category": "cyber"}),
			"Runtime is retrying Opus 5.5 → Opus 4.8. Reason: provider safety classification cyber. Scope: session."},
		{"refusal without category", base(map[string]any{"phase": "attempt", "reason": "refusal"}),
			"Runtime is retrying Opus 5.5 → Opus 4.8. Reason: provider safety classification. Scope: session."},
		{"availability fallback", base(map[string]any{"phase": "attempt", "reason": "overloaded"}),
			"Runtime is retrying Opus 5.5 → Opus 4.8. Reason: provider overloaded. Scope: session."},
		{"unknown provider reason is passed through", base(map[string]any{"phase": "attempt", "reason": "new_thing"}),
			"Runtime is retrying Opus 5.5 → Opus 4.8. Reason: provider-reported new_thing. Scope: session."},
		{"no reason reported", base(map[string]any{"phase": "attempt"}),
			"Runtime is retrying Opus 5.5 → Opus 4.8. Reason: not reported. Scope: session."},
		{"subagent scope says the DM model is unchanged", base(map[string]any{"phase": "attempt", "reason": "refusal", "category": "cyber", "scope": "local"}),
			"A subagent or side request is retrying Opus 5.5 → Opus 4.8. Reason: provider safety classification cyber. Scope: subagent only; the DM's model is unchanged."},
		{"sticky", base(map[string]any{"phase": "sticky", "reason": "refusal", "category": "cyber"}),
			"Runtime keeps Opus 4.8 instead of Opus 5.5 for this session. Reason: provider safety classification cyber. Scope: session."},
		{"return to primary", base(map[string]any{"phase": "return", "source_model": opus48, "target_model": opus55}),
			"Runtime returned to Opus 5.5 from Opus 4.8. Scope: session."},
		{"confirmed", base(map[string]any{"phase": "confirmed"}),
			"Confirmed: Opus 4.8 served a response (requested Opus 5.5). Scope: session."},
		{"failed fallback does not claim a switch", base(map[string]any{"phase": "failed"}),
			"The retry on Opus 4.8 failed before it responded, so no switch happened. Serving model: unknown. Scope: session."},
		{"refusal with no fallback available", map[string]any{"phase": "failed", "source_model": opus55, "reason": "", "category": "cyber"},
			"Opus 5.5 refused the request (provider safety classification cyber) and no fallback model was available."},
		{"unrecognized id is shown as reported", base(map[string]any{"phase": "attempt", "reason": "overloaded", "target_model": "gpt-9"}),
			"Runtime is retrying Opus 5.5 → gpt-9. Reason: provider overloaded. Scope: session."},
		{"dated id keeps family and version", base(map[string]any{"phase": "return", "source_model": "claude-opus-4-1-20250805", "target_model": "claude-opus-4-5"}),
			"Runtime returned to Opus 4.5 from Opus 4.1. Scope: session."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelChangeNoticeText(tc.in); got != tc.want {
				t.Fatalf("\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestModelChangeNoticeIDIsStablePerNativeEvent(t *testing.T) {
	t.Parallel()
	session := mustUUID(t, "11111111-1111-4111-8111-111111111111")
	run := mustUUID(t, "22222222-2222-4222-8222-222222222222")
	run2 := mustUUID(t, "33333333-3333-4333-8333-333333333333")
	in := map[string]any{"runtime": "claude", "native_event_id": "evt-1", "phase": "attempt"}
	// A replay of the same native event under a different run message id is the same notice.
	if modelChangeNoticeID(session, run, in) != modelChangeNoticeID(session, run2, in) {
		t.Fatal("replayed native event must keep its notice id")
	}
	// A genuine later A->B->A transition has its own native identity.
	other := map[string]any{"runtime": "claude", "native_event_id": "evt-2", "phase": "attempt"}
	if modelChangeNoticeID(session, run, in) == modelChangeNoticeID(session, run, other) {
		t.Fatal("distinct native events must not collapse")
	}
	// Same native event, different phase (attempt vs confirmed) stays distinct.
	confirmed := map[string]any{"runtime": "claude", "native_event_id": "evt-1", "phase": "confirmed"}
	if modelChangeNoticeID(session, run, in) == modelChangeNoticeID(session, run, confirmed) {
		t.Fatal("phase is part of the identity")
	}
	// No native identity: fall back to the run message id.
	bare := map[string]any{"phase": "attempt"}
	if modelChangeNoticeID(session, run, bare) == modelChangeNoticeID(session, run2, bare) {
		t.Fatal("without native identity each run message is its own notice")
	}
}
