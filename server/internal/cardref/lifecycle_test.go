package cardref

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var epochs = map[string]int64{"f-a": 1, "f-b": 2}

func r1(id string, e map[string]int64) Receipt {
	r := Receipt{Kind: "R1", CardRequestID: id, PinID: "pin-" + id, State: "HELD", Epochs: e}
	r.MAC = r.ComputeMAC(key)
	return r
}

func r2(id string, e map[string]int64, pick, frame string) Receipt {
	r := Receipt{Kind: "R2", CardRequestID: id, PinID: "pin-" + id, State: "SEALED", Epochs: e, PickRequestID: pick, PickedFrameID: frame}
	r.MAC = r.ComputeMAC(key)
	return r
}

// must returns the value of a call that cannot fail in the test, or fails the test.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func expect(t *testing.T, what string, got, want Status) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %s, want %s", what, got, want)
	}
}

// P3 -> P5 -> P7 -> K1 -> K4, each step retried: a retry answers the same thing.
func testLifecycle(t *testing.T, p *pgxpool.Pool) {
	ctx, id := context.Background(), "life-1"
	for i := 0; i < 2; i++ {
		expect(t, "P3", must(Create(ctx, p, id)), Pending)
	}
	for i := 0; i < 2; i++ {
		if err := RecordEpochs(ctx, p, id, epochs); err != nil {
			t.Fatalf("P5 try %d: %v", i, err)
		}
	}
	if err := RecordEpochs(ctx, p, id, map[string]int64{"f-a": 7}); !errors.Is(err, ErrNotPending) && !errors.Is(err, ErrWrongEpoch) {
		t.Fatalf("P5 with other epochs: %v", err)
	}
	for i := 0; i < 2; i++ {
		expect(t, "P7", must(OpenCard(ctx, p, key, id, r1(id, epochs))), Open)
	}
	if err := RecordEpochs(ctx, p, id, epochs); !errors.Is(err, ErrNotPending) {
		t.Fatalf("P5 after open: %v", err)
	}
	for i := 0; i < 2; i++ {
		expect(t, "K1", must(Pick(ctx, p, id, "pick-1", "f-a")), PickPending)
	}
	if _, err := Pick(ctx, p, id, "pick-2", "f-b"); !errors.Is(err, ErrPickConflict) {
		t.Fatalf("K1 with another pick: %v", err)
	}
	for i := 0; i < 2; i++ {
		expect(t, "K4", must(Decide(ctx, p, key, id, r2(id, epochs, "pick-1", "f-a"))), Decided)
	}
	expect(t, "ref_status", must(RefStatus(ctx, p, id)), Decided)
	// decided is final: nothing moves it
	expect(t, "C1 after decided", must(VoidCard(ctx, p, id, "late")), Decided)
	expect(t, "A1 after decided", must(AbortRef(ctx, p, id, "late")), Decided)
	expect(t, "P3 after decided", must(Create(ctx, p, id)), Decided)

	// C1 voids a pending card; X1: the same id then answers void
	expect(t, "P3", must(Create(ctx, p, "life-2")), Pending)
	expect(t, "C1", must(VoidCard(ctx, p, "life-2", "expired")), Void)
	expect(t, "P3 after void", must(Create(ctx, p, "life-2")), Void)
	if _, err := OpenCard(ctx, p, key, "life-2", r1("life-2", epochs)); !errors.Is(err, ErrNotPending) {
		t.Fatalf("P7 on a void card: %v", err)
	}
	// steps on a card nobody created
	if _, err := OpenCard(ctx, p, key, "life-none", r1("life-none", epochs)); !errors.Is(err, ErrNoCard) {
		t.Fatalf("P7 on no row: %v", err)
	}
	if _, err := Pick(ctx, p, "life-none", "k", "f"); !errors.Is(err, ErrNoCard) {
		t.Fatalf("K1 on no row: %v", err)
	}
	// K1 needs `open`, K4 needs `pick_pending`
	must(Create(ctx, p, "life-3"))
	if _, err := Pick(ctx, p, "life-3", "k", "f-a"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("K1 on pending: %v", err)
	}
	if _, err := Decide(ctx, p, key, "life-3", r2("life-3", epochs, "k", "f-a")); !errors.Is(err, ErrNotPickPending) {
		t.Fatalf("K4 on pending: %v", err)
	}
}

