package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocollint"
)

// TestBuildProtocolLintInput_WaiverGrantedBeforeRunStartIsFound is the CHE-551
// regression: a member's waiver grant posted BEFORE task.StartedAt — the
// normal case, since that is how an agent would know to cite it — must still
// surface for checkUnsupportedWaivers. windowComments (the since-StartedAt
// read) does not contain the grant at all; only memberComments (the
// full-history, member-authored read) does. If buildProtocolLintInput built
// OtherComments from windowComments instead of memberComments, this run's own
// waiver claim would wrongly report CodeUnsupportedWaiver.
func TestBuildProtocolLintInput_WaiverGrantedBeforeRunStartIsFound(t *testing.T) {
	t.Parallel()

	task := db.AgentTaskQueue{
		ID: parseUUID("11111111-1111-1111-1111-111111111111"),
	}

	grantComment := db.Comment{
		ID:         parseUUID("22222222-2222-2222-2222-222222222222"),
		AuthorType: "member",
		Content:    "You can skip the CI gate for this one, I already verified manually.",
		// Predates task.StartedAt: ListCommentsSinceForIssue's created_at > $3
		// would exclude this row from windowComments.
	}
	replyComment := db.Comment{
		ID:           parseUUID("33333333-3333-3333-3333-333333333333"),
		SourceTaskID: task.ID,
		Content:      "Done — this step was explicitly waived, per the earlier comment.",
	}

	windowComments := []db.Comment{replyComment}
	memberComments := []db.Comment{grantComment, replyComment}

	in := buildProtocolLintInput(task, "", windowComments, memberComments)

	violations := protocollint.Check(in)
	for _, v := range violations {
		if v.Code == protocollint.CodeUnsupportedWaiver {
			t.Fatalf("Check() = %v, want no %q violation: the grant predates task.StartedAt but is present in the full-history read",
				violations, protocollint.CodeUnsupportedWaiver)
		}
	}
}

// TestBuildProtocolLintInput_UnsupportedWaiverStillCaught is the inverse
// guard: widening the waiver scan to full history must not turn into "never
// flag anything" — a claim with no grant anywhere in history still fails.
func TestBuildProtocolLintInput_UnsupportedWaiverStillCaught(t *testing.T) {
	t.Parallel()

	task := db.AgentTaskQueue{
		ID: parseUUID("44444444-4444-4444-4444-444444444444"),
	}
	replyComment := db.Comment{
		ID:           parseUUID("55555555-5555-5555-5555-555555555555"),
		SourceTaskID: task.ID,
		Content:      "Done — this step was explicitly waived.",
	}

	windowComments := []db.Comment{replyComment}
	memberComments := []db.Comment{replyComment}

	in := buildProtocolLintInput(task, "", windowComments, memberComments)

	violations := protocollint.Check(in)
	found := false
	for _, v := range violations {
		if v.Code == protocollint.CodeUnsupportedWaiver {
			found = true
		}
	}
	if !found {
		t.Fatalf("Check() = %v, want a %q violation: no member comment anywhere grants a waiver", violations, protocollint.CodeUnsupportedWaiver)
	}
}

// TestBuildProtocolLintInput_ReplyParentStaysWindowScoped ensures the CHE-551
// widening is confined to the waiver-grant scan: PostedComments must still
// come from windowComments only, not memberComments, so a comment from a
// DIFFERENT, older run does not get misattributed as this run's own reply.
func TestBuildProtocolLintInput_ReplyParentStaysWindowScoped(t *testing.T) {
	t.Parallel()

	task := db.AgentTaskQueue{
		ID:               parseUUID("66666666-6666-6666-6666-666666666666"),
		TriggerCommentID: parseUUID("77777777-7777-7777-7777-777777777777"),
	}
	// Authored by this task, but predates the window (e.g. a stale retry) —
	// must not leak into PostedComments via the full-history read.
	oldOwnComment := db.Comment{
		ID:           parseUUID("88888888-8888-8888-8888-888888888888"),
		SourceTaskID: task.ID,
		ParentID:     parseUUID("99999999-9999-9999-9999-999999999999"), // wrong parent
		Content:      "stale",
	}
	validReply := db.Comment{
		ID:           parseUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		SourceTaskID: task.ID,
		ParentID:     task.TriggerCommentID,
		Content:      "Done.",
	}

	windowComments := []db.Comment{validReply}
	memberComments := []db.Comment{oldOwnComment, validReply}

	in := buildProtocolLintInput(task, "", windowComments, memberComments)

	if len(in.PostedComments) != 1 || in.PostedComments[0].ID != uuidToString(validReply.ID) {
		t.Fatalf("PostedComments = %v, want only the window-scoped reply (stale full-history-only comment must not leak in)", in.PostedComments)
	}

	violations := protocollint.Check(in)
	for _, v := range violations {
		if v.Code == protocollint.CodeReplyParentMismatch {
			t.Fatalf("Check() = %v, want no %q violation: the stale comment from memberComments must not be checked as a posted reply", violations, protocollint.CodeReplyParentMismatch)
		}
	}
}
