package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8962 (plan CHE-8945 r6, step 2, tests F1-F33): in block mode a one-shot
// `at --watch issue` fire is deferred when the issue is as the creating run's
// claim fence recorded it. The fence is written the way the claim handler does
// (the real IssueWatchFence projection, stored under snapshot key `watch`); the
// fire goes through the real scheduler dispatch.

type fenceEnv struct {
	skipEnv
	run pgtype.UUID // the creating run R
}

func newFenceEnv(t *testing.T) fenceEnv {
	t.Helper()
	e := fenceEnv{skipEnv: newSkipEnv(t, false)}
	e.blockMode(t)
	e.run = e.newRun(t)
	return e
}

func (e fenceEnv) newRun(t *testing.T) pgtype.UUID {
	t.Helper()
	id := e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "status": "running", "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')")})
	return parseTestUUID(t, id)
}

// claimFor stores run's fence the way loadIssueWatchFence does: no fence when
// something is attached or the issue has more than 200 comment rows.
func (e fenceEnv) claimFor(t *testing.T, run pgtype.UUID) {
	t.Helper()
	row, err := e.f.q.IssueWatchFence(context.Background(), db.IssueWatchFenceParams{IssueID: e.issue})
	if err != nil {
		t.Fatal(err)
	}
	snap := map[string]any{"v": 1, "title_sha256": "t", "description_sha256": "d"}
	if !row.Attached && row.CommentRows <= 200 {
		snap["watch"] = map[string]any{"v": 2, "row": row.RowHash, "revision": row.Revision, "comments": row.CommentHash, "at": row.ReadAt.Time}
	}
	raw, _ := json.Marshal(snap)
	e.f.Exec(t, `UPDATE agent_task_queue SET issue_snapshot=$2::jsonb WHERE id=$1`, run, string(raw))
}

func (e fenceEnv) claim(t *testing.T) { t.Helper(); e.claimFor(t, e.run) }

