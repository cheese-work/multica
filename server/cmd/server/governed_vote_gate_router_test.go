package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1862 correction: the vote gate judged through the real router with
// mat_ task tokens. The writer's token is bound to the workspace owner, so a
// writing agent holds the owner/admin authority that direct-handler tests
// cannot reproduce. Votes are posted through POST /api/issues/{id}/comments by
// each voter's own token.

const (
	rigLabs   = `{"claude/claude-test":"anthropic","codex/gpt-test":"openai"}`
	rigHuman  = "<!-- governed:human -->\nkeep this rule\n<!-- /governed:human -->"
	rigFields = "workspace.context,agent.instructions,squad.instructions,project.description"
)

func rigDigest(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

type voteRig struct {
	t                             *testing.T
	fx                            *testutil.Fixture
	writer, leader, peer          string
	writerTok, leaderTok, peerTok string
	claudeRT, codexRT             string
	issueID, targetSquad          string
}

func newVoteRig(t *testing.T) *voteRig {
	t.Helper()
	t.Setenv("MULTICA_VOTE_GATE_LABS", rigLabs)
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	r := &voteRig{t: t, fx: fx}
	r.claudeRT = fx.Runtime(t, "rig-claude-"+t.Name(), testutil.Cols{"provider": "claude"})
	r.codexRT = fx.Runtime(t, "rig-codex-"+t.Name(), testutil.Cols{"provider": "codex"})
	mk := func(role, rt, model string) (string, string) {
		id := fx.Agent(t, "rig-"+role+"-"+t.Name(), rt, testutil.Cols{
			"visibility": "workspace", "permission_mode": "public_to", "instructions": "",
			"custom_env": testutil.Raw("'{}'::jsonb"), "custom_args": testutil.Raw("'[]'::jsonb"), "model": model,
		})
		fx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2) ON CONFLICT DO NOTHING`, id, testWorkspaceID)
		task := fx.Task(t, id, testutil.Cols{
			"runtime_id": rt, "status": "running", "originator_user_id": testUserID,
			"accountable_user_id": testUserID, "originator_source": "direct_human",
		})
		return id, mintAgentTaskToken(t, id, task, testUserID)
	}
	r.writer, r.writerTok = mk("writer", r.claudeRT, "claude-test")
	r.leader, r.leaderTok = mk("leader", r.codexRT, "gpt-test")
	r.peer, r.peerTok = mk("peer", r.claudeRT, "claude-test")
	squad := fx.Squad(t, "rig-vote-"+t.Name(), r.leader)
	r.issueID = fx.Issue(t, "rig vote "+t.Name(), testutil.Cols{"assignee_type": "squad", "assignee_id": squad})
	r.targetSquad = fx.Squad(t, "rig-target-"+t.Name(), r.leader, testutil.Cols{"instructions": "before\n" + rigHuman})
	return r
}

func (r *voteRig) do(method, path, token string, body any) (int, string) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, testServer.URL+path, bytes.NewReader(b))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (r *voteRig) must(want int, method, path, token string, body any) string {
	r.t.Helper()
	code, out := r.do(method, path, token, body)
	if code != want {
		r.t.Fatalf("%s %s: expected %d, got %d: %s", method, path, want, code, out)
	}
	return out
}

// post creates a comment on the vote issue as the token's agent and returns its id.
func (r *voteRig) post(token, content string) string {
	r.t.Helper()
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.must(http.StatusCreated, "POST", "/api/issues/"+r.issueID+"/comments", token, map[string]any{"content": content})), &resp); err != nil || resp.ID == "" {
		r.t.Fatalf("comment create response: %v", err)
	}
	return resp.ID
}

// honestVotes posts the proposal and both PASS votes through the real route.
func (r *voteRig) honestVotes(label, after string) {
	r.t.Helper()
	r.post(r.writerTok, "Proposal: "+label+" "+after)
	r.post(r.leaderTok, "Vote: PASS "+label+" "+after)
	r.post(r.peerTok, "Vote: PASS "+label+" "+after)
}

func (r *voteRig) squadValue() string {
	var v string
	r.fx.QueryRow(r.t, `SELECT instructions FROM squad WHERE id = $1`, r.targetSquad).Scan(&v)
	return v
}

// writeSquad sends the receipt as the writer's token.
func (r *voteRig) writeSquad(before, next string) (int, string) {
	return r.do("PUT", "/api/squads/"+r.targetSquad, r.writerTok, map[string]any{
		"instructions": next, "vote_issue": r.issueID, "before_sha256": rigDigest(before), "after_sha256": rigDigest(next),
	})
}

func (r *voteRig) wantDenied(code int, out string) {
	r.t.Helper()
	if code != http.StatusForbidden {
		r.t.Fatalf("expected 403, got %d: %s", code, out)
	}
	if got := r.squadValue(); got != "before\n"+rigHuman {
		r.t.Fatalf("squad instructions must stay unchanged, got %q", got)
	}
}

const (
	rigBefore = "before\n" + rigHuman
	rigNext   = "after\n" + rigHuman
)

func TestVoteGateRouter_HonestVotesWrite(t *testing.T) {
	r := newVoteRig(t)
	r.honestVotes("instructions:"+r.targetSquad, rigDigest(rigNext))
	if code, out := r.writeSquad(rigBefore, rigNext); code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, out)
	}
	if got := r.squadValue(); got != rigNext {
		t.Fatalf("not written: %q", got)
	}
}

// F2: an owner/admin-backed writer edits other agents' comments into votes.
func TestVoteGateRouter_EditedCommentsAreNotVotes(t *testing.T) {
	r := newVoteRig(t)
	label, after := "instructions:"+r.targetSquad, rigDigest(rigNext)
	proposal := r.post(r.writerTok, "setup")
	leaderVote := r.post(r.leaderTok, "ack")
	peerVote := r.post(r.peerTok, "ack")
	r.must(http.StatusOK, "PUT", "/api/comments/"+proposal, r.writerTok, map[string]any{"content": "Proposal: " + label + " " + after})
	r.must(http.StatusOK, "PUT", "/api/comments/"+leaderVote, r.writerTok, map[string]any{"content": "Vote: PASS " + label + " " + after})
	r.must(http.StatusOK, "PUT", "/api/comments/"+peerVote, r.writerTok, map[string]any{"content": "Vote: PASS " + label + " " + after})
	r.wantDenied(r.writeSquad(rigBefore, rigNext))
}

// F2: the same authority removes or rewrites an existing BLOCK.
func TestVoteGateRouter_BlockSurvivesDeleteAndEdit(t *testing.T) {
	for _, tc := range []struct{ name, method string }{{"deleted", "DELETE"}, {"rewritten", "PUT"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVoteRig(t)
			label, after := "instructions:"+r.targetSquad, rigDigest(rigNext)
			r.honestVotes(label, after)
			block := r.post(r.peerTok, "Vote: BLOCK "+label+" "+after)
			if tc.method == "DELETE" {
				r.must(http.StatusNoContent, "DELETE", "/api/comments/"+block, r.writerTok, nil)
			} else {
				r.must(http.StatusOK, "PUT", "/api/comments/"+block, r.writerTok, map[string]any{"content": "withdrawn"})
			}
			r.wantDenied(r.writeSquad(rigBefore, rigNext))
		})
	}
}

// F3: the voter is rebound to another runtime family after it voted.
func TestVoteGateRouter_VoterReassignmentAfterVotingDoesNotCount(t *testing.T) {
	r := newVoteRig(t)
	// Same lab at vote time: the leader runs the writer's runtime and model.
	r.fx.Exec(t, `UPDATE agent SET runtime_id = $1, model = 'claude-test' WHERE id = $2`, r.claudeRT, r.leader)
	r.honestVotes("instructions:"+r.targetSquad, rigDigest(rigNext))
	r.must(http.StatusOK, "PUT", "/api/agents/"+r.leader, r.writerTok, map[string]any{"runtime_id": r.codexRT, "model": "gpt-test"})
	r.wantDenied(r.writeSquad(rigBefore, rigNext))
}

// F3: a different CLI family or an unlisted model is not a different lab.
func TestVoteGateRouter_LabIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
	}{
		{"leader model unlisted", `UPDATE agent SET model = 'gpt-unlisted' WHERE id = $1`},
		{"writer model unlisted", `UPDATE agent SET model = 'claude-unlisted' WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVoteRig(t)
			id := r.leader
			if tc.name == "writer model unlisted" {
				id = r.writer
			}
			r.fx.Exec(t, tc.sql, id)
			r.honestVotes("instructions:"+r.targetSquad, rigDigest(rigNext))
			r.wantDenied(r.writeSquad(rigBefore, rigNext))
		})
	}
}

