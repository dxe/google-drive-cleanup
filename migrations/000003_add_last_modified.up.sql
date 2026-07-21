-- last_modified estimates when a node's content was last actually changed. It
-- is populated by `crawl --update-last-modified` and is normally Drive's
-- top-level modifiedTime, but for a file owned by a configured
-- ownership-transfer account it holds the second-to-last revision's time: the
-- most recent change on such a file is most likely the ownership transfer
-- itself rather than a content edit. NULL until a crawl with that flag records
-- it. Used to decide whether a node should later be kept or deleted.
ALTER TABLE nodes ADD COLUMN last_modified TEXT;
