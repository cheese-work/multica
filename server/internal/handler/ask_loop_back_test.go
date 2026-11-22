package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6434 (S8 of CHE-6385, plan r3 T5c): a member answer in an ask thread is
// echoed as one system line in the source thread. No mention, no run, no
// resolution change; an edit posts nothing; a deleted source root is skipped.

func setAskLoopBack(t *testing.T, value string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object('ask_loop_back', $1::text) WHERE id = $2`, value, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'ask_loop_back' WHERE id = $1`, testWorkspaceID)
	})
}

type loopBackFixture struct {
	fromThreadFixture
	askRoot    string // the ask thread's root, opened with from_thread
	memberName string
}

func newLoopBackFixture(t *testing.T, setting string) loopBackFixture {
	t.Helper()
	if setting != "" {
		setAskLoopBack(t, setting)
	}
	f := loopBackFixture{fromThreadFixture: newFromThreadFixture(t)}
	f.askRoot = f.askOK(t, f.sourceRoot)
	dbfx.QueryRow(t, `SELECT name FROM "user" WHERE id = $1`, testUserID).Scan(&f.memberName)
	return f
}

// memberPost posts as the workspace member (web session), the way a person answers.
func (f loopBackFixture) memberPost(t *testing.T, parentID, content string) string {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": content, "parent_id": parentID}), "id", f.issueID)
	res := testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)
	var out CommentResponse
	res.JSON(&out)
	return out.ID
}

