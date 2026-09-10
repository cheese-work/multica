package handler

import (
	"strings"
	"testing"
)

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

// TestSquadOperatingProtocolReplayProvenThroughProductionBriefing is the
// production-level replay proof Opus's BLOCK on PR #12 (63f1e35a8) required:
// TestReconciliationKey_CrossChannelReplayCollapsesToSameKey above proves
// ReconciliationKey behaves correctly, but Opus's mutation experiment showed
// that reverting ONLY the shipped prose in squadOperatingProtocolHeader
// (leaving ReconciliationKey untouched) left that test green — because
// nothing calls ReconciliationKey in production. This test instead exercises
// the actual artifact a leader reads: squadOperatingProtocolFor's output.
//
// It proves the replay case through production text by checking that the
// SAME sentence squadOperatingProtocolFor renders is the one that asserts
// cross-channel collapse — i.e. the rendered protocol text is not merely
// "about" ReconciliationKey, it IS ReconciliationKey's output, embedded
// verbatim. A prose-only equal-footing revert (restoring the pre-fix
// wording in squadOperatingProtocolHeader without touching
// reconciliationKeyingParagraph/ReconciliationKey) is structurally
// impossible under the current composition: squadOperatingProtocolFor does
// not contain that paragraph as a literal, it substitutes in
// reconciliationKeyingParagraph()'s return value. This test fails if that
// substitution seam is ever removed or if the rendered text stops matching
// what ReconciliationKey actually computes.
func TestSquadOperatingProtocolReplayProvenThroughProductionBriefing(t *testing.T) {
	const mergedSHA = "19c756bce4ad20bb9ed5a479c3ba58fedeb00b4e"

	protocol := squadOperatingProtocolFor(true)

	webhookKey := ReconciliationKey(ReconciliationEvent{CandidateSHA: mergedSHA, ReportingCommentID: "webhook-comment-1"})
	humanKey := ReconciliationKey(ReconciliationEvent{CandidateSHA: mergedSHA, ReportingCommentID: "human-comment-2"})
	if webhookKey != humanKey {
		t.Fatalf("precondition broken: cross-channel replay of the same candidate must collapse to the same key: webhook=%q human=%q", webhookKey, humanKey)
	}

	// The production briefing must be the thing asserting this, not a
	// parallel literal: reconciliationKeyingParagraph()'s exact output —
	// which is itself built from ReconciliationKey's precedence — must
	// appear verbatim inside the rendered protocol.
	seam := reconciliationKeyingParagraph()
	if !strings.Contains(protocol, seam) {
		t.Fatalf("squadOperatingProtocolFor does not render reconciliationKeyingParagraph()'s output — the replay proof no longer covers the shipped artifact\n--- seam ---\n%s\n--- protocol ---\n%s", seam, protocol)
	}

	// Sanity: the shipped text actually names the collapsed key form (sha:),
	// so a leader reading it sees the SAME identity computed above, not an
	// unrelated key format.
	if !strings.Contains(seam, `"sha:<sha>"`) {
		t.Fatalf("production protocol's keying paragraph does not name the SHA-first key form it claims to use\n--- seam ---\n%s", seam)
	}

	// Mutation-equivalence check standing in for Opus's manual experiment:
	// a prose-only revert to the pre-fix equal-footing wording could only
	// land in squadOperatingProtocolHeader as a literal. Assert the current
	// header source has no such literal duplicate of the keying rule left
	// over — the ONLY place this rule can live is behind the
	// reconciliationKeyingParagraph() call.
	if strings.Contains(squadOperatingProtocolHeader, "candidate/SHA present") {
		t.Fatal("squadOperatingProtocolHeader must not contain a hand-written duplicate of the generated keying paragraph — it must come from reconciliationKeyingParagraph() via the {{RECONCILIATION_KEYING_PARAGRAPH}} substitution")
	}

	// Sol's BLOCK on 52d013197: squadOperatingProtocolFor substitutes the
	// placeholder with strings.Replace(..., 1) — a ONE-SHOT replace. The
	// prior containment check above (protocol contains seam) still passes
	// even if a second, un-substituted {{RECONCILIATION_KEYING_PARAGRAPH}}
	// placeholder ships to a leader, because strings.Contains only proves
	// the seam text is present SOMEWHERE, not that every placeholder was
	// replaced. Assert directly that no raw placeholder reaches the
	// rendered briefing, for both ownsIssueStatus branches.
	if strings.Contains(protocol, "{{RECONCILIATION_KEYING_PARAGRAPH}}") {
		t.Fatalf("squadOperatingProtocolFor(true) leaks a raw {{RECONCILIATION_KEYING_PARAGRAPH}} placeholder into the rendered briefing — strings.Replace's one-shot count=1 only guarantees the FIRST occurrence is substituted\n--- protocol ---\n%s", protocol)
	}
	if notOwned := squadOperatingProtocolFor(false); strings.Contains(notOwned, "{{RECONCILIATION_KEYING_PARAGRAPH}}") {
		t.Fatalf("squadOperatingProtocolFor(false) leaks a raw {{RECONCILIATION_KEYING_PARAGRAPH}} placeholder into the rendered briefing\n--- protocol ---\n%s", notOwned)
	}
}