// say posts n comments as run (the real CreateComment, which bumps the revision).
func (e fenceEnv) sayAs(t *testing.T, run pgtype.UUID, n int) []pgtype.UUID {
	t.Helper()
	var ids []pgtype.UUID
	for i := 0; i < n; i++ {
		c, err := e.f.q.CreateComment(context.Background(), db.CreateCommentParams{
			IssueID: e.issue, WorkspaceID: parseTestUUID(t, e.f.WorkspaceID), AuthorType: "agent", AuthorID: parseTestUUID(t, e.agent),
			Content: fmt.Sprintf("progress %d", i), Type: "comment", SourceTaskID: run,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	return ids
}

func (e fenceEnv) say(t *testing.T, n int) []pgtype.UUID { t.Helper(); return e.sayAs(t, e.run, n) }

func (e fenceEnv) armFrom(t *testing.T, run pgtype.UUID, inputs ...string) pgtype.UUID {
	t.Helper()
	if len(inputs) == 0 {
		inputs = []string{"issue"}
	}
	w, err := e.s.Create(context.Background(), e.issue, parseTestUUID(t, e.f.UserID), run, WakeupInput{
		AgentID: e.agent, Kind: "at", AfterSeconds: 600, Instruction: "Check back", WatchInputs: inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func (e fenceEnv) arm(t *testing.T, inputs ...string) pgtype.UUID {
	t.Helper()
	return e.armFrom(t, e.run, inputs...)
}

func (e fenceEnv) completeRun(t *testing.T, run pgtype.UUID) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run)
}

func (e fenceEnv) complete(t *testing.T) { t.Helper(); e.completeRun(t, e.run) }

// happy is the run R of the plan: claimed, two own comments, arms, completes.
func (e fenceEnv) happy(t *testing.T) pgtype.UUID {
	t.Helper()
	e.claim(t)
	e.say(t, 2)
	id := e.arm(t)
	e.complete(t)
	return id
}

func (e fenceEnv) ran(t *testing.T, id pgtype.UUID, want int) {
	t.Helper()
	if got := wakeRuns(t, e.f, id); got != want {
		t.Fatalf("runs = %d, want %d", got, want)
	}
}

// fenceGuard proves one fail-open guard against the comparator: the same
// eligible baseline defers (control), and with the one change applied the fire
// runs. A case that also passes with the comparator removed proves nothing.
func fenceGuard(t *testing.T, build func(t *testing.T, e fenceEnv, change bool) pgtype.UUID) {
	t.Helper()
	t.Run("control defers", func(t *testing.T) {
		e := newFenceEnv(t)
		e.mustDefer(t, build(t, e, false))
	})
	t.Run("change runs", func(t *testing.T) {
		e := newFenceEnv(t)
		e.mustRun(t, build(t, e, true))
	})
}

func (e fenceEnv) dropLabel(t *testing.T, label string) {
	t.Helper()
	e.f.Exec(t, `DELETE FROM issue_to_label WHERE label_id=$1`, label)
	e.f.Exec(t, `DELETE FROM issue_label WHERE id=$1`, label)
}

// armed is the plan's R after the claim: two own comments, arm, complete.
func (e fenceEnv) armed(t *testing.T, mid func()) pgtype.UUID {
	t.Helper()
	e.claim(t)
	if mid != nil {
		mid()
	}
	e.say(t, 2)
	id := e.arm(t)
	e.complete(t)
	return id
}

func (e fenceEnv) mustRun(t *testing.T, id pgtype.UUID) {
	t.Helper()
	wakeTick(t, e.f, e.s, id)
	e.ran(t, id, 1)
	if got := e.acts(t, "wake_deferred"); got != 0 {
		t.Fatalf("wake_deferred rows = %d, want 0", got)
	}
}

func (e fenceEnv) mustDefer(t *testing.T, id pgtype.UUID) {
	t.Helper()
	wakeTick(t, e.f, e.s, id)
	e.ran(t, id, 0)
	if got := e.acts(t, "wake_deferred"); got != 1 {
		t.Fatalf("wake_deferred rows = %d, want 1", got)
	}
}

func (e fenceEnv) bump(t *testing.T, set string) {
	t.Helper()
	e.f.Exec(t, `UPDATE issue SET `+set+`, revision=revision+1 WHERE id=$1`, e.issue)
}

func (e fenceEnv) memberComment(t *testing.T) string {
	t.Helper()
	c, err := e.f.q.CreateComment(context.Background(), db.CreateCommentParams{
		IssueID: e.issue, WorkspaceID: parseTestUUID(t, e.f.WorkspaceID), AuthorType: "member", AuthorID: parseTestUUID(t, e.f.UserID),
		Content: "A person wrote", Type: "comment",
	})
	if err != nil {
		t.Fatal(err)
	}
	return util.UUIDToString(c.ID)
}

func (e fenceEnv) label(t *testing.T) string {
	t.Helper()
	var id string
	e.f.QueryRow(t, `INSERT INTO issue_label (workspace_id, name, color) VALUES ($1, 'f-'||gen_random_uuid()::text, '#fff') RETURNING id`, e.f.WorkspaceID).Scan(&id)
	e.f.Cleanup(t, `DELETE FROM issue_label WHERE id=$1`, id)
	e.f.Cleanup(t, `DELETE FROM issue_to_label WHERE label_id=$1`, id)
	e.f.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, e.issue, id)
	return id
}

func (e fenceEnv) attachment(t *testing.T, issueLevel bool, commentID string) {
	t.Helper()
	if issueLevel {
		e.f.Exec(t, `INSERT INTO attachment (workspace_id, issue_id, uploader_type, uploader_id, filename, url, content_type, size_bytes) VALUES ($1,$2,'member',$3,'f.txt','u','text/plain',1)`, e.f.WorkspaceID, e.issue, e.f.UserID)
	} else {
		e.f.Exec(t, `INSERT INTO attachment (workspace_id, comment_id, uploader_type, uploader_id, filename, url, content_type, size_bytes) VALUES ($1,$2,'member',$3,'f.txt','u','text/plain',1)`, e.f.WorkspaceID, commentID, e.f.UserID)
	}
	e.f.Cleanup(t, `DELETE FROM attachment WHERE workspace_id=$1 AND filename='f.txt'`, e.f.WorkspaceID)
}

func (e fenceEnv) detail(t *testing.T, action, key string) string {
	t.Helper()
	var v *string
	e.f.QueryRow(t, `SELECT details->>$3 FROM activity_log WHERE issue_id=$1 AND action=$2 ORDER BY created_at DESC LIMIT 1`, e.issue, action, key).Scan(&v)
	if v == nil {
		return ""
	}
	return *v
}

func TestAtFenceF1DefersUnchangedIssue(t *testing.T) {
	e := newFenceEnv(t)
	id := e.happy(t)
	e.mustDefer(t, id)
	for key, want := range map[string]string{"rule": "declared_inputs", "baseline": "creating_run", "last_task_id": util.UUIDToString(e.run)} {
		if got := e.detail(t, "wake_deferred", key); got != want {
			t.Fatalf("wake_deferred details %s = %q, want %q", key, got, want)
		}
	}
	if !e.enabled(t, id) || e.pendingDue(t, id) != 1 {
		t.Fatalf("a deferred fire stays enabled and pending (enabled=%v pending=%d)", e.enabled(t, id), e.pendingDue(t, id))
	}
	e.tick(t)
	e.ran(t, id, 0)
	if got := e.acts(t, "wake_deferred"); got != 1 {
		t.Fatalf("second tick wrote another row: %d", got)
	}
}

func TestAtFenceF2MemberCommentRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		id := e.happy(t)
		if change {
			e.memberComment(t)
		}
		return id
	})
}

