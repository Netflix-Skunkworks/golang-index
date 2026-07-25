ALTER INDEX repo_module_versions_indexed_at_idx RENAME TO repo_tags_indexed_at_idx;
ALTER INDEX repo_module_versions_pkey RENAME TO repo_tags_pkey;
ALTER TABLE repo_module_versions RENAME COLUMN version TO tag_name;
ALTER TABLE repo_module_versions RENAME TO repo_tags;
