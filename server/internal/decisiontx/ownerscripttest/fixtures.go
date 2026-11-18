// Package ownerscripttest holds the shared fixtures F-1 to F-8 of the
// owner-script guard (CHE-7138 S7a). S7b to S7d reuse them, so a rule change is
// one edit here. It imports nothing from the guard, so any package may use it.
package ownerscripttest

// Fixture is one control from the plan's "Controls" table.
type Fixture struct {
	ID string
	// Surface is where the plan sends the message: "ask" (decision, action or
	// requester ask), "comment" or "dm". S7a runs every fixture through the
	// extractor and the "ask" ones through the ask handlers.
	Surface string
	// Text is the markdown body, as a person would read it.
	Text string
	// Related is the R8 context: the message links to, or follows within 30
	// minutes from the same author, a message with R1 to R7 features.
	Related bool
	// Hits are the rule ids the extractor must report, in order.
	Hits []string
	// BlockRefuses is true when block mode refuses (or holds) the message.
	BlockRefuses bool
	// WarnFails is true when the plan's S11 calculation over this one delivered
	// warn event gives FAIL.
	WarnFails bool
	// ResendOf names the fixture whose refused command this one re-sends (F-2).
	ResendOf string
}

const command = "sudo systemctl restart x"

// All is F-1 to F-8 in plan order.
var All = []Fixture{
	{ID: "F-1", Surface: "ask", Text: "Run this on the host:\n```text\n" + command + "\n```",
		Hits: []string{"R6", "R7"}, BlockRefuses: true, WarnFails: true},
	{ID: "F-2", Surface: "ask", Text: "Again, the same one:\n```text\n" + command + "\n```",
		Hits: []string{"R6"}, BlockRefuses: true, WarnFails: true, ResendOf: "F-1"},
	{ID: "F-3", Surface: "ask", Text: "```text\n" + command + "\n```",
		Hits: []string{"R6"}, BlockRefuses: true, WarnFails: true},
	{ID: "F-4", Surface: "ask", Text: "The service log shows:\n```log\n2026-11-18 10:00:01 error: connection refused\n2026-11-18 10:00:02 retrying in 5s\n```"},
	{ID: "F-5", Surface: "ask", Text: "Details are in comment [the thread](https://multica.example/issues/CHE-1#comment-0197d3a2-0000-7000-8000-000000000001)."},
	{ID: "F-6", Surface: "dm", Text: "```text\n" + command + "\n```",
		Hits: []string{"R6"}, BlockRefuses: true, WarnFails: true},
	{ID: "F-7", Surface: "ask", Text: "please run " + command,
		Hits: []string{"R7"}, BlockRefuses: true, WarnFails: true},
	{ID: "F-8", Surface: "comment", Text: "please run the commands in [the earlier comment](https://multica.example/issues/CHE-1#comment-0197d3a2-0000-7000-8000-000000000001)",
		Related: true, Hits: []string{"R8"}, BlockRefuses: true, WarnFails: true},
}

// ByID returns the fixture with the id, or panics: a typo in a test is a bug.
func ByID(id string) Fixture {
	for _, fixture := range All {
		if fixture.ID == id {
			return fixture
		}
	}
	panic("ownerscripttest: no fixture " + id)
}

// Command is the one command the F-1, F-2, F-3, F-6 and F-7 bodies share, so a
// test can compare their keyed hashes.
const Command = command
