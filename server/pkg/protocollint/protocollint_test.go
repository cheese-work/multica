package protocollint

import (
	"regexp"
	"testing"
)

// githubMergeAnnouncementPRURLPattern is a literal copy of
// server/internal/handler/github_merge_announcement.go's githubPRURLRe. This
// package cannot import internal/handler (the reverse would create an import
// cycle in the allowed dependency direction), so the two literals are pinned
// together here: a change to either pattern without updating the other fails
// this test.
const githubMergeAnnouncementPRURLPattern = `^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)/?$`

// TestEvidenceURLPatternMatchesGitHubHandlerPattern guards against
// githubPRURLPattern drifting from the server's own GitHub PR URL parser. If
// this test starts failing, the fix is to copy the new pattern from
// github_merge_announcement.go's githubPRURLRe into protocollint.go's
// githubPRURLPattern verbatim.
func TestEvidenceURLPatternMatchesGitHubHandlerPattern(t *testing.T) {
	t.Parallel()

	if githubPRURLPattern.String() != githubMergeAnnouncementPRURLPattern {
		t.Fatalf("protocollint's githubPRURLPattern = %q, want it to match github_merge_announcement.go's githubPRURLRe = %q",
			githubPRURLPattern.String(), githubMergeAnnouncementPRURLPattern)
	}

	// Belt and suspenders: also prove the two compiled patterns agree on
	// actual inputs, not just their source text.
	reference := regexp.MustCompile(githubMergeAnnouncementPRURLPattern)
	for _, url := range []string{
		"https://github.com/multica-ai/multica/pull/1234",
		"https://github.com/multica-ai/multica/pull/1234/",
		"https://github.com/multica-ai/multica/pull/1234/files",
		"https://github.com/multica-ai/multica/issues/1234",
		"not a url",
	} {
		if got, want := githubPRURLPattern.MatchString(url), reference.MatchString(url); got != want {
			t.Errorf("MatchString(%q) = %v, want %v (reference pattern)", url, got, want)
		}
	}
}

// TestCheckPassesOnValidNoActionTurn pins CHE-529 step 3's requirement that a
// turn with genuinely no issue/comment/status activity — no_action and
// non-issue runs alike — must pass cleanly rather than fail because "no
// comment was posted".
func TestCheckPassesOnValidNoActionTurn(t *testing.T) {
	t.Parallel()

	violations := Check(Input{RunID: "run-1"})
	if len(violations) != 0 {
		t.Fatalf("Check() on zero-value Input = %v, want no violations", violations)
	}
}

// TestCheckPassesOnValidReplyAndStatusFlow is the "everything done right"
// case: the run replies under its own trigger comment, changes status, and
// records a readback, with no evidence claim and no waiver claim.
func TestCheckPassesOnValidReplyAndStatusFlow(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID:            "run-2",
		TriggerCommentID: "trigger-1",
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "trigger-1", Content: "Done: shipped the fix."},
		},
		StatusChanged:  true,
		StatusReadBack: true,
	}
	if violations := Check(in); len(violations) != 0 {
		t.Fatalf("Check() on valid flow = %v, want no violations", violations)
	}
}

// TestCheckFailsOnReplyParentMismatch is the required failing case: a run
// triggered by a specific comment posts a reply parented somewhere else. This
// must fail loudly and name the exact assertion (CodeReplyParentMismatch),
// not return a silent boolean.
func TestCheckFailsOnReplyParentMismatch(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID:            "run-3",
		TriggerCommentID: "trigger-1",
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "some-other-comment", Content: "Done."},
		},
	}
	violations := Check(in)
	if len(violations) != 1 {
		t.Fatalf("Check() = %v, want exactly 1 violation", violations)
	}
	if violations[0].Code != CodeReplyParentMismatch {
		t.Fatalf("Check()[0].Code = %q, want %q", violations[0].Code, CodeReplyParentMismatch)
	}
	if violations[0].Message == "" {
		t.Fatal("violation message must not be empty — the check must fail loudly, not as a bare boolean")
	}
}

// TestCheckFailsOnTopLevelReplyUnderTrigger covers the other reply-mismatch
// shape: a comment-triggered run posting a brand new top-level comment
// instead of a reply. Mirrors taskCoversReplyParent's own write-time rule in
// server/internal/handler/comment.go, checked here again against persisted
// data.
func TestCheckFailsOnTopLevelReplyUnderTrigger(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID:            "run-4",
		TriggerCommentID: "trigger-1",
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "", Content: "Done."},
		},
	}
	violations := Check(in)
	if len(violations) != 1 || violations[0].Code != CodeReplyParentMismatch {
		t.Fatalf("Check() = %v, want exactly 1 %q violation", violations, CodeReplyParentMismatch)
	}
}

