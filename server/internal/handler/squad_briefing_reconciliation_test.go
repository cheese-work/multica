package handler

import "testing"

// TestReconciliationKey_DurableKeyTakesPrecedence exercises the actual
// precedence rule the squad leader protocol (responsibility 5,
// squadOperatingProtocolHeader) describes: a durable candidate/SHA or PR
// revision must win over a reporting-comment key, and CandidateSHA must win
// over PRRevision when both are present. This is the executable counterpart
// to the P1 finding on PR #11 (CHE-359) — the protocol prose alone was only
// ever checked by literal substring assertion; this exercises the actual
// decision.
func TestReconciliationKey_DurableKeyTakesPrecedence(t *testing.T) {
	tests := []struct {
		name string
		ev   ReconciliationEvent
		want string
	}{
		{
			name: "candidate SHA wins over revision and comment",
			ev: ReconciliationEvent{
				CandidateSHA:       "19c756bce4ad20bb9ed5a479c3ba58fedeb00b4e",
				PRRevision:         "rev-3",
				ReportingCommentID: "comment-1",
			},
			want: "sha:19c756bce4ad20bb9ed5a479c3ba58fedeb00b4e",
		},
		{
			name: "revision wins over comment when SHA unknown",
			ev: ReconciliationEvent{
				PRRevision:         "rev-3",
				ReportingCommentID: "comment-1",
			},
			want: "revision:rev-3",
		},
		{
			name: "comment is the fallback key only when no durable identity exists",
			ev: ReconciliationEvent{
				ReportingCommentID: "comment-1",
			},
			want: "comment:comment-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReconciliationKey(tc.ev); got != tc.want {
				t.Errorf("ReconciliationKey(%+v) = %q, want %q", tc.ev, got, tc.want)
			}
		})
	}
}

// TestReconciliationKey_CrossChannelReplayCollapsesToSameKey is the
// production-level cross-channel replay case required by decision PR #35
// Case B (multica-dotfiles docs/cheese-work/INCIDENT-CHE-329-ROUTING.md,
// "Verification Cases For The Future Change": "Replay of that event, or
// merge webhook plus human reply" -> "Same reconciliation key reused; no
// duplicate work or repeated courtesy comment.").
//
// A merge webhook and a human reply reporting the SAME candidate arrive as
// two different comments on two different channels; keying on the
// reporting comment (the pre-fix behavior) would treat them as two
// different events and risk a duplicate publish. Keying on the durable
// candidate/SHA collapses them to one event, as required.
func TestReconciliationKey_CrossChannelReplayCollapsesToSameKey(t *testing.T) {
	const mergedSHA = "19c756bce4ad20bb9ed5a479c3ba58fedeb00b4e"

	webhookReport := ReconciliationEvent{
		CandidateSHA:       mergedSHA,
		ReportingCommentID: "webhook-comment-1",
	}
	humanReplyReport := ReconciliationEvent{
		CandidateSHA:       mergedSHA,
		ReportingCommentID: "human-comment-2",
	}

	webhookKey := ReconciliationKey(webhookReport)
	humanKey := ReconciliationKey(humanReplyReport)

	if webhookKey != humanKey {
		t.Fatalf("cross-channel replay of the same candidate must collapse to the same key: webhook=%q human=%q", webhookKey, humanKey)
	}

	// Negative control: proves the assertion above is meaningful and not
	// vacuously true because ReconciliationKey ignores its input — two
	// reports of genuinely DIFFERENT candidates must NOT collapse.
	differentCandidateReport := ReconciliationEvent{
		CandidateSHA:       "3dbd42e2fd14c5366cd92c6996c58800ee5dcc59",
		ReportingCommentID: "human-comment-2",
	}
	if ReconciliationKey(differentCandidateReport) == webhookKey {
		t.Fatal("negative control is broken: a different candidate must not collapse to the same key")
	}
}
