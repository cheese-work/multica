package handler

import "unicode"

// Diacritic folding for issue search (CHE-2943). foldFrom[i] folds to foldTo[i];
// every accented Latin letter of Latin-1, Latin Extended-A and the Vietnamese
// block, both cases, plus đ/Đ (which has no Unicode decomposition). The same two
// strings feed translate() in migration 883 (issue_search_text,
// comment_search_text), so the stored column and the Go query side cannot drift:
// TestFoldTable_MatchesMigration and TestFoldDiacritics_StoredColumnParity pin it.
const (
	foldFrom = "àáâãäåçèéêëìíîïñòóôõöùúûüýÿāăąćĉċčďēĕėęěĝğġģĥĩīĭįĵķĺļľńņňōŏőŕŗřśŝşšţťũūŭůűųŵŷźżžơưạảấầẩẫậắằẳẵặẹẻẽếềểễệỉịọỏốồổỗộớờởỡợụủứừửữựỳỵỷỹđÀÁÂÃÄÅÇÈÉÊËÌÍÎÏÑÒÓÔÕÖÙÚÛÜÝĀĂĄĆĈĊČĎĒĔĖĘĚĜĞĠĢĤĨĪĬĮİĴĶĹĻĽŃŅŇŌŎŐŔŖŘŚŜŞŠŢŤŨŪŬŮŰŲŴŶŸŹŻŽƠƯẠẢẤẦẨẪẬẮẰẲẴẶẸẺẼẾỀỂỄỆỈỊỌỎỐỒỔỖỘỚỜỞỠỢỤỦỨỪỬỮỰỲỴỶỸĐ"
	foldTo   = "aaaaaaceeeeiiiinooooouuuuyyaaaccccdeeeeegggghiiiijklllnnnooorrrssssttuuuuuuwyzzzouaaaaaaaaaaaaeeeeeeeeiioooooooooooouuuuuuuyyyydaaaaaaceeeeiiiinooooouuuuyaaaccccdeeeeegggghiiiiijklllnnnooorrrssssttuuuuuuwyyzzzouaaaaaaaaaaaaeeeeeeeeiioooooooooooouuuuuuuyyyyd"
)

var foldMap = func() map[rune]rune {
	from, to := []rune(foldFrom), []rune(foldTo)
	m := make(map[rune]rune, len(from))
	for i, r := range from {
		m[r] = to[i]
	}
	return m
}()

// foldDiacritics lowercases s and replaces each accented letter by its base
// letter, so "Đăng Nhập" becomes "dang nhap". It is the query-side twin of
// translate(lower(text), foldFrom, foldTo) in the stored search columns, and it
// also locates matches inside snippets.
func foldDiacritics(s string) string {
	out := []rune(s)
	for i, r := range out {
		if b, ok := foldMap[r]; ok {
			out[i] = b
		} else {
			out[i] = unicode.ToLower(r)
		}
	}
	return string(out)
}
