package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/doopread"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// G4 at the create-decision API (CHE-7064 D4 criteria 1 to 6, S12). The doop
// side is the fakeDoop of the verdict suite; the verdict rows are written the
// way RecordDesignVerdict writes them.

const (
	cardAttestation = "run-card-1"
	cardFrame1      = verdictDoopOrigin + "/c/canvas-1?frame=frame-1"
	cardFrame2      = verdictDoopOrigin + "/c/canvas-1?frame=frame-2"
	// 1700000000000 ms is 14 Nov 2023 22:13:20 UTC, 15 Nov 2023 05:13 ICT.
	cardFrame1Line = "Frame: " + cardFrame1 + " · version 2023-11-15 05:13 ICT"
	cardFrame2Line = "Frame: " + cardFrame2 + " · version 2023-11-15 06:13 ICT"
)

type cardGuardFixture struct {
	singleCallFixture
	doop                                      *fakeDoop
	claudeRuntime, codexRuntime               string
	authorID, sameLabID, crossLabID, authorTk string
}

func newCardGuardFixture(t *testing.T, mode string) *cardGuardFixture {
	t.Helper()
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	f := &cardGuardFixture{singleCallFixture: newAskFixture(t), doop: &fakeDoop{}}
	f.claudeRuntime = dbfx.Runtime(t, "che7091-claude-"+safeTestName(t), testutil.Cols{"provider": "claude"})
	f.codexRuntime = dbfx.Runtime(t, "che7091-codex-"+safeTestName(t), testutil.Cols{"provider": "codex"})
	agent := func(name, runtimeID string) string {
		id := createHandlerTestAgent(t, name+"-"+safeTestName(t), nil)
		dbfx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, id)
		return id
	}
	f.authorID = agent("che7091-author", f.claudeRuntime)
	f.sameLabID = agent("che7091-same", f.claudeRuntime)
	f.crossLabID = agent("che7091-cross", f.codexRuntime)
	f.authorTk = f.task(t, f.authorID, f.claudeRuntime, "claude-test")

	var prev []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prev)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(prev))
	})
	if mode != "" {
		raw, _ := json.Marshal(map[string]any{"design_card_guard": map[string]any{"mode": mode, "doop_origin": verdictDoopOrigin, "design_url_property_id": verdictDesignPropertyID}})
		dbfx.Exec(t, `UPDATE workspace SET settings = settings || $2::jsonb WHERE id = $1`, testWorkspaceID, string(raw))
	}
	f.setDesignURL(t, verdictDoopOrigin+"/c/canvas-1?page=page-1&frame=frame-1")

	srv := httptest.NewServer(f.doop)
	t.Cleanup(srv.Close)
	previous := testHandler.DoopReader
	testHandler.DoopReader = doopread.New(doopread.Config{BaseURL: srv.URL, Token: verdictToken, AllowLoopbackHTTP: true})
	t.Cleanup(func() { testHandler.DoopReader = previous })
	f.serve(nil)
	return f
}

