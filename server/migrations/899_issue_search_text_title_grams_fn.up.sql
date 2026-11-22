-- CHE-7422: the instant title tier of Cmd+K must find a 1 or 2 character term
-- anywhere in a title, which a trigram index cannot do. title_grams lists every
-- distinct 1-character and 2-character substring of a title, so a GIN index on it
-- answers "title contains this short term" with an array @> lookup.
CREATE FUNCTION title_grams(title text) RETURNS text[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT COALESCE(array_agg(DISTINCT gram), '{}'::text[]) FROM (
        SELECT substr(title, i, 2) AS gram FROM generate_series(1, length(title) - 1) AS i
        UNION ALL
        SELECT substr(title, i, 1) FROM generate_series(1, length(title)) AS i
    ) grams
$$;
