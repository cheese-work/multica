package handler

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4447: a compaction must carry every recorded decision of the DM into the
// next session, whatever the agent's own summary kept.

type recordedDecision struct{ key, words, answerID string }

// recordDecision stores one answered, standing decision asked in sessionID and
// sourced from the same DM. A card answer is shaped as the card handler writes
// it: a card source, the DM row it made, the option plus note as the snapshot.
func recordDecision(t *testing.T, agentID, sessionID, title, words string, card bool) recordedDecision {
	t.Helper()
	return recordDecisionFrom(t, agentID, sessionID, sessionID, title, words, "", card)
}

// recordDecisionFrom lets the request lane and the source DM differ, as an
// agent record quoting another DM's message does (TestAskedDecisionRecordFromAnotherDM).
// note is a card's free text beside the option; words is the whole snapshot.
func recordDecisionFrom(t *testing.T, agentID, requestSession, sourceSession, title, words, note string, card bool) recordedDecision {
	t.Helper()
	issueID := dbfx.Issue(t, title)
	var key string
	dbfx.QueryRow(t, `SELECT workspace.issue_prefix || '-' || issue.number::text FROM issue JOIN workspace ON workspace.id = issue.workspace_id WHERE issue.id = $1`, issueID).Scan(&key)
	requestID := dbfx.Insert(t, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": requestSession, "escalation_class": "reversible", "origin": "asked", "status": "answered"})
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_request WHERE id = $1`, requestID) })
	messageID := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceSession, "role": "user", "content": words})
	cols := testutil.Cols{"decision_request_id": requestID, "source_chat_session_id": sourceSession, "chat_message_ids": "{" + messageID + "}", "content_snapshot": words, "effective_at": time.Now()}
	if card {
		cols["answered_by_user_id"], cols["answer_source"], cols["reply_text"] = testUserID, "card", note
	} else {
		cols["answered_by_task_id"] = dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "chat_session_id": sourceSession, "runtime_id": handlerTestRuntimeID(t)})
	}
	return recordedDecision{key: key, words: words, answerID: dbfx.Insert(t, "decision_answer", cols)}
}

func requireDecisions(t *testing.T, where, text string, want ...recordedDecision) {
	t.Helper()
	if !strings.Contains(text, "Decisions in this DM") {
		t.Fatalf("%s lacks the decisions block:\n%s", where, text)
	}
	for _, decision := range want {
		for _, part := range []string{decision.key, strconv.Quote(decision.words), decision.answerID, "ICT"} {
			if !strings.Contains(text, part) {
				t.Fatalf("%s lacks %q of decision %s:\n%s", where, part, decision.key, text)
			}
		}
	}
}

// requireDecisionRefs checks that each decision is still named (key, time, record
// id) even when its words were collapsed or cut to fit.
func requireDecisionRefs(t *testing.T, where, text string, want ...recordedDecision) {
	t.Helper()
	for _, decision := range want {
		for _, part := range []string{decision.key, decision.answerID} {
			if !strings.Contains(text, part) {
				t.Fatalf("%s no longer names %q of decision %s", where, part, decision.key)
			}
		}
	}
}

func TestCompactionCarriesEveryRecordedDecision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	first := recordDecision(t, agentID, sessionID, "decision one", "Ship the reversible default.", false)
	second := recordDecision(t, agentID, sessionID, "decision two", "Use option B for the retry cap.", false)
	third := recordDecision(t, agentID, sessionID, "decision three", "Approve the card as asked.", true)
	// Not this DM's: another DM's record, and a withdrawn one.
	otherSession := dbfx.ChatSession(t, agentID)
	other := recordDecision(t, agentID, otherSession, "other dm decision", "Other DM words must not leak.", false)
	withdrawn := recordDecision(t, agentID, sessionID, "withdrawn decision", "Withdrawn words must not appear.", false)
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, withdrawn.answerID)

	// The agent's summary keeps none of them.
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Goal: refactor. Nothing else.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	requireDecisions(t, "folded summary row", summary.Content, first, second, third)
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, "next session prompt", seed, first, second, third)
	for _, leaked := range []string{other.words, withdrawn.words} {
		if strings.Contains(seed, leaked) || strings.Contains(summary.Content, leaked) {
			t.Fatalf("block contains %q, which is not a standing decision of this DM", leaked)
		}
	}
	if strings.Index(seed, third.words) > strings.Index(seed, first.words) {
		t.Fatalf("decisions are not newest first:\n%s", seed)
	}

	// A later compaction keeps the older decisions and adds the new one.
	fourth := recordDecision(t, agentID, sessionID, "decision four", "Keep the DM block under 8 KB.", true)
	session, summary, fail = testHandler.compactChatInPlace(ctx, session.ID, pgtype.UUID{}, "Second summary, also silent.")
	if fail != nil {
		t.Fatalf("second compact: %s", fail.message)
	}
	seed, err = testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, "second prompt", seed, first, second, third, fourth)
	requireDecisions(t, "second folded row", summary.Content, first, second, third, fourth)
}

// The block never drops a decision: not at 51 records, not for one record
// larger than the old 8 KiB cap or the 64 KiB seed cap, and not across a repeated compaction.
func TestCompactionKeepsDecisionsPastTheOldCaps(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-bounds", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	var want []recordedDecision
	for index := 0; index < 51; index++ {
		want = append(want, recordDecision(t, agentID, sessionID, fmt.Sprintf("bounds %d", index), fmt.Sprintf("Decision number %d stands.", index), index%2 == 0))
	}
	big := recordDecision(t, agentID, sessionID, "bounds big", "Big decision: "+strings.Repeat("keep every word. ", 4500), false)
	for round := 1; round <= 2; round++ {
		session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Summary that keeps nothing.")
		if fail != nil {
			t.Fatalf("compact round %d: %s", round, fail.message)
		}
		recent := fmt.Sprintf("recent message of round %d", round)
		dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": uuidToString(session.ID), "role": "user", "content": recent})
		seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		// A decisions block past the seed cap must not push out the rest of the
		// compaction context: the agent's summary and the recent messages stay.
		for _, kept := range []string{"Summary that keeps nothing.", recent} {
			if !strings.Contains(seed, kept) {
				t.Fatalf("prompt round %d lost %q to the decisions block (prompt is %d bytes)", round, kept, len(seed))
			}
		}
		for where, text := range map[string]string{"folded row": summary.Content, "prompt": seed} {
			requireDecisions(t, fmt.Sprintf("%s round %d", where, round), text, want...)
			requireDecisionRefs(t, fmt.Sprintf("%s round %d", where, round), text, big)
			// An oversize record is cut visibly, never dropped silently.
			if !strings.Contains(text, "words cut") || strings.Contains(text, strconv.Quote(big.words)) {
				t.Fatalf("%s round %d does not mark the cut of the oversize record", where, round)
			}
		}
		sessionID = uuidToString(session.ID)
	}
}

// A record belongs to the DM its source message lives in, not to the DM that
// asked: DM A's block lists A-sourced answers only, DM B's block B-sourced ones.
func TestCompactionDecisionsFollowTheSourceDM(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-source", []byte("[]"))
	dmA, dmB := dbfx.ChatSession(t, agentID), dbfx.ChatSession(t, agentID)
	inA := recordDecisionFrom(t, agentID, dmA, dmA, "source a", "Decided in DM A.", "", false)
	askedInAAnsweredInB := recordDecisionFrom(t, agentID, dmA, dmB, "asked a answered b", "Decided in DM B only.", "", false)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(dmA), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	requireDecisions(t, "DM A row", summary.Content, inA)
	if strings.Contains(summary.Content, decisionWords(askedInAAnsweredInB)) {
		t.Fatalf("DM A lists an answer sourced from DM B:\n%s", summary.Content)
	}
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seed, decisionWords(askedInAAnsweredInB)) {
		t.Fatalf("DM A prompt lists an answer sourced from DM B:\n%s", seed)
	}
	_, summaryB, fail := testHandler.compactChatInPlace(ctx, parseUUID(dmB), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact B: %s", fail.message)
	}
	requireDecisions(t, "DM B row", summaryB.Content, askedInAAnsweredInB)
}

func decisionWords(d recordedDecision) string { return d.words }

// A card answer with a chosen option and a note carries both.
func TestCompactionDecisionsKeepCardOptionAndNote(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-card", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	card := recordDecisionFrom(t, agentID, sessionID, sessionID, "card with note", "Option B\n\nbut cap retries at 3", "but cap retries at 3", true)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
		requireDecisions(t, where, text, card)
		if !strings.Contains(text, "Option B") {
			t.Fatalf("%s lacks the chosen option:\n%s", where, text)
		}
	}
}

// A decision withdrawn after the compaction must not reach a fresh session as a
// standing record: the seed reconciles against the live records.
func TestCompactionSeedDropsDecisionsWithdrawnAfterCompaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-withdrawn", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	kept := recordDecision(t, agentID, sessionID, "still standing", "Keep the cache layer.", false)
	revoked := recordDecision(t, agentID, sessionID, "later withdrawn", "Approve the schema drop.", false)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	requireDecisions(t, "row at compaction", summary.Content, kept, revoked)
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, revoked.answerID)
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, "fresh session prompt", seed, kept)
	if strings.Contains(seed, revoked.words) || strings.Contains(seed, revoked.answerID) {
		t.Fatalf("fresh session prompt still lists the withdrawn decision as standing:\n%s", seed)
	}
}

// A provider token inside a decision's words is redacted in the live block the
// seed rebuilds, as it is in the stored divider row.
func TestCompactionSeedRedactsDecisionWords(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-redact", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	token := "ghp_" + strings.Repeat("a1B2c3D4e5", 3) + "a1B2c3"
	recordDecision(t, agentID, sessionID, "token decision", "Use this token "+token+" for the deploy.", false)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
		if strings.Contains(text, token) {
			t.Fatalf("%s carries a provider token:\n%s", where, text)
		}
	}
}

// A private key whose end marker lies past the retrieval cut is still redacted:
// the cut text holds an unterminated block, and everything from its begin
// marker on goes. The key is built at runtime so no key literal sits in the repo.
func TestCompactionRedactsPrivateKeyCrossingTheRetrievalCut(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-pem", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	label := "RSA " + "PRIVATE " + "KEY"
	body := strings.Repeat("KEYBODYSENTINEL0123456789abcdef", 200) // 6200 chars > decisionsWordsFetchChars
	key := "-----BEGIN " + label + "-----\n" + body + "\n-----END " + label + "-----"
	recordDecision(t, agentID, sessionID, "key decision", "Deploy with this key:\n"+key+"\nthen rotate it.", false)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
		if strings.Contains(text, "KEYBODYSENTINEL") {
			t.Fatalf("%s carries private key material:\n%.600s", where, text)
		}
	}
}

// A credential cut in two by the retrieval limit is not shown as a prefix:
// redacting the earlier tokens shrinks the text, which would pull the partial
// credential at the cut inside the 2 KiB window. The key is built at runtime.
func TestCompactionDropsAPartialCredentialAtTheRetrievalCut(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-split", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	longToken := "ghp_" + strings.Repeat("a1B2c3D4e5", 25) // 254 chars, redacted to a short marker
	prefix := strings.Repeat(longToken+" ", 15)            // shrinks a lot once redacted
	prefix += strings.Repeat(" ", decisionsWordsFetchChars-12-len(prefix))
	awsKey := "AK" + "IA" + strings.Repeat("ZQ", 8) // 20 chars: 12 of them land before the cut
	words := prefix + awsKey + " and the rest of the message."
	recordDecision(t, agentID, sessionID, "split decision", words, false)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
		if strings.Contains(text, awsKey[:12]) {
			t.Fatalf("%s carries a partial credential:\n%.900s", where, text)
		}
	}
}

// The redact rules read only ASCII whitespace as a word boundary, so a password
// that holds a no-break space is still one credential to them. A cut that ends
// the text at that space would leave the front of the password without its "@".
func TestCompactionKeepsAConnectionCredentialWholeAtTheRetrievalCut(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-nbsp", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	credential := "postgres" + "://" + "svc:" + "pw" + "\u00a0" + "tail@db.internal/app"
	words := "Use this: " + credential + strings.Repeat("z", decisionsWordsFetchChars)
	recordDecision(t, agentID, sessionID, "nbsp decision", words, false)
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
		if strings.Contains(text, "svc:pw") {
			t.Fatalf("%s carries the front of a connection credential:\n%.900s", where, text)
		}
	}
}

// Enough decisions to pass the block budget: the newest stay in full, older ones
// collapse to one line that still names key, time and record id, and a count
// line says so. Nothing is dropped silently, and the block stays bounded.
func TestCompactionDecisionsOverflowCollapsesOlderOnes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-overflow", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	const total = 150
	var all []recordedDecision
	for index := 0; index < total; index++ {
		all = append(all, recordDecision(t, agentID, sessionID, fmt.Sprintf("overflow %d", index), fmt.Sprintf("Decision %03d: %s", index, strings.Repeat("words ", 70)), false))
	}
	for round := 1; round <= 2; round++ {
		session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
		if fail != nil {
			t.Fatalf("compact round %d: %s", round, fail.message)
		}
		seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
			requireDecisionRefs(t, where, text, all...)
			requireDecisions(t, where+" newest", text, all[total-1])
			if strings.Contains(text, strconv.Quote(all[0].words)) {
				t.Fatalf("%s round %d keeps the oldest decision in full; the block has no budget", where, round)
			}
			if !strings.Contains(text, " older decisions collapsed") {
				t.Fatalf("%s round %d does not say how many decisions it collapsed", where, round)
			}
			head, _, _ := strings.Cut(text, decisionsBlockEnd)
			if len(head) > 2*decisionsBlockBudget {
				t.Fatalf("%s round %d block is %d bytes", where, round, len(head))
			}
		}
	}
}

// Far more decisions than fit even as one-line references: the block stays hard
// bounded (full lines, then references, each under its own budget), and a count
// line names how many decisions it did not list, so every omission is stated
// and full lines + reference lines + the count add up to every record. Retrieval
// is bounded too: 600 records exceed the row limit and are still all accounted
// for (CHE-4447, decision: hard-bound references).
func TestCompactionDecisionsHardBoundTheReferences(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions-hardbound", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	const total = 600
	var all []recordedDecision
	for index := 0; index < total; index++ {
		all = append(all, recordDecision(t, agentID, sessionID, fmt.Sprintf("hardbound %d", index), fmt.Sprintf("Decision %03d: %s", index, strings.Repeat("words ", 70)), false))
	}
	for round := 1; round <= 2; round++ {
		session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Nothing kept.")
		if fail != nil {
			t.Fatalf("compact round %d: %s", round, fail.message)
		}
		seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		for where, text := range map[string]string{"row": summary.Content, "prompt": seed} {
			requireDecisions(t, where+" newest", text, all[total-1])
			block, _, _ := strings.Cut(text, decisionsBlockEnd)
			if limit := decisionsBlockBudget + decisionsRefsBudget + 1<<10; len(block) > limit {
				t.Fatalf("%s round %d block is %d bytes, over the hard bound %d", where, round, len(block), limit)
			}
			listed := strings.Count(block, "\n- ") // one per decision line
			var unlisted int
			marker := regexp.MustCompile(`- (\d+) older decisions not listed`)
			match := marker.FindStringSubmatch(block)
			if match == nil {
				t.Fatalf("%s round %d does not say how many decisions it left unlisted", where, round)
			}
			unlisted, _ = strconv.Atoi(match[1])
			// Lines that begin "- CHE" are decisions; the two count lines are not.
			keyPrefix, _, _ := strings.Cut(all[0].key, "-")
			decisionLines := strings.Count(block, "\n- "+keyPrefix+"-")
			if decisionLines+unlisted != total || unlisted == 0 {
				t.Fatalf("%s round %d accounts for %d listed + %d unlisted, want %d (lines %d)", where, round, decisionLines, unlisted, total, listed)
			}
		}
	}
}
