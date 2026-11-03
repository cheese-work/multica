package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// The end-to-end matrix: every row of the guarantee table and the
// compatibility table, once per language. No check reads the text, so a row
// must give the same outcome in English, Vietnamese and Chinese.

var matrixLanguages = []string{"english", "vietnamese", "chinese"}

func eachMatrixLanguage(t *testing.T, run func(t *testing.T, lang string)) {
	t.Helper()
	for _, lang := range matrixLanguages {
		t.Run(lang, func(t *testing.T) { run(t, lang) })
	}
}

// twoTargetIntentText is twoTargetIntent with the intent comment in the
// language under test.
func twoTargetIntentText(t *testing.T, fixture intentFixture, text string) (a, b, intentID, sourceID string) {
	t.Helper()
	b = dbfx.User(t, "Second asked human", "matrix-b-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": b, "role": "owner"})
	both := fixture.mention() + " [@B](mention://member/" + b + ")"
	intentID, sourceID = openLaneIntent(t, fixture, strings.Replace(text, "%s", both, 1))
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE intent_id = $1`, intentID)
	})
	return fixture.humanID, b, intentID, sourceID
}

// Guarantee 1: a wakeup a member's comment can fire needs a bound request, an
// observe purpose or an agent-only filter. Both incidents registered the first
// two shapes.
func TestAskGuardMatrixWakeups(t *testing.T) {
	setAskGuardMode(t, "block")
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		text := askGuardWakeupTexts[lang]
		f := newAskGuardWakeupFixture(t)
		bound := f.request(t, f.issueID, f.humanID, "open")
		answered := f.request(t, f.issueID, f.humanID, "answered")
		unrelated := f.request(t, f.issueID, f.humanID, "open")

		refused := map[string]map[string]any{
			"member filter, no request": f.memberWait(text),
			"unfiltered reply":          f.replyWait(text, nil),
		}
		for name, body := range refused {
			f.wantFix(t, f.create(t, body).Want(http.StatusUnprocessableEntity))
			if n := f.events(t, "block"); n < 1 {
				t.Errorf("%s: no block event", name)
			}
		}
		allowed := map[string]map[string]any{
			"bound to an open request":              f.replyWait(text, map[string]any{"decision_request_id": bound}),
			"bound to an answered one":              f.replyWait(text, map[string]any{"decision_request_id": answered}),
			"agent-only observer":                   f.replyWait(text, map[string]any{"filter_actor_type": "agent"}),
			"observe":                               f.replyWait(text, map[string]any{"purpose": "observe"}),
			"timer (audit-only)":                    {"kind": "at", "after_seconds": 600, "instruction": text},
			"bound to an unrelated request (audit)": f.replyWait(text, map[string]any{"decision_request_id": unrelated, "filter_actor_type": "member", "filter_actor_id": f.humanID}),
			"task event (not a member)":             {"kind": "event", "event_types": []string{"task.completed"}, "instruction": text},
		}
		for name, body := range allowed {
			if res := f.create(t, body); res.Code != http.StatusCreated {
				t.Errorf("%s: status %d, want 201: %s", name, res.Code, res.Text())
			}
		}
		if n := f.wakeups(t); n != len(allowed) {
			t.Errorf("saved wakeups = %d, want %d", n, len(allowed))
		}
	})
}

// Guarantee 1, update path: dropping the agent-only filter makes a rule
// member-eligible, so the update is judged on the effective rule.
func TestAskGuardMatrixWakeupUpdate(t *testing.T) {
	setAskGuardMode(t, "block")
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		text := askGuardWakeupTexts[lang]
		f := newAskGuardWakeupFixture(t)
		var created struct {
			ID string `json:"id"`
		}
		f.create(t, f.replyWait(text, map[string]any{"filter_actor_type": "agent"})).Want(http.StatusCreated).JSON(&created)
		f.update(t, created.ID, f.replyWait(text, nil)).Want(http.StatusUnprocessableEntity)
		f.update(t, created.ID, f.replyWait(text, map[string]any{"decision_request_id": f.request(t, f.issueID, f.humanID, "open")})).Want(http.StatusOK)
	})
}

// Guarantee 2: an agent comment with a structural member mention is refused
// unless it is a typed kind. A link in code counts, as it does for the
// notification that would reach the member.
func TestAskGuardMatrixMentions(t *testing.T) {
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		text := askGuardCommentTexts[lang]
		for shape, wrap := range askGuardMentionShapes {
			t.Run(shape, func(t *testing.T) {
				setAskGuardMode(t, "block")
				f := newAskGuardCommentFixture(t)
				content := f.content(text, wrap)
				res := f.post(t, map[string]any{"content": content}).Want(http.StatusUnprocessableEntity)
				if !strings.Contains(res.Text(), "--notify-only") {
					t.Errorf("422 body lacks the fix command: %s", res.Text())
				}
				if f.comments(t) != 0 || f.events(t, "block") != 1 {
					t.Errorf("block: comments=%d events=%d, want 0 and 1", f.comments(t), f.events(t, "block"))
				}
				f.post(t, map[string]any{"content": content, "notify_only": true}).Want(http.StatusCreated)
				f.post(t, map[string]any{"content": content, "decision_intent": true}).Want(http.StatusCreated)
			})
			t.Run(shape+"/warn", func(t *testing.T) {
				setAskGuardMode(t, "warn")
				f := newAskGuardCommentFixture(t)
				f.post(t, map[string]any{"content": f.content(text, wrap)}).Want(http.StatusCreated)
				if f.comments(t) != 1 || f.events(t, "warn") != 1 || f.events(t, "block") != 0 {
					t.Errorf("warn: comments=%d warn=%d block=%d, want 1, 1 and 0", f.comments(t), f.events(t, "warn"), f.events(t, "block"))
				}
			})
		}
	})
}

// A --notify-only receipt does not satisfy a wait: the unfiltered wake that
// follows it is still refused.
func TestAskGuardMatrixNotifyOnlyThenWait(t *testing.T) {
	setAskGuardMode(t, "block")
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		f := newAskGuardWakeupFixture(t)
		f.post(t, map[string]any{"content": f.content(askGuardCommentTexts[lang], "%s"), "notify_only": true}).Want(http.StatusCreated)
		f.create(t, f.replyWait(askGuardWakeupTexts[lang], nil)).Want(http.StatusUnprocessableEntity)
	})
}

// Guarantee 3: a decision intent target that expires unserved gets one
// recovery notice, even when other targets of the intent were served. The
// notice quotes the source in its own language and reaches only that member.
func TestAskGuardMatrixIntentTargets(t *testing.T) {
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		setAskGuardMode(t, "block")
		fixture := newIntentFixture(t)
		a, b, intentID, _ := twoTargetIntentText(t, fixture, askGuardCommentTexts[lang])
		dbfx.Exec(t, `UPDATE decision_intent_target SET consumed_at = clock_timestamp() WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, a)
		endTaskAndSeal(t, fixture, intentID)
		reconcile(t)
		reconcile(t)
		if got := targetEventMembers(t, intentID); strings.Join(got, ",") != b {
			t.Fatalf("events for %v, want only %s", got, b)
		}
		var eventID string
		dbfx.QueryRow(t, `SELECT id::text FROM ask_guard_event WHERE intent_id = $1`, intentID).Scan(&eventID)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM ask_recovery_notice WHERE event_id = $1`, eventID)
		})

		deliverRecoveries(t)
		deliverRecoveries(t)
		if got, other := noticeMessages(t, b), noticeMessages(t, a); got != 1 || other != 0 {
			t.Fatalf("DM notices: B=%d A=%d, want 1 and 0", got, other)
		}
		if got, other := noticeInbox(t, b, fixture.issueID), noticeInbox(t, a, fixture.issueID); got != 1 || other != 0 {
			t.Fatalf("inbox items: B=%d A=%d, want 1 and 0", got, other)
		}
		var content, source string
		dbfx.QueryRow(t, `SELECT m.content FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.creator_id = $1 AND m.message_kind = 'ask_recovery'`, b).Scan(&content)
		dbfx.QueryRow(t, `SELECT c.content FROM decision_intent i JOIN comment c ON c.id = i.comment_id WHERE i.id = $1`, intentID).Scan(&source)
		if !strings.Contains(content, source) {
			t.Fatalf("notice does not quote the source verbatim:\n%s", content)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); n != 0 {
			t.Fatalf("a notice created %d decision requests", n)
		}
	})
}

// In warn mode the incident operations succeed and leave events only: no 422,
// no notice, no repair task.
func TestAskGuardMatrixWarnModeStoresEventsOnly(t *testing.T) {
	setAskGuardMode(t, "warn")
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		f := newAskGuardWakeupFixture(t)
		f.create(t, f.memberWait(askGuardWakeupTexts[lang])).Want(http.StatusCreated)
		f.post(t, map[string]any{"content": f.content(askGuardCommentTexts[lang], "%s")}).Want(http.StatusCreated)
		_, _, intentID, _ := twoTargetIntentText(t, f.intentFixture, askGuardCommentTexts[lang])
		endTaskAndSeal(t, f.intentFixture, intentID)
		reconcile(t)
		deliverRecoveries(t)

		if n := f.events(t, "warn"); n < 2 {
			t.Errorf("warn events = %d, want at least 2", n)
		}
		if n := f.events(t, "block"); n != 0 {
			t.Errorf("block events = %d, want 0", n)
		}
		for what, query := range map[string]string{
			"notices":          `SELECT count(*) FROM ask_recovery_notice WHERE event_id IN (SELECT id FROM ask_guard_event WHERE issue_id = $1)`,
			"recovery DMs":     `SELECT count(*) FROM chat_message WHERE message_kind = 'ask_recovery' AND content LIKE '%' || $1 || '%'`,
			"decision request": `SELECT count(*) FROM decision_request WHERE issue_id = $1`,
			"repair tasks":     `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND id <> $2`,
		} {
			args := []any{f.issueID}
			if what == "repair tasks" {
				args = append(args, f.taskID)
			}
			if n := dbfx.Count(t, query, args...); n != 0 {
				t.Errorf("%s = %d, want 0", what, n)
			}
		}
	})
}

// Compatibility rows. Each runs in block mode.
func TestAskGuardMatrixCompatibility(t *testing.T) {
	setAskGuardMode(t, "block")
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		text := askGuardCommentTexts[lang]

		t.Run("member-authored comment", func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": f.content(text, "%s")}), "id", f.issueID)
			testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)
			if f.events(t, "block")+f.events(t, "warn") != 0 {
				t.Error("member comment wrote an event")
			}
		})

		t.Run("verdict that mentions a member needs notify-only", func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			verdict := "Verdict PASS. " + f.content(text, "%s")
			f.post(t, map[string]any{"content": verdict}).Want(http.StatusUnprocessableEntity)
			f.post(t, map[string]any{"content": verdict, "notify_only": true}).Want(http.StatusCreated)
		})

		t.Run("ask without a structural link is audit-only", func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			f.post(t, map[string]any{"content": strings.Replace(text, "%s", "@Asked Human", 1)}).Want(http.StatusCreated)
			if f.events(t, "block") != 0 {
				t.Error("plain name wrote a block event")
			}
		})

		t.Run("squad leader task is guarded as the task's own agent", func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			squad := dbfx.Insert(t, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Matrix squad " + lang, "creator_id": testUserID, "leader_id": f.agentID})
			dbfx.Exec(t, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squad, f.agentID)
			dbfx.Exec(t, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squad, f.issueID)
			f.post(t, map[string]any{"content": f.content(text, "%s")}).Want(http.StatusUnprocessableEntity)
		})

		t.Run("agent call with no task token only warns", func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": f.content(text, "%s")}), "id", f.issueID)
			req.Header.Set("X-Actor-Source", "task_token")
			req.Header.Set("X-Agent-ID", f.agentID)
			testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)
			if f.events(t, "warn") != 1 || f.events(t, "block") != 0 {
				t.Errorf("events warn=%d block=%d, want 1 and 0", f.events(t, "warn"), f.events(t, "block"))
			}
		})

		t.Run("chat lane only warns", func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			session := dbfx.ChatSession(t, f.agentID)
			dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, session, f.taskID)
			f.post(t, map[string]any{"content": f.content(text, "%s")}).Want(http.StatusCreated)
			if f.events(t, "warn") != 1 || f.events(t, "block") != 0 {
				t.Errorf("events warn=%d block=%d, want 1 and 0", f.events(t, "warn"), f.events(t, "block"))
			}
		})

		t.Run("chat lane wakeup only warns", func(t *testing.T) {
			f := newAskGuardWakeupFixture(t)
			session := dbfx.ChatSession(t, f.agentID)
			dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, session, f.taskID)
			f.create(t, f.memberWait(askGuardWakeupTexts[lang])).Want(http.StatusCreated)
			if f.events(t, "warn") != 1 || f.events(t, "block") != 0 {
				t.Errorf("events warn=%d block=%d, want 1 and 0", f.events(t, "warn"), f.events(t, "block"))
			}
		})

		t.Run("answer arrives before the wakeup is registered", func(t *testing.T) {
			f := newAskGuardWakeupFixture(t)
			answered := f.request(t, f.issueID, f.humanID, "answered")
			f.create(t, f.replyWait(askGuardWakeupTexts[lang], map[string]any{"decision_request_id": answered})).Want(http.StatusCreated)
		})
	})
}

// The normal decision answer wake is unchanged under block mode: the member's
// card answer in any language wakes the assignee through the decision worker.
func TestAskGuardMatrixDecisionAnswerWakeUnchanged(t *testing.T) {
	setAskGuardMode(t, "block")
	labels := map[string]string{"english": "Option B", "vietnamese": "Phương án B", "chinese": "方案B"}
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		resetDecisionLease(t)
		fixture := newAskedRecordFixture(t)
		options := `[{"label":"Option A","what_happens":"A happens"},{"label":"` + labels[lang] + `","what_happens":"B happens"}]`
		dbfx.Exec(t, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, fixture.decision.ID, options)
		dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
		result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: labels[lang]})
		if err != nil {
			t.Fatal(err)
		}
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
		_, runner := newDecisionRunnerFixture(t)
		decisionPass(t, runner)
		if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running') AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, fixture.agentID, result.Comment.ID); n != 1 {
			t.Fatalf("assignee wake runs = %d, want 1", n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1`, fixture.issueID); n != 0 {
			t.Fatalf("the answer wake wrote %d guard events", n)
		}
	})
}

