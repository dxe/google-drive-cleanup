-- decision records the cleanup plan for a node, set by the review web UI:
-- 'keep', 'delete', or '' (undecided). Invariant maintained by the review
-- server: a folder marked 'delete' has every descendant marked 'delete' too,
-- so no kept item is orphaned inside a deleted subtree.
ALTER TABLE nodes ADD COLUMN decision TEXT NOT NULL DEFAULT '' CHECK (decision IN ('', 'keep', 'delete'));
CREATE INDEX IF NOT EXISTS idx_nodes_decision ON nodes(decision);
