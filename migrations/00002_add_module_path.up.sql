-- module_path stores the path to the Go module which might be different from
-- the VCS path for the repository
ALTER TABLE repo_tags
ADD COLUMN module_path varchar(255)
DEFAULT '';
