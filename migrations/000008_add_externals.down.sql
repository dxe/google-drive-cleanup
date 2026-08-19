ALTER TABLE pruned_nodes DROP COLUMN evicted_from_drive_id;
ALTER TABLE pruned_nodes DROP COLUMN externals_folder_drive_id;
DROP INDEX IF EXISTS idx_nodes_evicted_from;
DROP INDEX IF EXISTS idx_nodes_externals_folder;
ALTER TABLE nodes DROP COLUMN evicted_from_drive_id;
ALTER TABLE nodes DROP COLUMN externals_folder_drive_id;
