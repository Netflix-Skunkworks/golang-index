-- A repo with no semver tags gets one HEAD pseudo-version per module, and a
-- pseudo-version is derived from the commit rather than the module path, so two
-- v0 modules in the same repo share an identical version string. Keying on
-- (org_repo_name, version) alone made those rows collide; add module_path so a
-- repo's modules can share a version.
ALTER TABLE repo_module_versions ALTER COLUMN module_path SET NOT NULL;
ALTER TABLE repo_module_versions DROP CONSTRAINT repo_module_versions_pkey;
ALTER TABLE repo_module_versions ADD PRIMARY KEY (org_repo_name, module_path, version);
