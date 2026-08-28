DROP TABLE owners;

ALTER INDEX owner_indexing_pkey RENAME TO repo_indexing_pkey;
ALTER TABLE owner_indexing RENAME TO repo_indexing;
