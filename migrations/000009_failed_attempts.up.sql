-- Counting the attempts since a work item last completed one gives the indexer
-- something to give up on; see maxFailedAttempts.
--
-- A constant DEFAULT rewrites no rows, so this is safe to apply to a live table.
ALTER TABLE repos ADD COLUMN failed_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE owners ADD COLUMN failed_attempts INT NOT NULL DEFAULT 0;
