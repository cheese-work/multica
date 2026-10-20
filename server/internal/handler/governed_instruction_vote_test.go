package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1862: an agent may write a governed field when the request carries a
// vote receipt for the exact change. Body: the field, vote_issue,
// before_sha256, after_sha256. Every test runs against all four governed
// fields through the real update handlers.

const voteHuman = "<!-- governed:human -->\nkeep this rule\n<!-- /governed:human -->"

func voteTargets() []ownerCASTarget {
	targets := ownerCASTargets()
	return append(targets, ownerCASTarget{
		name:  "project.description",
		field: "description",
		seed: func(t *testing.T, initial string) string {
			id := dbfx.Project(t, "CHE-1862 "+safeTestName(t), testutil.Cols{"description": initial})
			return id
		},
		req: func(userID, id string, body map[string]any) *http.Request {
			return withURLParam(newRequestAs(userID, http.MethodPut, "/api/projects/"+id, body), "id", id)
		},
		handle: func(w http.ResponseWriter, r *http.Request) { testHandler.UpdateProject(w, r) },
		read: func(t *testing.T, id string) string {
			var v *string
			dbfx.QueryRow(t, `SELECT description FROM project WHERE id = $1`, id).Scan(&v)
			if v == nil {
				return ""
			}
			return *v
		},
	})
}

// voteFixture: a writer agent (claude), a squad leader (codex) and a peer
// (claude), plus an issue owned by the leader's squad.
type voteFixture struct {
	writerID, taskID, leaderID, peerID, issueID string
}

func newVoteFixture(t *testing.T) voteFixture {
	t.Helper()
	name := safeTestName(t)
	claude := dbfx.Runtime(t, "che1862-claude-"+name, testutil.Cols{"provider": "claude"})
	codex := dbfx.Runtime(t, "che1862-codex-"+name, testutil.Cols{"provider": "codex"})
	agentOn := func(role, runtimeID string) string {
		id := createHandlerTestAgent(t, "che1862-"+role+"-"+name, nil)
		dbfx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, id)
		return id
	}
	f := voteFixture{
		writerID: agentOn("writer", claude),
		leaderID: agentOn("leader", codex),
		peerID:   agentOn("peer", claude),
	}
	f.taskID = createHandlerTestTaskForAgent(t, f.writerID)
	squadID := dbfx.Squad(t, "che1862-"+name, f.leaderID)
	f.issueID = dbfx.Issue(t, "CHE-1862 vote "+name, testutil.Cols{"assignee_type": "squad", "assignee_id": squadID})
	return f
}

// say posts a comment on the vote issue; age is how many seconds old it is.
func (f voteFixture) say(t *testing.T, authorType, authorID string, age int, content string) {
	t.Helper()
	dbfx.Comment(t, f.issueID, content, testutil.Cols{
		"author_type": authorType,
		"author_id":   authorID,
		"created_at":  testutil.Raw(fmt.Sprintf("now() - interval '%d seconds'", age)),
	})
}

func voteLine(verdict, label, after string) string {
	return fmt.Sprintf("Vote: %s %s %s", verdict, label, after)
}

// approve posts the proposal by the writer and PASS votes by leader and peer.
func (f voteFixture) approve(t *testing.T, label, after string) {
	t.Helper()
	f.say(t, "agent", f.writerID, 60, fmt.Sprintf("Proposal: %s %s", label, after))
	f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, after))
	f.say(t, "agent", f.peerID, 20, voteLine("PASS", label, after))
}

func (f voteFixture) write(tg ownerCASTarget, id, content, before string, mutate ...func(map[string]any)) *httptest.ResponseRecorder {
	body := map[string]any{
		tg.field:        content,
		"vote_issue":    f.issueID,
		"before_sha256": before,
		"after_sha256":  testDigest(content),
	}
	for _, m := range mutate {
		m(body)
	}
	w := httptest.NewRecorder()
	tg.handle(w, asAgentActor(tg.req(testUserID, id, body), f.writerID, f.taskID))
	return w
}

