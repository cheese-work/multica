package ciwatch

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CHE-4279 review round 3: the notify recipient is not enough. The watcher acts as
// that member only if the repo's authorization record names the same member.

func (s *stormEnv) setAuthorization(t *testing.T, member any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE ci_watch_config SET authorized_by_member_id=$3::uuid
		WHERE workspace_id=$1::uuid AND repo_owner='o' AND repo_name=$2`, s.ws, s.repo, member); err != nil {
		t.Fatal(err)
	}
}

func TestStormRecipientWithoutAuthorizationCreatesNoIssueAndTakesNoClaim(t *testing.T) {
	for name, member := range map[string]any{"no record": nil, "other member": uuid.NewString()} {
		t.Run(name, func(t *testing.T) {
			s := newStormEnv(t)
			s.setAuthorization(t, member)
			s.admit(t, 901, "unauth", "P2")
			s.drain(t, true)
			if got := s.count(t, "issue"); got != 0 {
				t.Fatalf("issues for an unauthorized recipient = %d, want 0", got)
			}
			if got := s.claims.claims.Load(); got != 0 {
				t.Fatalf("red-main claims taken = %d, want 0 (the principal check runs first)", got)
			}
			if r := s.reasonOf(t, ActionIssueCreate, StateFailed); r != ReasonNoPrincipal {
				t.Fatalf("reason = %q, want %q", r, ReasonNoPrincipal)
			}
		})
	}
}

func TestStormAuthorizedRecipientCreatesTheIssueAsThatMember(t *testing.T) {
	s := newStormEnv(t)
	var notify string
	if err := s.pool.QueryRow(context.Background(), `SELECT notify_member_id::text FROM ci_watch_config
		WHERE workspace_id=$1::uuid AND repo_name=$2`, s.ws, s.repo).Scan(&notify); err != nil {
		t.Fatal(err)
	}
	s.admit(t, 911, "authd", "P2")
	s.drain(t, false)
	if len(s.d.specs) != 1 || s.d.specs[0].CreatorID != notify {
		t.Fatalf("specs = %+v, want one issue created as %s", s.d.specs, notify)
	}
}

func TestStormDeauthorizedRecipientPostsNoDigestComment(t *testing.T) {
	s := newStormEnv(t)
	s.admit(t, 921, "digZ", "P2")
	s.drain(t, false)
	s.setAuthorization(t, nil)
	s.clock.Advance(DigestEvery + time.Hour)
	s.admit(t, 922, "digZ", "P2")
	s.drain(t, true)
	if got := s.count(t, "comment"); got != 0 {
		t.Fatalf("digest comments for an unauthorized recipient = %d, want 0", got)
	}
}

// CHE-4279 review round 4: the principal must still be a workspace member at write
// time. A matching authorization record is not enough once the member is removed.

func (s *stormEnv) removeNotifyMember(t *testing.T) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id=$1::uuid
		AND user_id=(SELECT notify_member_id FROM ci_watch_config WHERE workspace_id=$1::uuid AND repo_name=$2)`, s.ws, s.repo); err != nil {
		t.Fatal(err)
	}
}

func TestStormRemovedMemberCreatesNoIssueAndTakesNoClaim(t *testing.T) {
	s := newStormEnv(t)
	s.removeNotifyMember(t)
	s.admit(t, 931, "gone", "P2")
	s.drain(t, true)
	if got := s.count(t, "issue"); got != 0 {
		t.Fatalf("issues for a removed member = %d, want 0", got)
	}
	if got := s.claims.claims.Load(); got != 0 {
		t.Fatalf("red-main claims taken = %d, want 0", got)
	}
	if r := s.reasonOf(t, ActionIssueCreate, StateFailed); r != ReasonPrincipalRemoved {
		t.Fatalf("reason = %q, want %q", r, ReasonPrincipalRemoved)
	}
}

func TestStormRemovedMemberPostsNoDigestComment(t *testing.T) {
	s := newStormEnv(t)
	s.admit(t, 941, "goneDigest", "P2")
	s.drain(t, false)
	s.removeNotifyMember(t)
	s.clock.Advance(DigestEvery + time.Hour)
	s.admit(t, 942, "goneDigest", "P2")
	s.drain(t, true)
	if got := s.count(t, "comment"); got != 0 {
		t.Fatalf("digest comments for a removed member = %d, want 0", got)
	}
}

func TestStormRemovedMemberGetsNoNotification(t *testing.T) {
	s := newStormEnv(t)
	s.removeNotifyMember(t)
	s.admit(t, 951, "goneNotify", "P4")
	s.drain(t, true)
	if len(s.d.notifies) != 0 {
		t.Fatalf("notifications to a removed member = %d, want 0", len(s.d.notifies))
	}
}

// A removal that lands after actor() but before the write must wait for the write:
// the guard share-locks the member row for the rest of the transaction.
func TestStormMemberRemovalWaitsForAnInFlightWrite(t *testing.T) {
	s := newStormEnv(t)
	ctx := context.Background()
	var member string
	if err := s.pool.QueryRow(ctx, `SELECT notify_member_id::text FROM ci_watch_config
		WHERE workspace_id=$1::uuid AND repo_name=$2`, s.ws, s.repo).Scan(&member); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := requireMember(ctx, tx, s.ws, member); err != nil {
		t.Fatal(err)
	}
	other, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx)
	if _, err := other.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `DELETE FROM member WHERE workspace_id=$1::uuid AND user_id=$2::uuid`, s.ws, member); err == nil {
		t.Fatal("member removal went through while a write held the membership check")
	}
}
