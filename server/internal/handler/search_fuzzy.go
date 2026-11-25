package handler

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// Typo-tolerant fallback for issue search (CHE-2944, CHE-5869). It runs only
// when the normal LIKE query finds nothing on the first page, so the common path
// pays nothing. It searches the title, the whole description and comments through
// the GIN trigram indexes of migrations 893-895: the index-backed operator
// `$1 <% column` picks candidates, and word_similarity() scores only those.

// fuzzySearchThreshold is the minimum pg_trgm word_similarity for a match.
// Lower values admit unrelated words; higher values reject one-letter typos in
// short words.
const fuzzySearchThreshold = 0.4

// fuzzySearchTimeout bounds the fallback (CHE-7519). It is best-effort: the
// normal search already answered, and a long query made of very common tokens
// (an owner/repo name) has trigrams that nearly every comment shares, so the
// scan can outlast the whole request budget. On timeout the caller keeps the
// empty normal result instead of failing the request with a 503.
const fuzzySearchTimeout = time.Second

// fuzzySearchSetup is run with SET LOCAL in the fallback's transaction: the
// operator `<%` compares against pg_trgm.word_similarity_threshold, which has
// no per-call argument, and the fallback gets its own, shorter statement
// timeout. The values are package constants, never user input.
var fuzzySearchSetup = []string{
	fmt.Sprintf("SET LOCAL pg_trgm.word_similarity_threshold = %v", fuzzySearchThreshold),
	fmt.Sprintf("SET LOCAL statement_timeout = %d", fuzzySearchTimeout/time.Millisecond),
}

// fuzzySearchMinRunes is the shortest folded query that gets a fallback. Two
// letters share too few trigrams to score meaningfully.
const fuzzySearchMinRunes = 3

// shouldRunFuzzyFallback reports whether a search that found nothing should be
// retried with typo tolerance.
func shouldRunFuzzyFallback(query string, hasNum bool, offset, normalResults int) bool {
	return normalResults == 0 && offset == 0 && !hasNum &&
		utf8.RuneCountInString(foldDiacritics(query)) >= fuzzySearchMinRunes
}

// buildFuzzySearchQuery matches the folded query against title, description and
// comment text and returns the same columns as buildSearchQuery, so the caller
// scans both with scanSearchIssue. Run it with fuzzySearchSetup. Ranking: a title
// match ranks above a description or comment match; inside that split by score,
// updated_at, id. match_source is the best-scoring field of the issue, kept
// apart from the ranking. Args: $1 folded query, $2 workspace_id (filled by the caller), [terminal
// status keys], [hidden project ids], [project filter], then limit and offset (the last two,
// filled by the caller). The project filter and the hidden projects are applied in every
// candidate arm, before the page LIMIT, exactly like buildSearchQuery: a typo must not reach
// an issue the exact query hides or one outside the requested project.
func buildFuzzySearchQuery(foldedQuery string, includeClosed bool, terminalStatusKeys []string, projectID pgtype.UUID, hiddenProjectIDs ...pgtype.UUID) (string, []any) {
	args := []any{foldedQuery, nil}
	nextArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	openIssue := ""
	if !includeClosed {
		openIssue = fmt.Sprintf(" AND NOT (i.status = ANY(%s::text[]))", nextArg(terminalStatusKeys))
	}
	if len(hiddenProjectIDs) > 0 {
		openIssue += " AND " + hiddenProjectPredicate("i.project_id", nextArg(hiddenProjectIDs))
	}
	if projectID.Valid {
		openIssue += fmt.Sprintf(" AND i.project_id = %s", nextArg(projectID))
	}
	limitParam := nextArg(nil)
	offsetParam := nextArg(nil)

	query := fmt.Sprintf(`WITH hits AS (
		SELECT i.id, i.updated_at, 0 AS class, 0 AS src, word_similarity($1, ist.title_folded) AS score, NULL::uuid AS comment_id
		FROM issue_search_text ist
		JOIN issue i ON i.id = ist.issue_id AND i.workspace_id = ist.workspace_id
		WHERE ist.workspace_id = $2 AND $1 <%% ist.title_folded%[1]s
		UNION ALL
		SELECT i.id, i.updated_at, 1, 1, word_similarity($1, ist.description_folded), NULL::uuid
		FROM issue_search_text ist
		JOIN issue i ON i.id = ist.issue_id AND i.workspace_id = ist.workspace_id
		WHERE ist.workspace_id = $2 AND $1 <%% ist.description_folded%[1]s
		UNION ALL
		SELECT i.id, i.updated_at, 1, 2, word_similarity($1, cst.content_folded), cst.comment_id
		FROM comment_search_text cst
		JOIN issue i ON i.id = cst.issue_id AND i.workspace_id = cst.workspace_id
		WHERE cst.workspace_id = $2 AND $1 <%% cst.content_folded%[1]s
	),
	best AS (
		SELECT DISTINCT ON (id) id, updated_at, class, src, score, comment_id
		FROM hits
		ORDER BY id, class, score DESC, src, comment_id
	),
	page_candidates AS MATERIALIZED (
		SELECT id, updated_at, class, src, score, comment_id
		FROM best
		ORDER BY class, score DESC, updated_at DESC, id ASC
		LIMIT %[2]s OFFSET %[3]s
	)
	SELECT i.id, i.workspace_id, i.title, i.description, i.status, i.priority,
		i.assignee_type, i.assignee_id, i.creator_type, i.creator_id, i.requester_user_id,
		i.parent_issue_id, i.acceptance_criteria, i.context_refs, i.position,
		i.start_date, i.due_date, i.created_at, i.updated_at, i.last_activity_at, i.number, i.project_id,
		i.revision, i.duplicate_of_issue_id, i.description_external_read,
		(ARRAY['title', 'description', 'comment'])[pc.src + 1] AS match_source,
		COALESCE(c.content, '') AS matched_comment_content
	FROM page_candidates pc
	JOIN issue i ON i.id = pc.id AND i.workspace_id = $2
	LEFT JOIN comment c ON c.id = pc.comment_id AND c.workspace_id = $2
	ORDER BY pc.class, pc.score DESC, pc.updated_at DESC, pc.id ASC`,
		openIssue, limitParam, offsetParam)

	return query, args
}
