-- Listing owners is its own indexing stage now: one pass enumerates every
-- repository owner on the host, and a worker per owner lists that owner's repos.
-- The single-row table that scheduled the whole-host repo sweep schedules the
-- owner pass instead, so rename it rather than seeding a second one.
ALTER TABLE repo_indexing RENAME TO owner_indexing;
ALTER INDEX repo_indexing_pkey RENAME TO owner_indexing_pkey;

-- A listing of all repository owners, and when to work on them next.
CREATE TABLE owners (
    -- A GitHub user or organization login, something like "corp". Both share one
    -- namespace on the host, so a single column suffices.
    owner_login VARCHAR(200) PRIMARY KEY,

    -- Workers should re-index an owner's repos when:
    --     NOW > indexing_finished + re-index-period, and
    --     NOW > indexing_began + indexing-ttl
    indexing_began TIMESTAMP DEFAULT TIMESTAMP '-infinity',
    indexing_finished TIMESTAMP DEFAULT TIMESTAMP '-infinity'
);

-- Serves the work queue's ORDER BY indexing_finished; without it, handing out one
-- owner sorts every owner on the host.
CREATE INDEX owners_next_work_idx ON owners (indexing_finished);
