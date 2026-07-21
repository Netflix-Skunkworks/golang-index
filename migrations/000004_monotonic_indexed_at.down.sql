ALTER TABLE repo_tags ALTER COLUMN indexed_at SET DEFAULT (now() AT TIME ZONE 'utc');