// TestCheckAllowsReplyUnderCoalescedComment covers MUL-4195: a run whose
// completion covers multiple triggering comments (agent_task_queue's
// coalesced_comment_ids) may legitimately reply under any of them, not just
// TriggerCommentID — mirroring taskCoversReplyParent server-side
// (comment.go), which accepts both sources as valid parents.
func TestCheckAllowsReplyUnderCoalescedComment(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID:               "run-3b",
		TriggerCommentID:    "trigger-1",
		CoalescedCommentIDs: []string{"trigger-2", "trigger-3"},
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "trigger-2", Content: "Addressing both threads."},
		},
	}
	if violations := Check(in); len(violations) != 0 {
		t.Fatalf("Check() = %v, want no violations for a reply parented under a coalesced comment", violations)
	}
}

// TestCheckFailsOnReplyParentNotInTriggerOrCoalesced ensures a coalesced list
// narrows, rather than widens, what still counts as a mismatch: a parent
// outside both TriggerCommentID and CoalescedCommentIDs must still fail.
func TestCheckFailsOnReplyParentNotInTriggerOrCoalesced(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID:               "run-3c",
		TriggerCommentID:    "trigger-1",
		CoalescedCommentIDs: []string{"trigger-2"},
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "some-unrelated-comment", Content: "Done."},
		},
	}
	violations := Check(in)
	if len(violations) != 1 || violations[0].Code != CodeReplyParentMismatch {
		t.Fatalf("Check() = %v, want exactly 1 %q violation", violations, CodeReplyParentMismatch)
	}
}

// TestCheckAllowsCoalescedOrUnrelatedIssueReplies: a run with no trigger
// comment (assignment-triggered) is free to post top-level comments — there is
// no parent to enforce.
func TestCheckAllowsTopLevelCommentsWithoutATrigger(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID: "run-5",
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "", Content: "Assignment-triggered run reporting in."},
		},
	}
	if violations := Check(in); len(violations) != 0 {
		t.Fatalf("Check() = %v, want no violations for a trigger-less run", violations)
	}
}

// TestCheckFailsOnStatusChangeWithoutReadback is the second required failing
// case: the run changed status but Input carries no evidence it observed the
// result.
func TestCheckFailsOnStatusChangeWithoutReadback(t *testing.T) {
	t.Parallel()

	in := Input{RunID: "run-6", StatusChanged: true, StatusReadBack: false}
	violations := Check(in)
	if len(violations) != 1 || violations[0].Code != CodeStatusChangeNotReadBack {
		t.Fatalf("Check() = %v, want exactly 1 %q violation", violations, CodeStatusChangeNotReadBack)
	}
}

// TestCheckSkipsUnverifiableReadback documents the intentionally conservative
// direction: StatusReadBack can only ever be set true by a caller holding
// independent evidence (see Input.StatusReadBack's doc). A caller that does
// not have such evidence must leave it false, which fails the check — Check
// never invents a passing verdict for a sub-assertion it cannot observe.
func TestCheckSkipsUnverifiableReadback(t *testing.T) {
	t.Parallel()

	noChange := Input{RunID: "run-7", StatusChanged: false, StatusReadBack: false}
	if violations := Check(noChange); len(violations) != 0 {
		t.Fatalf("Check() with no status change = %v, want no violations", violations)
	}
}

func TestCheckEvidenceURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "empty is fine", url: "", wantErr: false},
		{name: "valid github PR url", url: "https://github.com/multica-ai/multica/pull/1234", wantErr: false},
		{name: "valid github PR url with trailing slash", url: "https://github.com/multica-ai/multica/pull/1234/", wantErr: false},
		{name: "trailing content after the PR number is rejected", url: "https://github.com/multica-ai/multica/pull/1234/files", wantErr: true},
		{name: "not a PR url at all", url: "not a url", wantErr: true},
		{name: "github but not a PR path", url: "https://github.com/multica-ai/multica/issues/1234", wantErr: true},
		{name: "wrong host", url: "https://example.com/multica-ai/multica/pull/1234", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			violations := Check(Input{RunID: "run-evidence", ClaimedEvidenceURL: tc.url})
			gotErr := len(violations) != 0
			if gotErr != tc.wantErr {
				t.Fatalf("Check() with ClaimedEvidenceURL=%q violations=%v, wantErr=%v", tc.url, violations, tc.wantErr)
			}
			if gotErr && violations[0].Code != CodeEvidenceURLMalformed {
				t.Fatalf("Check()[0].Code = %q, want %q", violations[0].Code, CodeEvidenceURLMalformed)
			}
		})
	}
}

