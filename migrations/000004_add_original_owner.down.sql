DROP INDEX IF EXISTS idx_nodes_manual_transfer;
ALTER TABLE nodes DROP COLUMN manual_transfer_performed;
ALTER TABLE nodes DROP COLUMN original_owner_display_name;
ALTER TABLE nodes DROP COLUMN original_owner_id;
