package broker

import (
	"context"
	"reflect"
	"testing"
)

// CHE-2512: the elevated run's read proxy gets a host-side read token.
func TestReadSourceMintsReadOnlyReusesAndRevokes(t *testing.T) {
	gh := newFakeGitHub(t, "")
	src := gh.tokens().NewReadSource(42)
	ctx := context.Background()

	a, err := src.Token(ctx)
	if err != nil || a == "" {
		t.Fatalf("Token: %q %v", a, err)
	}
	b, err := src.Token(ctx)
	if err != nil || b != a {
		t.Fatalf("second Token %q %v, want the same live token %q", b, err, a)
	}
	if len(gh.mints) != 1 || !reflect.DeepEqual(gh.mints[0].Permissions, readPerms) || !reflect.DeepEqual(gh.mints[0].RepositoryIDs, []int64{42}) {
		t.Fatalf("mints %+v, want one read-only token for repo 42", gh.mints)
	}
	if err := src.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if gh.liveCount() != 0 {
		t.Fatalf("%d tokens still live after Close", gh.liveCount())
	}
	if _, err := src.Token(ctx); err == nil {
		t.Fatal("Token after Close succeeded")
	}
}
