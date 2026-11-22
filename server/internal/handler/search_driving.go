package handler

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Driving-term choice for multi-term issue search (CHE-7519). Every eligible
// issue holds every term, so any long term can drive the candidate scan, and the
// cost of the whole query follows how many issues and comments hold that term.
// The longest term is a poor guess: an owner/repo name is long and sits in most
// comments, which made the "driving" list the whole workspace. The probe counts,
// up to a cap, how many rows hold each term and the rarest one drives.

const (
	// drivingProbeCap bounds the rows counted per term. A term that reaches it is
	// common enough that it never beats a rarer one.
	drivingProbeCap = 200
	// drivingProbeTimeout bounds the probe. It is best-effort: on any failure the
	// longest term drives, as before the probe existed.
	drivingProbeTimeout = 500 * time.Millisecond
)

var drivingProbeSetup = []string{
	fmt.Sprintf("SET LOCAL statement_timeout = %d", drivingProbeTimeout/time.Millisecond),
}

// buildDrivingProbeQuery counts the comments and descriptions that hold each
// term, capped at drivingProbeCap, in one statement. It returns one column per
// term in terms, in order. $1 is the workspace id; the caller fills args[0].
func buildDrivingProbeQuery(terms []string) (string, []any) {
	args := []any{nil}
	counts := make([]string, len(terms))
	for i, term := range terms {
		args = append(args, "%"+escapeLike(foldDiacritics(term))+"%")
		p := fmt.Sprintf("$%d", len(args))
		counts[i] = fmt.Sprintf(`(SELECT count(*) FROM (
			SELECT 1 FROM (
				SELECT 1 FROM comment_search_text WHERE workspace_id = $1 AND content_folded LIKE %[1]s
				UNION ALL
				SELECT 1 FROM issue_search_text WHERE workspace_id = $1 AND description_folded LIKE %[1]s
			) held LIMIT %[2]d) capped)`, p, drivingProbeCap)
	}
	return "SELECT " + strings.Join(counts, ", "), args
}

// pickDrivingTerm returns the index of the term to drive the candidate scan, or
// -1 to keep the longest-term default. Only terms of 3 or more characters can
// drive, and the probe is skipped when fewer than two of them exist.
func (h *Handler) pickDrivingTerm(ctx context.Context, wsUUID pgtype.UUID, terms []string) int {
	var eligible []int
	for i, term := range terms {
		if utf8.RuneCountInString(foldDiacritics(term)) > 2 {
			eligible = append(eligible, i)
		}
	}
	if len(terms) < 2 || len(eligible) < 2 {
		return -1
	}
	probeTerms := make([]string, len(eligible))
	for i, idx := range eligible {
		probeTerms[i] = terms[idx]
	}
	query, args := buildDrivingProbeQuery(probeTerms)
	args[0] = wsUUID
	counts := make([]int64, len(eligible))
	err := runSearchQueryWithSetup(ctx, h.TxStarter, drivingProbeSetup, query, args, func(rows pgx.Rows) error {
		if !rows.Next() {
			return rows.Err()
		}
		dest := make([]any, len(counts))
		for i := range counts {
			dest[i] = &counts[i]
		}
		return rows.Scan(dest...)
	})
	if err != nil {
		return -1
	}
	best := 0
	for i := 1; i < len(eligible); i++ {
		// Fewer holders wins; on a tie the longer term, which has fewer trigram matches.
		if counts[i] < counts[best] ||
			(counts[i] == counts[best] &&
				utf8.RuneCountInString(probeTerms[i]) > utf8.RuneCountInString(probeTerms[best])) {
			best = i
		}
	}
	return eligible[best]
}