func (f *cardGuardFixture) setDesignURL(t *testing.T, url string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET properties = CASE WHEN $2 = '' THEN '{}'::jsonb ELSE jsonb_build_object($3::text, $2::text) END WHERE id = $1`, f.issueID, url, verdictDesignPropertyID)
}

func (f *cardGuardFixture) task(t *testing.T, agentID, runtimeID, model string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": f.issueID, "status": "completed", "claimed_model": model,
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
}

// serve sets the attestation view: complete and current, bound to the issue,
// written by the author task; over changes keys.
func (f *cardGuardFixture) serve(over map[string]any) {
	view := map[string]any{
		"id": cardAttestation, "canvasId": "canvas-1", "pageId": "page-1", "pageIssueId": f.issueID,
		"complete": true, "current": true, "ruleset": "ux-v1",
		"frames":             []map[string]any{{"frameId": "frame-1", "updatedAt": 1700000000000}, {"frameId": "frame-2", "updatedAt": 1700003600000}},
		"authors":            []map[string]any{{"kind": "agent", "agentId": f.authorID, "taskId": f.authorTk, "sources": []string{"write"}}},
		"provenanceComplete": true,
	}
	for k, v := range over {
		view[k] = v
	}
	f.doop.mu.Lock()
	f.doop.view, f.doop.status = view, 0
	f.doop.mu.Unlock()
}

// verdict records a design_review_verdict row of agentID, minutesAgo old.
func (f *cardGuardFixture) verdict(t *testing.T, agentID, taskID, verdict string, minutesAgo int) {
	t.Helper()
	details, _ := json.Marshal(map[string]any{"attestation_id": cardAttestation, "verdict": verdict, "task_id": taskID})
	dbfx.Insert(t, "activity_log", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": f.issueID, "actor_type": "agent", "actor_id": agentID,
		"action": "design_review_verdict", "details": string(details),
		"created_at": testutil.Raw(fmt.Sprintf("now() - interval '%d minutes'", minutesAgo)),
	})
}

// passByCrossLab is the accepted case: a reviewer of another lab, after the author's task ended.
func (f *cardGuardFixture) passByCrossLab(t *testing.T) {
	t.Helper()
	f.verdict(t, f.crossLabID, f.task(t, f.crossLabID, f.codexRuntime, "gpt-test"), "PASS", 1)
}

// designAsk is the fixture ask with every option citing a frame of the attestation.
func (f *cardGuardFixture) designAsk() decisiontx.Ask {
	ask := f.details
	ask.Options = append([]decisiontx.AskOption(nil), ask.Options...)
	for i := range ask.Options {
		frame := cardFrame1
		if i == 1 {
			frame = cardFrame2
		}
		ask.Options[i].Design = &decisiontx.OptionDesign{FrameURL: frame, AttestationID: cardAttestation}
	}
	return ask
}

var cardEntries = []string{"POST /api/decisions", "POST /api/decisions/ask"}

// send posts the ask through one of the two entry points.
func (f *cardGuardFixture) send(t *testing.T, entry string, ask decisiontx.Ask, class string) *testutil.Response {
	t.Helper()
	if entry == cardEntries[1] {
		body := f.askBody()
		body["ask"], body["escalation_class"] = ask, class
		return testutil.Call(t, testHandler.AskDecision, f.askCall(body))
	}
	body := f.body()
	body["ask"], body["escalation_class"] = ask, class
	return testutil.Call(t, testHandler.RequestDecision, f.call(body))
}

func (f *cardGuardFixture) cards(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, f.issueID)
}

func (f *cardGuardFixture) wantRefusal(t *testing.T, resp *testutil.Response, status int, code string) {
	t.Helper()
	resp.Want(status)
	if got, _ := resp.Map()["code"].(string); got != code {
		t.Fatalf("code = %q (body %s), want %q", got, resp.Text(), code)
	}
	if n := f.cards(t); n != 0 {
		t.Fatalf("%d cards after a refusal, want none", n)
	}
}

// storedDesigns is the frame_updated_at of each option of the stored ask, by label.
func (f *cardGuardFixture) storedDesigns(t *testing.T) map[string]int64 {
	t.Helper()
	var raw []byte
	dbfx.QueryRow(t, `SELECT ask_json FROM decision_request WHERE issue_id = $1`, f.issueID).Scan(&raw)
	var stored struct {
		Options []struct {
			Label  string
			Design *struct {
				FrameUpdatedAt int64 `json:"frame_updated_at"`
			}
		}
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, o := range stored.Options {
		if o.Design != nil {
			out[o.Label] = o.Design.FrameUpdatedAt
		}
	}
	return out
}

func (f *cardGuardFixture) askComment(t *testing.T) string {
	t.Helper()
	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask'`, f.issueID).Scan(&content)
	return content
}

// D4 criteria 3 and 5: an independent PASS after the designer's task ended is
// accepted; each option stores its frame version and the card shows one Frame
// line per option.
func TestDesignCardGuard_BlockAcceptsAnIndependentPassAndStampsTheVersion(t *testing.T) {
	for _, entry := range cardEntries {
		t.Run(entry, func(t *testing.T) {
			f := newCardGuardFixture(t, "block")
			f.passByCrossLab(t)
			f.send(t, entry, f.designAsk(), "reversible").Want(http.StatusCreated)
			stored := f.storedDesigns(t)
			if len(stored) != 2 || stored["Proceed"] != 1700000000000 || stored["Hold"] != 1700003600000 {
				t.Fatalf("stored frame_updated_at = %v, want Proceed 1700000000000 and Hold 1700003600000", stored)
			}
			comment := f.askComment(t)
			for _, line := range []string{cardFrame1Line, cardFrame2Line} {
				if strings.Count(comment, line) != 1 {
					t.Errorf("ask comment holds %q %d times, want once:\n%s", line, strings.Count(comment, line), comment)
				}
			}
		})
	}
}

