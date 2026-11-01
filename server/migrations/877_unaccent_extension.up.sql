-- Issue search folds Vietnamese diacritics on the column side with unaccent()
-- (CHE-2943). unaccent is a trusted contrib extension, so no superuser is needed.
CREATE EXTENSION IF NOT EXISTS unaccent;
