package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// CHE-1300: owner-only atomic compare-and-swap for workspace.context,
// squad.instructions and agent.instructions. Every test runs against all
// three fields through the real handlers.

type ownerCASTarget struct {
	name string
	// field is the JSON body key for the governed field.
	field string
	// seed creates the target holding initial and returns its id.
	seed   func(t *testing.T, initial string) string
	req    func(userID, id string, body map[string]any) *http.Request
	handle func(w http.ResponseWriter, r *http.Request)
	read   func(t *testing.T, id string) string
}

func ownerCASTargets() []ownerCASTarget {
	return []ownerCASTarget{
		{
			name:  "workspace.context",
			field: "context",
			seed: func(t *testing.T, initial string) string {
				var previous *string
				dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
				dbfx.Exec(t, `UPDATE workspace SET context = $1 WHERE id = $2`, initial, testWorkspaceID)
				t.Cleanup(func() {
					testPool.Exec(context.Background(), `UPDATE workspace SET context = $1 WHERE id = $2`, previous, testWorkspaceID)
				})
				return testWorkspaceID
			},
			req: func(userID, id string, body map[string]any) *http.Request {
				return withURLParam(newRequestAs(userID, http.MethodPatch, "/api/workspaces/"+id, body), "id", id)
			},
			handle: func(w http.ResponseWriter, r *http.Request) { testHandler.UpdateWorkspace(w, r) },
			read: func(t *testing.T, id string) string {
				var v *string
				dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, id).Scan(&v)
				if v == nil {
					return ""
				}
				return *v
			},
		},
		{
			name:  "squad.instructions",
			field: "instructions",
			seed: func(t *testing.T, initial string) string {
				leaderID := createHandlerTestAgent(t, "che1300-squad-leader-"+safeTestName(t), nil)
				squad := createSquadAs(t, testUserID, "CHE-1300 "+safeTestName(t), leaderID)
				dbfx.Exec(t, `UPDATE squad SET instructions = $1 WHERE id = $2`, initial, squad.ID)
				return squad.ID
			},
			req: func(userID, id string, body map[string]any) *http.Request {
				return squadReqWithParams(userID, http.MethodPatch, "/api/squads", body, map[string]string{"id": id})
			},
			handle: func(w http.ResponseWriter, r *http.Request) { testHandler.UpdateSquad(w, r) },
			read: func(t *testing.T, id string) string {
				var v string
				dbfx.QueryRow(t, `SELECT instructions FROM squad WHERE id = $1`, id).Scan(&v)
				return v
			},
		},
		{
			name:  "agent.instructions",
			field: "instructions",
			seed: func(t *testing.T, initial string) string {
				id := createHandlerTestAgent(t, "che1300-agent-"+safeTestName(t), nil)
				dbfx.Exec(t, `UPDATE agent SET instructions = $1 WHERE id = $2`, initial, id)
				return id
			},
			req: func(userID, id string, body map[string]any) *http.Request {
				return withURLParam(newRequestAs(userID, http.MethodPut, "/api/agents/"+id, body), "id", id)
			},
			handle: func(w http.ResponseWriter, r *http.Request) { testHandler.UpdateAgent(w, r) },
			read: func(t *testing.T, id string) string {
				var v string
				dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, id).Scan(&v)
				return v
			},
		},
	}
}

func safeTestName(t *testing.T) string {
	return strings.NewReplacer("/", "-", ".", "-", " ", "-").Replace(t.Name())
}

// eachOwnerCASTarget runs fn once per governed field as a subtest.
func eachOwnerCASTarget(t *testing.T, fn func(t *testing.T, tg ownerCASTarget)) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tg := range ownerCASTargets() {
		t.Run(tg.name, func(t *testing.T) { fn(t, tg) })
	}
}

func (tg ownerCASTarget) do(userID, id, content, digest string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	tg.handle(w, tg.req(userID, id, map[string]any{tg.field: content, "expected_before_digest": digest}))
	return w
}

func decodeCASResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestOwnerCAS_OwnerRoundtripsExactBytes(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		initial := "initial\n"
		// CRLF, trailing blank lines, multi-byte runes and a lone terminal
		// newline all have to survive byte for byte.
		next := "line one\r\nline two — café ☕ 日本語\n\n  indented\n\n"
		id := tg.seed(t, initial)

		w := tg.do(testUserID, id, next, testDigest(initial))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		resp := decodeCASResponse(t, w)
		if resp["before_digest"] != testDigest(initial) {
			t.Errorf("before_digest = %v, want %s", resp["before_digest"], testDigest(initial))
		}
		if resp["after_digest"] != testDigest(next) {
			t.Errorf("after_digest = %v, want %s", resp["after_digest"], testDigest(next))
		}
		if got := tg.read(t, id); got != next {
			t.Errorf("stored = %q, want %q", got, next)
		}
	})
}

func TestOwnerCAS_StaleDigestIs409AndWritesNothing(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live value")
		w := tg.do(testUserID, id, "candidate", testDigest("some older value"))
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != "live value" {
			t.Errorf("stale digest must not write, stored = %q", got)
		}
	})
}

// The Hermes exception hashes the raw UTF-8 bytes. The CHE-1286 card manifest
// strips one terminal LF before hashing. This path uses the raw bytes, so a
// manifest-style digest of a value ending in LF is stale, never accepted.
func TestOwnerCAS_DigestIsOverRawBytesNotLFStripped(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		live := "ends with newline\n"
		id := tg.seed(t, live)

		stripped := testDigest(strings.TrimSuffix(live, "\n"))
		if w := tg.do(testUserID, id, "candidate", stripped); w.Code != http.StatusConflict {
			t.Fatalf("LF-stripped digest: expected 409, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != live {
			t.Fatalf("LF-stripped digest must not write, stored = %q", got)
		}
		if w := tg.do(testUserID, id, "candidate", testDigest(live)); w.Code != http.StatusOK {
			t.Fatalf("raw-byte digest: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestOwnerCAS_UppercaseDigestAccepted(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		w := tg.do(testUserID, id, "next", strings.ToUpper(testDigest("live")))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestOwnerCAS_MalformedDigestIs400AndWritesNothing(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		for name, digest := range map[string]any{
			"empty":    "",
			"null":     nil,
			"short":    "abc123",
			"not hex":  strings.Repeat("z", 64),
			"too long": testDigest("live") + "00",
			"number":   7,
		} {
			w := httptest.NewRecorder()
			tg.handle(w, tg.req(testUserID, id, map[string]any{tg.field: "candidate", "expected_before_digest": digest}))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d: %s", name, w.Code, w.Body.String())
			}
		}
		if got := tg.read(t, id); got != "live" {
			t.Errorf("malformed digest must not write, stored = %q", got)
		}
	})
}

func TestOwnerCAS_DigestWithoutFieldOrWithExtraKeysIs400(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		digest := testDigest("live")
		for name, body := range map[string]map[string]any{
			"digest only":       {"expected_before_digest": digest},
			"null field":        {tg.field: nil, "expected_before_digest": digest},
			"extra description": {tg.field: "candidate", "description": "x", "expected_before_digest": digest},
			"extra name":        {tg.field: "candidate", "name": "x", "expected_before_digest": digest},
		} {
			w := httptest.NewRecorder()
			tg.handle(w, tg.req(testUserID, id, body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d: %s", name, w.Code, w.Body.String())
			}
		}
		if got := tg.read(t, id); got != "live" {
			t.Errorf("rejected request must not write, stored = %q", got)
		}
	})
}

func TestOwnerCAS_NULByteIs400(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		w := tg.do(testUserID, id, "bad\x00byte", testDigest("live"))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != "live" {
			t.Errorf("rejected request must not write, stored = %q", got)
		}
	})
}

