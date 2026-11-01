package handler

import (
	"fmt"
	"strings"
	"unicode/utf8"
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

// fuzzyDescriptionRunes caps how much of each description is scored.
// word_similarity cost grows with text length; scoring whole descriptions took
// 6.4 s over 8,000 issues averaging 2,100 characters, scoring the first 200
// runes keeps the fallback under the 1 s budget (CHE-2944).
const fuzzyDescriptionRunes = 200

// foldGroups is the single source for the diacritic fold: every rune in a
// group's letters folds to its base. The Go fold and the SQL translate() are
// both generated from it.
var foldGroups = []struct{ base, letters string }{
	{"a", "àáảãạăằắẳẵặâầấẩẫậ"},
	{"e", "èéẻẽẹêềếểễệ"},
	{"i", "ìíỉĩị"},
	{"o", "òóỏõọôồốổỗộơờớởỡợ"},
	{"u", "ùúủũụưừứửữự"},
	{"y", "ỳýỷỹỵ"},
	{"d", "đ"},
}

var foldReplacer, foldSQLFrom, foldSQLTo = buildFold()

func buildFold() (*strings.Replacer, string, string) {
	var pairs []string
	var from, to strings.Builder
	for _, group := range foldGroups {
		for _, r := range group.letters {
			pairs = append(pairs, string(r), group.base)
			from.WriteRune(r)
			to.WriteString(group.base)
		}
	}
	return strings.NewReplacer(pairs...), from.String(), to.String()
}

// foldSearchText lowercases s and folds Vietnamese diacritics to base letters.
func foldSearchText(s string) string {
	return foldReplacer.Replace(strings.ToLower(s))
}

// foldSQL returns the SQL expression equivalent to foldSearchText(expr). The
// mapping strings are generated from foldGroups and contain no quotes.
func foldSQL(expr string) string {
	return fmt.Sprintf("translate(LOWER(%s), '%s', '%s')", expr, foldSQLFrom, foldSQLTo)
}

// shouldRunFuzzyFallback reports whether a search that found nothing should be
// retried with typo tolerance.
func shouldRunFuzzyFallback(query string, hasNum bool, offset, normalResults int) bool {
	return normalResults == 0 && offset == 0 && !hasNum &&
		utf8.RuneCountInString(foldSearchText(query)) >= fuzzySearchMinRunes
}

// buildFuzzySearchQuery scores folded title and description against the folded
// query with word_similarity and returns the same columns as buildSearchQuery,
// so the caller scans both with scanSearchIssue. Args: $1 folded query,
// $2 workspace_id (filled by the caller), $3 threshold, [$4 terminal status
// keys], then limit and offset (the last two, filled by the caller).
func buildFuzzySearchQuery(foldedQuery string, includeClosed bool, terminalStatusKeys []string) (string, []any) {
	args := []any{foldedQuery, nil, fuzzySearchThreshold}
	nextArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	where := "i.workspace_id = $2"
	if !includeClosed {
		where += fmt.Sprintf(" AND NOT (i.status = ANY(%s::text[]))", nextArg(terminalStatusKeys))
	}
	limitParam := nextArg(nil)
	offsetParam := nextArg(nil)

	query := fmt.Sprintf(`WITH scored AS (
		SELECT i.id, i.updated_at,
			word_similarity($1, %[1]s) AS title_score,
			word_similarity($1, %[2]s) AS description_score
		FROM issue i
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
		i.revision, i.duplicate_of_issue_id,
		CASE WHEN pc.title_score >= $3 THEN 'title' ELSE 'description' END AS match_source,
		'' AS matched_comment_content
	FROM page_candidates pc
	JOIN issue i ON i.id = pc.id AND i.workspace_id = $2
	ORDER BY pc.score DESC, pc.updated_at DESC, pc.id ASC`,
		foldSQL("i.title"), foldSQL(fmt.Sprintf("left(COALESCE(i.description, ''), %d)", fuzzyDescriptionRunes)), where, limitParam, offsetParam)

	return query, args
}
