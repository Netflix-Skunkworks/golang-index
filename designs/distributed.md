# Distributed, resumable work

2025-05-27

## Background

The golang-index MVP is running into GitHub rate limits as a result of frequent
restarts causing complete re-indexing and blowing our rate-limit budget. The
memory budget needed to hold the index is also a concern.

## High level design

The index will be stored in a central database. Cache freshness will be
introduced to the index at the repository level for re-indexing, rather than
re-indexing the world each time the index starts up.

All Go repos will be re-indexed daily (low amount of API calls). Each repo's
tags will be also be re-indexed daily (high amount of API calls).

Re-indexing tags for each repo will be very expensive for the API budget, even
within a single indexer (nevermind across multiple indexers in the future). So,
a work queue will be added. The indexer will take work from the queue, attempt
to work on it, and store the results. When rate limiting occurs, the indexer
will enter exponential backoff.

With this design, one dependency is added: a database. Since pkgsite already
uses a database, this is a low-cost dependency.

With this design, the two main goals are accomplished:

- Indexing is resilient to rate limiting.
  - The re-index interval is divorved from application start intervals, reducing
  rate limiting occurrence.
  - The work queue allows resuming work once rate limiting has ended.
- The index is no longer in-memory.

A bonus goal is also accomplished: the index can scale to any number of
instances, and work is automatically sharded amongst them without explicit
coordination (no leader).

## Database design

A Postgres database will be used, to make use of the existing pkgsite Postgres
dependency.

### Database schema

```sql
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
```

### Presenting the index

The index will fetch tags as follows:

```sql
SELECT org_repo_name, tag_name, created
FROM repo_tags
WHERE created >= $1 -- ?since param.
ORDER BY created DESC
LIMIT $2; -- ?limit param.
```

The selected fields are presented as `Path`, `Version`, and `Timestamp`
respectively in the JSON output.

### Re-indexing all repos

An index worker takes work from the queue:

```sql
UPDATE repo_indexing
SET indexing_began = NOW()
WHERE indexing_began + ($1 * INTERVAL '1 SECOND') < NOW() -- Re-indexing TTL.
AND indexing_finished + ($2 * INTERVAL '1 SECOND') < NOW(); -- Re-indexing period.
```

If nothing is returned, there is no work to do and the index idles until trying
again. When there is work to do, the index queries the SCM for repos. Repos are
stored with an insert that skips over conflicts:

```sql
INSERT INTO repos (org_repo_name)
VALUES ("corp/foo"), ("corp/bar")
ON CONFLICT (org_repo_name) DO NOTHING;
```

Conflicts are skipped to avoid breaking existing indexing intervals.

### Re-indexing a repo's tags

An index worker takes work from the queue:

```sql
UPDATE repos
SET indexing_began = NOW()
WHERE org_repo_name = (
    SELECT org_repo_name
    FROM repos
    WHERE indexing_began + ($1 * INTERVAL '1 SECOND') < NOW() -- Re-indexing TTL.
    AND indexing_finished + ($2 * INTERVAL '1 SECOND') < NOW() -- Re-indexing period.
    ORDER BY indexing_finished ASC
    LIMIT 1 -- We could request more, but it's simpler to do one at a time.
)
RETURNING org_repo_name;
```

The index worker gets the `org_repo_name`, goes to the SCM, fetches tags, and
stores them (detailed below) along with a new timestamp for `indexing_finished`.

If no `org_repo_name` is returned, there's no work to do and the index worker
idles for some period of time until trying again.

Repo tags are stored with an upsert:

```sql
INSERT INTO repo_tags (org_repo_name, tag_name, created)
VALUES ("corp/foo", "v0.1.0", "2023-01-01 05:06:27"),
    ("corp/foo", "v0.1.1", "2024-01-01 05:06:27"),
    ("corp/foo", "v2.0.3", "2025-01-01 05:06:27"),
ON CONFLICT (org_repo_name, tag_name) DO UPDATE
SET created = EXCLUDED.created;

-- In the same transaction, update the indexing finish timestamp.
UPDATE repos
SET indexing_finished = NOW()
WHERE org_repo_name = "corp/foo";
```
