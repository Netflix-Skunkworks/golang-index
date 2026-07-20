-- indexed_at is when *this* index first observed a tag. The /index feed filters
-- and orders on it so it behaves like an append-only log consumers can poll
-- forward with ?since=, mirroring index.golang.org. Existing rows backfill to
-- the migration time via the DEFAULT.
ALTER TABLE repo_tags
ADD COLUMN indexed_at TIMESTAMP NOT NULL
DEFAULT (now() AT TIME ZONE 'utc');

CREATE INDEX repo_tags_indexed_at_idx ON repo_tags (indexed_at);