func TestOwnerCAS_NonOwnerActorsDenied(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		digest := testDigest("live")
		body := func() map[string]any {
			return map[string]any{tg.field: "PWNED", "expected_before_digest": digest}
		}

		adminID := createPlainMember(t, "che1300-admin-"+safeTestName(t)+"@multica.test")
		dbfx.Exec(t, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, adminID)
		memberID := createPlainMember(t, "che1300-member-"+safeTestName(t)+"@multica.test")
		if tg.name == "agent.instructions" {
			// A plain member who owns the target agent still is not a workspace owner.
			dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, memberID, id)
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `UPDATE agent SET owner_id = $1 WHERE id = $2`, testUserID, id)
			})
		}
		hostAgentID := createHandlerTestAgent(t, "che1300-host-"+safeTestName(t), nil)
		hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

		cases := map[string]*http.Request{
			"workspace admin": tg.req(adminID, id, body()),
			"plain member":    tg.req(memberID, id, body()),
			// The owner's own identity header plus an agent task stamp is an
			// agent actor, not the owner.
			"agent acting for owner": asAgentActor(tg.req(testUserID, id, body()), hostAgentID, hostTaskID),
			"cloud node PAT":         asCloudNodeActor(tg.req(testUserID, id, body())),
			// The digest key alone must not let a machine actor through even
			// without the governed field.
			"agent digest only": asAgentActor(tg.req(testUserID, id, map[string]any{"expected_before_digest": digest}), hostAgentID, hostTaskID),
		}
		for name, req := range cases {
			w := httptest.NewRecorder()
			tg.handle(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: expected 403, got %d: %s", name, w.Code, w.Body.String())
			}
		}
		if got := tg.read(t, id); got != "live" {
			t.Errorf("denied actors must not write, stored = %q", got)
		}
	})
}

// Missing digest keeps today's behavior: this path is entered only when the
// body carries expected_before_digest, so ordinary human writes are untouched.
func TestOwnerCAS_NoDigestKeyKeepsLegacyHumanWrite(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		w := httptest.NewRecorder()
		tg.handle(w, tg.req(testUserID, id, map[string]any{tg.field: "legacy write"}))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != "legacy write" {
			t.Errorf("stored = %q", got)
		}
	})
}

func TestOwnerCAS_ReverseRollbackRestoresBeforeBytes(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		before := "before bytes\r\n  with trailing\n"
		after := "after bytes ☕\n"
		id := tg.seed(t, before)

		apply := tg.do(testUserID, id, after, testDigest(before))
		if apply.Code != http.StatusOK {
			t.Fatalf("apply: expected 200, got %d: %s", apply.Code, apply.Body.String())
		}
		recordedAfter := decodeCASResponse(t, apply)["after_digest"].(string)

		// Rollback: the recorded after digest is the expected current value,
		// the saved before bytes are the replacement.
		if w := tg.do(testUserID, id, before, recordedAfter); w.Code != http.StatusOK {
			t.Fatalf("rollback: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := tg.read(t, id); got != before {
			t.Errorf("after rollback stored = %q, want %q", got, before)
		}
		// A second rollback from the same recorded digest is stale now.
		if w := tg.do(testUserID, id, before, recordedAfter); w.Code != http.StatusConflict {
			t.Errorf("repeat rollback: expected 409, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// The compare lives in the UPDATE statement, so concurrent writers holding the
// same expected digest produce exactly one winner.
func TestOwnerCAS_ConcurrentWritersExactlyOneWins(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		id := tg.seed(t, "live")
		digest := testDigest("live")

		const writers = 8
		codes := make([]int, writers)
		contents := make([]string, writers)
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			contents[i] = "candidate-" + string(rune('a'+i))
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = tg.do(testUserID, id, contents[i], digest).Code
			}(i)
		}
		wg.Wait()

		winner := -1
		for i, code := range codes {
			switch code {
			case http.StatusOK:
				if winner != -1 {
					t.Fatalf("two writers won: %d and %d", winner, i)
				}
				winner = i
			case http.StatusConflict:
			default:
				t.Fatalf("writer %d: unexpected status %d", i, code)
			}
		}
		if winner == -1 {
			t.Fatal("no writer won")
		}
		if got := tg.read(t, id); got != contents[winner] {
			t.Errorf("stored = %q, want winner's %q", got, contents[winner])
		}
	})
}

// Candidate text must never reach the logs: the audit line carries ids and
// digests only, on success and on every refusal.
func TestOwnerCAS_LogsCarryNoCandidateText(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		const secret = "CANDIDATE-SECRET-TEXT"
		id := tg.seed(t, "live")
		tg.do(testUserID, id, secret+" stale", testDigest("not live")) // 409
		tg.do(testUserID, id, secret+" ok", testDigest("live"))        // 200
		tg.do(testUserID, id, secret+" malformed", "nope")             // 400

		if !strings.Contains(buf.String(), "CHE-1300") {
			t.Fatalf("expected the CHE-1300 audit line, got: %s", buf.String())
		}
		if strings.Contains(buf.String(), secret) {
			t.Errorf("logs leak candidate text:\n%s", buf.String())
		}
	})
}