func (f loopBackFixture) systemLines(t *testing.T) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT content FROM comment WHERE issue_id = $1 AND parent_id = $2 AND author_type = 'system' ORDER BY created_at, id`, f.issueID, f.sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, s)
	}
	return lines
}

// loopLines are the source-thread system lines other than the "Asked" stub.
func (f loopBackFixture) loopLines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, s := range f.systemLines(t) {
		if !strings.HasPrefix(s, "Asked ") {
			out = append(out, s)
		}
	}
	return out
}

func (f loopBackFixture) runs(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
}

func TestAskLoopBackMemberReplyPostsOneQuotedLine(t *testing.T) {
	f := newLoopBackFixture(t, "on")

	f.memberPost(t, f.askRoot, "Ship it.\nUse option B.")

	want := f.memberName + " answered in thread " + f.askRoot + ":\n\n> Ship it.\n> Use option B."
	got := f.loopLines(t)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("source thread lines = %q, want exactly %q", got, want)
	}
	var parentID, authorType string
	dbfx.QueryRow(t, `SELECT parent_id, author_type FROM comment WHERE issue_id = $1 AND content = $2`, f.issueID, want).Scan(&parentID, &authorType)
	if parentID != f.sourceRoot || authorType != "system" {
		t.Errorf("loop-back parent=%s author=%s, want a system reply under %s", parentID, authorType, f.sourceRoot)
	}
}

func TestAskLoopBackCutsTheQuoteAt280Characters(t *testing.T) {
	f := newLoopBackFixture(t, "on")

	f.memberPost(t, f.askRoot, strings.Repeat("é", 280))
	f.memberPost(t, f.askRoot, strings.Repeat("é", 281))

	got := f.loopLines(t)
	if len(got) != 2 {
		t.Fatalf("lines = %d, want 2: %q", len(got), got)
	}
	header := f.memberName + " answered in thread " + f.askRoot + ":\n\n> "
	if want := header + strings.Repeat("é", 280); got[0] != want {
		t.Errorf("280 characters were cut: %q", got[0])
	}
	if want := header + strings.Repeat("é", 280) + "…"; got[1] != want {
		t.Errorf("281 characters: got %q, want a cut at 280 plus an ellipsis", got[1])
	}
}

func TestAskLoopBackQuoteCarriesNoMentionLink(t *testing.T) {
	f := newLoopBackFixture(t, "on")

	f.memberPost(t, f.askRoot, "Ask "+f.mention()+" please")

	got := f.loopLines(t)
	if len(got) != 1 || strings.Contains(got[0], "mention://") || !strings.Contains(got[0], "Ask Human please") {
		t.Fatalf("lines = %q, want the name without a mention link", got)
	}
}

func TestAskLoopBackStartsNoRunAndKeepsResolution(t *testing.T) {
	f := newLoopBackFixture(t, "on")
	dbfx.Exec(t, `UPDATE comment SET resolved_at = now(), resolved_by_type = 'member', resolved_by_id = $2 WHERE id = $1`, f.sourceRoot, testUserID)
	f.memberPost(t, f.askRoot, "Answer")

	lines := f.loopLines(t)
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want 1", lines)
	}
	var loopID string
	dbfx.QueryRow(t, `SELECT id FROM comment WHERE issue_id = $1 AND content = $2`, f.issueID, lines[0]).Scan(&loopID)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND (trigger_comment_id = $2 OR $2 = ANY(coalesced_comment_ids))`, f.issueID, loopID); n != 0 {
		t.Errorf("runs triggered by the loop-back = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id IN (SELECT id FROM comment WHERE issue_id = $1 AND author_type = 'system')`, f.issueID); n != 0 {
		t.Errorf("runs triggered by any system comment = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, f.sourceRoot); n != 1 {
		t.Errorf("source thread resolution changed")
	}
}

func TestAskLoopBackEditPostsNothing(t *testing.T) {
	f := newLoopBackFixture(t, "on")
	id := f.memberPost(t, f.askRoot, "First words")
	before := f.comments(t)

	req := withURLParam(newRequest(http.MethodPut, "/api/comments/"+id, map[string]any{"content": "Second words"}), "commentId", id)
	testutil.Call(t, testHandler.UpdateComment, req).Want(http.StatusOK)

	if after := f.comments(t); after != before {
		t.Errorf("comments before the edit %d, after %d; an edit must post nothing", before, after)
	}
	if n := len(f.loopLines(t)); n != 1 {
		t.Errorf("loop-back lines = %d, want 1 (the original)", n)
	}
}

func TestAskLoopBackSkipsADeletedSourceRoot(t *testing.T) {
	for name, kill := range map[string]string{
		"soft": `UPDATE comment SET deleted_at = now() WHERE id = $1`,
		"hard": `DELETE FROM comment WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newLoopBackFixture(t, "on")
			dbfx.Exec(t, kill, f.sourceRoot)
			before := f.comments(t)

			f.memberPost(t, f.askRoot, "Answer")

			if after := f.comments(t); after != before+1 {
				t.Errorf("comments before %d, after %d; only the member's comment should be new", before, after)
			}
		})
	}
}

func TestAskLoopBackOffByDefaultAndForOtherThreads(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		f := newLoopBackFixture(t, "")
		f.memberPost(t, f.askRoot, "Answer")
		if got := f.loopLines(t); len(got) != 0 {
			t.Errorf("lines = %q, want none with the setting absent", got)
		}
	})
	t.Run("off", func(t *testing.T) {
		f := newLoopBackFixture(t, "off")
		f.memberPost(t, f.askRoot, "Answer")
		if got := f.loopLines(t); len(got) != 0 {
			t.Errorf("lines = %q, want none", got)
		}
	})
	t.Run("a thread with no source", func(t *testing.T) {
		f := newLoopBackFixture(t, "on")
		f.memberPost(t, f.sourceRoot, "A plain reply")
		if got := f.loopLines(t); len(got) != 0 {
			t.Errorf("lines = %q, want none for a thread that is not an ask thread", got)
		}
	})
	t.Run("an agent reply in the ask thread", func(t *testing.T) {
		f := newLoopBackFixture(t, "on")
		f.insertComment(t, f.issueID, f.askRoot, "agent words")
		f.post(t, map[string]any{"content": "more", "parent_id": f.askRoot}).Want(http.StatusCreated)
		if got := f.loopLines(t); len(got) != 0 {
			t.Errorf("lines = %q, want none for agent comments", got)
		}
	})
	t.Run("the member's own ask root", func(t *testing.T) {
		f := newLoopBackFixture(t, "on")
		req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": "my question", "from_thread": f.sourceRoot}), "id", f.issueID)
		testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)
		if got := f.loopLines(t); len(got) != 0 {
			t.Errorf("lines = %q, want none: opening an ask is not an answer", got)
		}
	})
}

func TestAskLoopBackBadStoredValueNeverFailsTheComment(t *testing.T) {
	f := newLoopBackFixture(t, "")
	dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || '{"ask_loop_back": 7}'::jsonb WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'ask_loop_back' WHERE id = $1`, testWorkspaceID)
	})

	f.memberPost(t, f.askRoot, "Answer")

	if got := f.loopLines(t); len(got) != 0 {
		t.Errorf("lines = %q, want none for a bad stored value", got)
	}
}

