ALTER TABLE repo_tags DROP CONSTRAINT repo_tags_pkey;
ALTER TABLE repo_tags ADD PRIMARY KEY (org_repo_name, tag_name);
ALTER TABLE repo_tags ALTER COLUMN module_path DROP NOT NULL;