func TestAtFenceF3MetadataByAnotherActorRuns(t *testing.T) {
	t.Run("set during R", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			return e.armed(t, func() {
				if change {
					e.bump(t, `metadata='{"k":"v"}'::jsonb`)
				}
			})
		})
	})
	t.Run("F3b deleted during R", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			e.bump(t, `metadata='{"k":"v"}'::jsonb`)
			return e.armed(t, func() {
				if change {
					e.bump(t, `metadata='{}'::jsonb`)
				}
			})
		})
	})
}

func TestAtFenceF4OwnStatusOrSummaryWriteRuns(t *testing.T) {
	for name, set := range map[string]string{
		"status_summary": `metadata='{"status_summary":"x"}'::jsonb`,
		"status":         `status='in_review'`,
	} {
		t.Run(name, func(t *testing.T) {
			fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
				e.claim(t)
				e.say(t, 1)
				if change {
					e.bump(t, set)
				}
				id := e.arm(t)
				e.complete(t)
				return id
			})
		})
	}
}

func TestAtFenceF5ChangeRightAfterFenceReadRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		return e.armed(t, func() {
			if change {
				e.bump(t, `status='in_review'`) // another actor, before R's payload read
			}
		})
	})
}

func TestAtFenceF6NoFenceRuns(t *testing.T) {
	cases := map[string]string{
		"NULL snapshot":                    `UPDATE agent_task_queue SET issue_snapshot=NULL WHERE id=$1`,
		"v is 1":                           `UPDATE agent_task_queue SET issue_snapshot=jsonb_set(issue_snapshot,'{watch,v}','1') WHERE id=$1`,
		"no watch key (failed fence read)": `UPDATE agent_task_queue SET issue_snapshot=issue_snapshot-'watch' WHERE id=$1`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
				id := e.happy(t)
				if change {
					e.f.Exec(t, sql, e.run)
				}
				return id
			})
		})
	}
}