// Translated request creation: the typed ask passes in block mode whatever its
// language, and the ask comment that mentions the member is a typed kind.
func TestAskGuardMatrixTranslatedRequestCreation(t *testing.T) {
	setAskGuardMode(t, "block")
	asks := map[string]func(decisiontx.Ask) decisiontx.Ask{
		"english": func(a decisiontx.Ask) decisiontx.Ask { return a },
		"vietnamese": func(a decisiontx.Ask) decisiontx.Ask {
			a.ContextELI10 = "Một thay đổi đã sẵn sàng. Bạn quyết định khi nào phát hành."
			a.AgentDid = "Đã chạy các kiểm tra trong phạm vi và không thấy lỗi."
			a.StakesIfWrong = "Một thay đổi lỗi có thể đến tay người dùng."
			a.Recommendation, a.DefaultIfNoReply = "Giữ lại", "Không phát hành."
			a.Options = []decisiontx.AskOption{testAskOption("Phát hành"), testAskOption("Giữ lại")}
			return a
		},
		"chinese": func(a decisiontx.Ask) decisiontx.Ask {
			a.ContextELI10 = "有一项更改已准备好，由你决定何时发布。"
			a.AgentDid = "已运行范围内的全部检查，没有发现错误。"
			a.StakesIfWrong = "有缺陷的更改可能会到达用户手中。"
			a.Recommendation, a.DefaultIfNoReply = "暂缓", "不发布。"
			a.Options = []decisiontx.AskOption{testAskOption("发布"), testAskOption("暂缓")}
			return a
		},
	}
	decisions := map[string]string{"english": "Ship the change now?", "vietnamese": "Phát hành thay đổi ngay bây giờ?", "chinese": "现在发布这项更改吗？"}
	eachMatrixLanguage(t, func(t *testing.T, lang string) {
		fixture := newRequestCoreFixture(t, false)
		ask := asks[lang](fixture.structuredAsk())
		ask.Decision = decisions[lang]
		request := newRequest(http.MethodPost, "/api/decisions", map[string]any{
			"issue_id": fixture.issueID, "session_id": fixture.sessionID,
			"ask": ask, "escalation_class": fixture.input().EscalationClass,
		})
		request.Header.Set("X-Actor-Source", "task_token")
		request.Header.Set("X-Agent-ID", fixture.agentID)
		request.Header.Set("X-Task-ID", fixture.taskID)
		testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusCreated)
		fixture.assertCounts(t, 1)
		if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1`, fixture.issueID); n != 0 {
			t.Errorf("a typed request wrote %d guard events", n)
		}
	})
}
