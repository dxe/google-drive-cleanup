# Changing the database schema

The schema is defined by [golang-migrate](https://github.com/golang-migrate/migrate) SQL files in this directory (`migrations/`). `openDB` (in [db.go](../db.go)) applies all pending migrations on every open via `migrateDB`, so any DB the tool touches — fresh or existing — is auto-upgraded to the latest version.

To change the schema, add a new **pair** of files (never edit old ones):

```
migrations/000002_<short_snake_case_description>.up.sql    -- forward change
migrations/000002_<short_snake_case_description>.down.sql  -- exact inverse
```

Rules:
- **Number** = previous migration's number + 1, zero-padded to 6 digits. Both files in a pair share the same number and description.
- `.up.sql` makes the change (`ALTER TABLE`, `CREATE TABLE`, `CREATE INDEX`, …). `.down.sql` reverses it precisely (a new table → `DROP TABLE`; a new column → recreate the table without it, since SQLite lacks `DROP COLUMN` in older forms — prefer 12-step table rebuild if needed).
- After writing them, run `go test ./...`: tests open fresh DBs through `openDB`, so a broken migration fails the suite immediately.
- Migration bookkeeping lives in the `schema_migrations` table (managed by golang-migrate); leave it alone.

If you change a column that Go structs or queries reference (e.g. in [db.go](../db.go), [crawl.go](../crawl.go)), update that code in the same change.
