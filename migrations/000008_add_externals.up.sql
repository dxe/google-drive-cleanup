-- externals_folder_drive_id (folders only) caches the Drive ID of this folder's
-- replica inside the externals tree (config externals.root) -- the parallel
-- structure the evict-externals command moves externally-owned items into. It is
-- to evict-externals what archive_folder_drive_id is to archive: a cache so
-- repeat runs neither redo the lookup nor create a second replica. A cached id
-- that no longer exists on Drive is re-created.
ALTER TABLE nodes ADD COLUMN externals_folder_drive_id TEXT;

-- evicted_from_drive_id is the Drive ID of the folder an item lived in before
-- evict-externals moved it out of a subtree being prepared for a shared drive
-- and into the externals tree. Non-NULL means "evicted": it records where the
-- shortcut left behind in its place lives, and -- like original_parent_drive_id
-- -- exempts the row from stale-row pruning, so an externals tree that no crawl
-- visits (one placed outside the crawl root) does not lose its records.
ALTER TABLE nodes ADD COLUMN evicted_from_drive_id TEXT;

CREATE INDEX IF NOT EXISTS idx_nodes_externals_folder ON nodes(externals_folder_drive_id);
CREATE INDEX IF NOT EXISTS idx_nodes_evicted_from     ON nodes(evicted_from_drive_id);

-- pruned_nodes holds a nodes row verbatim (see backupNodes), so it grows the
-- same two columns.
ALTER TABLE pruned_nodes ADD COLUMN externals_folder_drive_id TEXT;
ALTER TABLE pruned_nodes ADD COLUMN evicted_from_drive_id TEXT;
