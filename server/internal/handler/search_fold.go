package handler

import (
	"context"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// stripMarks drops combining marks after NFD, which turns every precomposed
// Vietnamese vowel into its base letter. Only đ/Đ have no decomposition.
// A transformer carries state, so every call builds its own.
func stripMarks() transform.Transformer {
	return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
}

// foldDiacritics lowercases s and strips diacritics so "Đăng Nhập" becomes
// "dang nhap". It only locates matches inside snippets: search eligibility and
// ranking fold both sides in PostgreSQL (foldQueryTexts, foldSQL), because
// unaccent also maps ligatures and symbols that NFD does not. A mismatch here
// at worst leaves a snippet unhighlighted. TestFoldDiacritics_SQLParity keeps
// the Vietnamese letters in step.
func foldDiacritics(s string) string {
	folded, _, err := transform.String(stripMarks(), s)
	if err != nil {
		folded = s
	}
	return strings.Map(func(r rune) rune {
		if r == 'đ' || r == 'Đ' {
			return 'd'
		}
		return unicode.ToLower(r)
	}, folded)
}

// foldSQL is the column side of the fold. unaccent() is a C hash lookup, far
// cheaper than a translate() table (~25x slower on Vietnamese text, CHE-2943),
// and pure-ASCII text (octet length equals character length) skips it, so
// ASCII rows cost what the old LOWER(expr) did.
func foldSQL(expr string) string {
	return "LOWER(CASE WHEN octet_length(" + expr + ") = char_length(" + expr + ") THEN " + expr +
		" ELSE unaccent('unaccent'::regdictionary, " + expr + ") END)"
}

// foldQueryTexts folds the user's query the same way the columns are folded, in
// the same database, so the two sides cannot disagree on any character.
func foldQueryTexts(ctx context.Context, db dbExecutor, texts []string) ([]string, error) {
	rows, err := db.Query(ctx, "SELECT "+foldSQL("t")+" FROM unnest($1::text[]) WITH ORDINALITY AS u(t, n) ORDER BY n", texts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	folded := make([]string, 0, len(texts))
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		folded = append(folded, s)
	}
	return folded, rows.Err()
}