// B2 and B3: Multica opens and decides only on a receipt doop signed for this card, these epochs, this pick.
func testReceiptChecks(t *testing.T, p *pgxpool.Pool) {
	ctx := context.Background()
	forge := func(r Receipt) Receipt { r.MAC = r.ComputeMAC("not-the-secret"); return r }
	open := map[string]Receipt{
		"forged-r1":   forge(r1("rc-open", epochs)),
		"r2-as-r1":    r2("rc-open", epochs, "k", "f-a"),
		"other-card":  r1("rc-other", epochs),
		"unsigned":    {Kind: "R1", CardRequestID: "rc-open", State: "HELD", Epochs: epochs},
		"wrong-state": func() Receipt { r := r1("rc-open", epochs); r.State = "SEALED"; r.MAC = r.ComputeMAC(key); return r }(),
	}
	must(Create(ctx, p, "rc-open"))
	for name, r := range open {
		if _, err := OpenCard(ctx, p, key, "rc-open", r); !errors.Is(err, ErrBadReceipt) {
			t.Errorf("%s: err %v, want ErrBadReceipt", name, err)
		}
	}
	// no epochs recorded yet, then different epochs: both are wrong-epoch
	if _, err := OpenCard(ctx, p, key, "rc-open", r1("rc-open", epochs)); !errors.Is(err, ErrWrongEpoch) {
		t.Errorf("open before P5: %v", err)
	}
	if err := RecordEpochs(ctx, p, "rc-open", epochs); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCard(ctx, p, key, "rc-open", r1("rc-open", map[string]int64{"f-a": 1, "f-b": 3})); !errors.Is(err, ErrWrongEpoch) {
		t.Errorf("wrong epoch: %v", err)
	}
	if _, err := OpenCard(ctx, p, "", "rc-open", r1("rc-open", epochs)); !errors.Is(err, ErrBadReceipt) {
		t.Errorf("empty secret: %v", err)
	}
	expect(t, "after every refusal", statusOf(t, p, "rc-open"), Pending)

	expect(t, "P7", must(OpenCard(ctx, p, key, "rc-open", r1("rc-open", epochs))), Open)
	must(Pick(ctx, p, "rc-open", "pick-1", "f-a"))
	decide := map[string]Receipt{
		"forged-r2":   forge(r2("rc-open", epochs, "pick-1", "f-a")),
		"r1-as-r2":    r1("rc-open", epochs),
		"other-pick":  r2("rc-open", epochs, "pick-2", "f-a"),
		"other-frame": r2("rc-open", epochs, "pick-1", "f-b"),
		"wrong-epoch": r2("rc-open", map[string]int64{"f-a": 1, "f-b": 9}, "pick-1", "f-a"),
	}
	for name, r := range decide {
		_, err := Decide(ctx, p, key, "rc-open", r)
		want := ErrBadReceipt
		if name == "wrong-epoch" {
			want = ErrWrongEpoch
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: err %v, want %v", name, err, want)
		}
	}
	expect(t, "after every refusal", statusOf(t, p, "rc-open"), PickPending)
	expect(t, "K4", must(Decide(ctx, p, key, "rc-open", r2("rc-open", epochs, "pick-1", "f-a"))), Decided)
}

// A2 against P7 at the same instant: exactly one wins and the answers agree with the row.
// `open` must never be voided by reconcile (spec section 6, reconcile-vs-open).
func testAbortIfPendingRace(t *testing.T, p *pgxpool.Pool) {
	ctx := context.Background()
	opened, aborted := 0, 0
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("race-%d", i)
		must(Create(ctx, p, id))
		if err := RecordEpochs(ctx, p, id, epochs); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var openErr, abortErr error
		var abortStatus Status
		var notAborted bool
		wg.Add(2)
		go func() { defer wg.Done(); _, openErr = OpenCard(ctx, p, key, id, r1(id, epochs)) }()
		go func() { defer wg.Done(); abortStatus, notAborted, abortErr = AbortIfPending(ctx, p, id) }()
		wg.Wait()
		if abortErr != nil {
			t.Fatal(abortErr)
		}
		switch got := statusOf(t, p, id); got {
		case Open:
			opened++
			if openErr != nil || !notAborted || abortStatus != Open {
				t.Fatalf("%s: open won but open err=%v notAborted=%v abort answered %s", id, openErr, notAborted, abortStatus)
			}
		case Void:
			aborted++
			if !errors.Is(openErr, ErrNotPending) || notAborted || abortStatus != Void {
				t.Fatalf("%s: abort won but open err=%v notAborted=%v abort answered %s", id, openErr, notAborted, abortStatus)
			}
		default:
			t.Fatalf("%s: ended %s", id, got)
		}
	}
	t.Logf("open won %d, abort won %d", opened, aborted)
}
