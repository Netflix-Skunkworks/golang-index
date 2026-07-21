-- 000003 stamped every row with one identical indexed_at, stalling the feed cursor
-- (it can't advance past rows sharing a value). Spread the existing rows, and
-- default new rows to clock_timestamp() (per-row) rather than now() (per-txn).
WITH ordered AS (
    SELECT org_repo_name, tag_name,
           row_number() OVER (ORDER BY created, org_repo_name, tag_name) AS rn
    FROM repo_tags
)
UPDATE repo_tags rt
SET indexed_at = (now() AT TIME ZONE 'utc') + (ordered.rn - 1) * interval '1 microsecond'
FROM ordered
WHERE rt.org_repo_name = ordered.org_repo_name AND rt.tag_name = ordered.tag_name;

ALTER TABLE repo_tags ALTER COLUMN indexed_at SET DEFAULT (clock_timestamp() AT TIME ZONE 'utc');
