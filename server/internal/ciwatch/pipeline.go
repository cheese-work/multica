package ciwatch

import "context"

// Pipeline is the event path for one completed workflow run: collect evidence,
// store the failure event, record the System 1 observation.
type Pipeline struct {
	Collector *Collector
	Events    EventWriter
	Observer  Observer
}

// Process handles one delivery. Both writes are idempotent, so a failure at any
// step is returned and the caller redelivers the whole run: a stored event is
// not stored twice, and an observation that failed after the store is written
// on the retry (it is never gated on the store being a first insert).
func (p Pipeline) Process(ctx context.Context, in Input, h History) (Result, error) {
	res, err := p.Collector.Handle(ctx, in)
	if err != nil {
		return res, err
	}
	if res.Kind == KindEvidence {
		if _, err := Store(ctx, p.Events, res.Event); err != nil {
			return res, err
		}
	}
	return res, p.Observer.Observe(ctx, in.Run, res, h)
}