func TestCheckUnsupportedWaiver(t *testing.T) {
	t.Parallel()

	t.Run("claim without any grant fails", func(t *testing.T) {
		t.Parallel()
		in := Input{
			RunID: "run-8",
			PostedComments: []PostedComment{
				{ID: "c1", Content: "Skipping the CI gate — this step was explicitly waived."},
			},
		}
		violations := Check(in)
		if len(violations) != 1 || violations[0].Code != CodeUnsupportedWaiver {
			t.Fatalf("Check() = %v, want exactly 1 %q violation", violations, CodeUnsupportedWaiver)
		}
	})

	t.Run("claim with a genuine member grant passes", func(t *testing.T) {
		t.Parallel()
		in := Input{
			RunID: "run-9",
			PostedComments: []PostedComment{
				{ID: "c1", Content: "Skipping the CI gate — this step was explicitly waived."},
			},
			OtherComments: []OtherComment{
				{AuthorType: "member", Content: "You can skip the CI gate for this one, I'll verify manually."},
			},
		}
		if violations := Check(in); len(violations) != 0 {
			t.Fatalf("Check() with a genuine member grant = %v, want no violations", violations)
		}
	})

	t.Run("unrelated member grant does not suppress a protocol claim", func(t *testing.T) {
		t.Parallel()
		in := Input{
			RunID: "run-9b",
			PostedComments: []PostedComment{
				{ID: "c1", Content: "The protocol review was waived."},
			},
			OtherComments: []OtherComment{
				{AuthorType: "member", Content: "ABI waiver granted by release policy."},
			},
		}
		violations := Check(in)
		if len(violations) != 1 || violations[0].Code != CodeUnsupportedWaiver {
			t.Fatalf("Check() = %v, want exactly 1 %q violation", violations, CodeUnsupportedWaiver)
		}
	})

	t.Run("an agent's own claim does not count as a grant", func(t *testing.T) {
		t.Parallel()
		in := Input{
			RunID: "run-10",
			PostedComments: []PostedComment{
				{ID: "c1", Content: "This protocol review was waived per approval."},
			},
			OtherComments: []OtherComment{
				// Another agent (e.g. a squad leader) echoing the same claim
				// is not a human waiver — only "member" counts.
				{AuthorType: "agent", Content: "Waiver granted, you may skip that."},
			},
		}
		violations := Check(in)
		if len(violations) != 1 || violations[0].Code != CodeUnsupportedWaiver {
			t.Fatalf("Check() = %v, want exactly 1 %q violation (agent grant must not count)", violations, CodeUnsupportedWaiver)
		}
	})

	t.Run("no claim at all is unaffected by absence of any grant", func(t *testing.T) {
		t.Parallel()
		in := Input{
			RunID: "run-11",
			PostedComments: []PostedComment{
				{ID: "c1", Content: "Implemented the feature and opened a PR."},
			},
		}
		if violations := Check(in); len(violations) != 0 {
			t.Fatalf("Check() with no waiver claim = %v, want no violations", violations)
		}
	})
}

// TestCheckIgnoresUnrelatedWaiverVocabulary is CHE-681's regression coverage:
// waiverClaimRe must not fire on real engineering text that merely mentions
// the word "waiver" outside a fabricated-authorization claim. Cases are
// seeded verbatim from the false positives found in production data
// (protocol_lint_run, 2026-09-18..21): CI/ABI governance language and
// self-referential discussion of this very check.
func TestCheckIgnoresUnrelatedWaiverVocabulary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
	}{
		{
			name:    "CD/ABI waiver governance language",
			content: "This needs a scoped waiver for this stack before the gate reopens.",
		},
		{
			name:    "ABI check waiver reference",
			content: "Filed a waiver on `checkLegacyAbi` for the RevenueCat SDK bump.",
		},
		{
			name:    "self-referential discussion of this check",
			content: "This is about checkUnsupportedWaivers's own waiver-grant lookup bug, not a real claim.",
		},
		{
			name:    "ABI waiver with a later status reference",
			content: "The ABI check was waived by release policy. Status reporting remains unchanged.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := Input{
				RunID:          "run-che681",
				PostedComments: []PostedComment{{ID: "c1", Content: tc.content}},
			}
			if violations := Check(in); len(violations) != 0 {
				t.Fatalf("Check() with unrelated waiver text %q = %v, want no violations", tc.content, violations)
			}
		})
	}
}

