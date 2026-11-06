package ciwatch

import "context"

// Writers are the two writes of one run, valid inside one InTx call.
type Writers struct {
	Events       EventWriter
	Observations ObservationWriter
}

// InTx runs fn in one database transaction: every write fn makes commits, or
// none does.
type InTx func(ctx context.Context, fn func(Writers) error) error

// Pipeline is the event path for one completed workflow run: collect evidence,
// then store the failure event and the System 1 observation together.
type Pipeline struct {
	Collector *Collector
	Tx        InTx
	Observer  Observer
}

// Process handles one delivery. The event and the observation commit in one
// transaction, so a failure leaves neither behind and the retry makes its own
// first decision from fresh state; a stored event and its observation can
// never come from two different collections. A redelivery of a committed run
// recollects and then writes nothing (both inserts are idempotent).
func (p Pipeline) Process(ctx context.Context, in Input, h History) (Result, error) {
	res, err := p.Collector.Handle(ctx, in)
	if err != nil {
		return res, err
	}
	obs, err := p.Observer.Build(ctx, in.Run, res, h)
	if err != nil {
		return res, err
	}
	if obs == nil && res.Kind != KindEvidence {
		return res, nil
	}
	return res, p.Tx(ctx, func(w Writers) error {
		if res.Kind == KindEvidence {
			if _, err := Store(ctx, w.Events, res.Event); err != nil {
				return err
			}
		}
		if obs == nil {
			return nil
		}
		_, err := w.Observations.CreateSystem1CIObservation(ctx, *obs)
		return err
	})
}