// F1: agent instructions and workspace context stay denied before setup.
func TestVoteGateRouter_AgentAndWorkspaceFieldsNeedSetup(t *testing.T) {
	r := newVoteRig(t)
	target := r.fx.Agent(t, "rig-target-agent-"+t.Name(), r.claudeRT, testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to", "instructions": rigBefore,
		"custom_env": testutil.Raw("'{}'::jsonb"), "custom_args": testutil.Raw("'[]'::jsonb"),
	})
	var wsContext *string
	r.fx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&wsContext)
	r.fx.Exec(t, `UPDATE workspace SET context = $1 WHERE id = $2`, rigBefore, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(t.Context(), `UPDATE workspace SET context = $1 WHERE id = $2`, wsContext, testWorkspaceID)
	})

	type field struct{ name, label, path, key string }
	fields := []field{
		{"agent.instructions", "instructions:" + target, "/api/agents/" + target, "instructions"},
		{"workspace.context", "context:" + testWorkspaceID, "/api/workspaces/" + testWorkspaceID, "context"},
	}
	read := func(f field) string {
		var v string
		if f.key == "context" {
			r.fx.QueryRow(t, `SELECT COALESCE(context, '') FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&v)
		} else {
			r.fx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, target).Scan(&v)
		}
		return v
	}
	send := func(f field) (int, string) {
		method := "PUT"
		if f.key == "context" {
			method = "PATCH"
		}
		return r.do(method, f.path, r.writerTok, map[string]any{
			f.key: rigNext, "vote_issue": r.issueID, "before_sha256": rigDigest(rigBefore), "after_sha256": rigDigest(rigNext),
		})
	}
	for _, f := range fields {
		r.honestVotes(f.label, rigDigest(rigNext))
	}

	t.Run("before setup", func(t *testing.T) {
		for _, f := range fields {
			if code, out := send(f); code != http.StatusForbidden || read(f) != rigBefore {
				t.Fatalf("%s: expected 403 and no write, got %d: %s", f.name, code, out)
			}
		}
	})
	t.Run("setup done but no marker in the live value", func(t *testing.T) {
		t.Setenv("MULTICA_VOTE_GATE_FIELDS", rigFields)
		r.fx.Exec(t, `UPDATE agent SET instructions = 'before' WHERE id = $1`, target)
		r.fx.Exec(t, `UPDATE workspace SET context = 'before' WHERE id = $1`, testWorkspaceID)
		for _, f := range fields {
			r.honestVotes(f.label, rigDigest("after"))
		}
		for _, f := range fields {
			code, out := r.do(map[bool]string{true: "PATCH", false: "PUT"}[f.key == "context"], f.path, r.writerTok, map[string]any{
				f.key: "after", "vote_issue": r.issueID, "before_sha256": rigDigest("before"), "after_sha256": rigDigest("after"),
			})
			if code != http.StatusForbidden || read(f) != "before" {
				t.Fatalf("%s: expected 403 and no write, got %d: %s", f.name, code, out)
			}
		}
	})
	t.Run("after setup with a marker", func(t *testing.T) {
		t.Setenv("MULTICA_VOTE_GATE_FIELDS", rigFields)
		r.fx.Exec(t, `UPDATE agent SET instructions = $1 WHERE id = $2`, rigBefore, target)
		r.fx.Exec(t, `UPDATE workspace SET context = $1 WHERE id = $2`, rigBefore, testWorkspaceID)
		for _, f := range fields {
			if code, out := send(f); code != http.StatusOK || read(f) != rigNext {
				t.Fatalf("%s: expected 200 and a write, got %d: %s", f.name, code, out)
			}
		}
	})
}