// D4 criteria 1 and 2: each refusal code of D1.3 and D1.5, through both entry
// points, with no card row. The status classes are D1.7: 400 card defects, 409
// binding, stale, verdict and independence failures, 503 doop or config.
func TestDesignCardGuard_BlockRefusesEachCodeWithNoCard(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		setup  func(t *testing.T, f *cardGuardFixture)
		ask    func(f *cardGuardFixture) decisiontx.Ask
		class  string
	}{
		{"option without design", 400, "design_option_missing", func(t *testing.T, f *cardGuardFixture) {},
			func(f *cardGuardFixture) decisiontx.Ask { return f.details }, "product_direction"},
		{"frame url is not a doop link", 400, "design_frame_url_invalid", func(t *testing.T, f *cardGuardFixture) {},
			func(f *cardGuardFixture) decisiontx.Ask {
				a := f.designAsk()
				a.Options[0].Design.FrameURL = "not a link"
				return a
			}, "reversible"},
		{"issue has no Design url", 400, "design_url_missing", func(t *testing.T, f *cardGuardFixture) { f.setDesignURL(t, "") }, nil, "reversible"},
		{"unknown attestation", 400, "design_attestation_not_found", func(t *testing.T, f *cardGuardFixture) { f.doop.status = http.StatusNotFound }, nil, "reversible"},
		{"canvas differs", 409, "design_canvas_mismatch", func(t *testing.T, f *cardGuardFixture) {},
			func(f *cardGuardFixture) decisiontx.Ask {
				a := f.designAsk()
				a.Options[0].Design.FrameURL = verdictDoopOrigin + "/c/canvas-2?frame=frame-1"
				return a
			}, "reversible"},
		{"frame is not attested", 409, "design_frame_not_attested", func(t *testing.T, f *cardGuardFixture) {},
			func(f *cardGuardFixture) decisiontx.Ask {
				a := f.designAsk()
				a.Options[0].Design.FrameURL = verdictDoopOrigin + "/c/canvas-1?frame=frame-9"
				return a
			}, "reversible"},
		{"page not bound to the issue", 409, "design_page_not_bound", func(t *testing.T, f *cardGuardFixture) {
			f.serve(map[string]any{"pageIssueId": "00000000-0000-4000-8000-000000000000"})
		}, nil, "reversible"},
		{"attestation incomplete", 409, "design_attestation_incomplete", func(t *testing.T, f *cardGuardFixture) { f.serve(map[string]any{"complete": false}) }, nil, "reversible"},
		{"edit after PASS", 409, "design_attestation_stale", func(t *testing.T, f *cardGuardFixture) { f.serve(map[string]any{"current": false}) }, nil, "reversible"},
		{"no verdict", 409, "design_verdict_missing", func(t *testing.T, f *cardGuardFixture) {}, nil, "reversible"},
		{"newest verdict is CHANGES", 409, "design_verdict_missing", func(t *testing.T, f *cardGuardFixture) {
			task := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test")
			f.verdict(t, f.crossLabID, task, "PASS", 5)
			f.verdict(t, f.crossLabID, task, "CHANGES", 1)
		}, nil, "reversible"},
		{"author without trusted records", 409, "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(map[string]any{"provenanceComplete": false})
			f.passByCrossLab(t)
		}, nil, "reversible"},
		{"reviewer is an author", 409, "design_verdict_self_review", func(t *testing.T, f *cardGuardFixture) {
			f.verdict(t, f.authorID, f.task(t, f.authorID, f.claudeRuntime, "claude-test"), "PASS", 1)
		}, nil, "reversible"},
		{"reviewer of the author's lab", 409, "design_verdict_same_lab", func(t *testing.T, f *cardGuardFixture) {
			f.verdict(t, f.sameLabID, f.task(t, f.sameLabID, f.claudeRuntime, "claude-test"), "PASS", 1)
		}, nil, "reversible"},
		{"reviewer is the asking agent", 409, "design_verdict_same_agent", func(t *testing.T, f *cardGuardFixture) {
			f.verdict(t, f.agentID, f.task(t, f.agentID, f.codexRuntime, "gpt-test"), "PASS", 1)
		}, nil, "reversible"},
		{"doop answers 500", 503, "design_doop_unavailable", func(t *testing.T, f *cardGuardFixture) {
			f.passByCrossLab(t)
			f.doop.status = http.StatusInternalServerError
		}, nil, "reversible"},
	}
	for _, tc := range cases {
		for _, entry := range cardEntries {
			t.Run(tc.name+" "+entry, func(t *testing.T) {
				f := newCardGuardFixture(t, "block")
				tc.setup(t, f)
				ask := f.designAsk()
				if tc.ask != nil {
					ask = tc.ask(f)
				}
				f.wantRefusal(t, f.send(t, entry, ask, tc.class), tc.status, tc.code)
			})
		}
	}
}

