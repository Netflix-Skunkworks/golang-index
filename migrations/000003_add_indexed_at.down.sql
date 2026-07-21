DROP INDEX IF EXISTS repo_tags_indexed_at_idx;

ALTER TABLE repo_tags
DROP COLUMN indexed_at;
