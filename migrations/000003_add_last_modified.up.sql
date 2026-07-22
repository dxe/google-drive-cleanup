-- last_modified estimates when a node's content was last actually changed. It
-- is populated by `crawl` and is normally Drive's
-- top-level modifiedTime, but for a file owned by a configured manual
-- ownership-transfer account it holds the most recent revision's time: such a
-- file's modifiedTime was bumped by the ownership transfer, which is not a
-- content edit and does not create a revision, so the newest revision is the
-- last real edit. NULL until a crawl with that flag records it. Used to decide
-- whether a node should later be kept or deleted.
ALTER TABLE nodes ADD COLUMN last_modified TEXT;
