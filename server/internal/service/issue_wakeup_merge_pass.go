package service

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
)

// verdictLineRE is the contract of a signed verdict: the first line of the
// comment reads: **Verdict: WORD** ... head, then the head SHA in backticks.
// Only that line is read,
// so a quoted verdict further down never counts.
var verdictLineRE = regexp.MustCompile("^\\*\\*Verdict:\\s*([A-Za-z][A-Za-z ]*?)\\*\\*.*?\\bhead\\s+`([^`\\s]+)`")

// parseVerdict returns the verdict word and head of a first-hand verdict
// comment. A line carrying ", signed by" is a relay of someone else's verdict;
// the relaying agent did not sign it, so it is not one.
func parseVerdict(content string) (word, head string, ok bool) {
	first, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	if strings.Contains(strings.ToLower(first), "signed by") {
		return "", "", false
	}
	m := verdictLineRE.FindStringSubmatch(first)
	if m == nil {
		return "", "", false
	}
	return strings.ToUpper(strings.TrimSpace(m[1])), m[2], true
}

// LabOf names the lab behind an agent from its model, else its runtime
// provider. An agent it cannot place has no lab (""), and then it is neither a
// signer nor an author the evaluator accepts. The merge gate and reviewer
// routing share it. ponytail: closed set, add a lab here when a new provider
// joins.
func LabOf(model, provider string) string {
	if price, ok := obsmetrics.PriceForModelAlias(model); ok {
		return price.Provider
	}
	switch strings.ToLower(provider) {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "gemini", "agy":
		return "google"
	}
	return ""
}

// storedCrossLabPass derives the two PASS facts from Multica: the newest
// trusted PASS from an agent of another lab than the PR author, on the exact
// head, in a comment on the wakeup's issue. VerifiedSigners decides which PASS
// is trusted (an agent run wrote it, nobody edited it, a newer verdict has not
// withdrawn it), so the dispatch and the signer read share one filter. An
// unknown author or no qualifying signer gives no PASS. It reads one
// transaction and calls no network.
func storedCrossLabPass(ctx context.Context, tx pgx.Tx, c MergeCandidate, authorID pgtype.UUID) (*bool, *time.Time, error) {
	if !authorID.Valid {
		return nil, nil, nil
	}
	read, err := verifiedSigners(ctx, tx, c.WorkspaceID, c.IssueID, c.HeadSHA, authorID)
	if err != nil {
		return nil, nil, err
	}
	if read.PRAuthor == nil || read.PRAuthor.Lab == "" {
		return nil, nil, nil
	}
	var newest *time.Time
	for _, signer := range read.Signers {
		if signer.Lab == "" || signer.Lab == read.PRAuthor.Lab {
			continue
		}
		if newest == nil || signer.VerdictAt.After(*newest) {
			at := signer.VerdictAt
			newest = &at
		}
	}
	pass := newest != nil
	return &pass, newest, nil
}
