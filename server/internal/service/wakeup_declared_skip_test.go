package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-5457 (CHE-3592): an agent-set time wakeup that declared watch_inputs
// starts no run when every declared input is as it was when its last completed
// run started. Behind workspace settings.time_wakeup_skip.mode.

func (e skipEnv) mode(t *testing.T, settings string) {
	t.Helper()
	e.f.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, e.f.WorkspaceID, settings)
}

func (e skipEnv) blockMode(t *testing.T) {
	t.Helper()
	e.mode(t, `{"time_wakeup_skip":{"mode":"block"}}`)
}

// declared makes an agent-armed hourly rule that watches the given inputs.
func (e skipEnv) declared(t *testing.T, inputs ...string) pgtype.UUID {
	t.Helper()
	w := wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "every", IntervalSeconds: 3600, Instruction: "Look at the issue and report only if it moved"})
	e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$2, watch_inputs=$3 WHERE id=$1`, w.ID, dbid.NewV7(), inputs)
	return w.ID
}

func (e skipEnv) declaredSkips(t *testing.T) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped' AND details->>'rule'='declared_inputs'`, e.issue)
}

// twoFires runs a fire, completes its run, applies change, and fires again.
func (e skipEnv) twoFires(t *testing.T, id pgtype.UUID, change func()) {
	t.Helper()
	wakeTick(t, e.f, e.s, id)
	e.finishRuns(t, "completed")
	if change != nil {
		change()
	}
	wakeTick(t, e.f, e.s, id)
}

func TestDeclaredInputsBlockSkipsUnchangedFire(t *testing.T) {
	e := newSkipEnv(t, false)
	e.blockMode(t)
	id := e.declared(t, "issue")
	e.twoFires(t, id, nil)
	if got := wakeRuns(t, e.f, id); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
	if got := e.declaredSkips(t); got != 1 {
		t.Fatalf("declared_inputs skip entries = %d, want 1", got)
	}
	var details []byte
	var lastTask string
	e.f.QueryRow(t, `SELECT details, (SELECT id::text FROM agent_task_queue WHERE issue_id=$1 LIMIT 1) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped'`, e.issue).Scan(&details, &lastTask)
	var d struct {
		Rule       string   `json:"rule"`
		WakeupID   string   `json:"wakeup_id"`
		Inputs     []string `json:"inputs"`
		LastTaskID string   `json:"last_task_id"`
	}
	if err := json.Unmarshal(details, &d); err != nil {
		t.Fatal(err)
	}
	if d.Rule != "declared_inputs" || d.WakeupID != util.UUIDToString(id) || len(d.Inputs) != 1 || d.Inputs[0] != "issue" || d.LastTaskID != lastTask {
		t.Fatalf("details = %s, want rule, wakeup_id %s, inputs [issue], last_task_id %s", details, util.UUIDToString(id), lastTask)
	}
	// A third fire is skipped too: the skip itself is not a run to compare to.
	wakeTick(t, e.f, e.s, id)
	if got := wakeRuns(t, e.f, id); got != 1 {
		t.Fatalf("runs after a third unchanged fire = %d, want 1", got)
	}
}

func TestDeclaredInputsFirstFireRunsAndStoresFingerprint(t *testing.T) {
	e := newSkipEnv(t, false)
	e.blockMode(t)
	id := e.declared(t, "issue")
	wakeTick(t, e.f, e.s, id)
	if got := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND coalesce(context->>'wakeup_input_fingerprint','')<>''`, e.issue); got != 1 {
		t.Fatalf("runs with a stored fingerprint = %d, want 1", got)
	}
}

func TestDeclaredInputsOffWarnAndInvalidNeverSkip(t *testing.T) {
	for name, settings := range map[string]string{
		"missing": `{}`,
		"off":     `{"time_wakeup_skip":{"mode":"off"}}`,
		"warn":    `{"time_wakeup_skip":{"mode":"warn"}}`,
		"invalid": `{"time_wakeup_skip":{"mode":"skip"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newSkipEnv(t, false)
			e.mode(t, settings)
			id := e.declared(t, "issue")
			e.twoFires(t, id, nil)
			if got := wakeRuns(t, e.f, id); got != 2 {
				t.Fatalf("runs = %d, want 2", got)
			}
			if got := e.declaredSkips(t); got != 0 {
				t.Fatalf("skip entries = %d, want 0", got)
			}
		})
	}
}

func TestDeclaredInputsRunsWhenSomethingChanged(t *testing.T) {
	for name, change := range map[string]func(e skipEnv, t *testing.T, id pgtype.UUID){
		"input changed": func(e skipEnv, t *testing.T, _ pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue SET status='in_review', revision=revision+1 WHERE id=$1`, e.issue)
		},
		"instruction edited": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET instruction='New question' WHERE id=$1`, id)
		},
		"revision bumped": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET revision=revision+1 WHERE id=$1`, id)
		},
		"watch_inputs edited": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET watch_inputs='{issue,children}' WHERE id=$1`, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSkipEnv(t, false)
			e.blockMode(t)
			id := e.declared(t, "issue")
			e.twoFires(t, id, func() { change(e, t, id) })
			if got := wakeRuns(t, e.f, id); got != 2 {
				t.Fatalf("runs = %d, want 2", got)
			}
			if got := e.declaredSkips(t); got != 0 {
				t.Fatalf("skip entries = %d, want 0", got)
			}
		})
	}
}

