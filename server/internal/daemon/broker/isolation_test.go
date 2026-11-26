package broker

import (
	"context"
	"slices"
	"testing"
)

// X1 (CHE-10369, plan RSC-1 R4): one run cannot get, refresh or use another
// run's token through its own broker. Two brokers share one fake GitHub; the
// repository IDs differ so a mint can be traced to its broker.
func TestRunBBrokerRefusesRunAToken(t *testing.T) {
	t.Parallel()
	ra := newRig(t)
	f := newFakeGitHub(t, ra.remote)
	ra.pin.RemoteURL, ra.pin.Slug, ra.pin.Bases, ra.pin.Tokens = f.srv.URL+"/git/remote.git", "o/r", []string{"main"}, f.tokens()
	rb := newRig(t)
	rb.pin.RemoteURL, rb.pin.Slug, rb.pin.Bases, rb.pin.Tokens = f.srv.URL+"/git/remote.git", "o/r", []string{"main"}, f.tokens()
	rb.pin.Repo = "43"
	a, b := ra.start(t), rb.start(t)
	ctx := context.Background()

	tokA, err := GetReadToken(ctx, ra.socket)
	if err != nil {
		t.Fatal(err)
	}

	// B holds nothing: given A's token it mints for B's repository only.
	tokB, err := RefreshReadToken(ctx, rb.socket, tokA.Token)
	if err != nil {
		t.Fatalf("B refresh given A's token: %v", err)
	}
	if tokB.Token == tokA.Token {
		t.Fatal("B's refresh returned A's token")
	}
	if len(f.mints) != 2 || f.mints[0].RepositoryIDs[0] != 42 || f.mints[1].RepositoryIDs[0] != 43 {
		t.Fatalf("mints %+v: want A's repo 42 then B's repo 43 only", f.mints)
	}
	if !f.isLive(tokA.Token) || !f.isLive(tokB.Token) {
		t.Fatal("B's refresh touched A's token, or B's own token is not live")
	}

	// B now holds a token: A's ID names none of B's, so B changes nothing and
	// hands back its own newest token.
	again, err := RefreshReadToken(ctx, rb.socket, tokA.Token)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != tokB.Token || len(f.mints) != 2 {
		t.Fatalf("B returned its own token: %v, with %d mints; want its own token and no new mint", again.Token == tokB.Token, len(f.mints))
	}
	if !f.isLive(tokA.Token) || !f.isLive(tokB.Token) || len(f.revoked) != 0 {
		t.Fatalf("revoked %v: nothing may be revoked by a foreign ID", f.revoked)
	}

	// Run A ends: only A's token dies, B's keeps working.
	a.Close()
	if f.isLive(tokA.Token) || !f.isLive(tokB.Token) {
		t.Fatal("closing A must revoke A's token and leave B's alone")
	}
	b.Close()
	if f.liveCount() != 0 || !slices.Equal(f.revoked, []string{tokA.Token, tokB.Token}) {
		t.Fatalf("revoked %d tokens, live %d", len(f.revoked), f.liveCount())
	}
}
