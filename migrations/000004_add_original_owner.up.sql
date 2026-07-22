-- original_owner_id / original_owner_display_name record who owned a node before
-- its ownership was handed to one of the configured
-- migration.ownership-transfer-accounts. `crawl` keeps these pointed at the most
-- recent owner that is NOT a transfer account, so once a file is transferred to a
-- transfer account the original owner stays frozen at the last real owner. They
-- are seeded here to the current owner so an existing snapshot need not be
-- re-crawled to gain a value.
ALTER TABLE nodes ADD COLUMN original_owner_id TEXT;
ALTER TABLE nodes ADD COLUMN original_owner_display_name TEXT;
UPDATE nodes SET original_owner_id = owner_id, original_owner_display_name = owner_display_name;

-- manual_transfer_performed is 1 once a node has been crawled while owned by one
-- of migration.manual-ownership-transfer-accounts. A manual ownership transfer
-- through such an account permanently bumps the file's modifiedTime, so this flag
-- marks that the top-level modifiedTime is superfluously new and the last real
-- edit must be read from the most recent revision instead (the transfer itself
-- does not create a revision). Once set it is never
-- cleared, since the modifiedTime bump is permanent. Defaults to 0 (no manual
-- transfer observed).
ALTER TABLE nodes ADD COLUMN manual_transfer_performed INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_nodes_manual_transfer ON nodes(manual_transfer_performed);
