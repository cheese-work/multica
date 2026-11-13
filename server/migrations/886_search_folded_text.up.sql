-- Issue search matches Vietnamese text with or without diacritics (CHE-2943).
-- The folded text is stored beside the source rows, so a search compares plain
-- LIKE patterns against it and folds nothing per query. Side tables keep the
-- wide folded copy out of SELECT * on issue and comment. The mapping literals
-- mirror foldFrom/foldTo in server/internal/handler/search_fold.go.

CREATE TABLE issue_search_text (
    issue_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    title_folded text NOT NULL,
    description_folded text NOT NULL,
    -- Lowered, unfolded text, only where folding changed it. An accented query
    -- ranks exact-diacritic matches on it without reading the issue table.
    title_accents text,
    description_accents text
);

CREATE TABLE comment_search_text (
    comment_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    content_folded text NOT NULL,
    -- Lowered, unfolded text, only where folding changed it. An accented query
    -- ranks exact-diacritic matches on it without reading the comment table.
    content_accents text
);

CREATE FUNCTION search_fold(value text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT translate(lower(COALESCE(value, '')), 'àáâãäåçèéêëìíîïñòóôõöùúûüýÿāăąćĉċčďēĕėęěĝğġģĥĩīĭįĵķĺļľńņňōŏőŕŗřśŝşšţťũūŭůűųŵŷźżžơưạảấầẩẫậắằẳẵặẹẻẽếềểễệỉịọỏốồổỗộớờởỡợụủứừửữựỳỵỷỹđÀÁÂÃÄÅÇÈÉÊËÌÍÎÏÑÒÓÔÕÖÙÚÛÜÝĀĂĄĆĈĊČĎĒĔĖĘĚĜĞĠĢĤĨĪĬĮİĴĶĹĻĽŃŅŇŌŎŐŔŖŘŚŜŞŠŢŤŨŪŬŮŰŲŴŶŸŹŻŽƠƯẠẢẤẦẨẪẬẮẰẲẴẶẸẺẼẾỀỂỄỆỈỊỌỎỐỒỔỖỘỚỜỞỠỢỤỦỨỪỬỮỰỲỴỶỸĐ', 'aaaaaaceeeeiiiinooooouuuuyyaaaccccdeeeeegggghiiiijklllnnnooorrrssssttuuuuuuwyzzzouaaaaaaaaaaaaeeeeeeeeiioooooooooooouuuuuuuyyyydaaaaaaceeeeiiiinooooouuuuyaaaccccdeeeeegggghiiiiijklllnnnooorrrssssttuuuuuuwyyzzzouaaaaaaaaaaaaeeeeeeeeiioooooooooooouuuuuuuyyyyd')
$$;

CREATE FUNCTION issue_search_text_sync() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lowered_title text := lower(COALESCE(NEW.title, ''));
    lowered_description text := lower(COALESCE(NEW.description, ''));
    folded_title text := search_fold(NEW.title);
    folded_description text := search_fold(NEW.description);
BEGIN
    INSERT INTO issue_search_text (issue_id, workspace_id, title_folded, description_folded, title_accents, description_accents)
    VALUES (NEW.id, NEW.workspace_id, folded_title, folded_description,
        CASE WHEN lowered_title <> folded_title THEN lowered_title END,
        CASE WHEN lowered_description <> folded_description THEN lowered_description END)
    ON CONFLICT (issue_id) DO UPDATE SET
        workspace_id = EXCLUDED.workspace_id,
        title_folded = EXCLUDED.title_folded,
        description_folded = EXCLUDED.description_folded,
        title_accents = EXCLUDED.title_accents,
        description_accents = EXCLUDED.description_accents;
    RETURN NULL;
END
$$;

CREATE FUNCTION comment_search_text_sync() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lowered text := lower(COALESCE(NEW.content, ''));
    folded text := search_fold(NEW.content);
BEGIN
    INSERT INTO comment_search_text (comment_id, workspace_id, issue_id, created_at, content_folded, content_accents)
    VALUES (NEW.id, NEW.workspace_id, NEW.issue_id, NEW.created_at, folded,
        CASE WHEN lowered <> folded THEN lowered END)
    ON CONFLICT (comment_id) DO UPDATE SET
        workspace_id = EXCLUDED.workspace_id,
        issue_id = EXCLUDED.issue_id,
        created_at = EXCLUDED.created_at,
        content_folded = EXCLUDED.content_folded,
        content_accents = EXCLUDED.content_accents;
    RETURN NULL;
END
$$;

CREATE FUNCTION issue_search_text_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM issue_search_text WHERE issue_id = OLD.id;
    RETURN NULL;
END
$$;

CREATE FUNCTION comment_search_text_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM comment_search_text WHERE comment_id = OLD.id;
    RETURN NULL;
END
$$;

CREATE TRIGGER issue_search_text_sync
AFTER INSERT OR UPDATE OF title, description, workspace_id ON issue
FOR EACH ROW EXECUTE FUNCTION issue_search_text_sync();

CREATE TRIGGER comment_search_text_sync
AFTER INSERT OR UPDATE OF content, workspace_id, issue_id, created_at ON comment
FOR EACH ROW EXECUTE FUNCTION comment_search_text_sync();

CREATE TRIGGER issue_search_text_delete
AFTER DELETE ON issue
FOR EACH ROW EXECUTE FUNCTION issue_search_text_delete();

CREATE TRIGGER comment_search_text_delete
AFTER DELETE ON comment
FOR EACH ROW EXECUTE FUNCTION comment_search_text_delete();