// TestSquadOperatingProtocolKeyingParagraphIsGeneratedFromReconciliationKey
// is the executable seam Opus's BLOCK on PR #12 (63f1e35a8) required:
// squadOperatingProtocolFor's rendered text must be proven to come FROM
// ReconciliationKey's actual precedence, not from a second hand-maintained
// prose copy that merely happens to agree with it today.
//
// It does this two ways:
//  1. Exact-substitution proof: the protocol text must contain
//     reconciliationKeyingParagraph()'s output verbatim — there is no
//     separate literal a maintainer could edit without touching the
//     function.
//  2. Mutation proof: if ReconciliationKey's precedence were reverted to
//     comment-first (the pre-fix behavior), the rendered protocol text
//     would say so — because the paragraph is generated by calling it, a
//     prose-only equal-footing regression is no longer possible without
//     also changing ReconciliationKey, and changing ReconciliationKey is
//     exactly what TestReconciliationKey_CrossChannelReplayCollapsesToSameKey
//     above catches.
func TestSquadOperatingProtocolKeyingParagraphIsGeneratedFromReconciliationKey(t *testing.T) {
	protocol := squadOperatingProtocolFor(true)
	generated := reconciliationKeyingParagraph()

	if !strings.Contains(protocol, generated) {
		t.Fatalf("squadOperatingProtocolFor must render reconciliationKeyingParagraph()'s exact output — the seam is broken if the protocol text diverges from what the function produces\n--- generated ---\n%s\n--- protocol ---\n%s", generated, protocol)
	}

	// The generated paragraph must itself reflect ReconciliationKey's real
	// precedence order: SHA-first, revision-second, comment-last. If
	// ReconciliationKey's precedence changed, these substrings would change
	// with it (they are built from its actual return values, not typed
	// literals) and this assertion would catch the divergence at the
	// paragraph-generation level, independent of the protocol-composition
	// check above.
	shaKey := ReconciliationKey(ReconciliationEvent{CandidateSHA: "<sha>", PRRevision: "<revision>", ReportingCommentID: "<comment>"})
	revisionKey := ReconciliationKey(ReconciliationEvent{PRRevision: "<revision>", ReportingCommentID: "<comment>"})
	commentKey := ReconciliationKey(ReconciliationEvent{ReportingCommentID: "<comment>"})

	for _, want := range []string{shaKey, revisionKey, commentKey} {
		if !strings.Contains(generated, want) {
			t.Errorf("reconciliationKeyingParagraph() must embed ReconciliationKey's actual output %q\n--- paragraph ---\n%s", want, generated)
		}
	}
}
