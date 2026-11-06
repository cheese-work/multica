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