// The record is taken when the run is enqueued, so a change while that run is
// still going makes the next fire run.
func TestDeclaredInputsChangeDuringLastRunRunsNextFire(t *testing.T) {
	e := newSkipEnv(t, false)
	e.blockMode(t)
	id := e.declared(t, "issue")
	wakeTick(t, e.f, e.s, id)
	e.f.Exec(t, `UPDATE issue SET status='in_review', revision=revision+1 WHERE id=$1`, e.issue)
	e.finishRuns(t, "completed")
	wakeTick(t, e.f, e.s, id)
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

func TestDeclaredInputsLastRunNotCompletedRuns(t *testing.T) {
	e := newSkipEnv(t, false)
	e.blockMode(t)
	id := e.declared(t, "issue")
	wakeTick(t, e.f, e.s, id)
	e.finishRuns(t, "failed")
	wakeTick(t, e.f, e.s, id)
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

// Every other condition of the skip branch, broken one at a time.
func TestDeclaredInputsSkipConditionsEachRequired(t *testing.T) {
	for name, brk := range map[string]func(e skipEnv, t *testing.T, id pgtype.UUID){
		"no watch_inputs": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET watch_inputs='{}' WHERE id=$1`, id)
		},
		"incomplete input": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET watch_inputs='{pr}' WHERE id=$1`, id)
		},
		"unknown input": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET watch_inputs='{nope}' WHERE id=$1`, id)
		},
		"no agent source task": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=NULL WHERE id=$1`, id)
		},
		"system rule": func(e skipEnv, t *testing.T, id pgtype.UUID) {
			e.f.Exec(t, `UPDATE issue_wakeup SET system_rule='child_done' WHERE id=$1`, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSkipEnv(t, false)
			e.blockMode(t)
			id := e.declared(t, "issue")
			brk(e, t, id)
			e.twoFires(t, id, nil)
			if got := wakeRuns(t, e.f, id); got != 2 {
				t.Fatalf("runs = %d, want 2", got)
			}
			if got := e.declaredSkips(t); got != 0 {
				t.Fatalf("skip entries = %d, want 0", got)
			}
		})
	}
}

// Timeout, manual wake, condition.met and comment/task/issue events always run.
func TestDeclaredInputsAlwaysRunForOtherFireKinds(t *testing.T) {
	record := func(e skipEnv, t *testing.T, id pgtype.UUID, eventType string) {
		t.Helper()
		w, err := e.f.q.LocklessWakeup(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.f.q.RecordWakeupReceipt(context.Background(), recordReceipt(w.ID, w.Revision, eventType)); err != nil {
			t.Fatal(err)
		}
		wakeDispatch(t, e.s, w)
	}
	for _, eventType := range []string{"condition.met", "comment.created", "task.completed", "issue.status_changed"} {
		t.Run("receipt only "+eventType, func(t *testing.T) {
			e := newSkipEnv(t, false)
			e.blockMode(t)
			id := e.declared(t, "issue")
			e.twoFires(t, id, func() {})
			e.finishRuns(t, "completed")
			before := wakeRuns(t, e.f, id)
			record(e, t, id, eventType)
			if got := wakeRuns(t, e.f, id); got != before+1 {
				t.Fatalf("runs = %d, want %d", got, before+1)
			}
		})
		t.Run("mixed with time.due "+eventType, func(t *testing.T) {
			e := newSkipEnv(t, false)
			e.blockMode(t)
			id := e.declared(t, "issue")
			wakeTick(t, e.f, e.s, id)
			e.finishRuns(t, "completed")
			w, err := e.f.q.LocklessWakeup(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.f.q.RecordWakeupReceipt(context.Background(), recordReceipt(w.ID, w.Revision, eventType)); err != nil {
				t.Fatal(err)
			}
			wakeTick(t, e.f, e.s, id)
			if got := wakeRuns(t, e.f, id); got != 2 {
				t.Fatalf("runs = %d, want 2", got)
			}
		})
	}
	t.Run("manual wake", func(t *testing.T) {
		e := newSkipEnv(t, false)
		e.blockMode(t)
		id := e.declared(t, "issue")
		wakeTick(t, e.f, e.s, id)
		e.finishRuns(t, "completed")
		if err := e.s.Trigger(context.Background(), e.issue, id, parseTestUUID(t, e.f.UserID)); err != nil {
			t.Fatal(err)
		}
		if got := wakeRuns(t, e.f, id); got != 2 {
			t.Fatalf("runs = %d, want 2", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		e := newSkipEnv(t, false)
		e.blockMode(t)
		w := wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "event", EventTypes: []string{"comment.created"}, ExpiresInSeconds: 3600, OnTimeout: "wake", Instruction: "Escalate"})
		e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$2, watch_inputs='{issue}', expires_at=now()-interval '1 second' WHERE id=$1`, w.ID, dbid.NewV7())
		cur, err := e.f.q.LocklessWakeup(context.Background(), w.ID)
		if err != nil {
			t.Fatal(err)
		}
		wakeDispatch(t, e.s, cur)
		if got := wakeRuns(t, e.f, w.ID); got != 1 {
			t.Fatalf("runs = %d, want 1", got)
		}
	})
}

func recordReceipt(id pgtype.UUID, revision int64, eventType string) db.RecordWakeupReceiptParams {
	return db.RecordWakeupReceiptParams{ID: dbid.NewV7(), WakeupID: id, Revision: revision, EventKey: eventType + ":" + util.UUIDToString(dbid.NewV7()), EventType: eventType, Payload: []byte(`{}`)}
}
