package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type idleGuardFixture struct {
	issue, agent string
}

// idleGuardSetting enables the guard for the test workspace and restores the
// previous settings when the test ends.
func idleGuardSetting(test *testing.T, setting string) {
	test.Helper()
	var previous string
	dbfx.QueryRow(test, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, previous, testWorkspaceID)
	})
	dbfx.Exec(test, `UPDATE workspace SET settings = settings || jsonb_build_object('idle_started_guard', $1::jsonb) WHERE id = $2`, setting, testWorkspaceID)
}

// newIdleGuardFixture is the CHE-2433 shape: in_progress, last task and agent
// comment hours ago, nothing agent-owned left to wake it.
func newIdleGuardFixture(test *testing.T, hours float64) idleGuardFixture {
	test.Helper()
	idleGuardSetting(test, `{"mode":"shadow"}`)
	agent := createHandlerTestAgent(test, "idle guard "+test.Name(), []byte("[]"))
	issue := dbfx.Issue(test, "idle issue", testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": agent})
	dbfx.Exec(test, `UPDATE issue SET created_at = now() - make_interval(secs => $1) WHERE id = $2`, (hours+1)*3600, issue)
	task := dbfx.Task(test, agent, testutil.Cols{"issue_id": issue, "status": "completed", "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET created_at = now() - make_interval(secs => $1), completed_at = now() - make_interval(secs => $1) WHERE id = $2`, hours*3600, task)
	comment := dbfx.Comment(test, issue, "waiting for [@Cheese](mention://member/"+testUserID+")", testutil.Cols{"author_type": "agent", "author_id": agent})
	dbfx.Exec(test, `UPDATE comment SET created_at = now() - make_interval(secs => $1) WHERE id = $2`, hours*3600, comment)
	return idleGuardFixture{issue: issue, agent: agent}
}

func (fixture idleGuardFixture) wakeup(test *testing.T, cols testutil.Cols) {
	test.Helper()
	base := testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": fixture.issue, "agent_id": fixture.agent,
		"created_by": testUserID, "instruction": "resume", "kind": "event", "mode": "continuous"}
	for key, value := range cols {
		base[key] = value
	}
	dbfx.Insert(test, "issue_wakeup", base)
}

func (fixture idleGuardFixture) found(test *testing.T) bool {
	test.Helper()
	findings, err := testHandler.checkIdleStartedIssues(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	for _, finding := range findings {
		if uuidToString(finding.IssueID) == fixture.issue {
			return true
		}
	}
	return false
}

func TestIdleStartedGuardCHE2433Shape(test *testing.T) {
	fixture := newIdleGuardFixture(test, 5)
	fixture.wakeup(test, testutil.Cols{"event_types": testutil.Raw(`ARRAY['comment.created']`), "filter_actor_type": "member", "filter_actor_id": testUserID})
	if !fixture.found(test) {
		test.Fatal("idle in_progress issue waiting on a member wakeup was not reported")
	}
}

func TestIdleStartedGuardShadowWritesNothing(test *testing.T) {
	fixture := newIdleGuardFixture(test, 5)
	var before, after string
	snapshot := `SELECT concat_ws('|', status, revision, last_activity_at, updated_at, metadata) FROM issue WHERE id = $1`
	dbfx.QueryRow(test, snapshot, fixture.issue).Scan(&before)
	if !fixture.found(test) {
		test.Fatal("idle issue was not reported")
	}
	dbfx.QueryRow(test, snapshot, fixture.issue).Scan(&after)
	if before != after {
		test.Fatalf("shadow pass wrote to the issue:\nbefore %s\nafter  %s", before, after)
	}
}

func TestIdleStartedGuardModeOff(test *testing.T) {
	fixture := newIdleGuardFixture(test, 5)
	for _, setting := range []string{`{"mode":"off"}`, `{}`} {
		idleGuardSetting(test, setting)
		if fixture.found(test) {
			test.Fatalf("setting %s must not report", setting)
		}
	}
}

func TestIdleStartedGuardBoundaries(test *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		hours   float64
		want    bool
	}{
		{"just under four hours", `{"mode":"shadow"}`, 3.98, false},
		{"just over four hours", `{"mode":"shadow"}`, 4.02, true},
		{"after_hours 8 holds a five hour wait", `{"mode":"shadow","after_hours":8}`, 5, false},
		{"after_hours 8 reports nine hours", `{"mode":"shadow","after_hours":8}`, 9, true},
		{"after_hours out of range falls back to four", `{"mode":"shadow","after_hours":500}`, 5, true},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newIdleGuardFixture(test, tc.hours)
			idleGuardSetting(test, tc.setting)
			if got := fixture.found(test); got != tc.want {
				test.Fatalf("reported = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIdleStartedGuardSkipsAgentOwnedContinuation(test *testing.T) {
	for name, arrange := range map[string]func(*testing.T, idleGuardFixture){
		"active task": func(test *testing.T, f idleGuardFixture) {
			dbfx.Task(test, f.agent, testutil.Cols{"issue_id": f.issue, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
		},
		"at wakeup": func(test *testing.T, f idleGuardFixture) {
			f.wakeup(test, testutil.Cols{"kind": "at", "mode": "once", "next_fire_at": testutil.Raw(`now() + interval '2 days'`)})
		},
		"agent filtered event wakeup": func(test *testing.T, f idleGuardFixture) {
			f.wakeup(test, testutil.Cols{"event_types": testutil.Raw(`ARRAY['comment.created']`), "filter_actor_type": "agent", "filter_actor_id": f.agent})
		},
		"non-comment event wakeup": func(test *testing.T, f idleGuardFixture) {
			f.wakeup(test, testutil.Cols{"event_types": testutil.Raw(`ARRAY['issue.status_changed']`)})
		},
		"children_done condition": func(test *testing.T, f idleGuardFixture) {
			f.wakeup(test, testutil.Cols{"condition": `{"type":"children_done"}`})
		},
		"open child": func(test *testing.T, f idleGuardFixture) {
			dbfx.Issue(test, "child", testutil.Cols{"status": "in_progress", "parent_issue_id": f.issue})
		},
		"pending pr checks": func(test *testing.T, f idleGuardFixture) {
			pr := dbfx.Insert(test, "github_pull_request", testutil.Cols{"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
				"pr_number": 90001, "title": "t", "state": "open", "html_url": "https://example.test/pr", "pr_created_at": testutil.Raw("now()"),
				"pr_updated_at": testutil.Raw("now()"), "checks_rollup_state": "PENDING"})
			dbfx.InsertNoID(test, "issue_pull_request", testutil.Cols{"issue_id": f.issue, "pull_request_id": pr}, "issue_id = $1", f.issue)
		},
		"pr rule while pr open": func(test *testing.T, f idleGuardFixture) {
			pr := dbfx.Insert(test, "github_pull_request", testutil.Cols{"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
				"pr_number": 90002, "title": "t", "state": "draft", "html_url": "https://example.test/pr", "pr_created_at": testutil.Raw("now()"),
				"pr_updated_at": testutil.Raw("now()")})
			dbfx.InsertNoID(test, "issue_pull_request", testutil.Cols{"issue_id": f.issue, "pull_request_id": pr}, "issue_id = $1", f.issue)
			f.wakeup(test, testutil.Cols{"system_rule": "pr_merged"})
		},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newIdleGuardFixture(test, 5)
			arrange(test, fixture)
			if fixture.found(test) {
				test.Fatal("issue with an agent-owned continuation was reported")
			}
		})
	}
}

func TestIdleStartedGuardIgnoresHumanOnlyWakeups(test *testing.T) {
	for name, cols := range map[string]testutil.Cols{
		"unfiltered comment wakeup": {"event_types": testutil.Raw(`ARRAY['comment.created']`)},
		"member filtered":           {"event_types": testutil.Raw(`ARRAY['issue.status_changed']`), "filter_actor_type": "member", "filter_actor_id": testUserID},
		"issue_field condition":     {"condition": `{"type":"issue_field","field":"status","value":"done"}`},
		"disabled at wakeup":        {"kind": "at", "mode": "once", "next_fire_at": testutil.Raw(`now() + interval '1 day'`), "enabled": false},
		"paused every wakeup":       {"kind": "every", "interval_seconds": 3600, "next_fire_at": testutil.Raw(`now() + interval '1 day'`), "paused_reason": "loop"},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newIdleGuardFixture(test, 5)
			fixture.wakeup(test, cols)
			if !fixture.found(test) {
				test.Fatal("wakeup nothing agent-owned will satisfy kept the issue quiet")
			}
		})
	}
}

func TestIdleStartedGuardIdleClock(test *testing.T) {
	for name, arrange := range map[string]func(*testing.T, idleGuardFixture){
		"recent member comment": func(test *testing.T, f idleGuardFixture) { dbfx.Comment(test, f.issue, "still here") },
		"recent member activity": func(test *testing.T, f idleGuardFixture) {
			dbfx.Insert(test, "activity_log", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": f.issue, "actor_type": "member", "actor_id": testUserID, "action": "updated"})
		},
		"recent agent task": func(test *testing.T, f idleGuardFixture) {
			dbfx.Task(test, f.agent, testutil.Cols{"issue_id": f.issue, "status": "completed", "runtime_id": handlerTestRuntimeID(test), "completed_at": testutil.Raw("now()")})
		},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newIdleGuardFixture(test, 5)
			arrange(test, fixture)
			if fixture.found(test) {
				test.Fatal("recent activity did not reset the idle clock")
			}
		})
	}
	test.Run("system writes do not reset", func(test *testing.T) {
		fixture := newIdleGuardFixture(test, 5)
		dbfx.Comment(test, fixture.issue, "guard note", testutil.Cols{"author_type": "system", "author_id": "00000000-0000-0000-0000-000000000000"})
		dbfx.Insert(test, "activity_log", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issue, "actor_type": "system", "action": "status_changed"})
		dbfx.Exec(test, `UPDATE issue SET metadata = metadata || '{"k":1}'::jsonb, last_activity_at = now(), updated_at = now() WHERE id = $1`, fixture.issue)
		if !fixture.found(test) {
			test.Fatal("system comment, activity or metadata write reset the idle clock")
		}
	})
}

func TestIdleStartedGuardStatusOutOfScope(test *testing.T) {
	for _, status := range []string{"in_review", "need_decision", "blocked", "merged", "todo", "done"} {
		test.Run(status, func(test *testing.T) {
			fixture := newIdleGuardFixture(test, 5)
			dbfx.Exec(test, `UPDATE issue SET status = $1 WHERE id = $2`, status, fixture.issue)
			if fixture.found(test) {
				test.Fatalf("status %s must be outside the covered set", status)
			}
		})
	}
	test.Run("agent_review is covered", func(test *testing.T) {
		fixture := newIdleGuardFixture(test, 5)
		dbfx.Exec(test, `UPDATE issue SET status = 'agent_review' WHERE id = $1`, fixture.issue)
		if !fixture.found(test) {
			test.Fatal("agent_review must be covered")
		}
	})
	test.Run("statuses setting widens the set", func(test *testing.T) {
		fixture := newIdleGuardFixture(test, 5)
		dbfx.Exec(test, `UPDATE issue SET status = 'merged' WHERE id = $1`, fixture.issue)
		idleGuardSetting(test, `{"mode":"shadow","statuses":["merged"]}`)
		if !fixture.found(test) {
			test.Fatal("listed status was not covered")
		}
	})
}

func TestIdleStartedGuardSkipsIssueWithGuardReceipt(test *testing.T) {
	fixture := newIdleGuardFixture(test, 5)
	dbfx.Exec(test, `UPDATE issue SET metadata = metadata || '{"idle_guard":{"state":"done"}}'::jsonb WHERE id = $1`, fixture.issue)
	if fixture.found(test) {
		test.Fatal("issue with an idle_guard episode was reported again")
	}
}

// The boundary cases only mean something if the fixture stores the age it was
// asked for, so read the stored age back (fractional hours must not truncate).
func TestIdleStartedGuardFixtureStoresFractionalAge(test *testing.T) {
	fixture := newIdleGuardFixture(test, 3.98)
	var hours float64
	dbfx.QueryRow(test, `SELECT extract(epoch FROM now() - created_at) / 3600 FROM comment WHERE issue_id = $1`, fixture.issue).Scan(&hours)
	if hours < 3.97 || hours > 3.99 {
		test.Fatalf("stored comment age = %.4f h, want about 3.98 h", hours)
	}
}