// F7 narrows the CHE-6971 constraint "a wakeup an agent created for another
// agent fires once": the creating run's fence covers only a wakeup for the
// SAME agent. Cheese chose "Allow, keep fail-open" for the same-agent case
// (CHE-5458 comment 01a1fb5b-0648-708a-a032-b564085aeb9d).
func TestAtFenceF7WakeupForAnotherAgentFiresOnce(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		target := e.agent
		if change {
			target = e.f.privateAgentOwnedBy(t, e.f.UserID, "other")
		}
		e.claim(t)
		e.say(t, 2)
		w, err := e.s.Create(context.Background(), e.issue, parseTestUUID(t, e.f.UserID), e.run, WakeupInput{
			AgentID: target, Kind: "at", AfterSeconds: 600, Instruction: "Fix the P1 findings", WatchInputs: []string{"issue"},
		})
		if err != nil {
			t.Fatal(err)
		}
		e.complete(t)
		return w.ID
	})
}

func TestAtFenceF8WakeNowRuns(t *testing.T) {
	e := newFenceEnv(t)
	id := e.happy(t)
	e.mustDefer(t, id)
	if err := e.s.Trigger(context.Background(), e.issue, id, parseTestUUID(t, e.f.UserID)); err != nil {
		t.Fatal(err)
	}
	e.ran(t, id, 1)
}