// eachVoteCase seeds a target holding initial, approves next, and runs fn.
func eachVoteCase(t *testing.T, initial, next string, fn func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string)) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tg := range voteTargets() {
		t.Run(tg.name, func(t *testing.T) {
			f := newVoteFixture(t)
			id := tg.seed(t, initial)
			label := tg.field + ":" + id
			fn(t, tg, f, id, label)
		})
	}
}

func wantDenied(t *testing.T, tg ownerCASTarget, id, initial string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if got := tg.read(t, id); got != initial {
		t.Fatalf("value must stay %q, got %q", initial, got)
	}
}

func TestVoteGate_ValidReceiptWrites(t *testing.T) {
	initial, next := "before\n"+voteHuman+"\n", "after — café\n"+voteHuman+"\n"
	eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		f.approve(t, label, testDigest(next))
		w := f.write(tg, id, next, testDigest(initial))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != next {
			t.Fatalf("value not written: %q", got)
		}
		resp := decodeCASResponse(t, w)
		if resp["before_digest"] != testDigest(initial) || resp["after_digest"] != testDigest(next) {
			t.Fatalf("receipt digests missing from response: %v", resp)
		}
	})
}

func TestVoteGate_FailedChecksStay403(t *testing.T) {
	initial, next := "before\n", "after\n"
	cases := []struct {
		name   string
		setup  func(t *testing.T, f voteFixture, label string)
		mut    func(map[string]any)
		before string
	}{
		{name: "stale before digest", before: testDigest("not the live value"),
			setup: func(t *testing.T, f voteFixture, label string) { f.approve(t, label, testDigest(next)) }},
		{name: "after digest does not match content",
			setup: func(t *testing.T, f voteFixture, label string) { f.approve(t, label, testDigest(next)) },
			mut:   func(b map[string]any) { b["after_sha256"] = testDigest("something else") }},
		{name: "no votes", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
		}},
		{name: "one vote only", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
			f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "same voter twice", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
			f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", f.leaderID, 20, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "writer votes for itself", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
			f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", f.writerID, 20, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "member votes do not count", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
			f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, testDigest(next)))
			f.say(t, "member", testUserID, 20, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "no cross-lab voter", setup: func(t *testing.T, f voteFixture, label string) {
			var claude string
			dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, f.writerID).Scan(&claude)
			dbfx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, claude, f.leaderID)
			f.approve(t, label, testDigest(next))
		}},
		{name: "no squad leader voter", setup: func(t *testing.T, f voteFixture, label string) {
			other := createHandlerTestAgent(t, "che1862-other-"+safeTestName(t), nil)
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
			f.say(t, "agent", f.peerID, 30, voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", other, 20, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "votes name a different digest", setup: func(t *testing.T, f voteFixture, label string) {
			f.approve(t, label, testDigest("another change"))
		}},
		{name: "votes name a different target", setup: func(t *testing.T, f voteFixture, label string) {
			f.approve(t, "instructions:00000000-0000-0000-0000-000000000000", testDigest(next))
		}},
		{name: "votes predate the proposal", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.leaderID, 90, voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", f.peerID, 80, voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
		}},
		{name: "no proposal", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", f.peerID, 20, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "agent BLOCK", setup: func(t *testing.T, f voteFixture, label string) {
			f.approve(t, label, testDigest(next))
			f.say(t, "agent", f.leaderID, 10, voteLine("BLOCK", label, testDigest(next)))
		}},
		{name: "member BLOCK", setup: func(t *testing.T, f voteFixture, label string) {
			f.approve(t, label, testDigest(next))
			f.say(t, "member", testUserID, 10, voteLine("BLOCK", label, testDigest(next)))
		}},
		{name: "two vote lines in one comment", setup: func(t *testing.T, f voteFixture, label string) {
			f.say(t, "agent", f.writerID, 60, "Proposal: "+label+" "+testDigest(next))
			f.say(t, "agent", f.leaderID, 30, voteLine("PASS", label, testDigest(next))+"\n"+voteLine("PASS", label, testDigest(next)))
			f.say(t, "agent", f.peerID, 20, voteLine("PASS", label, testDigest(next)))
		}},
		{name: "unknown vote issue", setup: func(t *testing.T, f voteFixture, label string) { f.approve(t, label, testDigest(next)) },
			mut: func(b map[string]any) { b["vote_issue"] = "00000000-0000-0000-0000-000000000000" }},
		{name: "extra body field", setup: func(t *testing.T, f voteFixture, label string) { f.approve(t, label, testDigest(next)) },
			mut: func(b map[string]any) { b["name"] = "renamed" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
				tc.setup(t, f, label)
				before := tc.before
				if before == "" {
					before = testDigest(initial)
				}
				var mut []func(map[string]any)
				if tc.mut != nil {
					mut = append(mut, tc.mut)
				}
				w := f.write(tg, id, next, before, mut...)
				if tc.name == "extra body field" {
					// a request that smuggles other fields is refused outright
					if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
						t.Fatalf("expected 400/403, got %d", w.Code)
					}
					if got := tg.read(t, id); got != initial {
						t.Fatalf("value must stay %q, got %q", initial, got)
					}
					return
				}
				wantDenied(t, tg, id, initial, w)
			})
		})
	}
}

