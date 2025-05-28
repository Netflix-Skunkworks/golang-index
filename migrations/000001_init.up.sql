CREATE TABLE repo_indexing (
    -- Only the value "true" should be present. Limits the table to one row.
    id BOOL PRIMARY KEY DEFAULT TRUE,

    -- Workers should re-index the list of all repos when:
    --     NOW > indexing_finished + re-index-period, and
    --     NOW > indexing_began + indexing-ttl
    indexing_began TIMESTAMP,
    indexing_finished TIMESTAMP
);

-- Populate the initial value.
INSERT INTO repo_indexing (id, indexing_began, indexing_finished)
VALUES (TRUE, TIMESTAMP '-infinity', TIMESTAMP '-infinity')
ON CONFLICT (id) DO NOTHING;

-- A listing of all repos, and when to work on them next.
CREATE TABLE repos (
    -- Something like "corp/my-repo".
    -- A composite key of org & repo could be used, but it'd be harder to
    -- REFERENCE from the repo_tags table.
    org_repo_name VARCHAR(200) PRIMARY KEY,
    
    -- Workers should re-index a repo's tag when:
    --     NOW > indexing_finished + re-index-period, and
    --     NOW > indexing_began + indexing-ttl
    indexing_began TIMESTAMP DEFAULT TIMESTAMP '-infinity',
    indexing_finished TIMESTAMP DEFAULT TIMESTAMP '-infinity'
);

-- A listing of all tags for all repos.
CREATE TABLE repo_tags (
    org_repo_name VARCHAR(200) REFERENCES repos(org_repo_name),

    -- The tag name, ex "v0.3.0".
    tag_name VARCHAR(255) NOT NULL,
    
    -- When the tag was created.
    created TIMESTAMP NOT NULL,

    PRIMARY KEY(org_repo_name, tag_name)
);