// D4 criterion 6: off changes nothing and reads nothing; warn creates the card
// and returns the hint.
func TestDesignCardGuard_OffChangesNothing(t *testing.T) {
	for _, entry := range cardEntries {
		t.Run(entry, func(t *testing.T) {
			f := newCardGuardFixture(t, "")
			f.send(t, entry, f.designAsk(), "reversible").Want(http.StatusCreated)
			if f.doop.reads != 0 {
				t.Errorf("doop reads = %d, want 0", f.doop.reads)
			}
			if stored := f.storedDesigns(t); stored["Proceed"] != 0 || stored["Hold"] != 0 {
				t.Errorf("off stamped a version: %v", stored)
			}
			if strings.Contains(f.askComment(t), "Frame: ") {
				t.Error("off added a Frame line")
			}
		})
	}
}

func TestDesignCardGuard_WarnCreatesTheCardWithAHint(t *testing.T) {
	for _, entry := range cardEntries {
		t.Run(entry+" refusal becomes a hint", func(t *testing.T) {
			f := newCardGuardFixture(t, "warn")
			var created struct {
				Hints []string `json:"hints"`
			}
			f.send(t, entry, f.designAsk(), "reversible").Want(http.StatusCreated).JSON(&created) // no verdict yet
			if hints := strings.Join(created.Hints, "\n"); !strings.Contains(hints, "design_card_guard design_verdict_missing:") {
				t.Errorf("hints = %q, want design_card_guard design_verdict_missing", created.Hints)
			}
			if stored := f.storedDesigns(t); stored["Proceed"] != 0 {
				t.Errorf("a refused card was stamped: %v", stored)
			}
		})
		t.Run(entry+" doop down becomes a hint", func(t *testing.T) {
			f := newCardGuardFixture(t, "warn")
			f.doop.status = http.StatusInternalServerError
			var created struct {
				Hints []string `json:"hints"`
			}
			f.send(t, entry, f.designAsk(), "reversible").Want(http.StatusCreated).JSON(&created)
			if hints := strings.Join(created.Hints, "\n"); !strings.Contains(hints, "design_card_guard design_doop_unavailable:") {
				t.Errorf("hints = %q, want design_doop_unavailable", created.Hints)
			}
		})
		t.Run(entry+" a pass is stamped and shows the Frame lines", func(t *testing.T) {
			f := newCardGuardFixture(t, "warn")
			f.passByCrossLab(t)
			var created struct {
				Hints []string `json:"hints"`
			}
			f.send(t, entry, f.designAsk(), "reversible").Want(http.StatusCreated).JSON(&created)
			if strings.Contains(strings.Join(created.Hints, "\n"), "design_card_guard") {
				t.Errorf("a passing card got a guard hint: %q", created.Hints)
			}
			if stored := f.storedDesigns(t); stored["Proceed"] != 1700000000000 {
				t.Errorf("stored = %v, want a stamped version", stored)
			}
			if !strings.Contains(f.askComment(t), cardFrame1Line) {
				t.Error("ask comment lacks the Frame line")
			}
		})
	}
}

// The Frame lines grow the ask, so the length is checked again after the
// stamp: an ask that fits without them is refused with them.
func TestDesignCardGuard_ChecksTheLengthAgainAfterTheFrameLines(t *testing.T) {
	f := newCardGuardFixture(t, "block")
	f.passByCrossLab(t)
	ask := f.designAsk()
	render := func(a decisiontx.Ask) int {
		stored, err := a.RequireRecommendedFirst()
		if err != nil {
			t.Fatal(err)
		}
		return len(decisionAskContent(decisionRequestInput{Ask: stored.Content(handlerTestWorkspaceSlug), Footer: f.footer}))
	}
	pad := maxCommentContentBytes - render(ask) - 20 // room for 20 bytes, less than one Frame line
	ask.ContextELI10 += " " + strings.Repeat("a", pad)
	if got := render(ask); got > maxCommentContentBytes {
		t.Fatalf("setup: ask is %d bytes, over the limit before the Frame lines", got)
	}
	resp := f.send(t, cardEntries[0], ask, "reversible").Want(http.StatusBadRequest)
	if !strings.Contains(resp.Text(), "ask is too long") {
		t.Fatalf("body = %s, want ask is too long", resp.Text())
	}
	if n := f.cards(t); n != 0 {
		t.Fatalf("%d cards, want none", n)
	}
}

