package handler

import (
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// Typo-tolerant fallback for issue search (CHE-2944). It runs only when the
// normal LIKE query finds nothing on the first page, so the common path pays
// nothing. Comments are left out: scanning every comment with trigram scoring
// costs far more than the title/description pass.

// fuzzySearchThreshold is the minimum pg_trgm word_similarity for a match.
// Lower values admit unrelated words; higher values reject one-letter typos in
// short words.
const fuzzySearchThreshold = 0.4

// fuzzySearchMinRunes is the shortest folded query that gets a fallback. Two
// letters share too few trigrams to score meaningfully.
const fuzzySearchMinRunes = 3

// fuzzyDescriptionRunes is how much of each description the fallback scores:
// the first 200 runes. A typo in text past that window is not found by the
// fallback; the normal query still searches the whole description. Scoring
// whole descriptions took 4.6-10 s over 8,003 issues averaging 2,970 characters
// (GIN trigram index included); the 200-rune window took 684-703 ms over 8,000
// issues averaging 2,104 characters (CHE-2944). Widen it only with a new measurement.
const fuzzyDescriptionRunes = 200

// shouldRunFuzzyFallback reports whether a search that found nothing should be
// retried with typo tolerance.
func shouldRunFuzzyFallback(query string, hasNum bool, offset, normalResults int) bool {
	return normalResults == 0 && offset == 0 && !hasNum &&
		utf8.RuneCountInString(foldDiacritics(query)) >= fuzzySearchMinRunes
}

// buildFuzzySearchQuery scores folded title and description against the folded
// query with word_similarity and returns the same columns as buildSearchQuery,
// so the caller scans both with scanSearchIssue. Args: $1 folded query,
// $2 workspace_id (filled by the caller), $3 threshold, [$4 terminal status
// keys], then limit and offset (the last two, filled by the caller).
//
// projectID narrows to one project and hiddenProjectIDs are the private
// projects the caller cannot see, as in buildSearchQuery: the typo fallback must
// not return an issue the exact search withholds.
func buildFuzzySearchQuery(foldedQuery string, includeClosed bool, terminalStatusKeys []string, projectID pgtype.UUID, hiddenProjectIDs ...pgtype.UUID) (string, []any) {
	args := []any{foldedQuery, nil, fuzzySearchThreshold}
	nextArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	where := "i.workspace_id = $2"
	if !includeClosed {
		where += fmt.Sprintf(" AND NOT (i.status = ANY(%s::text[]))", nextArg(terminalStatusKeys))
	}
	if projectID.Valid {
		where += " AND i.project_id = " + nextArg(projectID) + "::uuid"
	}
	if len(hiddenProjectIDs) > 0 {
		where += " AND " + hiddenProjectPredicate("i.project_id", nextArg(hiddenProjectIDs))
	}
	limitParam := nextArg(nil)
	offsetParam := nextArg(nil)

	query := fmt.Sprintf(`WITH scored AS (
		SELECT i.id, i.updated_at,
			word_similarity($1, %[1]s) AS title_score,
			word_similarity($1, %[2]s) AS description_score
		FROM issue i
		JOIN issue_search_text ist ON ist.issue_id = i.id AND ist.workspace_id = i.workspace_id
		WHERE %[3]s
	),
	page_candidates AS MATERIALIZED (
		SELECT id, updated_at, title_score, description_score,
			GREATEST(title_score, description_score) AS score
		FROM scored
		WHERE title_score >= $3 OR description_score >= $3
		ORDER BY score DESC, updated_at DESC, id ASC
		LIMIT %[4]s OFFSET %[5]s
	)
	SELECT i.id, i.workspace_id, i.title, i.description, i.status, i.priority,
		i.assignee_type, i.assignee_id, i.creator_type, i.creator_id,
		i.parent_issue_id, i.acceptance_criteria, i.context_refs, i.position,
		i.start_date, i.due_date, i.created_at, i.updated_at, i.last_activity_at, i.number, i.project_id,
		i.revision, i.duplicate_of_issue_id, i.description_external_read,
		CASE WHEN pc.title_score >= $3 THEN 'title' ELSE 'description' END AS match_source,
		'' AS matched_comment_content
	FROM page_candidates pc
	JOIN issue i ON i.id = pc.id AND i.workspace_id = $2
	ORDER BY pc.score DESC, pc.updated_at DESC, pc.id ASC`,
		"ist.title_folded", fmt.Sprintf("left(ist.description_folded, %d)", fuzzyDescriptionRunes), where, limitParam, offsetParam)

	return query, args
}