// A deadline fire is never compared: the compare read is not sent for it, while
// the same fixture as a plain one-shot does send it (control).
func TestAtFenceF9DeadlineFireRunsWithoutCompare(t *testing.T) {
	reads := 0
	oldRead := atFenceRead
	t.Cleanup(func() { atFenceRead = oldRead })
	atFenceRead = func(ctx context.Context, sp pgx.Tx, arg db.IssueWatchFenceParams) (db.IssueWatchFenceRow, error) {
		reads++
		return oldRead(ctx, sp, arg)
	}
	t.Run("control: one-shot compares", func(t *testing.T) {
		reads = 0
		e := newFenceEnv(t)
		e.mustDefer(t, e.happy(t))
		if reads == 0 {
			t.Fatal("the control must send the compare read")
		}
	})
	t.Run("deadline fire", func(t *testing.T) {
		reads = 0
		e := newFenceEnv(t)
		w := wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "event", EventTypes: []string{"comment.created"}, ExpiresInSeconds: 7200, OnTimeout: "wake", Instruction: "Escalate"})
		e.claim(t)
		e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$2, watch_inputs='{issue}', expires_at=now()-interval '1 second' WHERE id=$1`, w.ID, e.run)
		e.complete(t)
		e.mustRun(t, w.ID)
		if reads != 0 {
			t.Fatalf("compare reads = %d, want 0", reads)
		}
	})
}

func TestAtFenceF10WarnLogsWouldDeferAndRuns(t *testing.T) {
	e := newFenceEnv(t)
	e.mode(t, `{"time_wakeup_skip":{"mode":"warn"}}`)
	id := e.happy(t)
	wakeTick(t, e.f, e.s, id)
	e.ran(t, id, 1)
	if got := e.acts(t, "wake_would_defer"); got != 1 {
		t.Fatalf("wake_would_defer rows = %d, want 1", got)
	}
	if got := e.detail(t, "wake_would_defer", "baseline"); got != "creating_run" {
		t.Fatalf("baseline = %q, want creating_run", got)
	}
	if left := e.detail(t, "wake_would_defer", "time_left_ms"); left == "" || left == "0" {
		t.Fatalf("the would-defer row must carry the time left at the compare point, got %q", left)
	}
	if got := e.acts(t, "wake_deferred"); got != 0 {
		t.Fatalf("warn deferred: %d rows", got)
	}
}

func TestAtFenceF11OffReplaysDeferredFire(t *testing.T) {
	e := newFenceEnv(t)
	id := e.happy(t)
	e.mustDefer(t, id)
	e.mode(t, `{"time_wakeup_skip":{"mode":"off"}}`)
	e.tick(t)
	e.ran(t, id, 1)
	if e.pendingDue(t, id) != 0 {
		t.Fatal("the replayed fire must be consumed")
	}
}

func TestAtFenceF12RunOlderThanSixHoursRuns(t *testing.T) {
	t.Run("never deferred", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			id := e.happy(t)
			if change {
				e.f.Exec(t, `UPDATE agent_task_queue SET completed_at=now()-interval '7 hours' WHERE id=$1`, e.run)
			}
			return id
		})
	})
	t.Run("already deferred", func(t *testing.T) {
		e := newFenceEnv(t)
		id := e.happy(t)
		e.mustDefer(t, id)
		e.f.Exec(t, `UPDATE agent_task_queue SET completed_at=now()-interval '7 hours' WHERE id=$1`, e.run)
		e.tick(t)
		e.ran(t, id, 1)
	})
}

func TestAtFenceF13EditedOrRearmedWakeup(t *testing.T) {
	in := WakeupInput{Kind: "at", AfterSeconds: 600, Instruction: "Check back, edited", WatchInputs: []string{"issue"}}
	t.Run("member edit has no creating run", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			id := e.happy(t)
			if change {
				in.AgentID = e.agent
				if _, err := e.s.Save(context.Background(), e.issue, parseTestUUID(t, e.f.UserID), pgtype.UUID{}, id, in); err != nil {
					t.Fatal(err)
				}
			}
			return id
		})
	})
	t.Run("agent re-arm takes the new run's fence", func(t *testing.T) {
		e := newFenceEnv(t)
		id := e.happy(t)
		e.memberComment(t) // a change R's fence does not cover
		e.ran(t, id, 0)
		r2 := e.newRun(t)
		e.claimFor(t, r2)
		in.AgentID = e.agent
		if _, err := e.s.Save(context.Background(), e.issue, parseTestUUID(t, e.f.UserID), r2, id, in); err != nil {
			t.Fatal(err)
		}
		e.completeRun(t, r2)
		e.mustDefer(t, id)
		if got := e.detail(t, "wake_deferred", "last_task_id"); got != util.UUIDToString(r2) {
			t.Fatalf("last_task_id = %q, want the re-arming run %s", got, util.UUIDToString(r2))
		}
	})
}

func TestAtFenceF14CreatingRunNotCompletedRuns(t *testing.T) {
	for _, status := range []string{"running", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
				id := e.happy(t)
				if change {
					e.f.Exec(t, `UPDATE agent_task_queue SET status=$2 WHERE id=$1`, e.run, status)
				}
				return id
			})
		})
	}
}

func TestAtFenceF15MoreThanIssueUsesTodaysRule(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		root := e.memberComment(t)
		e.claim(t)
		e.say(t, 2)
		inputs := []string{"issue"}
		if change {
			inputs = append(inputs, "thread:"+root)
		}
		id := e.arm(t, inputs...)
		e.complete(t)
		return id
	})
}

func TestAtFenceF16EditedOrDeletedFreshCommentRuns(t *testing.T) {
	cases := map[string]string{
		"R edits it":      `UPDATE comment SET content='edited', revision=revision+1, updated_at=now() WHERE id=$1`,
		"a member edits":  `UPDATE comment SET content='edited by a person', revision=revision+1, updated_at=now() WHERE id=$1`,
		"it is deleted":   `DELETE FROM comment WHERE id=$1`,
		"it is tombstone": `UPDATE comment SET deleted_at=now() WHERE id=$1`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
				e.claim(t)
				ids := e.say(t, 2)
				id := e.arm(t)
				e.complete(t)
				if change {
					e.f.Exec(t, sql, ids[0])
				}
				return id
			})
		})
	}
}

func TestAtFenceF17CreatingRunOnAnotherIssueRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		id := e.happy(t)
		if change {
			other := e.f.Issue(t, "Another issue")
			e.f.Exec(t, `UPDATE agent_task_queue SET issue_id=$2 WHERE id=$1`, e.run, other)
		}
		return id
	})
}

func TestAtFenceF18RestampedOlderCommentRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		old := e.memberComment(t)
		id := e.armed(t, nil)
		if change {
			// comment.go:4313: an edit re-stamps the comment to the editing run.
			e.f.Exec(t, `UPDATE comment SET source_task_id=$2, content='edited', revision=revision+1, updated_at=now() WHERE id=$1`, old, e.run)
		}
		return id
	})
}

func TestAtFenceF19LabelCases(t *testing.T) {
	t.Run("label at claim, workspace label deleted after", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			label := e.label(t)
			if !change {
				e.dropLabel(t, label) // control: no label at the claim, so a fence is stored
			}
			id := e.happy(t) // change: a label at claim stores no fence
			if change {
				e.dropLabel(t, label)
			}
			return id
		})
	})
	t.Run("F19b label added after claim", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			id := e.happy(t)
			if change {
				e.label(t)
			}
			return id
		})
	})
}

func TestAtFenceF20AttachmentCases(t *testing.T) {
	t.Run("attachment at claim, relinked to an older comment", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			old := e.memberComment(t)
			if change {
				e.attachment(t, true, "")
			}
			id := e.happy(t)
			if change {
				e.f.Exec(t, `UPDATE attachment SET issue_id=NULL, comment_id=$1 WHERE workspace_id=$2 AND filename='f.txt'`, old, e.f.WorkspaceID)
			}
			return id
		})
	})
	t.Run("F20b first attachment after claim", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			id := e.happy(t)
			if change {
				e.attachment(t, true, "")
			}
			return id
		})
	})
}

func TestAtFenceF21LabelRenamedAfterClaimRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		label := e.label(t)
		if !change {
			e.dropLabel(t, label) // control: no label at the claim
		}
		id := e.happy(t)
		if change {
			e.f.Exec(t, `UPDATE issue_label SET name=name||'-renamed', updated_at=now() WHERE id=$1`, label)
		}
		return id
	})
}

func TestAtFenceF22ReactionCases(t *testing.T) {
	t.Run("reaction on a comment from before the claim", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			old := e.memberComment(t)
			id := e.happy(t)
			if change {
				e.f.Exec(t, `INSERT INTO comment_reaction (comment_id, workspace_id, actor_type, actor_id, emoji) VALUES ($1,$2,'member',$3,'👍')`, old, e.f.WorkspaceID, e.f.UserID)
			}
			return id
		})
	})
	t.Run("F22b reaction on the issue", func(t *testing.T) {
		fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
			id := e.happy(t)
			if change {
				e.f.Exec(t, `INSERT INTO issue_reaction (issue_id, workspace_id, actor_type, actor_id, emoji) VALUES ($1,$2,'member',$3,'👍')`, e.issue, e.f.WorkspaceID, e.f.UserID)
			}
			return id
		})
	})
}

func TestAtFenceF23ResolvedOlderThreadRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		old := e.memberComment(t)
		id := e.happy(t)
		if change {
			// ResolveComment does not bump issue.revision.
			e.f.Exec(t, `UPDATE comment SET resolved_at=now(), resolved_by_type='member', resolved_by_id=$2 WHERE id=$1`, old, e.f.UserID)
		}
		return id
	})
}

func TestAtFenceF24DecisionLinkOnOlderCommentRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		old := e.memberComment(t)
		id := e.happy(t)
		if change {
			// decision.sql:18: no revision bump on either row.
			e.f.Exec(t, `UPDATE comment SET decision_request_id=gen_random_uuid() WHERE id=$1`, old)
		}
		return id
	})
}

func TestAtFenceF25IssueColumnWriteWithoutRevisionRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		id := e.happy(t)
		if change {
			e.f.Exec(t, `UPDATE issue SET priority='high' WHERE id=$1`, e.issue)
		}
		return id
	})
}

func TestAtFenceF26AttachedSourceContextRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		anchor := e.memberComment(t)
		id := e.happy(t)
		if change {
			e.f.Exec(t, `INSERT INTO issue_source_context (id, workspace_id, issue_id, source_issue_id, anchor_comment_id, captured_by_user_id, snapshot_version, snapshot, capture_digest, state, attached_at)
				VALUES (gen_random_uuid(), $1, $2, $2, $3, $4, 1, '{}'::jsonb, 'd', 'attached', now())`, e.f.WorkspaceID, e.issue, anchor, e.f.UserID)
			e.f.Cleanup(t, `DELETE FROM issue_source_context WHERE issue_id=$1`, e.issue)
		}
		return id
	})
}

func TestAtFenceF27FreshDecisionCommentRuns(t *testing.T) {
	cases := map[string]string{
		"decision card in place (F27)":   `UPDATE comment SET decision_request_id=gen_random_uuid() WHERE id=$1`,
		"stamped decision intent (F27b)": `UPDATE comment SET decision_intent_id=gen_random_uuid() WHERE id=$1`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
				e.claim(t)
				ids := e.say(t, 2)
				id := e.arm(t)
				e.complete(t)
				if change {
					e.f.Exec(t, sql, ids[0])
				}
				return id
			})
		})
	}
}

func TestAtFenceF28CompareTimeoutFallsThroughToNormalFire(t *testing.T) {
	e := newFenceEnv(t)
	id := e.happy(t)
	var want string
	e.f.QueryRow(t, `SELECT current_setting('statement_timeout')`).Scan(&want)
	// The task insert stamps the transaction's statement_timeout, which must be
	// back to its old value after the savepoint rollback.
	e.f.Exec(t, `CREATE OR REPLACE FUNCTION che8962_stamp_timeout() RETURNS trigger AS $$
		BEGIN NEW.context := COALESCE(NEW.context,'{}'::jsonb) || jsonb_build_object('stmt_timeout', current_setting('statement_timeout')); RETURN NEW; END $$ LANGUAGE plpgsql`)
	e.f.Exec(t, fmt.Sprintf(`CREATE TRIGGER che8962_stamp BEFORE INSERT ON agent_task_queue FOR EACH ROW WHEN (NEW.issue_id = '%s') EXECUTE FUNCTION che8962_stamp_timeout()`, util.UUIDToString(e.issue)))
	e.f.Cleanup(t, `DROP TRIGGER IF EXISTS che8962_stamp ON agent_task_queue`)

	var inside string
	old := atFenceRead
	t.Cleanup(func() { atFenceRead = old })
	atFenceRead = func(ctx context.Context, sp pgx.Tx, _ db.IssueWatchFenceParams) (db.IssueWatchFenceRow, error) {
		if err := sp.QueryRow(ctx, `SELECT current_setting('statement_timeout')`).Scan(&inside); err != nil {
			return db.IssueWatchFenceRow{}, err
		}
		_, err := sp.Exec(ctx, `SELECT pg_sleep(1)`)
		return db.IssueWatchFenceRow{}, err
	}
	e.mustRun(t, id)
	if inside != "300ms" {
		t.Fatalf("statement_timeout inside the compare = %q, want 300ms", inside)
	}
	var got string
	e.f.QueryRow(t, `SELECT context->>'stmt_timeout' FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, util.UUIDToString(id)).Scan(&got)
	if got != want {
		t.Fatalf("statement_timeout at delivery = %q, want the old value %q", got, want)
	}
	if e.pendingDue(t, id) != 0 {
		t.Fatal("the receipt must be consumed")
	}
}