func TestAskLoopBackSettingIsValidated(t *testing.T) {
	for _, ok := range []any{"on", "off"} {
		if err := validateAskLoopBackSetting(map[string]any{"ask_loop_back": ok}); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	if err := validateAskLoopBackSetting(map[string]any{}); err != nil {
		t.Errorf("an absent key refused: %v", err)
	}
	for _, bad := range []any{"warn", "", nil, 1, true} {
		if err := validateAskLoopBackSetting(map[string]any{"ask_loop_back": bad}); err == nil {
			t.Errorf("%v accepted, want a refusal", bad)
		}
	}
}

// cardAskRoot is the thread root of the card's ask comment.
func cardAskRoot(t *testing.T, card askedRecordFixture) string {
	t.Helper()
	var root string
	dbfx.QueryRow(t, `WITH RECURSIVE up AS (SELECT id, parent_id FROM comment WHERE id = $1 UNION ALL SELECT c.id, c.parent_id FROM comment c JOIN up ON c.id = up.parent_id) SELECT id FROM up WHERE parent_id IS NULL`, card.parentID).Scan(&root)
	return root
}

// The decision card answer: one line in the source thread naming the option.
func TestAskLoopBackCardAnswerPostsTheDecidedLine(t *testing.T) {
	setAskLoopBack(t, "on")
	card := newCardFixture(t)
	source := dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": card.issueID, "author_type": "member", "author_id": testUserID, "content": "source root", "type": "comment"})
	askRoot := cardAskRoot(t, card)
	dbfx.Exec(t, `UPDATE comment SET ask_source_root_id = $2 WHERE id = $1`, askRoot, source)
	var name string
	dbfx.QueryRow(t, `SELECT name FROM "user" WHERE id = $1`, card.humanID).Scan(&name)
	before := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, card.issueID)

	if _, err := card.answerOnCard(decisionCardAnswerInput{Option: "Option B"}); err != nil {
		t.Fatal(err)
	}

	want := name + " decided: Option B (thread " + askRoot + ")."
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND parent_id = $2 AND author_type = 'system' AND content = $3`, card.issueID, source, want); n != 1 {
		t.Fatalf("decided line %q in the source thread: %d rows, want 1", want, n)
	}
	if after := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, card.issueID); after != before {
		t.Errorf("agent_task_queue rows %d -> %d; the loop-back starts no run", before, after)
	}
}

func TestAskLoopBackCardAnswerOffPostsNothing(t *testing.T) {
	card := newCardFixture(t)
	source := dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": card.issueID, "author_type": "member", "author_id": testUserID, "content": "source root", "type": "comment"})
	askRoot := cardAskRoot(t, card)
	dbfx.Exec(t, `UPDATE comment SET ask_source_root_id = $2 WHERE id = $1`, askRoot, source)

	if _, err := card.answerOnCard(decisionCardAnswerInput{Option: "Option B"}); err != nil {
		t.Fatal(err)
	}

	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND parent_id = $2 AND author_type = 'system'`, card.issueID, source); n != 0 {
		t.Errorf("system lines in the source thread = %d, want 0 with the setting off", n)
	}
}