// D4 criterion 2, review r1 and r2 provenance cases, through both card
// endpoints: the ledger rows decide, never timing, and a row Multica cannot
// vouch for never becomes an author. Each case refuses with no card.
func TestDesignCardGuard_ProvenanceCasesAreRefused(t *testing.T) {
	type view = map[string]any
	agentRow := func(agentID, taskID string) map[string]any {
		return map[string]any{"kind": "agent", "agentId": agentID, "taskId": taskID, "sources": []string{"write"}}
	}
	cases := []struct {
		name  string
		code  string
		setup func(t *testing.T, f *cardGuardFixture)
	}{
		// 1. Another lab made only the final metadata edits; both are in the ledger.
		{"final metadata edits by another lab, the first author reviews", "design_verdict_self_review", func(t *testing.T, f *cardGuardFixture) {
			editor := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test")
			f.serve(view{"authors": []map[string]any{agentRow(f.authorID, f.authorTk), agentRow(f.crossLabID, editor)}})
			f.verdict(t, f.authorID, f.task(t, f.authorID, f.claudeRuntime, "claude-test"), "PASS", 1)
		}},
		{"final metadata edits by another lab, the editor reviews", "design_verdict_self_review", func(t *testing.T, f *cardGuardFixture) {
			editor := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test")
			f.serve(view{"authors": []map[string]any{agentRow(f.authorID, f.authorTk), agentRow(f.crossLabID, editor)}})
			f.verdict(t, f.crossLabID, editor, "PASS", 1)
		}},
		{"final metadata edits by another lab, a third agent of the author lab reviews", "design_verdict_same_lab", func(t *testing.T, f *cardGuardFixture) {
			editor := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test")
			f.serve(view{"authors": []map[string]any{agentRow(f.authorID, f.authorTk), agentRow(f.crossLabID, editor)}})
			f.verdict(t, f.sameLabID, f.task(t, f.sameLabID, f.claudeRuntime, "claude-test"), "PASS", 1)
		}},
		// 2. Copy or restore of the designer's page: the ledger keeps the designer's rows.
		{"restored page keeps the designer's rows, the designer reviews", "design_verdict_self_review", func(t *testing.T, f *cardGuardFixture) {
			f.verdict(t, f.authorID, f.task(t, f.authorID, f.claudeRuntime, "claude-test"), "PASS", 1)
		}},
		{"copied page keeps the designer's rows, another agent of the designer's lab reviews", "design_verdict_same_lab", func(t *testing.T, f *cardGuardFixture) {
			f.verdict(t, f.sameLabID, f.task(t, f.sameLabID, f.claudeRuntime, "claude-test"), "PASS", 1)
		}},
		// 3. Borrowed task id.
		{"author row names a task Multica does not know", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{agentRow(f.authorID, "00000000-0000-4000-8000-0000000000bb")}})
			f.passByCrossLab(t)
		}},
		{"author row names a task that belongs to another agent", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{agentRow(f.sameLabID, f.authorTk)}})
			f.passByCrossLab(t)
		}},
		{"author row has no task id", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{agentRow(f.authorID, "")}})
			f.passByCrossLab(t)
		}},
		// 4. A human or legacy write, even beside an unrelated live task, is no trusted author.
		{"legacy write only", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{{"kind": "unknown", "sources": []string{"legacy"}}}, "provenanceComplete": false})
			f.passByCrossLab(t)
		}},
		{"legacy write beside the agent's row", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{agentRow(f.authorID, f.authorTk), {"kind": "unknown", "sources": []string{"legacy"}}}, "provenanceComplete": false})
			f.passByCrossLab(t)
		}},
		{"human write with provenance incomplete", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{{"kind": "user", "userId": "u-1", "sources": []string{"write"}}}, "provenanceComplete": false})
			f.passByCrossLab(t)
		}},
		{"no author rows at all", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{}})
			f.passByCrossLab(t)
		}},
		{"system row", "design_author_unknown", func(t *testing.T, f *cardGuardFixture) {
			f.serve(view{"authors": []map[string]any{{"kind": "system", "sources": []string{"write"}}}})
			f.passByCrossLab(t)
		}},
	}
	for _, tc := range cases {
		for _, entry := range cardEntries {
			t.Run(tc.name+" "+entry, func(t *testing.T) {
				f := newCardGuardFixture(t, "block")
				tc.setup(t, f)
				f.wantRefusal(t, f.send(t, entry, f.designAsk(), "reversible"), http.StatusConflict, tc.code)
			})
		}
	}
}
