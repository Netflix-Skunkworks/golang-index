-- Serves the feed's lookup of the earliest indexed_at among the rows sharing a
-- (module_path, version). Without it, that lookup scans the table.
CREATE INDEX repo_module_versions_first_seen_idx
ON repo_module_versions (module_path, version, indexed_at);
