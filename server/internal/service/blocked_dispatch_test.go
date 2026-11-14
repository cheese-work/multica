package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestBlockedDispatchFingerprintChangesWithInputs(t *testing.T) {
	base := blockedDispatchInputs{Reason: "waiting", PRState: "head-1:checks-failed", Comments: "comment-1"}
	want := blockedDispatchFingerprint(base)
	if want == "" {
		t.Fatal("fingerprint is empty")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*blockedDispatchInputs)
	}{
		{name: "reason", mutate: func(i *blockedDispatchInputs) { i.Reason = "new blocker" }},
		{name: "PR head or checks", mutate: func(i *blockedDispatchInputs) { i.PRState = "head-2:checks-failed" }},
		{name: "other-author comment", mutate: func(i *blockedDispatchInputs) { i.Comments = "comment-1,comment-2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			tc.mutate(&got)
			if fingerprint := blockedDispatchFingerprint(got); fingerprint == want {
				t.Fatalf("fingerprint did not change after %s changed", tc.name)
			}
		})
	}
}

func TestBlockedDispatchFingerprintStableForUnchangedInputs(t *testing.T) {
	inputs := blockedDispatchInputs{Reason: "waiting", PRState: "head-1:checks-failed", Comments: "comment-1"}
	if a, b := blockedDispatchFingerprint(inputs), blockedDispatchFingerprint(inputs); a == "" || a != b {
		t.Fatalf("fingerprints = %q and %q, want identical non-empty values", a, b)
	}
}

func TestBlockedDispatchUnchangedRequiresNonMemberAndSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, actor, previous, current string
		want                           bool
	}{
		{name: "unchanged system re-entry", actor: "agent", previous: "same", current: "same", want: true},
		{name: "member re-entry", actor: "member", previous: "same", current: "same"},
		{name: "changed inputs", actor: "agent", previous: "old", current: "new"},
		{name: "missing snapshot", actor: "agent", current: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockedDispatchUnchanged(tc.actor, tc.previous, tc.current); got != tc.want {
				t.Fatalf("blockedDispatchUnchanged() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlockedDispatchDatabaseFingerprintTracksReasonPRAndComments(t *testing.T) {
	f, _, issue, agent := conditionFixture(t)
	ctx := context.Background()
	agentID, err := util.ParseUUID(agent)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func() string {
		t.Helper()
		got, err := f.q.GetBlockedDispatchFingerprint(ctx, db.GetBlockedDispatchFingerprintParams{IssueID: issue, TofuID: agentID})
		if err != nil {
			t.Fatal(err)
		}
		return blockedDispatchFingerprintRaw(got)
	}
	base := fingerprint()
	f.Exec(t, `UPDATE issue SET metadata=COALESCE(metadata,'{}'::jsonb)||'{"blocked_reason":"waiting"}'::jsonb WHERE id=$1`, issue)
	withReason := fingerprint()
	if withReason == base {
		t.Fatal("fingerprint did not change with blocked reason")
	}
	var prID string
	f.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, api_merge_state_status, checks_rollup_state)
		VALUES ($1, 1, 'acme', 'widget', 157, 't', 'open', 'https://github.com/acme/widget/pull/157', now(), now(), 'head-1', 'CLEAN', 'FAILURE') RETURNING id`, f.WorkspaceID).Scan(&prID)
	f.Cleanup(t, `DELETE FROM github_pull_request WHERE id=$1`, prID)
	f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1,$2)`, issue, prID)
	f.Cleanup(t, `DELETE FROM issue_pull_request WHERE issue_id=$1 AND pull_request_id=$2`, issue, prID)
	withPR := fingerprint()
	if withPR == withReason {
		t.Fatal("fingerprint did not change after linking a PR")
	}
	f.Exec(t, `UPDATE github_pull_request SET head_sha='head-2' WHERE id=$1`, prID)
	withChangedHead := fingerprint()
	if withChangedHead == withPR {
		t.Fatal("fingerprint did not change with the PR head")
	}
	f.Exec(t, `UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1`, prID)
	withChangedChecks := fingerprint()
	if withChangedChecks == withChangedHead {
		t.Fatal("fingerprint did not change with the required-check rollup")
	}
	f.Exec(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content) VALUES ($1, $2, 'member', $3, 'new blocker detail')`, f.WorkspaceID, issue, f.UserID)
	withMemberComment := fingerprint()
	if withMemberComment == withChangedChecks {
		t.Fatal("fingerprint did not change after a member comment")
	}
	f.Exec(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content) VALUES ($1, $2, 'agent', $3, 'tofu note')`, f.WorkspaceID, issue, agentID)
	if got := fingerprint(); got != withMemberComment {
		t.Fatalf("Tofu-authored comment changed the fingerprint: got %s, want %s", got, withMemberComment)
	}
	otherAgentID, err := util.ParseUUID(f.privateAgentOwnedBy(t, f.UserID, "other-wake"))
	if err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content) VALUES ($1, $2, 'agent', $3, 'other agent detail')`, f.WorkspaceID, issue, otherAgentID)
	if fingerprint() == withMemberComment {
		t.Fatal("fingerprint did not change after another agent comment")
	}
}
