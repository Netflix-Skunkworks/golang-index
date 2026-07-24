-- A repo with no semver tags gets one HEAD pseudo-version per module, and a
-- pseudo-version is derived from the commit rather than the module path, so two
-- v0 modules in the same repo share an identical version string. Keying repo_tags
-- on (org_repo_name, tag_name) alone made those rows collide; add module_path to
-- the key so a repo's modules can share a version.
ALTER TABLE repo_tags ALTER COLUMN module_path SET NOT NULL;
ALTER TABLE repo_tags DROP CONSTRAINT repo_tags_pkey;
ALTER TABLE repo_tags ADD PRIMARY KEY (org_repo_name, module_path, tag_name);