// TestWaiverClaimAndGrantBindToWorkflowStep is CHE-681's second regression
// table: the waiver verb must apply to a Multica workflow step, not merely
// share a sentence with one. Genuine claims, including passive forms, still
// fire; CI/ABI governance and quoted discussion of this check do not.
func TestWaiverClaimAndGrantBindToWorkflowStep(t *testing.T) {
	t.Parallel()

	claims := []struct {
		content string
		want    bool
	}{
		{"The protocol review was skipped with approval.", true},
		{"Skipped the status readback per waiver.", true},
		{"I waived the verification step since it was trivial.", true},
		{"Review #2 has been waived.", true},
		{"The protocol review waiver was granted.", true},
		{"Waiver granted for the verification step.", true},
		{"The `protocol review` was waived.", true},
		{"The independent review was skipped with approval.", true},
		{"The verification step was waived.", true},
		{"The review step was waived.", true},
		{"The evidence step was waived.", true},
		{"The review step was skipped with approval.", true},
		{"Skipped the verification step with approval.", true},
		{"The verification and review steps were waived.", true},
		{"The ABI step was waived.", false},
		{"The workflow steps were waived.", true},
		{"The protocol steps were skipped with approval.", true},
		{"All the review and verification steps were waived.", true},
		{"The ABI verification steps were waived under release policy.", false},
		{"The ABI review and verification steps were skipped with approval.", false},
		{"The ABI review was waived under release policy.", false},
		{"The ABI verification was skipped with approval.", false},
		{"The ABI check was waived by release policy; protocol review remains mandatory.", false},
		{"The ABI waiver was granted; protocol review remains mandatory.", false},
		{"The ABI check was waived by release policy, so the status comment follows.", false},
		{"The CI release was skipped with approval; review continues.", false},
		{"The required status check was waived for the hotfix branch.", false},
		{"checkUnsupportedWaivers reports when a posted comment says a step was waived.", false},
		{"The lint flags `this step was explicitly waived` as a claim.", false},
		{"Regression input: \"The protocol review was skipped with approval.\"", false},
	}
	for _, tc := range claims {
		t.Run("claim/"+tc.content, func(t *testing.T) {
			t.Parallel()
			in := Input{RunID: "run-che681", PostedComments: []PostedComment{{ID: "c1", Content: tc.content}}}
			if got := len(Check(in)) == 1; got != tc.want {
				t.Fatalf("Check(%q) flagged=%v, want %v", tc.content, got, tc.want)
			}
		})
	}

	grants := []struct {
		content string
		want    bool
	}{
		{"You may skip the protocol review here.", true},
		{"Okay to skip the verification step.", true},
		{"Protocol review waiver granted for this PR.", true},
		{"I waive `protocol review`.", true},
		{"You may skip the workflow steps for this hotfix.", true},
		{"You may skip the ABI verification steps.", false},
		{"You may skip review of the ABI check.", false},
		{"I waive the ABI review.", false},
		{"The protocol review waiver was granted.", true},
		{"ABI waiver granted by release policy; the review stays required.", false},
		{"ABI waiver granted; protocol review remains mandatory.", false},
		{"You can skip the ABI check, but the status comment still needs posting.", false},
		{"waiverGrantRe should match `you can skip the CI gate`.", false},
	}
	for _, tc := range grants {
		t.Run("grant/"+tc.content, func(t *testing.T) {
			t.Parallel()
			in := Input{
				RunID:          "run-che681",
				PostedComments: []PostedComment{{ID: "c1", Content: "The protocol review was skipped with approval."}},
				OtherComments:  []OtherComment{{AuthorType: "member", Content: tc.content}},
			}
			if got := len(Check(in)) == 0; got != tc.want {
				t.Fatalf("member grant %q accepted=%v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestCheckReportsEveryUnsupportedWaiverClaim: each fabricated claim is an
// independent violation, not just the first one found.
func TestCheckReportsEveryUnsupportedWaiverClaim(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID: "run-12",
		PostedComments: []PostedComment{
			{ID: "c1", Content: "Protocol review A was explicitly waived."},
			{ID: "c2", Content: "Verification B was also explicitly waived."},
		},
	}
	violations := Check(in)
	if len(violations) != 2 {
		t.Fatalf("Check() = %v, want 2 violations (one per fabricated claim)", violations)
	}
}

// TestCheckAggregatesMultipleIndependentViolations: a turn that fails more
// than one assertion at once must report all of them, not stop at the first.
func TestCheckAggregatesMultipleIndependentViolations(t *testing.T) {
	t.Parallel()

	in := Input{
		RunID:              "run-13",
		TriggerCommentID:   "trigger-1",
		StatusChanged:      true,
		StatusReadBack:     false,
		ClaimedEvidenceURL: "not-a-url",
		PostedComments: []PostedComment{
			{ID: "reply-1", ParentID: "wrong-parent", Content: "Done, the remaining protocol review was waived."},
		},
	}
	violations := Check(in)
	codes := make(map[string]bool, len(violations))
	for _, v := range violations {
		codes[v.Code] = true
	}
	for _, want := range []string{
		CodeReplyParentMismatch,
		CodeStatusChangeNotReadBack,
		CodeEvidenceURLMalformed,
		CodeUnsupportedWaiver,
	} {
		if !codes[want] {
			t.Errorf("Check() = %v, missing expected violation %q", violations, want)
		}
	}
}
