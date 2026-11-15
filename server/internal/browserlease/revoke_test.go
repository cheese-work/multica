package browserlease

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func openTestDB(t *testing.T) (*pgxpool.Pool, *db.Queries) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err == nil {
		err = pool.Ping(context.Background())
	}
	if err != nil {
		if ci := os.Getenv("CI"); ci == "true" || ci == "1" {
			t.Fatalf("database is required in CI: %v", err)
		}
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, db.New(pool)
}

func newID() pgtype.UUID { return pgtype.UUID{Bytes: uuid.New(), Valid: true} }

// world is one workspace with an instance, a profile and a lease factory. Soft references: no
// other rows are needed.
type world struct {
	t         *testing.T
	pool      *pgxpool.Pool
	q         *db.Queries
	workspace pgtype.UUID
	profile   pgtype.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool, q := openTestDB(t)
	w := &world{t: t, pool: pool, q: q, workspace: newID(), profile: newID()}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM browser_lease WHERE workspace_id = $1`, w.workspace)
	})
	return w
}

type leaseOpt func(*leaseSeed)

type leaseSeed struct {
	task, user, agent, profile pgtype.UUID
	state                      string
	instance                   pgtype.UUID
}

func withUser(id pgtype.UUID) leaseOpt    { return func(s *leaseSeed) { s.user = id } }
func withProfile(id pgtype.UUID) leaseOpt { return func(s *leaseSeed) { s.profile = id } }
func queued() leaseOpt                    { return func(s *leaseSeed) { s.state = "queued" } }
func ended() leaseOpt                     { return func(s *leaseSeed) { s.state = "ended" } }

// lease inserts a lease straight into the table (the grant path is W3-3). Each lease gets its
// own instance: one lease holds an instance at a time (index 99963).
func (w *world) lease(opts ...leaseOpt) db.BrowserLease {
	w.t.Helper()
	s := leaseSeed{task: newID(), user: newID(), agent: newID(), profile: w.profile, state: "active", instance: newID()}
	for _, o := range opts {
		o(&s)
	}
	var id pgtype.UUID
	var query string
	switch s.state {
	case "queued":
		query = `INSERT INTO browser_lease (workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'S0','queued', now() + interval '15 minutes') RETURNING id`
	case "ended":
		query = `INSERT INTO browser_lease (workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, granted_at, ended_at, end_reason, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'S0','ended', 1, now(), now(), 'release', now() + interval '15 minutes') RETURNING id`
	default:
		query = `INSERT INTO browser_lease (workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, granted_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'S0','active', 1, now(), now() + interval '15 minutes') RETURNING id`
	}
	if err := w.pool.QueryRow(context.Background(), query,
		w.workspace, s.instance, s.profile, s.agent, s.task, newID(), s.user).Scan(&id); err != nil {
		w.t.Fatalf("insert lease: %v", err)
	}
	l, err := w.q.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: id, WorkspaceID: w.workspace})
	if err != nil {
		w.t.Fatalf("read lease: %v", err)
	}
	return l
}

func (w *world) reload(l db.BrowserLease) (db.BrowserLease, bool) {
	w.t.Helper()
	got, err := w.q.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: l.ID, WorkspaceID: w.workspace})
	if err != nil {
		return db.BrowserLease{}, false
	}
	return got, true
}

// recorder collects the revoke events a test bus delivers.
type recorder struct {
	got []events.Event
}

func newRevoker(t *testing.T, w *world) (*Revoker, *recorder) {
	t.Helper()
	bus := events.New()
	rec := &recorder{}
	bus.Subscribe(protocol.EventBrowserLeaseRevoked, func(e events.Event) {
		rec.got = append(rec.got, e)
	})
	return &Revoker{Queries: w.q, Bus: bus}, rec
}

func TestRevokeTriggersEndLeaseAndEmitAtOnce(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		filter func(w *world, target db.BrowserLease) Filter
	}{
		{"release", ReasonRelease, func(w *world, l db.BrowserLease) Filter {
			return Filter{WorkspaceID: w.workspace, LeaseID: l.ID}
		}},
		{"task end", ReasonTaskEnd, func(w *world, l db.BrowserLease) Filter {
			return Filter{WorkspaceID: w.workspace, TaskID: l.TaskID}
		}},
		{"task cancelled", ReasonTaskCancelled, func(w *world, l db.BrowserLease) Filter {
			return Filter{WorkspaceID: w.workspace, TaskID: l.TaskID}
		}},
		{"permission removed", ReasonPermissionRemoved, func(w *world, l db.BrowserLease) Filter {
			return Filter{WorkspaceID: w.workspace, UserID: l.UserID, ProfileID: l.ProfileID}
		}},
		{"plugin disabled", ReasonPluginDisabled, func(w *world, l db.BrowserLease) Filter {
			return Filter{WorkspaceID: w.workspace}
		}},
		{"revoke all", ReasonRevokeAll, func(w *world, l db.BrowserLease) Filter {
			return Filter{WorkspaceID: w.workspace}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			r, rec := newRevoker(t, w)
			target := w.lease()

			before := time.Now()
			got, err := r.Revoke(context.Background(), tc.filter(w, target), tc.reason)
			if err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			if len(got) != 1 || got[0].ID != target.ID {
				t.Fatalf("revoked %d leases, want exactly the target", len(got))
			}
			row, _ := w.reload(target)
			if row.State != "ended" || row.EndReason.String != tc.reason || !row.EndedAt.Valid {
				t.Fatalf("row state=%q reason=%q ended=%v", row.State, row.EndReason.String, row.EndedAt.Valid)
			}
			if row.EndedAt.Time.Before(before.Add(-2 * time.Second)) {
				t.Fatalf("ended_at %v is not fresh", row.EndedAt.Time)
			}
			// The ended lease still holds the instance until cleanup-ack (W3-7).
			if _, err := w.q.GetBrowserInstanceHolder(context.Background(), db.GetBrowserInstanceHolderParams{InstanceID: target.InstanceID, WorkspaceID: w.workspace}); err != nil {
				t.Fatalf("instance holder after revoke: %v", err)
			}
			if len(rec.got) != 1 {
				t.Fatalf("published %d events, want 1", len(rec.got))
			}
			e := rec.got[0]
			p, _ := e.Payload.(map[string]any)
			if e.Type != protocol.EventBrowserLeaseRevoked || e.WorkspaceID != uuid.UUID(w.workspace.Bytes).String() ||
				p["lease_id"] != uuid.UUID(target.ID.Bytes).String() ||
				p["instance_id"] != uuid.UUID(target.InstanceID.Bytes).String() ||
				p["epoch"] != int64(1) || p["reason"] != tc.reason {
				t.Fatalf("event = %+v", e)
			}
		})
	}
}

func TestRevokeIsScopedToTheFilter(t *testing.T) {
	w := newWorld(t)
	r, rec := newRevoker(t, w)
	hit := w.lease()
	otherTask := w.lease()
	otherProfile := w.lease(withProfile(newID()), withUser(hit.UserID))

	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, UserID: hit.UserID, ProfileID: hit.ProfileID}, ReasonPermissionRemoved); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	for name, l := range map[string]db.BrowserLease{"other user": otherTask, "other profile": otherProfile} {
		if row, _ := w.reload(l); row.State != "active" {
			t.Errorf("%s lease state = %q, want active", name, row.State)
		}
	}
	if len(rec.got) != 1 {
		t.Fatalf("published %d events, want 1", len(rec.got))
	}

	// Another workspace is never touched, even by a workspace-wide revoke.
	foreign := newWorld(t)
	f := foreign.lease()
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace}, ReasonRevokeAll); err != nil {
		t.Fatalf("Revoke all: %v", err)
	}
	if row, _ := foreign.reload(f); row.State != "active" {
		t.Fatalf("foreign workspace lease state = %q, want active", row.State)
	}
}

func TestRevokeDropsQueuedLeaseAndEmitsWithoutEpoch(t *testing.T) {
	w := newWorld(t)
	r, rec := newRevoker(t, w)
	q := w.lease(queued())

	got, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, TaskID: q.TaskID}, ReasonTaskCancelled)
	if err != nil || len(got) != 1 {
		t.Fatalf("Revoke: %v (%d leases)", err, len(got))
	}
	if _, ok := w.reload(q); ok {
		t.Fatal("queued lease still in the queue")
	}
	p, _ := rec.got[0].Payload.(map[string]any)
	if len(rec.got) != 1 || p["epoch"] != int64(0) {
		t.Fatalf("events = %+v", rec.got)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	w := newWorld(t)
	r, rec := newRevoker(t, w)
	done := w.lease(ended())

	got, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, LeaseID: done.ID}, ReasonRelease)
	if err != nil || len(got) != 0 || len(rec.got) != 0 {
		t.Fatalf("second revoke: err=%v leases=%d events=%d, want a silent no-op", err, len(got), len(rec.got))
	}
	if row, _ := w.reload(done); row.EndReason.String != "release" {
		t.Fatalf("end_reason rewritten to %q", row.EndReason.String)
	}
}

func TestRevokeRequiresAWorkspace(t *testing.T) {
	w := newWorld(t)
	r, _ := newRevoker(t, w)
	l := w.lease()
	if _, err := r.Revoke(context.Background(), Filter{}, ReasonRevokeAll); err == nil {
		t.Fatal("Revoke without a workspace succeeded")
	}
	if row, _ := w.reload(l); row.State != "active" {
		t.Fatalf("lease state = %q after a rejected revoke", row.State)
	}
}

func TestRegisterRevokesOnTaskEnd(t *testing.T) {
	cases := []struct {
		event  string
		reason string
	}{
		{protocol.EventTaskCompleted, ReasonTaskEnd},
		{protocol.EventTaskFailed, ReasonTaskEnd},
		{protocol.EventTaskCancelled, ReasonTaskCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			w := newWorld(t)
			r, rec := newRevoker(t, w)
			r.Register()
			mine := w.lease()
			other := w.lease()

			r.Bus.Publish(events.Event{
				Type:        tc.event,
				WorkspaceID: uuid.UUID(w.workspace.Bytes).String(),
				TaskID:      uuid.UUID(mine.TaskID.Bytes).String(),
				Payload:     map[string]any{"task_id": uuid.UUID(mine.TaskID.Bytes).String()},
			})

			if row, _ := w.reload(mine); row.State != "ended" || row.EndReason.String != tc.reason {
				t.Fatalf("lease state=%q reason=%q, want ended %q", row.State, row.EndReason.String, tc.reason)
			}
			if row, _ := w.reload(other); row.State != "active" {
				t.Fatalf("another task's lease state = %q", row.State)
			}
			if len(rec.got) != 1 {
				t.Fatalf("published %d revoke events, want 1", len(rec.got))
			}
		})
	}
}
