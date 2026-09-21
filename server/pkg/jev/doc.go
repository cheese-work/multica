// Package jev is a Go client for the TypeSafe System One evaluation
// endpoint (POST https://api.typesafe.ai/v1/systemone).
//
// TypeSafe publishes Python and JavaScript SDKs but no Go SDK, so this
// package owns its own transport, retry policy, and validation. Like
// [github.com/multica-ai/multica/server/pkg/composio] it is self-contained:
// the only third-party dependency is [github.com/go-resty/resty/v2] and it
// imports no Multica package, so it can be extracted unchanged.
//
// # What the model is
//
// Jev is a non-generative typed decision model. It takes a state and a map
// of typed questions and returns one typed answer per question. It does not
// emit free text. One request carries many questions: the state is ingested
// and billed once, and the questions are evaluated against it in parallel.
//
// Three question types, mirrored by three answer types:
//
//   - Noul   — yes/no, answered as a probability in 0..1. Carries NO
//     confidence field; see [Answer.AsNoul].
//   - Choice — one winner from options you define, plus the full
//     probability distribution and a confidence.
//   - Score  — a probability-weighted number across ordered levels, plus a
//     legend, the distribution, and a confidence.
//
// # Limits this package enforces or assumes
//
// Published limits for jev-1.13.0 as of 2026-09:
//
//   - Context: 64k tokens total per request; at most 32k for the state plus
//     the longest single question. [StateBuilder] budgets against that.
//   - Rate: 250,000 tokens/sec and 1,200 requests/min. Either one over the
//     line returns 429.
//   - Price: input is billed, output is free.
//   - Text only, English primary.
//
// # Known jaggedness
//
// The vendor documents where jev-1.13 is unreliable, and callers must design
// around it rather than around what the model ideally would do:
//
//   - It reads literally. Ask narrow, explicit, atomic questions.
//   - It does not count reliably and is not a calculator.
//   - It reads dates as text, not as ordered quantities.
//   - Accuracy degrades as the state grows ("context rot"), which is the
//     reason [StateBuilder] drops sections instead of letting a state grow
//     unbounded.
//   - It does NOT treat state content as hostile by default. Instructions
//     embedded in the state can move the answer, so never put untrusted
//     text in the state without an upstream provenance decision.
//   - Structural invariants are not guaranteed: a question and its negation
//     do not reliably sum to 1, and a Noul and an equivalent Choice can
//     disagree. Do not build logic that assumes either.
//
// Calibration is measured across groups of predictions. It does not
// guarantee any individual answer is correct — which is why callers gate on
// confidence and escalate below a threshold rather than trusting one answer.
//
// # Design rule
//
// Build a normal software workflow and insert System One only where a
// judgment is genuinely needed. If a question can be answered by a query, it
// must be a query.
package jev