func TestAtFenceF29CommentsGrowPastCapRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		bulk := func(n int) {
			e.f.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
				SELECT $1, $2, 'member', $3, 'c'||g, 'comment' FROM generate_series(1,$4) g`, e.issue, e.f.WorkspaceID, e.f.UserID, n)
			e.f.Exec(t, `UPDATE issue SET revision=revision+$2 WHERE id=$1`, e.issue, n)
		}
		bulk(150)
		id := e.happy(t) // 150 comments at the claim: a fence is stored
		if change {
			bulk(60) // 212 at fire
		}
		return id
	})
}

func TestAtFenceF30SystemCommentCarryingRunIDRuns(t *testing.T) {
	fenceGuard(t, func(t *testing.T, e fenceEnv, change bool) pgtype.UUID {
		id := e.armed(t, nil)
		if change {
			// agent.sql:2975: a platform recovery comment carries source_task_id = R.
			e.f.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, source_task_id) VALUES ($1,$2,'system','00000000-0000-0000-0000-000000000000','recovery','system',$3)`, e.issue, e.f.WorkspaceID, e.run)
			e.bump(t, `title=title`)
		}
		return id
	})
}

func TestAtFenceF31LowBudgetSendsNoCompareAndRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration // 0 keeps the default
	}{
		{"control: default budget compares and defers", 0},
		{"budget above the 2 s rule budget", time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFenceEnv(t)
			id := e.happy(t)
			oldBudget, oldRead := atFenceMinBudget, atFenceRead
			t.Cleanup(func() { atFenceMinBudget, atFenceRead = oldBudget, oldRead })
			if tc.budget > 0 {
				atFenceMinBudget = tc.budget
			}
			reads := 0
			atFenceRead = func(ctx context.Context, sp pgx.Tx, arg db.IssueWatchFenceParams) (db.IssueWatchFenceRow, error) {
				reads++
				return oldRead(ctx, sp, arg)
			}
			if tc.budget == 0 {
				e.mustDefer(t, id)
				if reads == 0 {
					t.Fatal("the control must send the compare read")
				}
				return
			}
			e.mustRun(t, id)
			if reads != 0 {
				t.Fatalf("compare queries sent = %d, want 0", reads)
			}
			if e.pendingDue(t, id) != 0 {
				t.Fatal("the receipt must be consumed")
			}
		})
	}
}

