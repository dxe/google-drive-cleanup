-- original_owner_email is the email counterpart of original_owner_id /
-- original_owner_display_name added in 000004: the email of the last owner
-- before the file was handed to a migration.ownership-transfer account. Kept in
-- lock-step with the other two by `crawl` (frozen once ownership reaches a
-- transfer account, refreshed while a real owner holds the file). Seeded here to
-- the current owner_email so an existing snapshot need not be re-crawled.
ALTER TABLE nodes ADD COLUMN original_owner_email TEXT;
UPDATE nodes SET original_owner_email = owner_email;
