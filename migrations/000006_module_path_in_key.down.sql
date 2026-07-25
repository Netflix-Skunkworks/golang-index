ALTER TABLE repo_module_versions DROP CONSTRAINT repo_module_versions_pkey;
ALTER TABLE repo_module_versions ADD PRIMARY KEY (org_repo_name, version);
ALTER TABLE repo_module_versions ALTER COLUMN module_path DROP NOT NULL;
