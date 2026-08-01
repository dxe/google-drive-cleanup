-- original_parent_drive_id is the Drive ID of the folder an item lived in
-- before the archive command moved it into the archive tree. Non-NULL means
-- "archived": it is the restore command's target, the delete command's
-- prerequisite, and it shields the row from stale-row pruning if the archive
-- tree is ever not re-crawled. A Drive ID (not a row id) so restore still
-- works if the parent's row is later pruned.
ALTER TABLE nodes ADD COLUMN original_parent_drive_id TEXT;

-- archive_folder_drive_id (folders only) caches the Drive ID of this folder's
-- "ARCH <name>" replica inside the archive tree, so archive runs don't repeat
-- API lookups or create duplicate replicas. Cleared by the delete command when
-- the replica is pruned; a cached id that no longer exists on Drive is
-- re-created.
ALTER TABLE nodes ADD COLUMN archive_folder_drive_id TEXT;

-- delete_skipped records that the delete command skipped this archived item
-- because it is owned by an external account (re-run with --remove-unowned to
-- handle those). Cleared by restore.
ALTER TABLE nodes ADD COLUMN delete_skipped INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_nodes_original_parent ON nodes(original_parent_drive_id);
CREATE INDEX IF NOT EXISTS idx_nodes_archive_folder ON nodes(archive_folder_drive_id);
