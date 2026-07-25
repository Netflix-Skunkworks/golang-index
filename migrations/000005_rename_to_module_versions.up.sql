-- A row is a module version — a module_path at a version, where the version is a
-- git tag or a synthesized pseudo-version — not just a tag. Rename the table and
-- column (and their indexes) to match. All of these are metadata-only changes.
ALTER TABLE repo_tags RENAME TO repo_module_versions;
ALTER TABLE repo_module_versions RENAME COLUMN tag_name TO version;
ALTER INDEX repo_tags_pkey RENAME TO repo_module_versions_pkey;
ALTER INDEX repo_tags_indexed_at_idx RENAME TO repo_module_versions_indexed_at_idx;
