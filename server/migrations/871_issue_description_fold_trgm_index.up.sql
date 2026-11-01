-- Typo-tolerant issue search (CHE-2944) matches descriptions with
-- `query <% folded description`. The expression is the Vietnamese diacritic fold
-- from foldSQL in internal/handler/search_fuzzy.go; a test fails if the two
-- drift apart. Single-statement file: CREATE INDEX CONCURRENTLY cannot share a
-- transaction with other commands.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_description_fold_trgm
    ON issue USING gin (translate(LOWER(COALESCE(description, '')), 'àáảãạăằắẳẵặâầấẩẫậèéẻẽẹêềếểễệìíỉĩịòóỏõọôồốổỗộơờớởỡợùúủũụưừứửữựỳýỷỹỵđ', 'aaaaaaaaaaaaaaaaaeeeeeeeeeeeiiiiiooooooooooooooooouuuuuuuuuuuyyyyyd') gin_trgm_ops);
