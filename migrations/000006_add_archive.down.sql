DROP INDEX IF EXISTS idx_nodes_archive_folder;
DROP INDEX IF EXISTS idx_nodes_original_parent;
ALTER TABLE nodes DROP COLUMN delete_skipped;
ALTER TABLE nodes DROP COLUMN archive_folder_drive_id;
ALTER TABLE nodes DROP COLUMN original_parent_drive_id;