func TestVoteGate_ProtectedBlocks(t *testing.T) {
	other := "<!-- governed:human -->\nrewritten rule\n<!-- /governed:human -->"
	cases := []struct {
		name          string
		initial, next string
		want          int
	}{
		{"block unchanged", "a\n" + voteHuman + "\nz", "b\n" + voteHuman + "\ny", http.StatusOK},
		{"block edited", "a\n" + voteHuman + "\nz", "a\n" + other + "\nz", http.StatusForbidden},
		{"block removed", "a\n" + voteHuman + "\nz", "a\nz", http.StatusForbidden},
		{"block added", "a\nz", "a\n" + voteHuman + "\nz", http.StatusForbidden},
		{"block reordered", voteHuman + "\n" + other, other + "\n" + voteHuman, http.StatusForbidden},
		{"unbalanced marker added", "a\n" + voteHuman, "a\n" + voteHuman + "\n<!-- governed:human -->\nopen", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			eachVoteCase(t, tc.initial, tc.next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
				f.approve(t, label, testDigest(tc.next))
				w := f.write(tg, id, tc.next, testDigest(tc.initial))
				if w.Code != tc.want {
					t.Fatalf("expected %d, got %d: %s", tc.want, w.Code, w.Body.String())
				}
				want := tc.initial
				if tc.want == http.StatusOK {
					want = tc.next
				}
				if got := tg.read(t, id); got != want {
					t.Fatalf("value %q, want %q", got, want)
				}
			})
		})
	}
}

// A receipt does not help a cloud-node PAT: only an agent identity can write.
func TestVoteGate_CloudNodeStillDenied(t *testing.T) {
	initial, next := "before\n", "after\n"
	eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		f.approve(t, label, testDigest(next))
		w := httptest.NewRecorder()
		tg.handle(w, asCloudNodeActor(tg.req(testUserID, id, map[string]any{
			tg.field: next, "vote_issue": f.issueID, "before_sha256": testDigest(initial), "after_sha256": testDigest(next),
		})))
		wantDenied(t, tg, id, initial, w)
	})
}

// Without a receipt the agent is denied exactly as before (CHE-455).
func TestVoteGate_NoReceiptStillDenied(t *testing.T) {
	initial, next := "before\n", "after\n"
	eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		f.approve(t, label, testDigest(next))
		w := httptest.NewRecorder()
		tg.handle(w, asAgentActor(tg.req(testUserID, id, map[string]any{tg.field: next}), f.writerID, f.taskID))
		wantDenied(t, tg, id, initial, w)
	})
}

// Members are unaffected: the same body without an agent identity writes as
// before, and vote_issue is ignored.
func TestVoteGate_MemberWriteUnchanged(t *testing.T) {
	initial, next := "before\n", "after\n"
	eachVoteCase(t, initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		w := httptest.NewRecorder()
		tg.handle(w, tg.req(testUserID, id, map[string]any{tg.field: next}))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != next {
			t.Fatalf("member write lost: %q", got)
		}
	})
}
