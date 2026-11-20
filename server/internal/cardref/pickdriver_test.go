package cardref

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeDoop struct {
	calls  []string
	err    error
	tamper bool
}

// PinForPick answers the R2 doop's K2 would: idempotent on the pick request.
func (f *fakeDoop) PinForPick(_ context.Context, card, pick, frame string) (Receipt, error) {
	f.calls = append(f.calls, card)
	if f.err != nil {
		return Receipt{}, f.err
	}
	r := r2(card, epochs, pick, frame)
	if f.tamper {
		r.MAC = r.ComputeMAC("not-the-secret")
	}
	return r, nil
}

func openCard(t *testing.T, p *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	must(Create(ctx, p, id))
	if err := RecordEpochs(ctx, p, id, epochs); err != nil {
		t.Fatal(err)
	}
	must(OpenCard(ctx, p, key, id, r1(id, epochs)))
}

// pick_driver (spec section 5): a pick_pending card is finished by asking doop for R2, then K4.
func testPickDriver(t *testing.T, p *pgxpool.Pool) {
	ctx := context.Background()
	// rows the other tests seeded by hand wait on a pick with no epochs; they are not this test's cards
	if _, err := p.Exec(ctx, `DELETE FROM card_ref WHERE status = 'pick_pending'`); err != nil {
		t.Fatal(err)
	}

	run(t, "pick_pending becomes decided, and a second pass changes nothing", func(t *testing.T) {
		openCard(t, p, "pd-1")
		must(Pick(ctx, p, "pd-1", "pick-1", "f-a"))
		d := &fakeDoop{}
		expect(t, "first pass", must(Drive(ctx, p, key, d, "pd-1")), Decided)
		expect(t, "second pass", must(Drive(ctx, p, key, d, "pd-1")), Decided)
		if len(d.calls) != 1 {
			t.Fatalf("doop asked %d times; a decided card needs no R2", len(d.calls))
		}
	})
	run(t, "cards that wait on nothing are left alone", func(t *testing.T) {
		openCard(t, p, "pd-open")
		must(Create(ctx, p, "pd-pending"))
		d := &fakeDoop{}
		for id, want := range map[string]Status{"pd-open": Open, "pd-pending": Pending, "pd-none": Missing} {
			expect(t, id, must(Drive(ctx, p, key, d, id)), want)
		}
		if len(d.calls) != 0 {
			t.Fatalf("doop was asked %v", d.calls)
		}
	})
	run(t, "doop down: the card stays pick_pending and a later pass finishes it", func(t *testing.T) {
		openCard(t, p, "pd-down")
		must(Pick(ctx, p, "pd-down", "pick-1", "f-a"))
		down := &fakeDoop{err: errors.New("doop down")}
		if _, err := Drive(ctx, p, key, down, "pd-down"); err == nil {
			t.Fatal("want the doop error")
		}
		expect(t, "after failure", statusOf(t, p, "pd-down"), PickPending)
		expect(t, "after recovery", must(Drive(ctx, p, key, &fakeDoop{}, "pd-down")), Decided)
	})
	run(t, "a receipt doop did not sign is refused", func(t *testing.T) {
		openCard(t, p, "pd-forged")
		must(Pick(ctx, p, "pd-forged", "pick-1", "f-a"))
		if _, err := Drive(ctx, p, key, &fakeDoop{tamper: true}, "pd-forged"); !errors.Is(err, ErrBadReceipt) {
			t.Fatalf("err %v, want ErrBadReceipt", err)
		}
		expect(t, "after refusal", statusOf(t, p, "pd-forged"), PickPending)
	})
	run(t, "DriveAll goes on past a failed card", func(t *testing.T) {
		for _, id := range []string{"pda-1", "pda-2", "pda-3"} {
			openCard(t, p, id)
			must(Pick(ctx, p, id, "pick-"+id, "f-a"))
		}
		d := &perCardDoop{fail: "pda-2"}
		out, err := DriveAll(ctx, p, key, d, 100)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]DriveResult{}
		for _, r := range out {
			got[r.CardRequestID] = r
		}
		if got["pda-1"].Status != Decided || got["pda-3"].Status != Decided || got["pda-2"].Err == nil {
			t.Fatalf("results %+v", got)
		}
		expect(t, "failed card", statusOf(t, p, "pda-2"), PickPending)
		out, err = DriveAll(ctx, p, key, &fakeDoop{}, 1)
		if err != nil || len(out) != 1 || out[0].Status != Decided {
			t.Fatalf("limit 1 pass: %+v %v", out, err)
		}
	})
}

type perCardDoop struct{ fail string }

func (f *perCardDoop) PinForPick(ctx context.Context, card, pick, frame string) (Receipt, error) {
	if card == f.fail {
		return Receipt{}, errors.New("doop down")
	}
	return (&fakeDoop{}).PinForPick(ctx, card, pick, frame)
}
