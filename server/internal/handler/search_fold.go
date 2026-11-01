package handler

import (
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
// "dang nhap". It is the Go twin of foldSQL: the query is folded here, the
// columns in SQL, and TestFoldDiacritics_SQLParity keeps the two in step.
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

// foldSQL is the column side of the fold. unaccent() is a C hash lookup;
// a translate() table was ~25x slower on Vietnamese text (CHE-2943), so the
// unaccent extension (migration 871) is a deliberate dependency.
func foldSQL(expr string) string {
	return "LOWER(unaccent('unaccent'::regdictionary, " + expr + "))"
}