func TestAtFenceF32SavepointRollbackFailureRetriesNextTick(t *testing.T) {
	e := newFenceEnv(t)
	id := e.happy(t)
	old := atFenceRollback
	t.Cleanup(func() { atFenceRollback = old })
	boom := errors.New("savepoint rollback failed")
	atFenceRollback = func(context.Context, pgx.Tx) error { return boom }

	e.f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", id)
	w, err := e.f.q.LocklessWakeup(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.dispatch(context.Background(), w); !errors.Is(err, boom) {
		t.Fatalf("dispatch error = %v, want the rollback error", err)
	}
	e.ran(t, id, 0)
	if got := e.acts(t, "wake_deferred"); got != 0 {
		t.Fatalf("wake_deferred rows = %d, want 0", got)
	}
	if got := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL AND payload ? 'deferred_at'`, id); got != 0 {
		t.Fatalf("a deferred_at stamp survived the failed dispatch (%d)", got)
	}
	if !e.enabled(t, id) {
		t.Fatal("the wakeup must stay on and due for the next pass")
	}

	// Next tick without the fault: changed input, so it takes the normal fire path.
	atFenceRollback = old
	e.memberComment(t)
	e.tick(t)
	e.ran(t, id, 1)
	if e.pendingDue(t, id) != 0 {
		t.Fatal("the receipt must be consumed once")
	}
}

func TestAtFenceF33NoDeadlineSkipsCompare(t *testing.T) {
	e := newFenceEnv(t)
	id := e.happy(t)
	oldRead := atFenceRead
	t.Cleanup(func() { atFenceRead = oldRead })
	reads := 0
	atFenceRead = func(ctx context.Context, sp pgx.Tx, arg db.IssueWatchFenceParams) (db.IssueWatchFenceRow, error) {
		reads++
		return oldRead(ctx, sp, arg)
	}
	e.f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", id)
	w, err := e.f.q.LocklessWakeup(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background() // no deadline
	tx, err := e.s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	res, err := atFenceCompare(ctx, tx, w)
	if err != nil {
		t.Fatal(err)
	}
	if res.Defer || res.Reason != atFenceReasonNoDeadline {
		t.Fatalf("result = %+v, want no compare (%s)", res, atFenceReasonNoDeadline)
	}
	if reads != 0 {
		t.Fatalf("compare queries sent = %d, want 0", reads)
	}
}
