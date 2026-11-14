package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-5456: a wakeup's declared inputs fingerprint to one string; any doubt
// about completeness yields "" so the wakeup runs.

type declaredEnv struct {
	skipEnv
}

func newDeclaredEnv(t *testing.T, linkPR bool) declaredEnv {
	t.Helper()
	return declaredEnv{newSkipEnv(t, linkPR)}
}

func (e declaredEnv) fp(t *testing.T, inputs ...string) string {
	t.Helper()
	return e.fpAt(t, 1, "watch", inputs...)
}

func (e declaredEnv) fpAt(t *testing.T, revision int32, instruction string, inputs ...string) string {
	t.Helper()
	fp, err := declaredInputsFingerprint(context.Background(), e.f.Pool, e.issue, inputs, revision, instruction)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func (e declaredEnv) comment(t *testing.T, issue pgtype.UUID, parent any, content string) string {
	t.Helper()
	var id string
	e.f.QueryRow(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content, parent_id)
		VALUES ($1, $2, 'member', $3, $4, $5) RETURNING id`, e.f.WorkspaceID, issue, e.f.UserID, content, parent).Scan(&id)
	e.f.Cleanup(t, `DELETE FROM comment WHERE id=$1`, id)
	return id
}

// changes asserts the fingerprint is stable across a no-op and differs after mutate.
func (e declaredEnv) changes(t *testing.T, mutate func(), inputs ...string) {
	t.Helper()
	before := e.fp(t, inputs...)
	if before == "" {
		t.Fatalf("fingerprint of %v is empty, want a value", inputs)
	}
	if again := e.fp(t, inputs...); again != before {
		t.Fatalf("fingerprint of %v changed with no change: %q vs %q", inputs, before, again)
	}
	mutate()
	if after := e.fp(t, inputs...); after == before || after == "" {
		t.Fatalf("fingerprint of %v after a change = %q, want a value different from %q", inputs, after, before)
	}
}

func TestDeclaredInputsFingerprintIssue(t *testing.T) {
	e := newDeclaredEnv(t, false)
	e.changes(t, func() { e.f.Exec(t, `UPDATE issue SET status='in_review' WHERE id=$1`, e.issue) }, "issue")
	e.changes(t, func() { e.f.Exec(t, `UPDATE issue SET revision=revision+1 WHERE id=$1`, e.issue) }, "issue")
	e.changes(t, func() {
		e.f.Exec(t, `UPDATE issue SET assignee_type='member', assignee_id=$2 WHERE id=$1`, e.issue, e.f.UserID)
	}, "issue")
}

func TestDeclaredInputsFingerprintPR(t *testing.T) {
	e := newDeclaredEnv(t, true)
	e.changes(t, func() { e.f.Exec(t, `UPDATE github_pull_request SET head_sha='h2' WHERE id=$1`, e.prID) }, "pr")
	e.changes(t, func() {
		e.f.Exec(t, `UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1`, e.prID)
	}, "pr")
	e.changes(t, func() {
		e.f.Exec(t, `UPDATE github_pull_request SET api_merge_state_status='DIRTY' WHERE id=$1`, e.prID)
	}, "pr")
}

func TestDeclaredInputsFingerprintChildren(t *testing.T) {
	e := newDeclaredEnv(t, false)
	child := e.f.Issue(t, "child")
	e.f.Exec(t, `UPDATE issue SET parent_issue_id=$2 WHERE id=$1`, child, e.issue)
	e.changes(t, func() { e.f.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, child) }, "children")
	e.changes(t, func() { e.f.Exec(t, `UPDATE issue SET revision=revision+1 WHERE id=$1`, child) }, "children")
	e.changes(t, func() {
		c2 := e.f.Issue(t, "second child")
		e.f.Exec(t, `UPDATE issue SET parent_issue_id=$2 WHERE id=$1`, c2, e.issue)
	}, "children")
}

func TestDeclaredInputsFingerprintThread(t *testing.T) {
	e := newDeclaredEnv(t, false)
	root := e.comment(t, e.issue, nil, "root")
	reply := e.comment(t, e.issue, root, "reply")
	in := "thread:" + root
	e.changes(t, func() { e.f.Exec(t, `UPDATE comment SET content='edited', revision=revision+1 WHERE id=$1`, root) }, in)
	e.changes(t, func() { e.f.Exec(t, `UPDATE comment SET content='edited reply' WHERE id=$1`, reply) }, in)
	e.changes(t, func() { e.comment(t, e.issue, root, "new reply") }, in)
	e.changes(t, func() {
		e.f.Exec(t, `UPDATE comment SET resolved_at=now(), resolved_by_type='member', resolved_by_id=$2 WHERE id=$1`, root, e.f.UserID)
	}, in)
	e.changes(t, func() { e.f.Exec(t, `UPDATE comment SET deleted_at=now() WHERE id=$1`, reply) }, in)
}

func TestDeclaredInputsFingerprintIgnoresOtherInputs(t *testing.T) {
	e := newDeclaredEnv(t, false)
	before := e.fp(t, "issue")
	e.comment(t, e.issue, nil, "unrelated")
	if after := e.fp(t, "issue"); after != before {
		t.Fatalf("an undeclared comment changed the issue fingerprint: %q vs %q", before, after)
	}
}

func TestDeclaredInputsFingerprintFailsOpen(t *testing.T) {
	e := newDeclaredEnv(t, false)
	otherIssue := parseTestUUID(t, e.f.Issue(t, "other issue"))
	foreign := e.comment(t, otherIssue, nil, "on another issue")
	for name, inputs := range map[string][]string{
		"empty list":        nil,
		"unknown value":     {"issue", "deadline"},
		"no linked PR":      {"issue", "pr"},
		"foreign thread":    {"issue", "thread:" + foreign},
		"missing thread":    {"thread:00000000-0000-0000-0000-000000000000"},
		"malformed thread":  {"thread:not-a-uuid"},
		"thread without id": {"thread:"},
	} {
		if got := e.fp(t, inputs...); got != "" {
			t.Errorf("%s: fingerprint = %q, want empty", name, got)
		}
	}
}

func TestDeclaredInputsFingerprintTracksRevisionAndInstruction(t *testing.T) {
	e := newDeclaredEnv(t, false)
	base := e.fpAt(t, 1, "watch", "issue")
	if base == "" {
		t.Fatal("base fingerprint is empty")
	}
	if got := e.fpAt(t, 2, "watch", "issue"); got == base {
		t.Fatal("a rule revision edit did not change the fingerprint")
	}
	if got := e.fpAt(t, 1, "watch more", "issue"); got == base {
		t.Fatal("an instruction edit did not change the fingerprint")
	}
}
